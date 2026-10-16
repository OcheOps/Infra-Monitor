package stack

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
)

func testConfig() config.Config {
	c := config.Default()
	c.Grafana.AdminPassword = "pw-123456"
	c.Probes = []string{"https://example.com/health", "tcp://db.local:5432"}
	c.Targets = []config.Target{{Name: "web-1", Address: "10.0.0.5:9100"}, {Name: "app", Address: "app:8080", Path: "/stats"}}
	return c
}

func TestImagesCoverTemplates(t *testing.T) {
	for _, svc := range []string{"prometheus", "alertmanager", "grafana", "node-exporter", "blackbox", "loki", "fluentbit", "jaeger", "cadvisor", "kube-state-metrics"} {
		if Images[svc] == "" {
			t.Errorf("no image for %s", svc)
		}
	}
	// Every image a Kubernetes template asks for must exist.
	re := regexp.MustCompile(`index \.Images "([^"]+)"`)
	fs.WalkDir(deploy.FS, "k8s", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := deploy.FS.ReadFile(p)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if Images[m[1]] == "" {
				t.Errorf("%s uses unknown image %q", p, m[1])
			}
		}
		return nil
	})
	for name, img := range Images {
		if !regexp.MustCompile(`:[A-Za-z0-9._-]+$`).MatchString(img) || strings.HasSuffix(img, ":latest") {
			t.Errorf("%s image %q is not pinned to a version", name, img)
		}
	}
}

type amConfig struct {
	Route struct {
		Receiver string `yaml:"receiver"`
	} `yaml:"route"`
	Receivers []map[string]any `yaml:"receivers"`
}

func parseAM(t *testing.T, c config.Config) amConfig {
	t.Helper()
	out, err := AlertmanagerConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	var am amConfig
	if err := yaml.Unmarshal(out, &am); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "[[") {
		t.Errorf("unrendered placeholder:\n%s", out)
	}
	return am
}

func TestAlertmanagerNoChannels(t *testing.T) {
	am := parseAM(t, testConfig())
	if am.Route.Receiver != "default" || len(am.Receivers) != 1 || len(am.Receivers[0]) != 1 {
		t.Errorf("want a single empty default receiver, got %+v", am.Receivers)
	}
}

func TestAlertmanagerAllChannels(t *testing.T) {
	c := testConfig()
	c.Notifications = config.Notifications{
		Telegram: &config.Telegram{BotToken: "123:abc", ChatID: "-42"},
		Slack:    &config.Slack{WebhookURL: "https://hooks.slack.com/services/a/b/c", Channel: "#ops"},
		Email:    &config.Email{To: "a@b.co", From: "m@b.co", Smarthost: "smtp.b.co:587", Username: "u", Password: `p"w: #x`},
		Webhook:  &config.Webhook{URL: "http://h:9000/x"},
	}
	am := parseAM(t, c)
	r := am.Receivers[0]
	for _, k := range []string{"telegram_configs", "slack_configs", "email_configs", "webhook_configs"} {
		if r[k] == nil {
			t.Errorf("missing %s", k)
		}
	}
	email := r["email_configs"].([]any)[0].(map[string]any)
	if email["auth_password"] != `p"w: #x` {
		t.Errorf("password not preserved: %q", email["auth_password"])
	}
	tg := r["telegram_configs"].([]any)[0].(map[string]any)
	if tg["chat_id"] != -42 || !strings.Contains(tg["message"].(string), "{{ .CommonLabels.alertname }}") {
		t.Errorf("telegram config wrong: %+v", tg)
	}
}

type promConfig struct {
	RuleFiles     []string `yaml:"rule_files"`
	ScrapeConfigs []struct {
		JobName       string `yaml:"job_name"`
		MetricsPath   string `yaml:"metrics_path"`
		StaticConfigs []struct {
			Targets []string `yaml:"targets"`
		} `yaml:"static_configs"`
		Params map[string][]string `yaml:"params"`
	} `yaml:"scrape_configs"`
}

func TestPrometheusConfig(t *testing.T) {
	d, err := NewData(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []Backend{Compose, Kubernetes} {
		out, err := PrometheusConfig(b, d)
		if err != nil {
			t.Fatal(err)
		}
		var pc promConfig
		if err := yaml.Unmarshal(out, &pc); err != nil {
			t.Fatalf("%s: invalid YAML: %v\n%s", b, err, out)
		}
		jobs := map[string][]string{}
		paths := map[string]string{}
		for _, j := range pc.ScrapeConfigs {
			if _, dup := jobs[j.JobName]; dup {
				t.Errorf("%s: duplicate job %s", b, j.JobName)
			}
			for _, s := range j.StaticConfigs {
				jobs[j.JobName] = append(jobs[j.JobName], s.Targets...)
			}
			if jobs[j.JobName] == nil {
				jobs[j.JobName] = []string{}
			}
			paths[j.JobName] = j.MetricsPath
		}
		if got := strings.Join(jobs["blackbox"], ","); got != "https://example.com/health" {
			t.Errorf("%s: blackbox targets = %q", b, got)
		}
		if got := strings.Join(jobs["blackbox-tcp"], ","); got != "db.local:5432" {
			t.Errorf("%s: blackbox-tcp targets = %q", b, got)
		}
		if got := strings.Join(jobs["web-1"], ","); got != "10.0.0.5:9100" {
			t.Errorf("%s: web-1 targets = %q", b, got)
		}
		if paths["app"] != "/stats" {
			t.Errorf("%s: app metrics_path = %q", b, paths["app"])
		}
		if pc.RuleFiles[0] != "/etc/prometheus/rules/*.yml" {
			t.Errorf("%s: rule_files = %v", b, pc.RuleFiles)
		}
		want := []string{"prometheus", "node", "alertmanager", "cadvisor"}
		if b == Kubernetes {
			want = append(want, "kubelet", "pods", "kube-state-metrics")
		}
		for _, j := range want {
			if _, ok := jobs[j]; !ok {
				t.Errorf("%s: missing job %s", b, j)
			}
		}
	}

	// Without probes there are no blackbox jobs at all.
	c := testConfig()
	c.Probes = nil
	d, _ = NewData(c)
	out, _ := PrometheusConfig(Compose, d)
	if strings.Contains(string(out), "blackbox") {
		t.Errorf("blackbox job rendered without probes:\n%s", out)
	}
}

func TestRetentionRendered(t *testing.T) {
	c := testConfig()
	c.Retention = config.Retention{Metrics: "2w", Logs: "7d", Traces: "48h"}
	d, err := NewData(c)
	if err != nil {
		t.Fatal(err)
	}
	loki, _ := LokiConfig(d)
	jaeger, _ := JaegerConfig(d)
	for _, tc := range []struct {
		name, out, want string
	}{{"loki", string(loki), "retention_period: 168h"}, {"jaeger", string(jaeger), "spans: 48h"}} {
		var v any
		if err := yaml.Unmarshal([]byte(tc.out), &v); err != nil {
			t.Errorf("%s: invalid YAML: %v", tc.name, err)
		}
		if !strings.Contains(tc.out, tc.want) {
			t.Errorf("%s: missing %q:\n%s", tc.name, tc.want, tc.out)
		}
	}
	if d.Retention.Metrics != 336 {
		t.Errorf("metrics hours = %d", d.Retention.Metrics)
	}
}

func TestLoadExtras(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("rules/mine.yml", "groups:\n  - name: mine\n    rules: []\n")
	write("rules/notes.txt", "ignored")
	write("dashboards/app.json", `{"title":"App"}`)

	e, err := LoadExtras(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e.Rules["custom-mine.yml"] == nil || e.Dashboards["custom-app.json"] == nil || len(e.Rules) != 1 {
		t.Errorf("extras not loaded: %v %v", SortedKeys(e.Rules), SortedKeys(e.Dashboards))
	}

	rules, _ := Rules(Compose, e)
	if rules["rules.yml"] == nil || rules["custom-mine.yml"] == nil || rules["kubernetes.yml"] != nil {
		t.Errorf("compose rules = %v", SortedKeys(rules))
	}
	rules, _ = Rules(Kubernetes, e)
	if rules["kubernetes.yml"] == nil {
		t.Errorf("k8s rules = %v", SortedKeys(rules))
	}
	dash, _ := Dashboards(e)
	if dash["custom-app.json"] == nil || dash["node-exporter-full.json"] == nil || dash["dashboard.yml"] != nil {
		t.Errorf("dashboards = %v", SortedKeys(dash))
	}

	write("rules/bad.yml", "not: rules\n")
	write("dashboards/bad.json", "{")
	_, err = LoadExtras(dir)
	if err == nil || !strings.Contains(err.Error(), "bad.yml") || !strings.Contains(err.Error(), "bad.json") {
		t.Errorf("expected errors for both bad files, got %v", err)
	}
}

func TestLoadExtrasMissingDirs(t *testing.T) {
	if _, err := LoadExtras(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

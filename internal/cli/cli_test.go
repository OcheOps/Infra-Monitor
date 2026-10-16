package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/prompt"
)

// newApp returns an app for tests. A non-empty script makes it interactive
// and feeds the answers line by line.
func newApp(t *testing.T, script string) (*app, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return &app{
		dir:         t.TempDir(),
		version:     "test",
		stdout:      &out,
		stderr:      &out,
		p:           prompt.New(strings.NewReader(script), &out),
		interactive: script != "",
	}, &out
}

func (a *app) runT(t *testing.T, args ...string) {
	t.Helper()
	if err := a.run(append([]string{"--dir", a.dir}, args...)); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
}

func (a *app) cfgT(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	ns := fs.String("n", "default", "")
	pos, err := parse(fs, []string{"grafana", "-n", "mon", "8080"})
	if err != nil {
		t.Fatal(err)
	}
	if *ns != "mon" || !reflect.DeepEqual(pos, []string{"grafana", "8080"}) {
		t.Errorf("ns=%q pos=%v", *ns, pos)
	}
}

func TestInitWithFlags(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init", "--telegram-token", "1:a", "--telegram-chat-id", "2", "--probe", "https://example.com", "--retention-logs", "7d")
	cfg := a.cfgT(t)
	if cfg.Grafana.AdminPassword == "" || cfg.Notifications.Telegram.ChatID != "2" || cfg.Retention.Logs != "7d" {
		t.Errorf("unexpected config %+v", cfg)
	}
	if !strings.Contains(out.String(), "Grafana login: admin / "+cfg.Grafana.AdminPassword) {
		t.Error("generated password not printed")
	}
	for _, d := range []string{"rules", "dashboards"} {
		if _, err := os.Stat(filepath.Join(a.dir, d)); err != nil {
			t.Errorf("%s dir not created", d)
		}
	}

	// Re-running init keeps everything not passed.
	a.runT(t, "init", "--telegram-chat-id", "3")
	cfg2 := a.cfgT(t)
	if cfg2.Grafana.AdminPassword != cfg.Grafana.AdminPassword || cfg2.Notifications.Telegram.BotToken != "1:a" ||
		cfg2.Notifications.Telegram.ChatID != "3" || len(cfg2.Probes) != 1 {
		t.Errorf("init did not preserve config: %+v", cfg2)
	}
}

func TestInitRejectsBadInput(t *testing.T) {
	a, _ := newApp(t, "")
	if err := a.run([]string{"--dir", a.dir, "init", "--telegram-token", "nope", "--telegram-chat-id", "2"}); err == nil {
		t.Error("expected error for invalid token")
	}
	if _, err := os.Stat(config.Path(a.dir)); err == nil {
		t.Error("invalid config was saved")
	}
}

func TestProbesAndTargets(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init")
	a.runT(t, "probes", "add", "https://a.example.com", "tcp://db:5432")
	a.runT(t, "probes", "add", "https://a.example.com") // duplicate is a no-op
	a.runT(t, "targets", "add", "web-1", "10.0.0.5:9100")
	a.runT(t, "targets", "add", "app", "app:8080", "--path", "/stats")

	cfg := a.cfgT(t)
	if !reflect.DeepEqual(cfg.Probes, []string{"https://a.example.com", "tcp://db:5432"}) {
		t.Errorf("probes = %v", cfg.Probes)
	}
	if len(cfg.Targets) != 2 || cfg.Targets[1].Path != "/stats" {
		t.Errorf("targets = %+v", cfg.Targets)
	}

	out.Reset()
	a.runT(t, "probes", "list")
	if out.String() != "https://a.example.com\ntcp://db:5432\n" {
		t.Errorf("probes list = %q", out.String())
	}

	a.runT(t, "probes", "remove", "tcp://db:5432")
	a.runT(t, "targets", "remove", "web-1")
	cfg = a.cfgT(t)
	if len(cfg.Probes) != 1 || len(cfg.Targets) != 1 || cfg.Targets[0].Name != "app" {
		t.Errorf("after remove: %+v %+v", cfg.Probes, cfg.Targets)
	}

	for _, args := range [][]string{
		{"probes", "add", "example.com"},
		{"probes", "remove", "https://nope"},
		{"targets", "add", "app", "app:9000"}, // duplicate name
		{"targets", "add", "node", "h:9100"},  // reserved name
		{"targets", "add", "x", "no-port"},    // bad address
		{"targets", "remove", "missing"},
		{"probes", "add"}, // needs args when not interactive
	} {
		if err := a.run(append([]string{"--dir", a.dir}, args...)); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

func TestNotifyAddRemoveAndRedaction(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init")
	a.runT(t, "notify", "add", "slack", "--webhook-url", "https://hooks.slack.com/services/T0/B0/zq9TOKENzq9", "--channel", "#ops")
	a.runT(t, "notify", "add", "email", "--to", "ops@example.com", "--from", "mon@example.com", "--smarthost", "smtp.example.com:587", "--username", "mon", "--password", "hunter2")
	a.runT(t, "notify", "add", "webhook", "--url", "http://hook.local:9000/x?token=abc")

	out.Reset()
	a.runT(t, "config", "show")
	shown := out.String()
	cfg := a.cfgT(t)
	for _, secret := range []string{cfg.Grafana.AdminPassword, "zq9TOKENzq9", "hunter2", "token=abc"} {
		if strings.Contains(shown, secret) {
			t.Errorf("config show leaks %q:\n%s", secret, shown)
		}
	}
	if !strings.Contains(shown, "ops@example.com") || !strings.Contains(shown, "https://hooks.slack.com/") {
		t.Errorf("config show hides too much:\n%s", shown)
	}

	out.Reset()
	a.runT(t, "notify", "list")
	if got := out.String(); !strings.Contains(got, "slack     #ops") || !strings.Contains(got, "email") || !strings.Contains(got, "webhook") {
		t.Errorf("notify list = %q", got)
	}

	a.runT(t, "notify", "remove", "slack")
	if a.cfgT(t).Notifications.Slack != nil {
		t.Error("slack not removed")
	}
	if err := a.run([]string{"--dir", a.dir, "notify", "add", "slack", "--webhook-url", "http://insecure"}); err == nil {
		t.Error("expected error for non-https Slack URL")
	}
}

func TestNotifyTest(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
	}))
	defer srv.Close()

	a, _ := newApp(t, "")
	a.runT(t, "notify", "test", "--alertmanager", srv.URL)
	if len(got) != 1 || got[0]["labels"].(map[string]any)["alertname"] != "InfraMonitorTest" {
		t.Errorf("alert posted = %+v", got)
	}
	if err := a.run([]string{"notify", "test", "--alertmanager", "http://127.0.0.1:1"}); err == nil {
		t.Error("expected an error when Alertmanager is unreachable")
	}
}

func TestK8sRenderUsesConfig(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init")
	a.runT(t, "k8s", "configure", "--namespace", "mon", "--ingress-host", "grafana.example.com", "--cluster-issuer", "le")
	out.Reset()
	a.runT(t, "k8s", "render")
	s := out.String()
	if !strings.Contains(s, "namespace: mon\n") || !strings.Contains(s, "kind: Ingress") {
		t.Error("render ignored the saved Kubernetes settings")
	}
	out.Reset()
	a.runT(t, "k8s", "render", "-n", "other")
	if !strings.Contains(out.String(), "namespace: other\n") {
		t.Error("-n did not override the namespace")
	}
}

func TestComposeSync(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init", "--probe", "https://example.com")
	a.runT(t, "compose", "sync")
	prom, err := os.ReadFile(filepath.Join(a.dir, "compose", "config", "prometheus.yml"))
	if err != nil || !strings.Contains(string(prom), "https://example.com") {
		t.Errorf("prometheus.yml = %s, %v", prom, err)
	}
	if !strings.Contains(out.String(), "not started") {
		t.Errorf("output = %q", out.String())
	}
}

func TestCommandsNeedInit(t *testing.T) {
	a, _ := newApp(t, "")
	for _, args := range [][]string{{"probes"}, {"compose", "up"}, {"k8s", "render"}, {"config", "show"}} {
		err := a.run(append([]string{"--dir", a.dir}, args...))
		if err == nil || !strings.Contains(err.Error(), "infra-monitor init") {
			t.Errorf("%v: got %v, want a hint to run init", args, err)
		}
	}
}

func TestWizard(t *testing.T) {
	script := strings.Join([]string{
		"",                    // generate a random password? yes
		"https://example.com", // URL to check
		"tcp://db:5432",       // another
		"not a url",           // rejected, asked again
		"",                    // done with URLs
		"y",                   // add a scrape target?
		"10.0.0.5:9100",       // address
		"",                    // name: accept suggested
		"",                    // metrics path: default
		"n",                   // another target? no
		"y",                   // Telegram? yes
		"123:abc",             // bot token
		"42",                  // chat id
		"n", "n", "n",         // Slack, email, webhook
		"", "7d", "", // retention: metrics default, logs 7d, traces default
		"", // save? yes
		"", // start now? default: not now
	}, "\n") + "\n"
	a, out := newApp(t, script)
	a.runT(t, "init")

	cfg := a.cfgT(t)
	if !reflect.DeepEqual(cfg.Probes, []string{"https://example.com", "tcp://db:5432"}) {
		t.Errorf("probes = %v", cfg.Probes)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0] != (config.Target{Name: "10-0-0-5", Address: "10.0.0.5:9100"}) {
		t.Errorf("targets = %+v", cfg.Targets)
	}
	if tg := cfg.Notifications.Telegram; tg == nil || tg.BotToken != "123:abc" || tg.ChatID != "42" {
		t.Errorf("telegram = %+v", tg)
	}
	if cfg.Retention.Logs != "7d" || cfg.Retention.Metrics != "15d" {
		t.Errorf("retention = %+v", cfg.Retention)
	}
	if len(cfg.Grafana.AdminPassword) != 32 {
		t.Errorf("password not generated: %q", cfg.Grafana.AdminPassword)
	}
	if !strings.Contains(out.String(), "✗ probe") {
		t.Error("invalid URL was not reported")
	}

	// Running the wizard again offers the existing values as defaults.
	script = strings.Join([]string{
		"n",      // don't regenerate the password
		"",       // keep current password
		"y", "2", // remove a URL: tcp://db:5432
		"n",    // remove another? no
		"",     // no new URLs
		"n",    // no more targets
		"", "", // Telegram: keep, keep token
		"", // keep chat id
		"n", "n", "n",
		"", "", "",
		"", "",
	}, "\n") + "\n"
	a2 := &app{dir: a.dir, stdout: out, stderr: out, p: prompt.New(strings.NewReader(script), out), interactive: true}
	a2.runT(t, "init")
	cfg2 := a2.cfgT(t)
	if cfg2.Grafana.AdminPassword != cfg.Grafana.AdminPassword || !reflect.DeepEqual(cfg2.Probes, []string{"https://example.com"}) ||
		cfg2.Notifications.Telegram == nil || cfg2.Notifications.Telegram.BotToken != "123:abc" {
		t.Errorf("second run changed values it should have kept: %+v", cfg2)
	}
}

func TestMenuAddProbeThenQuit(t *testing.T) {
	a, _ := newApp(t, "")
	a.runT(t, "init")
	script := strings.Join([]string{
		"1",                       // Check a website or service
		"1",                       // Add
		"https://new.example.com", // URL
		"",                        // done
		"3",                       // apply now? not now
		"9",                       // Quit
	}, "\n") + "\n"
	a.p = prompt.New(strings.NewReader(script), a.stdout)
	a.interactive = true
	a.runT(t)
	if got := a.cfgT(t).Probes; !reflect.DeepEqual(got, []string{"https://new.example.com"}) {
		t.Errorf("probes = %v", got)
	}
}

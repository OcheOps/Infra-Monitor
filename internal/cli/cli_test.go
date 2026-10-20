package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/doctor"
	"github.com/OcheOps/Infra-Monitor/internal/promapi"
	"github.com/OcheOps/Infra-Monitor/internal/promapi/promapitest"
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
	slackPath, hookQuery := fake("slack"), fake("hook")
	a.runT(t, "notify", "add", "slack", "--webhook-url", "https://hooks.slack.com/services/T0/B0/"+slackPath, "--channel", "#ops")
	a.runT(t, "notify", "add", "email", "--to", "ops@example.com", "--from", "mon@example.com", "--smarthost", "smtp.example.com:587", "--username", "mon", "--password", fake("smtp"))
	a.runT(t, "notify", "add", "webhook", "--url", "http://hook.local:9000/x?q="+hookQuery)

	out.Reset()
	a.runT(t, "config", "show")
	shown := out.String()
	cfg := a.cfgT(t)
	for _, hiddenValue := range []string{cfg.Grafana.AdminPassword, slackPath, fake("smtp"), hookQuery} {
		if strings.Contains(shown, hiddenValue) {
			t.Errorf("config show leaks %q:\n%s", hiddenValue, shown)
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
		"12",                      // Quit
	}, "\n") + "\n"
	a.p = prompt.New(strings.NewReader(script), a.stdout)
	a.interactive = true
	a.runT(t)
	if got := a.cfgT(t).Probes; !reflect.DeepEqual(got, []string{"https://new.example.com"}) {
		t.Errorf("probes = %v", got)
	}
}

// fake returns a placeholder credential for tests. It is built at runtime so
// secret scanners don't mistake test fixtures for real credentials.
func fake(s string) string { return "fake-" + s }

func TestCompletion(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "completion", "bash")
	bash := out.String()
	for _, w := range []string{"complete -F _infra_monitor infra-monitor", `"k8s port-forward") opts="grafana`, "targets list"} {
		if !strings.Contains(bash, w) {
			t.Errorf("bash completion missing %q", w)
		}
	}
	for path := range completionTree {
		if !strings.Contains(bash, strconv.Quote(path)+")") {
			t.Errorf("bash completion has no case for %q", path)
		}
	}

	out.Reset()
	a.runT(t, "completion", "zsh")
	if !strings.HasPrefix(out.String(), "#compdef infra-monitor") || !strings.Contains(out.String(), "bashcompinit") {
		t.Errorf("zsh completion = %q", out.String()[:60])
	}

	out.Reset()
	a.runT(t, "completion", "fish")
	fish := out.String()
	if !strings.Contains(fish, "-n '__fish_use_subcommand' -a 'init status digest explain ai probes") ||
		!strings.Contains(fish, "__fish_seen_subcommand_from notify; and __fish_seen_subcommand_from add") {
		t.Errorf("fish completion:\n%s", fish)
	}

	if err := a.run([]string{"completion", "powershell"}); err == nil {
		t.Error("expected an error for an unknown shell")
	}
}

// fakeDoctor returns a Doctor for a host with the given tools installed and
// healthy, and nothing else.
func fakeDoctor(installed ...string) *doctor.Doctor {
	have := map[string]bool{}
	for _, n := range installed {
		have[n] = true
	}
	return &doctor.Doctor{
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			if !have[name] {
				return "", errors.New("not found")
			}
			switch cmd := strings.Join(args, " "); {
			case strings.HasPrefix(cmd, "compose version"):
				return "2.29.1", nil
			case strings.HasPrefix(cmd, "info"):
				return "27.1.1", nil
			case strings.HasPrefix(cmd, "config current-context"):
				return "kind-test", nil
			case strings.HasPrefix(cmd, "version"):
				return `{"serverVersion":{}}`, nil
			case strings.HasPrefix(cmd, "auth can-i"):
				return "yes", nil
			case strings.HasPrefix(cmd, "get storageclass"):
				return "standard=true", nil
			}
			return "", errors.New("unexpected: " + name + " " + strings.Join(args, " "))
		},
		LookPath: func(n string) (string, error) {
			if have[n] {
				return "/usr/bin/" + n, nil
			}
			return "", errors.New("not found")
		},
		PortFree: func(int) bool { return true },
		GOOS:     "linux",
		Timeout:  time.Second,
	}
}

func TestDoctorCommand(t *testing.T) {
	a, out := newApp(t, "")
	a.doctor = fakeDoctor("kubectl")
	a.runT(t, "doctor")
	s := out.String()
	for _, want := range []string{"not set up yet", "✗ Docker installed", "https://docs.docker.com/engine/install/", "✓ Cluster reachable", "Ready: `infra-monitor k8s apply`"} {
		if !strings.Contains(s, want) {
			t.Errorf("doctor output missing %q:\n%s", want, s)
		}
	}

	out.Reset()
	if err := a.run([]string{"--dir", a.dir, "doctor", "compose"}); err == nil {
		t.Error("doctor compose should fail without Docker")
	}
	a.doctor = fakeDoctor("docker", "kubectl")
	out.Reset()
	a.runT(t, "init")
	out.Reset()
	a.runT(t, "doctor")
	if !strings.Contains(out.String(), "✓ "+config.Path(a.dir)) || !strings.Contains(out.String(), "compose up` or `infra-monitor k8s apply") {
		t.Errorf("doctor output:\n%s", out.String())
	}
}

func TestPreflightBlocksComposeUp(t *testing.T) {
	a, _ := newApp(t, "")
	a.runT(t, "init")
	a.doctor = fakeDoctor() // nothing installed
	err := a.run([]string{"--dir", a.dir, "compose", "up"})
	if err == nil || !strings.Contains(err.Error(), "Docker installed") || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("compose up error = %v", err)
	}
	err = a.run([]string{"--dir", a.dir, "k8s", "apply"})
	if err == nil || !strings.Contains(err.Error(), "kubectl installed") {
		t.Errorf("k8s apply error = %v", err)
	}
}

func TestExposeAndStatusPage(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init", "--probe", "https://shop.example.com/health", "--probe", "tcp://db:5432")

	if err := a.run([]string{"--dir", a.dir, "expose", "--domain", "not a domain"}); err == nil {
		t.Error("expected an error for an invalid domain")
	}
	a.runT(t, "expose", "--domain", "mon.example.com", "--email", "ops@example.com")
	if g := a.cfgT(t).Gateway; g.Domain != "mon.example.com" || g.TLS != "auto" {
		t.Errorf("gateway = %+v", g)
	}
	if !strings.Contains(out.String(), "DNS A/AAAA record for mon.example.com") {
		t.Errorf("expose output missing DNS instructions:\n%s", out.String())
	}

	// Scripted enable shows the HTTP check but not the (often internal) TCP one.
	a.runT(t, "statuspage", "enable", "--title", "Acme status")
	sp := a.cfgT(t).StatusPage
	if !sp.Enabled || sp.Title != "Acme status" || len(sp.Monitors) != 1 || sp.Monitors[0].Name != "shop.example.com/health" {
		t.Errorf("status page = %+v", sp)
	}

	a.runT(t, "statuspage", "add", "tcp://db:5432", "--name", "Database")
	a.runT(t, "statuspage", "add", "https://api.example.com", "--name", "API") // also adds the probe
	cfg := a.cfgT(t)
	if len(cfg.StatusPage.Monitors) != 3 || !slices.Contains(cfg.Probes, "https://api.example.com") {
		t.Errorf("after add: %+v / %v", cfg.StatusPage.Monitors, cfg.Probes)
	}

	// Removing a probe also takes it off the page.
	a.runT(t, "probes", "remove", "tcp://db:5432")
	if m := a.cfgT(t).StatusPage.Monitors; len(m) != 2 {
		t.Errorf("monitors after probe removal = %+v", m)
	}

	out.Reset()
	a.runT(t, "statuspage", "show")
	if !strings.Contains(out.String(), "https://mon.example.com/status/") || !strings.Contains(out.String(), "API") {
		t.Errorf("statuspage show:\n%s", out.String())
	}

	// The gateway files reflect it.
	a.runT(t, "compose", "sync")
	caddy, _ := os.ReadFile(filepath.Join(a.dir, "compose", "gateway", "Caddyfile"))
	page, _ := os.ReadFile(filepath.Join(a.dir, "compose", "gateway", "status", "config.json"))
	if !strings.Contains(string(caddy), "mon.example.com {") || !strings.Contains(string(page), `"name": "API"`) {
		t.Errorf("gateway files not rendered:\n%s\n%s", caddy, page)
	}

	a.runT(t, "expose", "off")
	if a.cfgT(t).Gateway.Domain != "" {
		t.Error("expose off did not clear the domain")
	}
	a.runT(t, "statuspage", "disable")
	if a.cfgT(t).StatusPage.Enabled {
		t.Error("statuspage disable did not turn it off")
	}
}

func TestDoctorPublicChecks(t *testing.T) {
	d := fakeDoctor("docker")
	d.Resolve = func(h string) ([]string, error) {
		if h == "mon.example.com" {
			return []string{"203.0.113.7"}, nil
		}
		return nil, errors.New("no such host")
	}
	checks := d.Public([]string{"mon.example.com", "status.example.com"}, false, false)
	byName := map[string]doctor.Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if byName["DNS for mon.example.com"].Status != doctor.OK || byName["DNS for status.example.com"].Status != doctor.Fail {
		t.Errorf("checks = %+v", checks)
	}
	if internal := d.Public([]string{"status.example.com"}, true, true); internal[0].Status != doctor.Warn || len(internal) != 1 {
		t.Errorf("internal TLS should only warn, and skip ports when running: %+v", internal)
	}
}

// fakeGateway is a TLS server standing in for the gateway's /ingest routes.
// It returns the server, its certificate as PEM, and that certificate's
// SHA-256 (what `agent add` prints as --ca-sha256).
func fakeGateway(t *testing.T, name, token string) (*httptest.Server, []byte, string) {
	t.Helper()
	var caPEM []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ingest/ca.crt":
			w.Write(caPEM)
		case "/ingest/ping":
			if u, p, ok := r.BasicAuth(); !ok || u != name || p != token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, caPEM, hex.EncodeToString(sum[:])
}

func TestAgentAddOnServer(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init")
	if err := a.run([]string{"--dir", a.dir, "agent", "add", "web-2"}); err == nil || !strings.Contains(err.Error(), "expose --domain") {
		t.Errorf("agent add without a domain: %v", err)
	}
	a.runT(t, "expose", "--domain", "mon.example.com")
	out.Reset()
	a.runT(t, "agent", "add", "web-2")

	m := regexp.MustCompile(`agent join https://mon\.example\.com --name web-2 --token (\S+)`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no join command in output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "--ca-sha256") {
		t.Error("Let's Encrypt servers need no CA pin")
	}
	cfg := a.cfgT(t)
	if len(cfg.Agents) != 1 || bcrypt.CompareHashAndPassword([]byte(cfg.Agents[0].PasswordHash), []byte(m[1])) != nil {
		t.Fatalf("stored hash doesn't match the printed token: %+v", cfg.Agents)
	}
	if strings.Contains(mustRead(t, config.Path(a.dir)), m[1]) {
		t.Error("the plaintext token was saved in the config")
	}
	if err := a.run([]string{"--dir", a.dir, "agent", "add", "web-2"}); err == nil {
		t.Error("adding the same name twice should fail")
	}

	a.runT(t, "compose", "sync")
	caddy := mustRead(t, filepath.Join(a.dir, "compose", "gateway", "Caddyfile"))
	for _, want := range []string{"handle_path /ingest/*", "web-2 " + cfg.Agents[0].PasswordHash, "rewrite * /api/v1/write", "rewrite * /loki/api/v1/push"} {
		if !strings.Contains(caddy, want) {
			t.Errorf("Caddyfile missing %q", want)
		}
	}
	if strings.Contains(caddy, "/ca.crt") {
		t.Error("CA download served without internal TLS")
	}
	rules := mustRead(t, filepath.Join(a.dir, "compose", "alerts", "agents.yml"))
	if !strings.Contains(rules, `absent_over_time(up{job="node",agent="web-2"}[5m])`) {
		t.Errorf("agents.yml:\n%s", rules)
	}

	a.runT(t, "agent", "remove", "web-2")
	a.runT(t, "compose", "sync")
	if _, err := os.Stat(filepath.Join(a.dir, "compose", "alerts", "agents.yml")); err == nil {
		t.Error("agents.yml left behind after removing the last agent")
	}
}

func TestAgentAddPinsInternalCA(t *testing.T) {
	_, caPEM, fp := fakeGateway(t, "x", "y")
	a, out := newApp(t, "")
	a.runT(t, "init")
	a.runT(t, "expose", "--domain", "mon.example.com", "--tls", "internal")
	a.gatewayCA = func() ([]byte, error) { return caPEM, nil }
	out.Reset()
	a.runT(t, "agent", "add", "db-1")
	if !strings.Contains(out.String(), "--ca-sha256 "+fp) {
		t.Errorf("join command missing the CA pin:\n%s", out.String())
	}
	a.runT(t, "compose", "sync")
	if !strings.Contains(mustRead(t, filepath.Join(a.dir, "compose", "gateway", "Caddyfile")), "handle /ca.crt") {
		t.Error("gateway doesn't serve its CA for agents to fetch")
	}
}

func TestAgentJoin(t *testing.T) {
	token := strings.Repeat("A", 43)
	srv, _, fp := fakeGateway(t, "web-2", token)

	a, out := newApp(t, "")
	join := func(extra ...string) error {
		return a.run(append([]string{"--dir", a.dir, "agent", "join", srv.URL, "--name", "web-2", "--no-start"}, extra...))
	}
	if err := join("--token", token); err == nil || !strings.Contains(err.Error(), "--ca-sha256") {
		t.Errorf("join to an untrusted cert without a pin: %v", err)
	}
	if err := join("--token", token, "--ca-sha256", strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Errorf("join with the wrong pin: %v", err)
	}
	if err := join("--token", strings.Repeat("B", 43), "--ca-sha256", fp); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("join with the wrong token: %v", err)
	}
	if _, err := os.Stat(config.Path(a.dir)); err == nil {
		t.Fatal("a failed join saved a config")
	}

	if err := join("--token", token, "--ca-sha256", fp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "matches the fingerprint") || !strings.Contains(out.String(), `as "web-2"`) {
		t.Errorf("join output:\n%s", out.String())
	}
	cfg := a.cfgT(t)
	if !cfg.IsAgent() || cfg.Grafana.AdminPassword != "" {
		t.Errorf("agent config = %+v", cfg)
	}

	a.runT(t, "compose", "sync")
	dir := filepath.Join(a.dir, "agent")
	prom := mustRead(t, filepath.Join(dir, "config", "prometheus.yml"))
	for _, want := range []string{srv.URL + "/ingest/metrics", "password_file: /etc/infra-monitor/token", "ca_file: /etc/infra-monitor/ca.crt", `agent: "web-2"`} {
		if !strings.Contains(prom, want) {
			t.Errorf("agent prometheus.yml missing %q:\n%s", want, prom)
		}
	}
	if strings.Contains(prom, token) {
		t.Error("token written into prometheus.yml instead of the token file")
	}
	fb := mustRead(t, filepath.Join(dir, "config", "fluent-bit.conf"))
	if !strings.Contains(fb, "Uri          /ingest/logs") || !strings.Contains(fb, "tls.ca_file  /etc/infra-monitor/ca.crt") || !strings.Contains(fb, "agent=web-2") {
		t.Errorf("fluent-bit.conf:\n%s", fb)
	}
	if mustRead(t, filepath.Join(dir, "config", "token")) != token {
		t.Error("token file wrong")
	}

	// Server settings are refused on an agent host.
	if err := a.run([]string{"--dir", a.dir, "probes", "add", "https://x.example.com"}); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("probes add on an agent: %v", err)
	}

	// Agents monitor their own services too.
	a.runT(t, "integrations", "add", "redis", "cache", "cache:6379")
	a.runT(t, "compose", "sync")
	if prom := mustRead(t, filepath.Join(dir, "config", "prometheus.yml")); !strings.Contains(prom, `"integration-cache:9121"`) {
		t.Errorf("agent prometheus.yml doesn't scrape the integration:\n%s", prom)
	}
	if f := mustRead(t, filepath.Join(dir, "integrations.yml")); !strings.Contains(f, "- agent") {
		t.Errorf("agent integrations.yml:\n%s", f)
	}

	a.runT(t, "agent", "leave")
	if _, err := os.Stat(config.Path(a.dir)); err == nil {
		t.Error("agent leave kept the config")
	}
}

func TestAgentJoinRefusesServer(t *testing.T) {
	a, _ := newApp(t, "")
	a.runT(t, "init")
	err := a.run([]string{"--dir", a.dir, "agent", "join", "https://mon.example.com", "--name", "x", "--token", strings.Repeat("A", 43)})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("join on a server host: %v", err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIntegrations(t *testing.T) {
	a, out := newApp(t, "")
	a.runT(t, "init")
	a.runT(t, "integrations", "add", "postgres", "db", "shop-db-1:5432", "--network", "shop_default", "--password", fake("pg"))
	a.runT(t, "integrations", "add", "rabbitmq", "mq", "host.docker.internal:15692")
	if err := a.run([]string{"--dir", a.dir, "integrations", "add", "oracle", "x", "o:1521"}); err == nil || !strings.Contains(err.Error(), "supported") {
		t.Errorf("unknown type: %v", err)
	}
	if err := a.run([]string{"--dir", a.dir, "integrations", "add", "redis", "db", "r:6379"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate name: %v", err)
	}

	out.Reset()
	a.runT(t, "config", "show")
	if strings.Contains(out.String(), fake("pg")) {
		t.Error("config show printed the integration password")
	}

	a.runT(t, "compose", "sync")
	dir := a.composeDir()
	prom := mustRead(t, filepath.Join(dir, "config", "prometheus.yml"))
	for _, want := range []string{`job_name: "integration-db"`, `"integration-db:9187"`, `job_name: "integration-mq"`, `replacement: "mq"`} {
		if !strings.Contains(prom, want) {
			t.Errorf("prometheus.yml missing %q", want)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "integrations", "db.env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("db.env: %v %v", fi, err)
	}
	if !strings.Contains(mustRead(t, filepath.Join(dir, "integrations.yml")), "shop_default") {
		t.Error("integrations.yml doesn't join the service's network")
	}
	out.Reset()
	a.runT(t, "k8s", "render")
	if strings.Contains(out.String(), "integration-db") {
		t.Error("Kubernetes Prometheus scrapes a Compose-only exporter")
	}

	a.runT(t, "integrations", "remove", "db")
	a.runT(t, "compose", "sync")
	if _, err := os.Stat(filepath.Join(dir, "integrations", "db.env")); err == nil {
		t.Error("removed integration's credentials left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "integrations.yml")); err == nil {
		t.Error("integrations.yml kept with no exporters left")
	}

	// detect without a terminal prints the commands to run.
	detectRunner = func(name string, args ...string) (string, error) {
		if args[0] == "ps" {
			return "cache\tredis:7\t\nshop-db-1\tpostgres:16\tshop\n", nil
		}
		if args[0] == "inspect" {
			return "shop_default ", nil
		}
		return "", errors.New("no")
	}
	t.Cleanup(func() { detectRunner = nil })
	out.Reset()
	a.runT(t, "integrations", "detect")
	for _, want := range []string{"infra-monitor integrations add postgres shop-db-1 shop-db-1:5432 --network shop_default --username postgres --password PASSWORD", "integrations add redis cache cache:6379"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("detect output missing %q:\n%s", want, out.String())
		}
	}
}

func TestIntegrationsDetectInteractive(t *testing.T) {
	a, _ := newApp(t, "")
	a.runT(t, "init")
	a.interactive = true
	a.p = prompt.New(strings.NewReader("y\n\n"+fake("pg")+"\nn\n3\n"), io.Discard)
	detectRunner = func(name string, args ...string) (string, error) {
		if args[0] == "ps" {
			return "shop-db-1\tpostgres:16\tshop\ncache\tredis:7\tshop\n", nil
		}
		return "shop_default ", nil
	}
	t.Cleanup(func() { detectRunner = nil })
	if err := a.integrationsDetect(nil); err != nil {
		t.Fatal(err)
	}
	got := a.cfgT(t).Integrations
	want := []config.Integration{{Type: "postgres", Name: "shop-db-1", Address: "shop-db-1:5432", Network: "shop_default", Username: "postgres", Password: fake("pg")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("integrations = %+v", got)
	}
}

type memCrontab struct{ s string }

func (m *memCrontab) Read() (string, error) { return m.s, nil }
func (m *memCrontab) Write(s string) error  { m.s = s; return nil }

func TestStatusDigestExplain(t *testing.T) {
	f := &promapitest.Fake{
		Vectors: map[string][]promapi.Sample{"probe_success": {promapitest.S(0, "instance", "https://shop.example.com")}},
		Alerts: []promapi.Alert{{Name: "ProbeFailed", State: "firing", ActiveAt: time.Now(),
			Labels: map[string]string{"alertname": "ProbeFailed", "instance": "https://shop.example.com"}}},
		Rules: []promapi.Rule{{Name: "ProbeFailed", Query: "probe_success == 0"}},
	}
	e := f.Start(t)
	a, out := newApp(t, "")
	a.endpoints = &e
	a.runT(t, "init", "--telegram-token", "1:"+fake("tg"), "--telegram-chat-id", "5")

	a.runT(t, "status")
	if !strings.Contains(out.String(), "✗ https://shop.example.com") || !strings.Contains(out.String(), "Firing ProbeFailed") || !strings.Contains(out.String(), "infra-monitor explain") {
		t.Errorf("status:\n%s", out.String())
	}

	out.Reset()
	a.runT(t, "digest", "show")
	if !strings.Contains(out.String(), "daily summary") {
		t.Errorf("digest show:\n%s", out.String())
	}
	cron := &memCrontab{}
	a.crontab = cron
	a.runT(t, "digest", "enable", "--at", "07:30")
	if !strings.Contains(cron.s, "30 7 * * * ") || !strings.Contains(cron.s, "digest send") || !strings.Contains(cron.s, a.dir) {
		t.Errorf("crontab = %q", cron.s)
	}
	if err := a.run([]string{"--dir", a.dir, "digest", "enable", "--at", "25:00"}); err == nil {
		t.Error("accepted --at 25:00")
	}
	a.runT(t, "digest", "disable")
	if cron.s != "" {
		t.Errorf("crontab after disable = %q", cron.s)
	}

	// explain is opt-in.
	t.Setenv("ANTHROPIC_API_KEY", "")
	if err := a.run([]string{"--dir", a.dir, "explain", "ProbeFailed"}); err == nil || !strings.Contains(err.Error(), "ai enable") {
		t.Errorf("explain while off: %v", err)
	}
	out.Reset()
	a.runT(t, "explain", "ProbeFailed", "--dry-run")
	if !strings.Contains(out.String(), "--- user ---\nAlert: ProbeFailed") || !strings.Contains(out.String(), "Model: claude-opus-5-5") {
		t.Errorf("dry run:\n%s", out.String())
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-"+fake("key"))
	a.runT(t, "ai", "enable")
	if cfg := a.cfgT(t); cfg.AI == nil || cfg.AI.APIKey != "" {
		t.Errorf("ai enable with the env var stored %+v", cfg.AI)
	}
	claude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "sk-ant-"+fake("key") {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The shop is down."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		} {
			var m map[string]any
			json.Unmarshal([]byte(ev), &m)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m["type"], ev)
		}
	}))
	defer claude.Close()
	a.aiBaseURL = claude.URL
	out.Reset()
	a.runT(t, "explain", "ProbeFailed")
	if !strings.Contains(out.String(), "The shop is down.") || !strings.Contains(out.String(), "Asking Claude (claude-opus-5-5)") {
		t.Errorf("explain:\n%s", out.String())
	}

	a.runT(t, "ai", "disable")
	if a.cfgT(t).AI != nil {
		t.Error("ai disable kept the settings")
	}
}

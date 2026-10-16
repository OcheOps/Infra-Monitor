package compose

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

func testConfig() config.Config {
	c := config.Default()
	c.Grafana.AdminPassword = "pw-123456"
	c.Notifications.Telegram = &config.Telegram{BotToken: "1:a", ChatID: "2"}
	return c
}

func TestSyncWritesEveryMountedFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "compose")
	changed, err := Sync(dir, testConfig(), stack.Extras{})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("first sync reported a change to existing files")
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("project dir mode = %v, want 0700", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dir, EnvFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v, want 0600", fi.Mode().Perm())
	}

	// Every ./path bind-mounted by docker-compose.yml must exist.
	compose, _ := deploy.FS.ReadFile("compose/docker-compose.yml")
	for _, m := range regexp.MustCompile(`- \./([^:]+):`).FindAllStringSubmatch(string(compose), -1) {
		if _, err := os.Stat(filepath.Join(dir, m[1])); err != nil {
			t.Errorf("mounted path %s missing: %v", m[1], err)
		}
	}
	for _, p := range []string{"alerts/rules.yml", "provisioning/dashboards/node-exporter-full.json", "provisioning/dashboards/dashboard.yml"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config/prometheus.yml.tmpl")); err == nil {
		t.Error("template source copied into the project")
	}
	if _, err := os.Stat(filepath.Join(dir, "alerts/kubernetes.yml")); err == nil {
		t.Error("Kubernetes-only rules copied into the compose project")
	}
	env, _ := os.ReadFile(filepath.Join(dir, EnvFile))
	if !strings.Contains(string(env), "GRAFANA_ADMIN_PASSWORD='pw-123456'") || !strings.Contains(string(env), "PROMETHEUS_RETENTION=360h") {
		t.Errorf(".env = %s", env)
	}
}

func TestSyncChangeDetectionAndPruning(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "compose")
	cfg := testConfig()
	extras := stack.Extras{
		Rules:      map[string][]byte{"custom-mine.yml": []byte("groups: []\n")},
		Dashboards: map[string][]byte{"custom-app.json": []byte("{}")},
	}
	if _, err := Sync(dir, cfg, extras); err != nil {
		t.Fatal(err)
	}
	if changed, err := Sync(dir, cfg, extras); err != nil || changed {
		t.Errorf("re-sync with same config: changed=%v err=%v", changed, err)
	}

	cfg.Notifications.Telegram.ChatID = "3"
	if changed, err := Sync(dir, cfg, extras); err != nil || !changed {
		t.Errorf("re-sync with new config: changed=%v err=%v", changed, err)
	}
	am, _ := os.ReadFile(filepath.Join(dir, "config/alertmanager.yml"))
	if !strings.Contains(string(am), "chat_id: 3") {
		t.Errorf("alertmanager.yml not updated:\n%s", am)
	}

	// Removing a custom rule or dashboard deletes it from the project.
	if changed, err := Sync(dir, cfg, stack.Extras{}); err != nil || !changed {
		t.Errorf("sync after removing extras: changed=%v err=%v", changed, err)
	}
	for _, p := range []string{"alerts/custom-mine.yml", "provisioning/dashboards/custom-app.json"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s was not pruned", p)
		}
	}
}

func TestSyncRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Probes = []string{"not a url"}
	if _, err := Sync(t.TempDir(), cfg, stack.Extras{}); err == nil {
		t.Error("expected validation error")
	}
}

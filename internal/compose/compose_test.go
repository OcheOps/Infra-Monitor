package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OcheOps/Infra-Monitor/internal/config"
)

func TestSync(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "compose")
	cfg := config.Config{TelegramToken: "1:a", TelegramChatID: "2", GrafanaPassword: "x"}

	changed, err := Sync(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("first sync reported a change to existing files")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("project dir mode = %v, want 0700", fi.Mode().Perm())
	}
	// Every host path mounted by docker-compose.yml must exist.
	for _, p := range []string{
		"docker-compose.yml",
		"config/prometheus.yml",
		"config/alertmanager.yml",
		"config/fluentbit.conf",
		"config/parsers.conf",
		"alerts/rules.yml",
		"provisioning/datasources/datasources.yml",
		"provisioning/dashboards/dashboard.yml",
		"provisioning/dashboards/node-exporter-full.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	if changed, err = Sync(dir, cfg); err != nil || changed {
		t.Errorf("re-sync with same config: changed=%v err=%v", changed, err)
	}
	cfg.TelegramChatID = "3"
	if changed, err = Sync(dir, cfg); err != nil || !changed {
		t.Errorf("re-sync with new config: changed=%v err=%v", changed, err)
	}
	am, _ := os.ReadFile(filepath.Join(dir, "config/alertmanager.yml"))
	if !strings.Contains(string(am), "chat_id: 3") {
		t.Errorf("alertmanager.yml not updated:\n%s", am)
	}
}

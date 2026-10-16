package cli

import (
	"bytes"
	"flag"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/OcheOps/Infra-Monitor/internal/config"
)

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

func TestInitAndRender(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	a := &app{stdout: &out}
	if err := a.run([]string{"--dir", dir, "init", "--telegram-token", "1:a", "--telegram-chat-id", "2"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GrafanaPassword == "" || cfg.TelegramChatID != "2" {
		t.Errorf("unexpected config %+v", cfg)
	}

	// Re-running init keeps the existing password and updates only what's given.
	if err := a.run([]string{"--dir", dir, "init", "--telegram-chat-id", "3"}); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := config.Load(filepath.Join(dir, ".env"))
	if cfg2.GrafanaPassword != cfg.GrafanaPassword || cfg2.TelegramChatID != "3" || cfg2.TelegramToken != "1:a" {
		t.Errorf("init did not preserve config: %+v", cfg2)
	}

	out.Reset()
	if err := a.run([]string{"--dir", dir, "k8s", "render", "-n", "mon"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("name: mon\n")) {
		t.Error("render output missing namespace")
	}
}

func TestInitRejectsBadTelegram(t *testing.T) {
	a := &app{stdout: &bytes.Buffer{}}
	if err := a.run([]string{"--dir", t.TempDir(), "init", "--telegram-token", "nope", "--telegram-chat-id", "2"}); err == nil {
		t.Error("expected error for invalid token")
	}
}

package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func valid() Config {
	c := Default()
	c.Grafana.AdminPassword = "s3cret-pass"
	return c
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	want := valid()
	want.Probes = []string{"https://example.com", "tcp://db:5432"}
	want.Targets = []Target{{Name: "web-1", Address: "10.0.0.5:9100"}, {Name: "app", Address: "app:8080", Path: "/stats"}}
	want.Notifications = Notifications{
		Telegram: &Telegram{BotToken: "123:abc-DEF_9", ChatID: "-1001"},
		Slack:    &Slack{WebhookURL: "https://hooks.slack.com/services/x/y/z", Channel: "#alerts"},
		Email:    &Email{To: "ops@example.com", From: "mon@example.com", Smarthost: "smtp.example.com:587", Username: "u", Password: `p:a"s#s`},
		Webhook:  &Webhook{URL: "http://hook.local/alerts"},
	}
	want.Kubernetes.Ingress = Ingress{Host: "grafana.example.com", ClusterIssuer: "letsencrypt"}
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := want.Save(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir), []byte("grafana:\n  admin_pasword: x\n"), 0o600)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "admin_pasword") {
		t.Fatalf("expected an error naming the typo, got %v", err)
	}
}

func TestMigrateEnv(t *testing.T) {
	dir := t.TempDir()
	env := "# old\nTELEGRAM_API_KEY=1:abc\nTELEGRAM_CHAT_ID=42\nGRAFANA_ADMIN_PASSWORD='pw-123'\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Grafana.AdminPassword != "pw-123" || c.Notifications.Telegram == nil || c.Notifications.Telegram.ChatID != "42" {
		t.Errorf("migration lost values: %+v", c)
	}
	if _, err := os.Stat(Path(dir)); err != nil {
		t.Error("config.yaml not written")
	}
	if _, err := os.Stat(filepath.Join(dir, ".env.migrated")); err != nil {
		t.Error(".env not renamed")
	}
	if c2, err := Load(dir); err != nil || c2.Grafana.AdminPassword != "pw-123" {
		t.Errorf("reload after migration: %+v, %v", c2, err)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Config)
		want string // substring of the error, "" for valid
	}{
		{"defaults", func(c *Config) {}, ""},
		{"no password", func(c *Config) { c.Grafana.AdminPassword = "" }, "admin_password"},
		{"quote in password", func(c *Config) { c.Grafana.AdminPassword = "a'b" }, "admin_password"},
		{"http probe", func(c *Config) { c.Probes = []string{"http://x.local:8080/health"} }, ""},
		{"tcp probe", func(c *Config) { c.Probes = []string{"tcp://db:5432"} }, ""},
		{"tcp probe no port", func(c *Config) { c.Probes = []string{"tcp://db"} }, "host:port"},
		{"bare host probe", func(c *Config) { c.Probes = []string{"example.com"} }, "full URL"},
		{"ftp probe", func(c *Config) { c.Probes = []string{"ftp://x"} }, "scheme"},
		{"duplicate probe", func(c *Config) { c.Probes = []string{"https://a.b", "https://a.b"} }, "twice"},
		{"target ok", func(c *Config) { c.Targets = []Target{{Name: "db", Address: "10.0.0.1:9100"}} }, ""},
		{"target bad name", func(c *Config) { c.Targets = []Target{{Name: "Bad Name", Address: "h:1"}} }, "target name"},
		{"target reserved", func(c *Config) { c.Targets = []Target{{Name: "node", Address: "h:1"}} }, "used by the stack"},
		{"target no port", func(c *Config) { c.Targets = []Target{{Name: "x", Address: "host"}} }, "host:port"},
		{"target bad path", func(c *Config) { c.Targets = []Target{{Name: "x", Address: "h:1", Path: "metrics"}} }, "path"},
		{"target dup", func(c *Config) { c.Targets = []Target{{Name: "x", Address: "h:1"}, {Name: "x", Address: "h:2"}} }, "twice"},
		{"telegram placeholder", func(c *Config) {
			c.Notifications.Telegram = &Telegram{BotToken: "your_bot_api_key", ChatID: "1"}
		}, "bot token"},
		{"telegram chat", func(c *Config) { c.Notifications.Telegram = &Telegram{BotToken: "1:a", ChatID: "abc"} }, "chat id"},
		{"slack http", func(c *Config) { c.Notifications.Slack = &Slack{WebhookURL: "http://hooks.slack.com/x"} }, "https"},
		{"email bad", func(c *Config) { c.Notifications.Email = &Email{To: "nope", From: "a@b.c", Smarthost: "smtp"} }, "email"},
		{"webhook ok", func(c *Config) { c.Notifications.Webhook = &Webhook{URL: "http://h:9000/x"} }, ""},
		{"retention", func(c *Config) { c.Retention.Logs = "30 days" }, "retention.logs"},
		{"retention short", func(c *Config) { c.Retention.Traces = "1h" }, "24h minimum"},
		{"namespace", func(c *Config) { c.Kubernetes.Namespace = "Bad_NS" }, "namespace"},
		{"storage size", func(c *Config) { c.Kubernetes.Storage.Loki = "10GB" }, "storage.loki"},
		{"ingress host", func(c *Config) { c.Kubernetes.Ingress.Host = "not a host" }, "ingress.host"},
		{"ingress without host", func(c *Config) { c.Kubernetes.Ingress.TLSSecret = "tls" }, "need a host"},
	} {
		c := valid()
		tc.mod(&c)
		err := c.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestHours(t *testing.T) {
	for in, want := range map[string]int{"72h": 72, "15d": 360, "2w": 336} {
		if got, err := Hours(in); err != nil || got != want {
			t.Errorf("Hours(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "15", "1.5d", "10m", "12h"} {
		if _, err := Hours(in); err == nil {
			t.Errorf("Hours(%q) succeeded, want error", in)
		}
	}
}

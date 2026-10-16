package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", ".env")
	want := Config{TelegramToken: "123:abc-DEF_9", TelegramChatID: "-1001", GrafanaPassword: "p@ss word$1"}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := (Config{GrafanaPassword: "file"}).Save(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(KeyGrafanaPassword, "env")
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.GrafanaPassword != "env" {
		t.Errorf("GrafanaPassword = %q, want env", got.GrafanaPassword)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"no telegram", Config{GrafanaPassword: "x"}, true},
		{"telegram", Config{TelegramToken: "1:a", TelegramChatID: "42", GrafanaPassword: "x"}, true},
		{"placeholder token", Config{TelegramToken: "your_bot_api_key", TelegramChatID: "42", GrafanaPassword: "x"}, false},
		{"missing chat id", Config{TelegramToken: "1:a", GrafanaPassword: "x"}, false},
		{"non-numeric chat id", Config{TelegramToken: "1:a", TelegramChatID: "abc", GrafanaPassword: "x"}, false},
		{"no password", Config{}, false},
		{"quote in password", Config{GrafanaPassword: "a'b"}, false},
	} {
		if err := tc.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

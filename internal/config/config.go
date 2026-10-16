// Package config reads and writes the CLI's .env file, which holds the
// credentials shared by the Compose and Kubernetes backends.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	KeyTelegramToken   = "TELEGRAM_API_KEY"
	KeyTelegramChatID  = "TELEGRAM_CHAT_ID"
	KeyGrafanaPassword = "GRAFANA_ADMIN_PASSWORD"
)

type Config struct {
	TelegramToken   string
	TelegramChatID  string
	GrafanaPassword string
}

var (
	telegramTokenRe = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
	// Values matching this are written unquoted; anything else is single-quoted.
	plainValueRe = regexp.MustCompile(`^[A-Za-z0-9_:.,/+@=-]*$`)
)

// ErrNotFound is returned by Load when the .env file does not exist.
var ErrNotFound = errors.New("config not found")

// Load parses a KEY=VALUE env file. Process environment variables with the
// same names take precedence over values in the file.
func Load(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("%w: %s (run `infra-monitor init` first)", ErrNotFound, path)
	}
	if err != nil {
		return c, err
	}
	defer f.Close()

	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return c, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		vals[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		return c, err
	}
	for _, k := range []string{KeyTelegramToken, KeyTelegramChatID, KeyGrafanaPassword} {
		if v, ok := os.LookupEnv(k); ok {
			vals[k] = v
		}
	}
	c.TelegramToken = vals[KeyTelegramToken]
	c.TelegramChatID = vals[KeyTelegramChatID]
	c.GrafanaPassword = vals[KeyGrafanaPassword]
	return c, nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

func quote(v string) string {
	if plainValueRe.MatchString(v) {
		return v
	}
	return "'" + v + "'"
}

// Save writes the config with owner-only permissions, since it holds secrets.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# infra-monitor configuration. Holds secrets: keep it private.\n")
	b.WriteString("# Leave the Telegram values empty to run without alert notifications.\n")
	fmt.Fprintf(&b, "%s=%s\n", KeyTelegramToken, quote(c.TelegramToken))
	fmt.Fprintf(&b, "%s=%s\n", KeyTelegramChatID, quote(c.TelegramChatID))
	fmt.Fprintf(&b, "%s=%s\n", KeyGrafanaPassword, quote(c.GrafanaPassword))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// TelegramEnabled reports whether alert notifications should go to Telegram.
func (c Config) TelegramEnabled() bool {
	return c.TelegramToken != "" || c.TelegramChatID != ""
}

func (c Config) Validate() error {
	var errs []error
	if c.TelegramEnabled() {
		if !telegramTokenRe.MatchString(c.TelegramToken) {
			errs = append(errs, fmt.Errorf("%s must look like 123456:ABC-xyz (from @BotFather)", KeyTelegramToken))
		}
		if _, err := strconv.ParseInt(c.TelegramChatID, 10, 64); err != nil {
			errs = append(errs, fmt.Errorf("%s must be a numeric chat id", KeyTelegramChatID))
		}
	}
	switch {
	case c.GrafanaPassword == "":
		errs = append(errs, fmt.Errorf("%s is empty", KeyGrafanaPassword))
	case strings.ContainsAny(c.GrafanaPassword, "'\n\r"):
		errs = append(errs, fmt.Errorf("%s must not contain quotes or newlines", KeyGrafanaPassword))
	}
	return errors.Join(errs...)
}

// GeneratePassword returns a random 32-character hex password.
func GeneratePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

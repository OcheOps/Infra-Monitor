package digest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OcheOps/Infra-Monitor/internal/config"
)

func TestSend(t *testing.T) {
	got := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		got[r.URL.Path] = m
		if r.URL.Path == "/broken" {
			http.Error(w, "nope", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	TelegramAPI = srv.URL
	t.Cleanup(func() { TelegramAPI = "https://api.telegram.org" })

	n := config.Notifications{
		Telegram: &config.Telegram{BotToken: "1:fake", ChatID: "-42"},
		Slack:    &config.Slack{WebhookURL: srv.URL + "/slack", Channel: "#ops"},
		Webhook:  &config.Webhook{URL: srv.URL + "/broken"},
	}
	errs := Send(context.Background(), n, "Infra Monitor: all good", "hello\n")
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "webhook: 502") {
		t.Errorf("errs = %v", errs)
	}
	if m := got["/bot1:fake/sendMessage"]; m["chat_id"] != "-42" || m["text"] != "hello\n" {
		t.Errorf("telegram got %v", m)
	}
	if m := got["/slack"]; m["channel"] != "#ops" || !strings.Contains(m["text"].(string), "hello") {
		t.Errorf("slack got %v", m)
	}
	if m := got["/broken"]; m["type"] != "infra-monitor.digest" || m["subject"] != "Infra Monitor: all good" {
		t.Errorf("webhook got %v", m)
	}
}

func TestSendHidesTokenInErrors(t *testing.T) {
	TelegramAPI = "http://127.0.0.1:1"
	t.Cleanup(func() { TelegramAPI = "https://api.telegram.org" })
	errs := Send(context.Background(), config.Notifications{Telegram: &config.Telegram{BotToken: "1:fake-secret", ChatID: "1"}}, "s", "t")
	if len(errs) != 1 || strings.Contains(errs[0].Error(), "fake-secret") {
		t.Errorf("errs = %v", errs)
	}
}

type memCrontab struct{ s string }

func (m *memCrontab) Read() (string, error) { return m.s, nil }
func (m *memCrontab) Write(s string) error  { m.s = s; return nil }

func TestSchedule(t *testing.T) {
	c := &memCrontab{s: "MAILTO=me\n0 3 * * * backup.sh\n"}
	line := CronLine(8, 5, "/usr/local/bin/infra-monitor", "/home/o p/.infra-monitor")
	if line != "5 8 * * * /usr/local/bin/infra-monitor --dir '/home/o p/.infra-monitor' digest send >/dev/null 2>&1 # infra-monitor digest" {
		t.Errorf("CronLine = %q", line)
	}
	if had, _ := Schedule(c, line); had {
		t.Error("reported an existing line")
	}
	// Rescheduling replaces the line instead of adding another.
	line2 := CronLine(9, 0, "/usr/local/bin/infra-monitor", "/root/.infra-monitor")
	if had, _ := Schedule(c, line2); !had {
		t.Error("didn't find the existing line")
	}
	if c.s != "MAILTO=me\n0 3 * * * backup.sh\n"+line2+"\n" {
		t.Errorf("crontab = %q", c.s)
	}
	if got, _ := Scheduled(c); got != line2 {
		t.Errorf("Scheduled = %q", got)
	}
	Schedule(c, "")
	if c.s != "MAILTO=me\n0 3 * * * backup.sh\n" {
		t.Errorf("after disable: %q", c.s)
	}
}

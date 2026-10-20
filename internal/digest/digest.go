// Package digest sends the daily summary to the configured notification
// channels and schedules it with cron.
package digest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/OcheOps/Infra-Monitor/internal/config"
)

// TelegramAPI is the Bot API base URL; tests replace it.
var TelegramAPI = "https://api.telegram.org"

// Channels lists the configured channels by name.
func Channels(n config.Notifications) []string {
	var out []string
	if n.Telegram != nil {
		out = append(out, "telegram")
	}
	if n.Slack != nil {
		out = append(out, "slack")
	}
	if n.Email != nil {
		out = append(out, "email")
	}
	if n.Webhook != nil {
		out = append(out, "webhook")
	}
	return out
}

// Send delivers the digest to every configured channel and returns one
// error per channel that failed.
func Send(ctx context.Context, n config.Notifications, subject, text string) []error {
	client := &http.Client{Timeout: 20 * time.Second}
	var errs []error
	post := func(name, u, contentType string, body []byte) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, scrub(err)))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			errs = append(errs, fmt.Errorf("%s: %s %s", name, resp.Status, strings.TrimSpace(string(b))))
		}
	}
	if t := n.Telegram; t != nil {
		body, _ := json.Marshal(map[string]any{"chat_id": t.ChatID, "text": text, "disable_web_page_preview": true})
		post("telegram", TelegramAPI+"/bot"+t.BotToken+"/sendMessage", "application/json", body)
	}
	if s := n.Slack; s != nil {
		msg := map[string]any{"text": "```\n" + text + "```"}
		if s.Channel != "" {
			msg["channel"] = s.Channel
		}
		body, _ := json.Marshal(msg)
		post("slack", s.WebhookURL, "application/json", body)
	}
	if w := n.Webhook; w != nil {
		body, _ := json.Marshal(map[string]any{"type": "infra-monitor.digest", "subject": subject, "text": text})
		post("webhook", w.URL, "application/json", body)
	}
	if e := n.Email; e != nil {
		if err := sendMail(*e, subject, text); err != nil {
			errs = append(errs, fmt.Errorf("email: %w", err))
		}
	}
	return errs
}

// scrub drops URLs (which can carry tokens, e.g. Telegram's) from errors.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s request failed: %w", ue.Op, ue.Err)
	}
	return err
}

func sendMail(e config.Email, subject, text string) error {
	host, _, err := net.SplitHostPort(e.Smarthost)
	if err != nil {
		return err
	}
	var auth smtp.Auth
	if e.Username != "" {
		auth = smtp.PlainAuth("", e.Username, e.Password, host)
	}
	msg := "From: " + e.From + "\r\nTo: " + e.To + "\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.ReplaceAll(text, "\n", "\r\n")
	var to []string
	for _, a := range strings.Split(e.To, ",") {
		to = append(to, strings.TrimSpace(a))
	}
	return smtp.SendMail(e.Smarthost, auth, e.From, to, []byte(msg))
}

// cronTag marks the crontab line Schedule manages.
const cronTag = "# infra-monitor digest"

// CronLine is the crontab entry that sends the digest at hh:mm local time.
func CronLine(hh, mm int, binary, dir string) string {
	return fmt.Sprintf("%d %d * * * %s --dir %s digest send >/dev/null 2>&1 %s",
		mm, hh, shellQuote(binary), shellQuote(dir), cronTag)
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Crontab reads and writes the user's crontab; tests replace it.
type Crontab interface {
	Read() (string, error)
	Write(string) error
}

// SystemCrontab uses the crontab command.
type SystemCrontab struct{}

func (SystemCrontab) Read() (string, error) {
	if _, err := exec.LookPath("crontab"); err != nil {
		return "", errors.New("cron isn't installed (no `crontab` command)")
	}
	out, err := exec.Command("crontab", "-l").Output()
	if err != nil {
		// "no crontab for user" exits 1 with nothing on stdout.
		if ee, ok := err.(*exec.ExitError); ok && len(out) == 0 && ee.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return string(out), nil
}

func (SystemCrontab) Write(s string) error {
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(s)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("crontab: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Schedule replaces any digest line in the crontab with line, or removes it
// when line is empty. It reports whether a line was there before.
func Schedule(c Crontab, line string) (bool, error) {
	cur, err := c.Read()
	if err != nil {
		return false, err
	}
	var keep []string
	had := false
	for _, l := range strings.Split(strings.TrimRight(cur, "\n"), "\n") {
		if strings.HasSuffix(l, cronTag) {
			had = true
			continue
		}
		if l != "" || len(keep) > 0 {
			keep = append(keep, l)
		}
	}
	if line != "" {
		keep = append(keep, line)
	}
	out := strings.Join(keep, "\n")
	if out != "" {
		out += "\n"
	}
	return had, c.Write(out)
}

// Scheduled returns the digest line in the crontab, if any.
func Scheduled(c Crontab) (string, error) {
	cur, err := c.Read()
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(cur, "\n") {
		if strings.HasSuffix(l, cronTag) {
			return l, nil
		}
	}
	return "", nil
}

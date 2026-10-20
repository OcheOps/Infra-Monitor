// Package config holds the user's settings: what to monitor, where to send
// alerts, retention, and Kubernetes options. It is stored as YAML in the
// working directory and shared by the Compose and Kubernetes backends.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the config file inside the working directory.
const FileName = "config.yaml"

type Config struct {
	Grafana       Grafana       `yaml:"grafana"`
	Probes        []string      `yaml:"probes,omitempty"`
	Targets       []Target      `yaml:"targets,omitempty"`
	Notifications Notifications `yaml:"notifications,omitempty"`
	Retention     Retention     `yaml:"retention"`
	Gateway       Gateway       `yaml:"gateway,omitempty"`
	StatusPage    StatusPage    `yaml:"status_page,omitempty"`
	Kubernetes    Kubernetes    `yaml:"kubernetes"`
}

// Gateway puts Grafana and the status page behind HTTPS on a public domain.
// With no Domain, the gateway only listens on localhost.
type Gateway struct {
	Domain       string `yaml:"domain,omitempty"`
	StatusDomain string `yaml:"status_domain,omitempty"`
	// TLS is "auto" (Let's Encrypt, the default) or "internal" (a certificate
	// from the gateway's own CA, for hosts without public DNS).
	TLS   string `yaml:"tls,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// StatusPage is a public page showing the state of selected probes.
type StatusPage struct {
	Enabled  bool      `yaml:"enabled,omitempty"`
	Title    string    `yaml:"title,omitempty"`
	Monitors []Monitor `yaml:"monitors,omitempty"`
}

// Monitor is a probe shown on the status page under a display name.
type Monitor struct {
	Probe string `yaml:"probe"`
	Name  string `yaml:"name"`
}

type Grafana struct {
	AdminPassword string `yaml:"admin_password"`
}

// Target is an extra Prometheus scrape target, such as a node exporter on
// another host or an application's /metrics endpoint.
type Target struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
	Path    string `yaml:"path,omitempty"`
}

type Notifications struct {
	Telegram *Telegram `yaml:"telegram,omitempty"`
	Slack    *Slack    `yaml:"slack,omitempty"`
	Email    *Email    `yaml:"email,omitempty"`
	Webhook  *Webhook  `yaml:"webhook,omitempty"`
}

type Telegram struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

type Slack struct {
	WebhookURL string `yaml:"webhook_url"`
	Channel    string `yaml:"channel,omitempty"`
}

type Email struct {
	To        string `yaml:"to"`
	From      string `yaml:"from"`
	Smarthost string `yaml:"smarthost"`
	Username  string `yaml:"username,omitempty"`
	Password  string `yaml:"password,omitempty"`
}

type Webhook struct {
	URL string `yaml:"url"`
}

// Retention values are durations like 15d, 72h or 2w.
type Retention struct {
	Metrics string `yaml:"metrics"`
	Logs    string `yaml:"logs"`
	Traces  string `yaml:"traces"`
}

type Kubernetes struct {
	Namespace    string  `yaml:"namespace"`
	Context      string  `yaml:"context,omitempty"`
	StorageClass string  `yaml:"storage_class,omitempty"`
	Storage      Storage `yaml:"storage"`
	Ingress      Ingress `yaml:"ingress,omitempty"`
}

type Storage struct {
	Prometheus string `yaml:"prometheus"`
	Loki       string `yaml:"loki"`
	Jaeger     string `yaml:"jaeger"`
	Grafana    string `yaml:"grafana"`
}

// Ingress exposes Grafana through an Ingress when Host is set.
type Ingress struct {
	Host          string `yaml:"host,omitempty"`
	ClassName     string `yaml:"class_name,omitempty"`
	TLSSecret     string `yaml:"tls_secret,omitempty"`
	ClusterIssuer string `yaml:"cluster_issuer,omitempty"`
}

// Default returns a config with every optional field at its default.
func Default() Config {
	var c Config
	c.ApplyDefaults()
	return c
}

// ApplyDefaults fills empty fields with their defaults.
func (c *Config) ApplyDefaults() {
	set := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	set(&c.Retention.Metrics, "15d")
	set(&c.Retention.Logs, "30d")
	set(&c.Retention.Traces, "3d")
	set(&c.Kubernetes.Namespace, "infra-monitor")
	set(&c.Kubernetes.Storage.Prometheus, "10Gi")
	set(&c.Kubernetes.Storage.Loki, "10Gi")
	set(&c.Kubernetes.Storage.Jaeger, "5Gi")
	set(&c.Kubernetes.Storage.Grafana, "1Gi")
	if c.Gateway.Domain != "" {
		set(&c.Gateway.TLS, "auto")
	}
	set(&c.StatusPage.Title, "Service status")
}

// PublicHost is the hostname Grafana is served on, for either backend:
// gateway.domain, or the older kubernetes.ingress.host.
func (c Config) PublicHost() string {
	if c.Gateway.Domain != "" {
		return c.Gateway.Domain
	}
	return c.Kubernetes.Ingress.Host
}

// ErrNotFound is returned by Load when no config exists yet.
var ErrNotFound = errors.New("config not found")

// Path returns the config file path inside dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Load reads dir/config.yaml. If it is missing but a .env file from an older
// version exists, the .env is imported, saved as config.yaml and renamed.
func Load(dir string) (Config, error) {
	var c Config
	data, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		if c, ok, err := migrateEnv(dir); ok || err != nil {
			return c, err
		}
		return Default(), fmt.Errorf("%w: %s (run `infra-monitor init` first)", ErrNotFound, Path(dir))
	}
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("%s: %w", Path(dir), err)
	}
	c.ApplyDefaults()
	return c, nil
}

// migrateEnv imports the KEY=VALUE .env file written by earlier versions.
func migrateEnv(dir string) (Config, bool, error) {
	envPath := filepath.Join(dir, ".env")
	f, err := os.Open(envPath)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			vals[strings.TrimSpace(k)] = v
		}
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return Config{}, false, err
	}

	c := Default()
	c.Grafana.AdminPassword = vals["GRAFANA_ADMIN_PASSWORD"]
	if tok, chat := vals["TELEGRAM_API_KEY"], vals["TELEGRAM_CHAT_ID"]; tok != "" || chat != "" {
		c.Notifications.Telegram = &Telegram{BotToken: tok, ChatID: chat}
	}
	if err := c.Save(dir); err != nil {
		return c, true, err
	}
	return c, true, os.Rename(envPath, envPath+".migrated")
}

// Save writes the config with owner-only permissions, since it holds secrets.
func (c Config) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# infra-monitor configuration. Holds secrets: keep it private.\n" +
		"# Edit with `infra-monitor init`, `probes`, `targets` and `notify`, or by hand.\n"
	p := Path(dir)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), data...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

var (
	telegramTokenRe = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
	targetNameRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,61}[a-z0-9])?$`)
	durationRe      = regexp.MustCompile(`^([0-9]+)(h|d|w)$`)
	quantityRe      = regexp.MustCompile(`^[0-9]+(Mi|Gi|Ti)$`)
	dnsLabelRe      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsNameRe       = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	hostnameRe      = regexp.MustCompile(`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)
	urlPathRe       = regexp.MustCompile(`^/[A-Za-z0-9/_.~-]*$`)
)

// reservedJobs are scrape job names the stack already uses.
var reservedJobs = map[string]bool{
	"prometheus": true, "node": true, "cadvisor": true, "alertmanager": true,
	"blackbox": true, "blackbox-tcp": true, "kubelet": true, "pods": true,
	"kube-state-metrics": true,
}

// Validate checks every field and returns all problems at once.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch {
	case c.Grafana.AdminPassword == "":
		add("grafana.admin_password is empty")
	case strings.ContainsAny(c.Grafana.AdminPassword, "\n\r$'\\"):
		add("grafana.admin_password must not contain quotes, backslashes, '$' or newlines")
	}

	seen := map[string]bool{}
	for _, p := range c.Probes {
		if err := ValidateProbe(p); err != nil {
			errs = append(errs, err)
		}
		if seen[p] {
			add("probe %s is listed twice", p)
		}
		seen[p] = true
	}
	names := map[string]bool{}
	for _, t := range c.Targets {
		if err := t.Validate(); err != nil {
			errs = append(errs, err)
		}
		if names[t.Name] {
			add("target name %q is used twice", t.Name)
		}
		names[t.Name] = true
	}

	g := c.Gateway
	for _, h := range []struct{ name, v string }{{"domain", g.Domain}, {"status_domain", g.StatusDomain}} {
		if h.v != "" && (!hostnameRe.MatchString(h.v) || strings.HasPrefix(h.v, "*.")) && h.v != "localhost" {
			add("gateway.%s %q is not a valid hostname", h.name, h.v)
		}
	}
	if g.StatusDomain != "" && g.Domain == "" {
		add("gateway.status_domain needs gateway.domain to be set too")
	}
	if g.StatusDomain != "" && g.StatusDomain == g.Domain {
		add("gateway.status_domain must differ from gateway.domain (the status page is already at /status)")
	}
	if g.TLS != "" && g.TLS != "auto" && g.TLS != "internal" {
		add("gateway.tls must be auto or internal")
	}
	if g.Email != "" {
		if _, err := mail.ParseAddress(g.Email); err != nil {
			add("gateway.email %q is invalid", g.Email)
		}
	}

	sp := c.StatusPage
	if len(sp.Title) > 80 || strings.ContainsAny(sp.Title, "\n\r<>") {
		add("status_page.title must be at most 80 characters without <, > or newlines")
	}
	shown := map[string]bool{}
	for _, m := range sp.Monitors {
		if !slices.Contains(c.Probes, m.Probe) {
			add("status_page monitor %q is not one of your probes", m.Probe)
		}
		if shown[m.Probe] {
			add("status_page shows %q twice", m.Probe)
		}
		shown[m.Probe] = true
		if m.Name == "" || len(m.Name) > 80 || strings.ContainsAny(m.Name, "\n\r") {
			add("status_page monitor %q needs a name of at most 80 characters", m.Probe)
		}
	}

	n := c.Notifications
	if t := n.Telegram; t != nil {
		if !telegramTokenRe.MatchString(t.BotToken) {
			add("telegram bot token must look like 123456:ABC-xyz (from @BotFather)")
		}
		if _, err := strconv.ParseInt(t.ChatID, 10, 64); err != nil {
			add("telegram chat id must be a number")
		}
	}
	if s := n.Slack; s != nil {
		if err := validateHTTPURL(s.WebhookURL, true); err != nil {
			add("slack webhook url: %v", err)
		}
	}
	if e := n.Email; e != nil {
		for _, a := range []struct{ name, v string }{{"to", e.To}, {"from", e.From}} {
			if _, err := mail.ParseAddress(a.v); err != nil {
				add("email %s address %q is invalid", a.name, a.v)
			}
		}
		if _, port, err := net.SplitHostPort(e.Smarthost); err != nil || port == "" {
			add("email smarthost must be host:port, e.g. smtp.gmail.com:587")
		}
	}
	if w := n.Webhook; w != nil {
		if err := validateHTTPURL(w.URL, false); err != nil {
			add("webhook url: %v", err)
		}
	}

	for _, r := range []struct{ name, v string }{
		{"metrics", c.Retention.Metrics}, {"logs", c.Retention.Logs}, {"traces", c.Retention.Traces},
	} {
		if _, err := Hours(r.v); err != nil {
			add("retention.%s: %v", r.name, err)
		}
	}

	k := c.Kubernetes
	if !dnsLabelRe.MatchString(k.Namespace) {
		add("kubernetes.namespace %q must be a DNS-1123 label", k.Namespace)
	}
	if k.StorageClass != "" && !dnsNameRe.MatchString(k.StorageClass) {
		add("kubernetes.storage_class %q is invalid", k.StorageClass)
	}
	for _, s := range []struct{ name, v string }{
		{"prometheus", k.Storage.Prometheus}, {"loki", k.Storage.Loki},
		{"jaeger", k.Storage.Jaeger}, {"grafana", k.Storage.Grafana},
	} {
		if !quantityRe.MatchString(s.v) {
			add("kubernetes.storage.%s %q must look like 10Gi or 500Mi", s.name, s.v)
		}
	}
	if in := k.Ingress; in.Host != "" {
		if !hostnameRe.MatchString(in.Host) {
			add("kubernetes.ingress.host %q is not a valid hostname", in.Host)
		}
		for _, f := range []struct{ name, v string }{
			{"class_name", in.ClassName}, {"tls_secret", in.TLSSecret}, {"cluster_issuer", in.ClusterIssuer},
		} {
			if f.v != "" && !dnsNameRe.MatchString(f.v) {
				add("kubernetes.ingress.%s %q is invalid", f.name, f.v)
			}
		}
	} else if (in.TLSSecret != "" || in.ClusterIssuer != "" || in.ClassName != "") && c.Gateway.Domain == "" {
		add("kubernetes.ingress settings need a host (set one with `infra-monitor expose --domain`)")
	}
	return errors.Join(errs...)
}

// ValidateProbe checks a URL for the Blackbox exporter: http(s)://... for
// HTTP checks or tcp://host:port for TCP connect checks.
func ValidateProbe(p string) error {
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return fmt.Errorf("probe %q: must be a full URL like https://example.com or tcp://db:5432", p)
	}
	switch u.Scheme {
	case "http", "https":
	case "tcp":
		if _, port, err := net.SplitHostPort(u.Host); err != nil || port == "" || u.Path != "" {
			return fmt.Errorf("probe %q: TCP checks need tcp://host:port", p)
		}
	default:
		return fmt.Errorf("probe %q: scheme must be http, https or tcp", p)
	}
	if strings.ContainsAny(p, " \"'\n\\") {
		return fmt.Errorf("probe %q: contains invalid characters", p)
	}
	return nil
}

func (t Target) Validate() error {
	var errs []error
	if !targetNameRe.MatchString(t.Name) {
		errs = append(errs, fmt.Errorf("target name %q: use lowercase letters, digits, '-' or '_'", t.Name))
	} else if reservedJobs[t.Name] {
		errs = append(errs, fmt.Errorf("target name %q is used by the stack itself; pick another", t.Name))
	}
	host, port, err := net.SplitHostPort(t.Address)
	if err != nil || host == "" || port == "" || strings.ContainsAny(t.Address, " \"'") {
		errs = append(errs, fmt.Errorf("target %q address %q must be host:port, e.g. 10.0.0.5:9100", t.Name, t.Address))
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		errs = append(errs, fmt.Errorf("target %q port %q is invalid", t.Name, port))
	}
	if t.Path != "" && !urlPathRe.MatchString(t.Path) {
		errs = append(errs, fmt.Errorf("target %q path %q must start with / and contain only URL path characters", t.Name, t.Path))
	}
	return errors.Join(errs...)
}

func validateHTTPURL(s string, httpsOnly bool) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && (httpsOnly || u.Scheme != "http")) {
		if httpsOnly {
			return fmt.Errorf("%q must be an https:// URL", s)
		}
		return fmt.Errorf("%q must be an http:// or https:// URL", s)
	}
	if strings.ContainsAny(s, " \"'\n\\") {
		return fmt.Errorf("%q contains invalid characters", s)
	}
	return nil
}

// Hours converts a retention duration (e.g. 15d, 72h, 2w) to whole hours.
func Hours(d string) (int, error) {
	m := durationRe.FindStringSubmatch(d)
	if m == nil {
		return 0, fmt.Errorf("%q must be a number followed by h, d or w (e.g. 72h, 15d, 2w)", d)
	}
	n, _ := strconv.Atoi(m[1])
	switch m[2] {
	case "d":
		n *= 24
	case "w":
		n *= 24 * 7
	}
	if n < 24 {
		return 0, fmt.Errorf("%q is shorter than the 24h minimum", d)
	}
	return n, nil
}

// NotificationsEnabled reports whether any alert channel is configured.
func (c Config) NotificationsEnabled() bool {
	n := c.Notifications
	return n.Telegram != nil || n.Slack != nil || n.Email != nil || n.Webhook != nil
}

// Channels lists the configured notification channels by name.
func (c Config) Channels() []string {
	var out []string
	n := c.Notifications
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

// GeneratePassword returns a random 32-character hex password.
func GeneratePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

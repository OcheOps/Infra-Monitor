package cli

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"

	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/prompt"
)

// wizard walks through the whole configuration, then offers to start the stack.
func (a *app) wizard() error {
	cfg, existed, err := a.loadOrDefault()
	if err != nil {
		return err
	}
	p := a.p
	p.Printf("\nInfra Monitor setup\n")
	p.Printf("Settings are saved to %s.\n", config.Path(a.dir))
	if existed {
		p.Printf("Press Enter to keep the current value shown in [brackets].\n")
	}

	p.Printf("\n[1/5] Grafana admin password\n")
	generate, err := p.Confirm("Generate a random password?", cfg.Grafana.AdminPassword == "")
	if err != nil {
		return err
	}
	generated := false
	if generate {
		if cfg.Grafana.AdminPassword, err = config.GeneratePassword(); err != nil {
			return err
		}
		generated = true
	} else if cfg.Grafana.AdminPassword, err = p.Secret("Password", cfg.Grafana.AdminPassword, func(s string) error {
		c := cfg
		c.Grafana.AdminPassword = s
		if len(s) < 8 {
			return errors.New("use at least 8 characters")
		}
		return passwordErr(c)
	}); err != nil {
		return err
	}

	p.Printf("\n[2/5] Websites and services to check\n")
	p.Printf("Each URL is checked every 15 seconds; you get an alert when it fails,\n")
	p.Printf("is slow, or its TLS certificate is about to expire.\n")
	p.Printf("Use https://... or http://... for web pages, tcp://host:port for other services.\n")
	if err := a.editProbes(&cfg); err != nil {
		return err
	}

	p.Printf("\n[3/5] Other metrics endpoints\n")
	p.Printf("Prometheus already collects this host's metrics. Add other machines' node\n")
	p.Printf("exporters or your apps' /metrics endpoints here.\n")
	if err := a.editTargets(&cfg); err != nil {
		return err
	}

	p.Printf("\n[4/5] Where should alerts go?\n")
	if err := a.askNotifications(&cfg); err != nil {
		return err
	}

	p.Printf("\n[5/5] How long to keep data (e.g. 72h, 15d, 2w)\n")
	if err := a.askRetention(&cfg); err != nil {
		return err
	}

	p.Printf("\nSummary\n")
	a.printSummary(cfg)
	ok, err := p.Confirm("Save these settings?", true)
	if err != nil {
		return err
	}
	if !ok {
		p.Printf("Nothing was saved.\n")
		return nil
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	p.Printf("Saved.\n")
	if generated {
		p.Printf("Grafana login: admin / %s  (shown once; also in %s)\n", cfg.Grafana.AdminPassword, config.Path(a.dir))
	}
	return a.offerApply("Start the stack now?")
}

func passwordErr(c config.Config) error {
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "admin_password") {
		return errors.New("must not contain quotes, backslashes, '$' or newlines")
	}
	return nil
}

func (a *app) editProbes(cfg *config.Config) error {
	p := a.p
	if len(cfg.Probes) > 0 {
		p.Printf("Currently checked:\n")
		for _, u := range cfg.Probes {
			p.Printf("  - %s\n", u)
		}
		remove, err := p.Confirm("Remove any of these?", false)
		if err != nil {
			return err
		}
		for remove && len(cfg.Probes) > 0 {
			i, err := p.Select("Remove which?", cfg.Probes, -1)
			if err != nil {
				return err
			}
			cfg.Probes = slices.Delete(cfg.Probes, i, i+1)
			if remove, err = p.Confirm("Remove another?", false); err != nil {
				return err
			}
		}
	}
	added, err := a.askProbes(cfg.Probes)
	cfg.Probes = append(cfg.Probes, added...)
	return err
}

// askProbes prompts for new URLs, skipping ones already in existing.
func (a *app) askProbes(existing []string) ([]string, error) {
	var added []string
	_, err := a.p.List("URL to check", func(s string) error {
		if err := config.ValidateProbe(s); err != nil {
			return err
		}
		if slices.Contains(existing, s) || slices.Contains(added, s) {
			return errors.New("already in the list")
		}
		added = append(added, s)
		return nil
	})
	return added, err
}

func (a *app) editTargets(cfg *config.Config) error {
	p := a.p
	for _, t := range cfg.Targets {
		p.Printf("  - %s (%s)\n", t.Name, t.Address)
	}
	for {
		more, err := p.Confirm("Add a scrape target?", false)
		if err != nil || !more {
			return err
		}
		t, err := a.askTarget(*cfg)
		if err != nil {
			return err
		}
		cfg.Targets = append(cfg.Targets, t)
	}
}

func (a *app) askTarget(cfg config.Config) (config.Target, error) {
	p := a.p
	var t config.Target
	var err error
	if t.Address, err = p.Input("Address (host:port, e.g. 10.0.0.5:9100)", "", func(s string) error {
		return config.Target{Name: "x", Address: s}.Validate()
	}); err != nil {
		return t, err
	}
	host, _, _ := net.SplitHostPort(t.Address)
	def := strings.ToLower(strings.NewReplacer(".", "-", ":", "-").Replace(host))
	if (config.Target{Name: def, Address: t.Address}).Validate() != nil {
		def = ""
	}
	if t.Name, err = p.Input("Name", def, func(s string) error {
		if slices.ContainsFunc(cfg.Targets, func(x config.Target) bool { return x.Name == s }) {
			return errors.New("that name is already used")
		}
		return config.Target{Name: s, Address: t.Address}.Validate()
	}); err != nil {
		return t, err
	}
	path, err := p.Input("Metrics path", "/metrics", func(s string) error {
		return config.Target{Name: t.Name, Address: t.Address, Path: s}.Validate()
	})
	if path != "/metrics" {
		t.Path = path
	}
	return t, err
}

func (a *app) askNotifications(cfg *config.Config) error {
	p := a.p
	n := &cfg.Notifications
	if keep, err := p.Confirm("Send alerts to Telegram?", n.Telegram != nil); err != nil {
		return err
	} else if !keep {
		n.Telegram = nil
	} else {
		t := config.Telegram{}
		if n.Telegram != nil {
			t = *n.Telegram
		}
		p.Printf("  Create a bot with @BotFather, message it once, then open\n")
		p.Printf("  https://api.telegram.org/bot<TOKEN>/getUpdates and copy chat.id.\n")
		var err error
		if t.BotToken, err = p.Secret("  Bot token", t.BotToken, validField(func(c *config.Config, v string) {
			c.Notifications.Telegram = &config.Telegram{BotToken: v, ChatID: "1"}
		})); err != nil {
			return err
		}
		if t.ChatID, err = p.Input("  Chat ID", t.ChatID, validField(func(c *config.Config, v string) {
			c.Notifications.Telegram = &config.Telegram{BotToken: "1:a", ChatID: v}
		})); err != nil {
			return err
		}
		n.Telegram = &t
	}

	if keep, err := p.Confirm("Send alerts to Slack?", n.Slack != nil); err != nil {
		return err
	} else if !keep {
		n.Slack = nil
	} else {
		s := config.Slack{}
		if n.Slack != nil {
			s = *n.Slack
		}
		p.Printf("  Create an incoming webhook at https://api.slack.com/messaging/webhooks\n")
		var err error
		if s.WebhookURL, err = p.Secret("  Webhook URL", s.WebhookURL, validField(func(c *config.Config, v string) {
			c.Notifications.Slack = &config.Slack{WebhookURL: v}
		})); err != nil {
			return err
		}
		if s.Channel, err = p.Input("  Channel (optional, e.g. #alerts)", s.Channel, nil); err != nil {
			return err
		}
		n.Slack = &s
	}

	if keep, err := p.Confirm("Send alerts by email?", n.Email != nil); err != nil {
		return err
	} else if !keep {
		n.Email = nil
	} else {
		e := config.Email{}
		if n.Email != nil {
			e = *n.Email
		}
		email := validField(func(c *config.Config, v string) {
			c.Notifications.Email = &config.Email{To: v, From: v, Smarthost: "h:1"}
		})
		var err error
		if e.To, err = p.Input("  Send to", e.To, email); err != nil {
			return err
		}
		if e.From, err = p.Input("  Send from", orDefault(e.From, e.To), email); err != nil {
			return err
		}
		if e.Smarthost, err = p.Input("  SMTP server (host:port)", orDefault(e.Smarthost, "smtp.gmail.com:587"), validField(func(c *config.Config, v string) {
			c.Notifications.Email = &config.Email{To: "a@b.c", From: "a@b.c", Smarthost: v}
		})); err != nil {
			return err
		}
		if e.Username, err = p.Input("  SMTP username (blank for none)", orDefault(e.Username, e.From), nil); err != nil {
			return err
		}
		if e.Username != "" {
			if e.Password, err = p.Secret("  SMTP password", e.Password, nil); err != nil {
				return err
			}
		} else {
			e.Password = ""
		}
		n.Email = &e
	}

	if keep, err := p.Confirm("Send alerts to a webhook (JSON POST)?", n.Webhook != nil); err != nil {
		return err
	} else if !keep {
		n.Webhook = nil
	} else {
		w := config.Webhook{}
		if n.Webhook != nil {
			w = *n.Webhook
		}
		var err error
		if w.URL, err = p.Input("  Webhook URL", w.URL, validField(func(c *config.Config, v string) {
			c.Notifications.Webhook = &config.Webhook{URL: v}
		})); err != nil {
			return err
		}
		n.Webhook = &w
	}
	if !cfg.NotificationsEnabled() {
		p.Printf("No channels chosen: alerts will only show in Prometheus and Alertmanager.\n")
	}
	return nil
}

// validField builds a Validator that sets one field on an otherwise valid
// default config, so only that field's errors are reported.
func validField(set func(c *config.Config, v string)) prompt.Validator {
	return func(v string) error {
		c := config.Default()
		c.Grafana.AdminPassword = "placeholder"
		set(&c, v)
		return c.Validate()
	}
}

func (a *app) askRetention(cfg *config.Config) error {
	p := a.p
	valid := func(s string) error { _, err := config.Hours(s); return err }
	r := &cfg.Retention
	var err error
	if r.Metrics, err = p.Input("Metrics", r.Metrics, valid); err != nil {
		return err
	}
	if r.Logs, err = p.Input("Logs", r.Logs, valid); err != nil {
		return err
	}
	r.Traces, err = p.Input("Traces", r.Traces, valid)
	return err
}

func (a *app) askKubernetes(cfg *config.Config) error {
	p := a.p
	k := &cfg.Kubernetes
	p.Printf("\nKubernetes settings\n")
	if ctx := currentContext(); ctx != "" {
		p.Printf("kubectl's current context is %q.\n", ctx)
	}
	var err error
	if k.Context, err = p.Input("kubectl context (blank: current)", k.Context, nil); err != nil {
		return err
	}
	check := validField
	if k.Namespace, err = p.Input("Namespace", k.Namespace, check(func(c *config.Config, v string) { c.Kubernetes.Namespace = v })); err != nil {
		return err
	}
	if k.StorageClass, err = p.Input("StorageClass (blank: cluster default)", k.StorageClass, check(func(c *config.Config, v string) { c.Kubernetes.StorageClass = v })); err != nil {
		return err
	}
	size := check(func(c *config.Config, v string) { c.Kubernetes.Storage.Prometheus = v })
	if k.Storage.Prometheus, err = p.Input("Prometheus volume size", k.Storage.Prometheus, size); err != nil {
		return err
	}
	if k.Storage.Loki, err = p.Input("Loki volume size", k.Storage.Loki, size); err != nil {
		return err
	}
	if k.Storage.Jaeger, err = p.Input("Jaeger volume size", k.Storage.Jaeger, size); err != nil {
		return err
	}

	expose, err := p.Confirm("Expose Grafana through an Ingress?", k.Ingress.Host != "")
	if err != nil {
		return err
	}
	if !expose {
		k.Ingress = config.Ingress{}
		return nil
	}
	in := &k.Ingress
	host := func(c *config.Config, v string) { c.Kubernetes.Ingress.Host = v }
	if in.Host, err = p.Input("Hostname (e.g. grafana.example.com)", in.Host, check(host)); err != nil {
		return err
	}
	if in.ClassName, err = p.Input("IngressClass (blank: cluster default, e.g. nginx, traefik)", in.ClassName, check(func(c *config.Config, v string) {
		c.Kubernetes.Ingress = config.Ingress{Host: "a.example.com", ClassName: v}
	})); err != nil {
		return err
	}
	opts := []string{"cert-manager issues a certificate", "Use an existing TLS Secret", "No TLS (plain HTTP)"}
	def := 2
	switch {
	case in.ClusterIssuer != "":
		def = 0
	case in.TLSSecret != "":
		def = 1
	}
	choice, err := p.Select("HTTPS for Grafana:", opts, def)
	if err != nil {
		return err
	}
	name := func(c *config.Config, v string) {
		c.Kubernetes.Ingress = config.Ingress{Host: "a.example.com", ClusterIssuer: v}
	}
	switch choice {
	case 0:
		in.TLSSecret = ""
		in.ClusterIssuer, err = p.Input("ClusterIssuer name", orDefault(in.ClusterIssuer, "letsencrypt-prod"), check(name))
	case 1:
		in.ClusterIssuer = ""
		in.TLSSecret, err = p.Input("TLS Secret name (in the same namespace)", in.TLSSecret, check(name))
	default:
		in.ClusterIssuer, in.TLSSecret = "", ""
	}
	return err
}

func currentContext() string {
	out, err := exec.Command("kubectl", "config", "current-context").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// offerApply asks where to deploy and does it.
func (a *app) offerApply(question string) error {
	opts := []string{
		"Docker Compose on this host",
		"Kubernetes (" + orDefault(currentContext(), "current kubectl context") + ")",
		"Not now",
	}
	choice, err := a.p.Select(question, opts, 2)
	if err != nil {
		return err
	}
	switch choice {
	case 0:
		return a.composeUp(false)
	case 1:
		return a.offerK8s()
	default:
		fmt.Fprintln(a.stdout, applyHint)
		return nil
	}
}

// menu is the interactive entry point when no command is given.
func (a *app) menu() error {
	if _, err := a.load(); errors.Is(err, config.ErrNotFound) {
		a.p.Printf("Welcome to Infra Monitor! Let's set things up.\n")
		if err := a.wizard(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	actions := []struct {
		label string
		run   func() error
	}{
		{"Check a website or service (add / remove URLs)", a.menuProbes},
		{"Scrape another metrics endpoint (add / remove targets)", a.menuTargets},
		{"Monitor a database, cache or web server on this host", a.menuIntegrations},
		{"Alert notifications (Telegram, Slack, email, webhook)", a.menuNotify},
		{"Public access over HTTPS and the status page", a.menuPublic},
		{"Monitor another server (add an agent)", a.menuAgent},
		{"Start or update on this host (Docker Compose)", func() error { return a.composeUp(false) }},
		{"Deploy or update on Kubernetes", a.offerK8s},
		{"Show status", a.menuStatus},
		{"Show configuration", func() error { return a.cmdConfig([]string{"show"}) }},
		{"Run the full setup again", a.wizard},
		{"Quit", nil},
	}
	labels := make([]string, len(actions))
	for i, x := range actions {
		labels[i] = x.label
	}
	for {
		a.p.Printf("\n")
		i, err := a.p.Select("What would you like to do?", labels, -1)
		if err != nil {
			return err
		}
		if actions[i].run == nil {
			return nil
		}
		if err := actions[i].run(); err != nil {
			if errors.Is(err, prompt.ErrAborted) {
				return err
			}
			// Report and return to the menu rather than exiting.
			a.p.Printf("✗ %v\n", err)
		}
	}
}

func (a *app) offerK8s() error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	review, err := a.p.Confirm(fmt.Sprintf("Review Kubernetes settings first (namespace %s)?", cfg.Kubernetes.Namespace), false)
	if err != nil {
		return err
	}
	if review {
		if err := a.askKubernetes(&cfg); err != nil {
			return err
		}
		if err := a.save(cfg); err != nil {
			return err
		}
	}
	return a.cmdK8s([]string{"apply"})
}

func (a *app) menuProbes() error {
	if err := a.cmdProbes([]string{"list"}); err != nil {
		return err
	}
	i, err := a.p.Select("URLs:", []string{"Add", "Remove", "Back"}, 2)
	if err != nil {
		return err
	}
	switch i {
	case 0:
		return a.cmdProbes([]string{"add"})
	case 1:
		return a.cmdProbes([]string{"remove"})
	}
	return nil
}

func (a *app) menuTargets() error {
	if err := a.cmdTargets([]string{"list"}); err != nil {
		return err
	}
	i, err := a.p.Select("Scrape targets:", []string{"Add", "Remove", "Back"}, 2)
	if err != nil {
		return err
	}
	switch i {
	case 0:
		return a.cmdTargets([]string{"add"})
	case 1:
		return a.cmdTargets([]string{"remove"})
	}
	return nil
}

func (a *app) menuNotify() error {
	if err := a.cmdNotify([]string{"list"}); err != nil {
		return err
	}
	i, err := a.p.Select("Notifications:", []string{"Change channels", "Send a test alert", "Back"}, 2)
	if err != nil {
		return err
	}
	switch i {
	case 0:
		return a.cmdNotify([]string{"setup"})
	case 1:
		return a.cmdNotify([]string{"test"})
	}
	return nil
}

func (a *app) menuPublic() error {
	if err := a.cmdExpose(nil); err != nil {
		return err
	}
	return a.cmdStatusPage([]string{"enable"})
}

func (a *app) menuAgent() error {
	if err := a.agentList(); err != nil {
		return err
	}
	name, err := a.p.Input("Name for the new server (blank to go back)", "", func(v string) error {
		if v == "" {
			return nil
		}
		return config.Target{Name: v, Address: "x:1"}.Validate()
	})
	if err != nil || name == "" {
		return err
	}
	return a.agentAdd([]string{name})
}

func (a *app) menuStatus() error {
	i, err := a.p.Select("Status of:", []string{"Checks, hosts and alerts", "Docker Compose containers on this host", "Kubernetes pods", "Back"}, 0)
	if err != nil {
		return err
	}
	switch i {
	case 0:
		return a.cmdStatus(nil)
	case 1:
		return a.cmdCompose([]string{"status"})
	case 2:
		return a.cmdK8s([]string{"status"})
	}
	return nil
}

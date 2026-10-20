package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/OcheOps/Infra-Monitor/internal/compose"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/doctor"
	"github.com/OcheOps/Infra-Monitor/internal/k8s"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

// ---- init -------------------------------------------------------------

func (a *app) cmdInit(args []string) error {
	fs := a.newFlags("init")
	password := fs.String("grafana-password", "", "Grafana admin password (generated if unset)")
	var probes stringList
	fs.Var(&probes, "probe", "URL to check, e.g. https://example.com or tcp://db:5432 (repeatable)")
	tgToken := fs.String("telegram-token", "", "Telegram bot token from @BotFather")
	tgChat := fs.String("telegram-chat-id", "", "Telegram chat id to send alerts to")
	noTelegram := fs.Bool("no-telegram", false, "remove the Telegram channel")
	slackURL := fs.String("slack-webhook", "", "Slack incoming webhook URL")
	slackChannel := fs.String("slack-channel", "", "Slack channel, e.g. #alerts (optional)")
	webhookURL := fs.String("webhook-url", "", "generic webhook URL that receives alerts as JSON")
	retMetrics := fs.String("retention-metrics", "", "how long to keep metrics (e.g. 15d)")
	retLogs := fs.String("retention-logs", "", "how long to keep logs (e.g. 30d)")
	retTraces := fs.String("retention-traces", "", "how long to keep traces (e.g. 3d)")
	noInput := fs.Bool("no-input", false, "never prompt; use flags and defaults only")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if a.interactive && !*noInput && fs.NFlag() == 0 {
		return a.wizard()
	}

	cfg, existed, err := a.loadOrDefault()
	if err != nil {
		return err
	}
	if *password != "" {
		cfg.Grafana.AdminPassword = *password
	}
	for _, p := range probes {
		if !slices.Contains(cfg.Probes, p) {
			cfg.Probes = append(cfg.Probes, p)
		}
	}
	if *noTelegram {
		cfg.Notifications.Telegram = nil
	}
	if *tgToken != "" || *tgChat != "" {
		t := cfg.Notifications.Telegram
		if t == nil {
			t = &config.Telegram{}
		}
		if *tgToken != "" {
			t.BotToken = *tgToken
		}
		if *tgChat != "" {
			t.ChatID = *tgChat
		}
		cfg.Notifications.Telegram = t
	}
	if *slackURL != "" {
		cfg.Notifications.Slack = &config.Slack{WebhookURL: *slackURL, Channel: *slackChannel}
	}
	if *webhookURL != "" {
		cfg.Notifications.Webhook = &config.Webhook{URL: *webhookURL}
	}
	for _, r := range []struct {
		dst *string
		v   string
	}{{&cfg.Retention.Metrics, *retMetrics}, {&cfg.Retention.Logs, *retLogs}, {&cfg.Retention.Traces, *retTraces}} {
		if r.v != "" {
			*r.dst = r.v
		}
	}

	generated := cfg.Grafana.AdminPassword == ""
	if err := a.save(cfg); err != nil {
		return err
	}
	cfg, _ = a.load()
	fmt.Fprintf(a.stdout, "Config written to %s\n", config.Path(a.dir))
	a.printSummary(cfg)
	if generated {
		fmt.Fprintf(a.stdout, "\nGrafana login: admin / %s\n", cfg.Grafana.AdminPassword)
	}
	if !existed {
		fmt.Fprintln(a.stdout, "\nNext: `infra-monitor compose up` (this host) or `infra-monitor k8s apply` (Kubernetes)")
	}
	return nil
}

func (a *app) printSummary(cfg config.Config) {
	w := a.stdout
	fmt.Fprintf(w, "  URLs checked:   %s\n", orNone(cfg.Probes))
	var targets []string
	for _, t := range cfg.Targets {
		targets = append(targets, t.Name+" ("+t.Address+")")
	}
	fmt.Fprintf(w, "  Scrape targets: %s\n", orNone(targets))
	ch := cfg.Channels()
	if len(ch) == 0 {
		fmt.Fprintln(w, "  Alerts sent to: nowhere yet (add a channel with `infra-monitor notify setup`)")
	} else {
		fmt.Fprintf(w, "  Alerts sent to: %s\n", strings.Join(ch, ", "))
	}
	r := cfg.Retention
	fmt.Fprintf(w, "  Retention:      metrics %s, logs %s, traces %s\n", r.Metrics, r.Logs, r.Traces)
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// ---- probes -----------------------------------------------------------

func (a *app) cmdProbes(args []string) error {
	sub, rest := "list", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	switch sub {
	case "list", "ls":
		if len(cfg.Probes) == 0 {
			fmt.Fprintln(a.stdout, "No URLs are checked yet. Add one with `infra-monitor probes add https://example.com`.")
		}
		for _, p := range cfg.Probes {
			fmt.Fprintln(a.stdout, p)
		}
		return nil
	case "add":
		urls := rest
		if len(urls) == 0 {
			if !a.interactive {
				return errors.New("usage: infra-monitor probes add URL [URL...]")
			}
			if urls, err = a.askProbes(cfg.Probes); err != nil {
				return err
			}
		}
		added := 0
		for _, u := range urls {
			if err := config.ValidateProbe(u); err != nil {
				return err
			}
			if slices.Contains(cfg.Probes, u) {
				fmt.Fprintf(a.stdout, "Already checked: %s\n", u)
				continue
			}
			cfg.Probes = append(cfg.Probes, u)
			fmt.Fprintf(a.stdout, "Added %s\n", u)
			added++
		}
		if added == 0 {
			return nil
		}
		if err := a.save(cfg); err != nil {
			return err
		}
		return a.changed()
	case "remove", "rm", "delete":
		urls := rest
		if len(urls) == 0 {
			if !a.interactive || len(cfg.Probes) == 0 {
				return errors.New("usage: infra-monitor probes remove URL [URL...]")
			}
			i, err := a.p.Select("Which URL should no longer be checked?", cfg.Probes, -1)
			if err != nil {
				return err
			}
			urls = []string{cfg.Probes[i]}
		}
		for _, u := range urls {
			i := slices.Index(cfg.Probes, u)
			if i < 0 {
				return fmt.Errorf("%s is not in the list; see `infra-monitor probes list`", u)
			}
			cfg.Probes = slices.Delete(cfg.Probes, i, i+1)
			removeMonitor(&cfg, u)
			fmt.Fprintf(a.stdout, "Removed %s\n", u)
		}
		if err := a.save(cfg); err != nil {
			return err
		}
		return a.changed()
	default:
		return fmt.Errorf("unknown probes command %q (use list, add or remove)", sub)
	}
}

// ---- targets ----------------------------------------------------------

func (a *app) cmdTargets(args []string) error {
	sub, rest := "list", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	switch sub {
	case "list", "ls":
		if len(cfg.Targets) == 0 {
			fmt.Fprintln(a.stdout, "No extra scrape targets. Add one with `infra-monitor targets add NAME HOST:PORT`.")
		}
		for _, t := range cfg.Targets {
			p := t.Path
			if p == "" {
				p = "/metrics"
			}
			fmt.Fprintf(a.stdout, "%-20s %s%s\n", t.Name, t.Address, p)
		}
		return nil
	case "add":
		fs := a.newFlags("targets add")
		metricsPath := fs.String("path", "", "metrics path (default /metrics)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		var t config.Target
		switch {
		case len(pos) == 2:
			t = config.Target{Name: pos[0], Address: pos[1], Path: *metricsPath}
		case len(pos) == 0 && a.interactive:
			if t, err = a.askTarget(cfg); err != nil {
				return err
			}
		default:
			return errors.New("usage: infra-monitor targets add NAME HOST:PORT [--path /metrics]")
		}
		if slices.ContainsFunc(cfg.Targets, func(x config.Target) bool { return x.Name == t.Name }) {
			return fmt.Errorf("a target named %q already exists", t.Name)
		}
		if err := t.Validate(); err != nil {
			return err
		}
		cfg.Targets = append(cfg.Targets, t)
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Added target %s (%s)\n", t.Name, t.Address)
		return a.changed()
	case "remove", "rm", "delete":
		names := rest
		if len(names) == 0 {
			if !a.interactive || len(cfg.Targets) == 0 {
				return errors.New("usage: infra-monitor targets remove NAME [NAME...]")
			}
			var opts []string
			for _, t := range cfg.Targets {
				opts = append(opts, t.Name+" ("+t.Address+")")
			}
			i, err := a.p.Select("Which target should be removed?", opts, -1)
			if err != nil {
				return err
			}
			names = []string{cfg.Targets[i].Name}
		}
		for _, n := range names {
			i := slices.IndexFunc(cfg.Targets, func(t config.Target) bool { return t.Name == n })
			if i < 0 {
				return fmt.Errorf("no target named %q; see `infra-monitor targets list`", n)
			}
			cfg.Targets = slices.Delete(cfg.Targets, i, i+1)
			fmt.Fprintf(a.stdout, "Removed target %s\n", n)
		}
		if err := a.save(cfg); err != nil {
			return err
		}
		return a.changed()
	default:
		return fmt.Errorf("unknown targets command %q (use list, add or remove)", sub)
	}
}

// ---- notify -----------------------------------------------------------

var channelNames = []string{"telegram", "slack", "email", "webhook"}

func (a *app) cmdNotify(args []string) error {
	sub, rest := "list", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		cfg, err := a.load()
		if err != nil {
			return err
		}
		if !cfg.NotificationsEnabled() {
			fmt.Fprintln(a.stdout, "No alert channels yet. Add one with `infra-monitor notify setup`.")
			return nil
		}
		n := cfg.Notifications
		if n.Telegram != nil {
			fmt.Fprintf(a.stdout, "telegram  chat %s\n", n.Telegram.ChatID)
		}
		if n.Slack != nil {
			fmt.Fprintf(a.stdout, "slack     %s\n", orDefault(n.Slack.Channel, "(webhook's default channel)"))
		}
		if n.Email != nil {
			fmt.Fprintf(a.stdout, "email     to %s via %s\n", n.Email.To, n.Email.Smarthost)
		}
		if n.Webhook != nil {
			fmt.Fprintf(a.stdout, "webhook   %s\n", redactURL(n.Webhook.URL))
		}
		return nil
	case "setup":
		if !a.interactive {
			return errors.New("`notify setup` is interactive; use `notify add CHANNEL --flags` in scripts")
		}
		cfg, err := a.load()
		if err != nil {
			return err
		}
		if err := a.askNotifications(&cfg); err != nil {
			return err
		}
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "Notifications saved.")
		return a.changed()
	case "add":
		return a.notifyAdd(rest)
	case "remove", "rm", "delete":
		cfg, err := a.load()
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: infra-monitor notify remove %s", strings.Join(channelNames, "|"))
		}
		n := &cfg.Notifications
		switch rest[0] {
		case "telegram":
			n.Telegram = nil
		case "slack":
			n.Slack = nil
		case "email":
			n.Email = nil
		case "webhook":
			n.Webhook = nil
		default:
			return fmt.Errorf("unknown channel %q (use %s)", rest[0], strings.Join(channelNames, ", "))
		}
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed %s.\n", rest[0])
		return a.changed()
	case "test":
		fs := a.newFlags("notify test")
		amURL := fs.String("alertmanager", "http://localhost:9093", "Alertmanager URL (for Kubernetes, run `k8s port-forward alertmanager` first)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		return a.sendTestAlert(*amURL)
	default:
		return fmt.Errorf("unknown notify command %q (use list, setup, add, remove or test)", sub)
	}
}

func (a *app) notifyAdd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: infra-monitor notify add %s [flags]", strings.Join(channelNames, "|"))
	}
	channel, rest := args[0], args[1:]
	cfg, err := a.load()
	if err != nil {
		return err
	}
	fs := a.newFlags("notify add " + channel)
	n := &cfg.Notifications
	switch channel {
	case "telegram":
		token := fs.String("token", "", "bot token from @BotFather")
		chat := fs.String("chat-id", "", "chat id")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		n.Telegram = &config.Telegram{BotToken: *token, ChatID: *chat}
	case "slack":
		url := fs.String("webhook-url", "", "incoming webhook URL")
		ch := fs.String("channel", "", "channel, e.g. #alerts (optional)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		n.Slack = &config.Slack{WebhookURL: *url, Channel: *ch}
	case "email":
		e := &config.Email{}
		fs.StringVar(&e.To, "to", "", "recipient address")
		fs.StringVar(&e.From, "from", "", "sender address")
		fs.StringVar(&e.Smarthost, "smarthost", "", "SMTP server host:port, e.g. smtp.gmail.com:587")
		fs.StringVar(&e.Username, "username", "", "SMTP username (optional)")
		fs.StringVar(&e.Password, "password", "", "SMTP password (optional)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		n.Email = e
	case "webhook":
		url := fs.String("url", "", "URL that receives alerts as JSON")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		n.Webhook = &config.Webhook{URL: *url}
	default:
		return fmt.Errorf("unknown channel %q (use %s)", channel, strings.Join(channelNames, ", "))
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Saved %s. Send a test with `infra-monitor notify test` once the stack is running.\n", channel)
	return a.changed()
}

// sendTestAlert posts a short-lived alert straight to Alertmanager.
func (a *app) sendTestAlert(amURL string) error {
	now := time.Now().UTC()
	body, _ := json.Marshal([]map[string]any{{
		"labels": map[string]string{
			"alertname": "InfraMonitorTest",
			"severity":  "info",
			"instance":  "infra-monitor-cli",
		},
		"annotations": map[string]string{
			"summary": "Test alert from infra-monitor. If you can read this, notifications work.",
		},
		"startsAt": now.Format(time.RFC3339),
		"endsAt":   now.Add(5 * time.Minute).Format(time.RFC3339),
	}})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(strings.TrimRight(amURL, "/")+"/api/v2/alerts", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not reach Alertmanager at %s: %w (is the stack running?)", amURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Alertmanager returned %s", resp.Status)
	}
	fmt.Fprintln(a.stdout, "Test alert sent. It should arrive on every configured channel within about 30 seconds.")
	return nil
}

// ---- config -----------------------------------------------------------

func (a *app) cmdConfig(args []string) error {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "path":
		fmt.Fprintln(a.stdout, config.Path(a.dir))
		return nil
	case "show":
		cfg, err := a.load()
		if err != nil {
			return err
		}
		out, err := yaml.Marshal(redact(cfg))
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "# %s (secrets hidden)\n%s", config.Path(a.dir), out)
		return nil
	default:
		return fmt.Errorf("unknown config command %q (use show or path)", sub)
	}
}

const hidden = "********"

func redact(c config.Config) config.Config {
	c.Grafana.AdminPassword = hidden
	n := c.Notifications
	if n.Telegram != nil {
		t := *n.Telegram
		t.BotToken = hidden
		n.Telegram = &t
	}
	if n.Slack != nil {
		s := *n.Slack
		s.WebhookURL = redactURL(s.WebhookURL)
		n.Slack = &s
	}
	if n.Email != nil {
		e := *n.Email
		if e.Password != "" {
			e.Password = hidden
		}
		n.Email = &e
	}
	if n.Webhook != nil {
		w := *n.Webhook
		w.URL = redactURL(w.URL)
		n.Webhook = &w
	}
	c.Notifications = n
	ints := slices.Clone(c.Integrations)
	for i := range ints {
		if ints[i].Password != "" {
			ints[i].Password = hidden
		}
	}
	c.Integrations = ints
	if c.AI != nil && c.AI.APIKey != "" {
		ai := *c.AI
		ai.APIKey = hidden
		c.AI = &ai
	}
	if c.Agent != nil {
		ag := *c.Agent
		ag.Token = hidden
		c.Agent = &ag
	}
	return c
}

// redactURL keeps scheme and host but hides the path, which often holds a token.
func redactURL(s string) string {
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return hidden
	}
	host, _, _ := strings.Cut(rest, "/")
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	return scheme + "://" + host + "/" + hidden
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---- compose ----------------------------------------------------------

func (a *app) composeDir() string { return a.dir + "/compose" }
func (a *app) agentDir() string   { return a.dir + "/agent" }
func (a *app) caPath() string     { return a.dir + "/server-ca.crt" }

// composeProject is the Compose project and directory for this host's mode.
func (a *app) composeProject() (project, dir string) {
	if cfg, err := a.load(); err == nil && cfg.IsAgent() {
		return compose.AgentProjectName, a.agentDir()
	}
	return compose.ProjectName, a.composeDir()
}

func (a *app) cmdCompose(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infra-monitor compose up|down|restart|status|logs [SERVICE]|sync")
	}
	sub, args := args[0], args[1:]
	fs := a.newFlags("compose " + sub)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	project, dir := a.composeProject()
	switch sub {
	case "up", "restart":
		return a.composeUp(sub == "restart")
	case "sync":
		if _, err := a.syncProject(); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Compose project written to %s (not started).\n", dir)
		return nil
	case "down":
		return compose.Command(project, dir, "down").Run()
	case "status", "ps":
		return compose.Command(project, dir, "ps").Run()
	case "logs":
		return compose.Command(project, dir, append([]string{"logs", "--follow", "--tail", "100"}, pos...)...).Run()
	default:
		return fmt.Errorf("unknown compose command %q", sub)
	}
}

// syncProject writes the Compose project for this host's mode and reports
// whether existing files changed.
func (a *app) syncProject() (bool, error) {
	cfg, err := a.load()
	if err != nil {
		return false, err
	}
	if cfg.IsAgent() {
		ca, err := os.ReadFile(a.caPath())
		if errors.Is(err, os.ErrNotExist) {
			ca = nil
		} else if err != nil {
			return false, err
		}
		if cfg.Agent.CASHA256 != "" && ca == nil {
			return false, fmt.Errorf("the server's CA certificate is missing (%s); run `infra-monitor agent join` again", a.caPath())
		}
		return compose.SyncAgent(a.agentDir(), cfg, ca)
	}
	cfg, extras, err := a.loadAll()
	if err != nil {
		return false, err
	}
	return compose.Sync(a.composeDir(), cfg, extras)
}

func (a *app) composeUp(restartOnly bool) error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	project, dir := a.composeProject()
	if err := a.preflight(func(d *doctor.Doctor) []doctor.Check {
		running := a.composeRunning(project)
		if cfg.IsAgent() {
			return d.Compose(true) // agents publish no ports
		}
		checks := d.Compose(running)
		if g := cfg.Gateway; g.Domain != "" {
			checks = append(checks, d.Public(publicHosts(cfg), g.TLS == "internal", running)...)
		}
		return checks
	}); err != nil {
		return err
	}
	changed, err := a.syncProject()
	if err != nil {
		return err
	}
	if restartOnly {
		return compose.Command(project, dir, "restart").Run()
	}
	if err := compose.Command(project, dir, "up", "-d", "--remove-orphans").Run(); err != nil {
		return err
	}
	if changed {
		// Bind-mounted config files changed; `up -d` alone doesn't reload them.
		if err := compose.Command(project, dir, "restart").Run(); err != nil {
			return err
		}
	}
	if cfg.IsAgent() {
		fmt.Fprintf(a.stdout, "\nAgent %s is sending metrics and logs to %s.\n", cfg.Agent.Name, cfg.Agent.Server)
		return nil
	}
	a.printComposeEndpoints(cfg)
	return nil
}

// ---- k8s --------------------------------------------------------------

func (a *app) cmdK8s(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infra-monitor k8s configure|apply|render|status|port-forward|delete")
	}
	sub, args := args[0], args[1:]
	if sub == "configure" {
		return a.k8sConfigure(args)
	}

	fs := a.newFlags("k8s " + sub)
	var namespace, kubeContext, storageClass string
	fs.StringVar(&namespace, "namespace", "", "namespace (default from config)")
	fs.StringVar(&namespace, "n", "", "shorthand for --namespace")
	fs.StringVar(&kubeContext, "context", "", "kubectl context (default from config, then current)")
	if sub == "render" || sub == "apply" {
		fs.StringVar(&storageClass, "storage-class", "", "StorageClass for volumes (default from config)")
	}
	purge := false
	if sub == "delete" {
		fs.BoolVar(&purge, "purge", false, "also delete the namespace, including all stored data")
	}
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}

	var cfg config.Config
	var extras stack.Extras
	if sub == "delete" || sub == "status" || sub == "port-forward" {
		// These work without a valid config.
		if cfg, _, err = a.loadOrDefault(); err != nil {
			cfg = config.Default()
		}
	} else if cfg, extras, err = a.loadAll(); err != nil {
		return err
	}
	k := &cfg.Kubernetes
	if namespace != "" {
		k.Namespace = namespace
	}
	if kubeContext != "" {
		k.Context = kubeContext
	}
	if storageClass != "" {
		k.StorageClass = storageClass
	}
	kubectl := func(stdin []byte, args ...string) error { return k8s.Kubectl(k.Context, stdin, args...).Run() }

	switch sub {
	case "render", "apply":
		out, err := k8s.Render(cfg, extras, k8s.Options{})
		if err != nil {
			return err
		}
		if sub == "render" {
			_, err := a.stdout.Write(out)
			return err
		}
		if err := a.preflight(func(d *doctor.Doctor) []doctor.Check { return d.Kubernetes(k.Context) }); err != nil {
			return err
		}
		// Server-side apply: the dashboard ConfigMaps are too large for the
		// last-applied-configuration annotation used by client-side apply.
		if err := kubectl(out, "apply", "--server-side", "--force-conflicts", "--field-manager", "infra-monitor", "-f", "-"); err != nil {
			return err
		}
		a.pruneDashboards(cfg, extras, kubectl)
		fmt.Fprintf(a.stdout, "\nDeployed to namespace %s.\n", k.Namespace)
		if k.Ingress.Host != "" {
			fmt.Fprintf(a.stdout, "Grafana: https://%s (once DNS and the ingress controller are set up)\n", k.Ingress.Host)
		}
		fmt.Fprintf(a.stdout, "Open Grafana locally with: infra-monitor k8s port-forward grafana -n %s\n", k.Namespace)
		return nil
	case "status":
		return kubectl(nil, "get", "pods,services,pvc,ingress", "--namespace", k.Namespace)
	case "port-forward":
		if len(pos) < 1 {
			return fmt.Errorf("usage: infra-monitor k8s port-forward SERVICE [LOCAL_PORT]; services: %s", serviceNames())
		}
		svc := pos[0]
		port, ok := k8s.Ports[svc]
		if !ok {
			return fmt.Errorf("unknown service %q; choose one of: %s", svc, serviceNames())
		}
		local := port
		if len(pos) > 1 {
			if local, err = strconv.Atoi(pos[1]); err != nil {
				return fmt.Errorf("invalid local port %q", pos[1])
			}
		}
		fmt.Fprintf(a.stdout, "Forwarding http://localhost:%d -> %s (Ctrl-C to stop)\n", local, svc)
		return kubectl(nil, "port-forward", "--namespace", k.Namespace, "service/"+svc, fmt.Sprintf("%d:%d", local, port))
	case "delete":
		if cfg.Grafana.AdminPassword == "" {
			cfg.Grafana.AdminPassword = "unused"
		}
		out, err := k8s.Render(cfg, extras, k8s.Options{SkipNamespace: true})
		if err != nil {
			return err
		}
		if err := kubectl(out, "delete", "--ignore-not-found", "-f", "-"); err != nil {
			return err
		}
		// Custom dashboards removed from the config dir aren't in the render.
		_ = kubectl(nil, "delete", "configmap", "--namespace", k.Namespace, "-l", "infra-monitor/dashboard=true", "--ignore-not-found")
		if purge {
			return kubectl(nil, "delete", "namespace", k.Namespace, "--ignore-not-found")
		}
		fmt.Fprintf(a.stdout, "Stack removed; volumes in namespace %s were kept. Use --purge to delete them.\n", k.Namespace)
		return nil
	default:
		return fmt.Errorf("unknown k8s command %q", sub)
	}
}

// pruneDashboards deletes dashboard ConfigMaps that are no longer rendered,
// for example after a custom dashboard file is removed.
func (a *app) pruneDashboards(cfg config.Config, extras stack.Extras, kubectl func([]byte, ...string) error) {
	keep, err := k8s.DashboardConfigMaps(extras)
	if err != nil {
		return
	}
	cmd := k8s.Kubectl(cfg.Kubernetes.Context, nil, "get", "configmap", "--namespace", cfg.Kubernetes.Namespace,
		"-l", "infra-monitor/dashboard=true", "-o", "name")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if cmd.Run() != nil {
		return
	}
	for _, line := range strings.Fields(buf.String()) {
		if name := strings.TrimPrefix(line, "configmap/"); !slices.Contains(keep, name) {
			_ = kubectl(nil, "delete", "configmap", "--namespace", cfg.Kubernetes.Namespace, name)
		}
	}
}

func serviceNames() string {
	names := make([]string, 0, len(k8s.Ports))
	for n := range k8s.Ports {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (a *app) k8sConfigure(args []string) error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	fs := a.newFlags("k8s configure")
	k := &cfg.Kubernetes
	fs.StringVar(&k.Namespace, "namespace", k.Namespace, "namespace to deploy into")
	fs.StringVar(&k.Context, "context", k.Context, "kubectl context (empty: current context)")
	fs.StringVar(&k.StorageClass, "storage-class", k.StorageClass, "StorageClass (empty: cluster default)")
	fs.StringVar(&k.Storage.Prometheus, "prometheus-storage", k.Storage.Prometheus, "Prometheus volume size")
	fs.StringVar(&k.Storage.Loki, "loki-storage", k.Storage.Loki, "Loki volume size")
	fs.StringVar(&k.Storage.Jaeger, "jaeger-storage", k.Storage.Jaeger, "Jaeger volume size")
	fs.StringVar(&k.Storage.Grafana, "grafana-storage", k.Storage.Grafana, "Grafana volume size")
	fs.StringVar(&k.Ingress.Host, "ingress-host", k.Ingress.Host, "expose Grafana at this hostname (empty: no Ingress)")
	fs.StringVar(&k.Ingress.ClassName, "ingress-class", k.Ingress.ClassName, "IngressClass, e.g. nginx or traefik")
	fs.StringVar(&k.Ingress.TLSSecret, "tls-secret", k.Ingress.TLSSecret, "existing TLS Secret for the host")
	fs.StringVar(&k.Ingress.ClusterIssuer, "cluster-issuer", k.Ingress.ClusterIssuer, "cert-manager ClusterIssuer that issues the certificate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NFlag() == 0 {
		if !a.interactive {
			return errors.New("pass flags (see `infra-monitor k8s configure --help`), or run it in a terminal")
		}
		if err := a.askKubernetes(&cfg); err != nil {
			return err
		}
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "Kubernetes settings saved. Deploy with `infra-monitor k8s apply`.")
	return nil
}

// ---- doctor -----------------------------------------------------------

func (a *app) cmdDoctor(args []string) error {
	d := a.doctor
	if d == nil {
		d = doctor.New()
	}
	which := "all"
	if len(args) > 0 {
		which = args[0]
	}
	if which != "all" && which != "compose" && which != "k8s" && which != "kubernetes" {
		return fmt.Errorf("usage: infra-monitor doctor [compose|k8s]")
	}

	if cfg, err := a.load(); err == nil && cfg.IsAgent() {
		return a.doctorAgent(d, cfg)
	}

	cfg, _, err := a.loadOrDefault()
	switch {
	case err != nil:
		fmt.Fprintf(a.stdout, "Configuration\n  ✗ %v\n\n", err)
	case cfg.Grafana.AdminPassword == "":
		fmt.Fprintf(a.stdout, "Configuration\n  ! not set up yet\n      → run `infra-monitor` to start the setup wizard\n\n")
	default:
		if verr := cfg.Validate(); verr != nil {
			fmt.Fprintf(a.stdout, "Configuration\n  ✗ %s: %v\n\n", config.Path(a.dir), verr)
		} else if _, xerr := stack.LoadExtras(a.dir); xerr != nil {
			fmt.Fprintf(a.stdout, "Configuration\n  ✗ %v\n\n", xerr)
		} else {
			fmt.Fprintf(a.stdout, "Configuration\n  ✓ %s\n\n", config.Path(a.dir))
		}
	}

	var composeOK, k8sOK bool
	if which == "all" || which == "compose" {
		project, _ := a.composeProject()
		running := a.composeRunning(project)
		checks := d.Compose(running || cfg.IsAgent())
		if g := cfg.Gateway; g.Domain != "" && !cfg.IsAgent() {
			checks = append(checks, d.Public(publicHosts(cfg), g.TLS == "internal", running)...)
		}
		doctor.Print(a.stdout, "Docker Compose (this host)", checks)
		fmt.Fprintln(a.stdout)
		composeOK = !doctor.Failed(checks)
	}
	if which != "compose" {
		checks := d.Kubernetes(cfg.Kubernetes.Context)
		doctor.Print(a.stdout, "Kubernetes", checks)
		fmt.Fprintln(a.stdout)
		k8sOK = !doctor.Failed(checks)
	}

	switch {
	case which == "all" && (composeOK || k8sOK):
		var ready []string
		if composeOK {
			ready = append(ready, "`infra-monitor compose up`")
		}
		if k8sOK {
			ready = append(ready, "`infra-monitor k8s apply`")
		}
		fmt.Fprintf(a.stdout, "Ready: %s\n", strings.Join(ready, " or "))
		return nil
	case composeOK || k8sOK:
		fmt.Fprintln(a.stdout, "Ready.")
		return nil
	default:
		return errors.New("prerequisites missing; see the fixes above")
	}
}

// preflight runs prerequisite checks and returns the first failure.
func (a *app) preflight(checks func(*doctor.Doctor) []doctor.Check) error {
	if a.doctor == nil {
		return nil
	}
	return doctor.FirstFailure(checks(a.doctor))
}

// composeRunning reports whether a Compose project's containers are up.
func (a *app) composeRunning(project string) bool {
	out, err := exec.Command("docker", "compose", "--project-name", project, "ps", "-q").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// publicHosts are the hostnames the gateway serves publicly.
func publicHosts(cfg config.Config) []string {
	hosts := []string{cfg.Gateway.Domain}
	if sd := cfg.Gateway.StatusDomain; sd != "" {
		hosts = append(hosts, sd)
	}
	return hosts
}

// doctorAgent checks an agent host: Docker, and that the server accepts
// this agent's credentials.
func (a *app) doctorAgent(d *doctor.Doctor, cfg config.Config) error {
	ag := *cfg.Agent
	fmt.Fprintf(a.stdout, "Agent %q of %s\n\n", ag.Name, ag.Server)
	checks := d.Compose(true) // agents publish no ports
	ca, _ := os.ReadFile(a.caPath())
	if ag.CASHA256 != "" && ca == nil {
		checks = append(checks, doctor.Check{Name: "Server CA certificate", Status: doctor.Fail,
			Detail: "missing " + a.caPath(), Fix: "Run `infra-monitor agent join` again"})
	} else if err := pingServer(ag, ca); err != nil {
		checks = append(checks, doctor.Check{Name: "Server accepts this agent", Status: doctor.Fail, Detail: err.Error()})
	} else {
		checks = append(checks, doctor.Check{Name: "Server accepts this agent", Status: doctor.OK})
	}
	doctor.Print(a.stdout, "Agent", checks)
	if doctor.Failed(checks) {
		return errors.New("the agent can't send data; see the fixes above")
	}
	fmt.Fprintln(a.stdout, "\nReady.")
	return nil
}

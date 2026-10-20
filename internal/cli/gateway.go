package cli

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/OcheOps/Infra-Monitor/internal/config"
)

// ---- expose -----------------------------------------------------------

func (a *app) cmdExpose(args []string) error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if len(args) > 0 && (args[0] == "off" || args[0] == "disable") {
		cfg.Gateway = config.Gateway{}
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "Public access turned off; Grafana is only reachable on this host again.")
		return a.changed()
	}

	fs := a.newFlags("expose")
	g := &cfg.Gateway
	fs.StringVar(&g.Domain, "domain", g.Domain, "public hostname for Grafana, e.g. mon.example.com")
	fs.StringVar(&g.StatusDomain, "status-domain", g.StatusDomain, "optional separate hostname for the status page")
	fs.StringVar(&g.TLS, "tls", g.TLS, "auto (Let's Encrypt) or internal (self-signed, for hosts without public DNS)")
	fs.StringVar(&g.Email, "email", g.Email, "email for Let's Encrypt expiry notices (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NFlag() == 0 {
		if !a.interactive {
			a.printExposeStatus(cfg)
			return nil
		}
		if err := a.askExpose(&cfg); err != nil {
			return err
		}
	}
	if g.Domain == "" {
		return errors.New("--domain is required (or `infra-monitor expose off`)")
	}
	if g.TLS == "" {
		g.TLS = "auto"
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Grafana will be served at https://%s/\n", g.Domain)
	if cfg.StatusPage.Enabled {
		fmt.Fprintf(a.stdout, "Status page: %s\n", strings.Join(statusURLs(cfg), " and "))
	}
	if g.TLS == "auto" && g.Domain != "localhost" {
		fmt.Fprintf(a.stdout, "\nBefore applying:\n  1. Point a DNS A/AAAA record for %s", g.Domain)
		if g.StatusDomain != "" {
			fmt.Fprintf(a.stdout, " (and %s)", g.StatusDomain)
		}
		fmt.Fprintln(a.stdout, " at this server's public IP.")
		fmt.Fprintln(a.stdout, "  2. Open ports 80 and 443; Let's Encrypt needs them to issue the certificate.")
		fmt.Fprintln(a.stdout, "On Kubernetes, the Ingress serves the hostname instead; set its certificate with `k8s configure --cluster-issuer`.")
	}
	return a.changed()
}

func (a *app) printExposeStatus(cfg config.Config) {
	g := cfg.Gateway
	if g.Domain == "" {
		fmt.Fprintln(a.stdout, "Not exposed: Grafana is only reachable on this host (http://localhost:3000, and the gateway on http://localhost:8081).")
		fmt.Fprintln(a.stdout, "Expose it with: infra-monitor expose --domain mon.example.com")
		return
	}
	fmt.Fprintf(a.stdout, "Grafana: https://%s/ (TLS: %s)\n", g.Domain, g.TLS)
	if g.StatusDomain != "" {
		fmt.Fprintf(a.stdout, "Status domain: https://%s/\n", g.StatusDomain)
	}
}

func (a *app) askExpose(cfg *config.Config) error {
	p := a.p
	g := &cfg.Gateway
	p.Printf("\nPublic access over HTTPS\n")
	p.Printf("Grafana (with its login) and the status page will be reachable on a domain you own.\n")
	var err error
	if g.Domain, err = p.Input("Domain (e.g. mon.example.com)", g.Domain, validField(func(c *config.Config, v string) {
		c.Gateway.Domain = v
	})); err != nil {
		return err
	}
	def := 0
	if g.TLS == "internal" {
		def = 1
	}
	choice, err := p.Select("Certificate:", []string{
		"Let's Encrypt (the domain's DNS points at this server, ports 80/443 open)",
		"Self-signed (private network, no public DNS; browsers will warn)",
	}, def)
	if err != nil {
		return err
	}
	g.TLS = []string{"auto", "internal"}[choice]
	if g.TLS == "auto" {
		if g.Email, err = p.Input("Email for certificate expiry notices (optional)", g.Email, func(v string) error {
			if v == "" {
				return nil
			}
			return validField(func(c *config.Config, v string) { c.Gateway.Email = v })(v)
		}); err != nil {
			return err
		}
	}
	g.StatusDomain, err = p.Input("Separate domain for the status page (optional, e.g. status.example.com)", g.StatusDomain, func(v string) error {
		if v == "" {
			return nil
		}
		return validField(func(c *config.Config, v string) {
			c.Gateway.Domain, c.Gateway.StatusDomain = "mon.example.com", v
		})(v)
	})
	return err
}

// ---- statuspage -------------------------------------------------------

func (a *app) cmdStatusPage(args []string) error {
	sub, rest := "show", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	sp := &cfg.StatusPage
	switch sub {
	case "show", "list", "ls":
		if !sp.Enabled {
			fmt.Fprintln(a.stdout, "The status page is off. Turn it on with `infra-monitor statuspage enable`.")
			return nil
		}
		fmt.Fprintf(a.stdout, "%s — %s\n", sp.Title, strings.Join(statusURLs(cfg), " and "))
		if len(sp.Monitors) == 0 {
			fmt.Fprintln(a.stdout, "No services listed yet. Add one with `infra-monitor statuspage add URL`.")
		}
		for _, m := range sp.Monitors {
			fmt.Fprintf(a.stdout, "  %-30s %s\n", m.Name, m.Probe)
		}
		return nil

	case "enable", "on":
		fs := a.newFlags("statuspage enable")
		fs.StringVar(&sp.Title, "title", sp.Title, "page title")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		sp.Enabled = true
		if len(sp.Monitors) == 0 {
			if err := a.pickMonitors(&cfg); err != nil {
				return err
			}
		}
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Status page on: %s (%d services)\n", strings.Join(statusURLs(cfg), " and "), len(sp.Monitors))
		if cfg.Gateway.Domain == "" {
			fmt.Fprintln(a.stdout, "It's only reachable on this host until you run `infra-monitor expose --domain ...`.")
		}
		return a.changed()

	case "disable", "off":
		sp.Enabled = false
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "Status page off.")
		return a.changed()

	case "add":
		fs := a.newFlags("statuspage add")
		name := fs.String("name", "", "name shown on the page (default: the host name)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		var probe string
		switch {
		case len(pos) == 1:
			probe = pos[0]
		case len(pos) == 0 && a.interactive:
			var choices []string
			for _, p := range cfg.Probes {
				if !slices.ContainsFunc(sp.Monitors, func(m config.Monitor) bool { return m.Probe == p }) {
					choices = append(choices, p)
				}
			}
			if len(choices) == 0 {
				return errors.New("every probe is already on the page; add more with `infra-monitor probes add`")
			}
			i, err := a.p.Select("Which check should the page show?", choices, -1)
			if err != nil {
				return err
			}
			probe = choices[i]
			if *name, err = a.p.Input("Name shown on the page", defaultMonitorName(probe), nil); err != nil {
				return err
			}
		default:
			return errors.New("usage: infra-monitor statuspage add URL [--name NAME]")
		}
		if !slices.Contains(cfg.Probes, probe) {
			cfg.Probes = append(cfg.Probes, probe)
			fmt.Fprintf(a.stdout, "Also added %s to the checked URLs.\n", probe)
		}
		if *name == "" {
			*name = defaultMonitorName(probe)
		}
		sp.Monitors = slices.DeleteFunc(sp.Monitors, func(m config.Monitor) bool { return m.Probe == probe })
		sp.Monitors = append(sp.Monitors, config.Monitor{Probe: probe, Name: *name})
		sp.Enabled = true
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Showing %q on the status page.\n", *name)
		return a.changed()

	case "remove", "rm":
		if len(rest) != 1 {
			return errors.New("usage: infra-monitor statuspage remove URL")
		}
		before := len(sp.Monitors)
		sp.Monitors = slices.DeleteFunc(sp.Monitors, func(m config.Monitor) bool { return m.Probe == rest[0] })
		if len(sp.Monitors) == before {
			return fmt.Errorf("%s isn't on the status page; see `infra-monitor statuspage show`", rest[0])
		}
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed %s from the status page (it's still checked).\n", rest[0])
		return a.changed()

	default:
		return fmt.Errorf("unknown statuspage command %q (use show, enable, disable, add or remove)", sub)
	}
}

// pickMonitors chooses what a newly enabled page shows: interactively, or
// every HTTP(S) check when scripted (TCP checks are often internal).
func (a *app) pickMonitors(cfg *config.Config) error {
	sp := &cfg.StatusPage
	for _, p := range cfg.Probes {
		if strings.HasPrefix(p, "tcp://") && !a.interactive {
			continue
		}
		show := true
		if a.interactive {
			var err error
			if show, err = a.p.Confirm("Show "+p+" on the public page?", !strings.HasPrefix(p, "tcp://")); err != nil {
				return err
			}
		}
		if !show {
			continue
		}
		name := defaultMonitorName(p)
		if a.interactive {
			var err error
			if name, err = a.p.Input("  Name shown on the page", name, nil); err != nil {
				return err
			}
		}
		sp.Monitors = append(sp.Monitors, config.Monitor{Probe: p, Name: name})
	}
	return nil
}

// defaultMonitorName is a readable name for a probe: its host, plus the
// path when there is one.
func defaultMonitorName(probe string) string {
	u, err := url.Parse(probe)
	if err != nil || u.Host == "" {
		return probe
	}
	name := u.Host
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		name += p
	}
	return name
}

// statusURLs are the addresses the status page is served on.
func statusURLs(cfg config.Config) []string {
	g := cfg.Gateway
	if g.Domain == "" {
		return []string{"http://localhost:8081/status/"}
	}
	urls := []string{"https://" + g.Domain + "/status/"}
	if g.StatusDomain != "" {
		urls = append(urls, "https://"+g.StatusDomain+"/")
	}
	return urls
}

// removeMonitor drops a probe from the status page when the probe goes away.
func removeMonitor(cfg *config.Config, probe string) {
	cfg.StatusPage.Monitors = slices.DeleteFunc(cfg.StatusPage.Monitors, func(m config.Monitor) bool { return m.Probe == probe })
}

func (a *app) printComposeEndpoints(cfg config.Config) {
	w := a.stdout
	fmt.Fprintln(w, "\nInfra Monitor is running:")
	if d := cfg.Gateway.Domain; d != "" {
		fmt.Fprintf(w, "  Grafana:      https://%s/  (and http://localhost:3000)\n", d)
	} else {
		fmt.Fprintln(w, "  Grafana:      http://localhost:3000")
	}
	fmt.Fprintf(w, "                login: admin, password in %s\n", config.Path(a.dir))
	if cfg.StatusPage.Enabled {
		fmt.Fprintf(w, "  Status page:  %s\n", strings.Join(statusURLs(cfg), " and "))
	}
	fmt.Fprint(w, `  Prometheus:   http://localhost:9090
  Alertmanager: http://localhost:9093
  Jaeger:       http://localhost:16686  (OTLP: localhost:4317 gRPC, localhost:4318 HTTP)
  Loki:         http://localhost:3100
  cAdvisor:     http://localhost:8080
`)
}

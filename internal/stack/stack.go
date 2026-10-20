// Package stack renders the configuration files both backends share:
// Prometheus, Alertmanager, Loki and Jaeger configs, alert rules and
// dashboards, including the user's own rules and dashboards.
package stack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/integrations"
)

// Images maps each component to its pinned image. Compose services are read
// from deploy/compose/docker-compose.yml, so that file is the single place to
// bump a version; Kubernetes-only components are listed here.
var Images = func() map[string]string {
	data, err := deploy.FS.ReadFile("compose/docker-compose.yml")
	if err != nil {
		panic(err)
	}
	var f struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		panic(fmt.Sprintf("deploy/compose/docker-compose.yml: %v", err))
	}
	images := map[string]string{
		"kube-state-metrics": "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.13.0",
	}
	for name, s := range f.Services {
		images[name] = s.Image
	}
	return images
}()

// Backend selects backend-specific files.
type Backend string

const (
	Compose    Backend = "compose"
	Kubernetes Backend = "k8s"
)

// Data is what the templates see.
type Data struct {
	HTTPProbes   []string
	TCPProbes    []string
	Targets      []config.Target
	Integrations []integrations.Target
	Retention    struct{ Metrics, Logs, Traces int } // hours
	Namespace    string
}

func NewData(cfg config.Config) (Data, error) {
	var d Data
	for _, p := range cfg.Probes {
		u, err := url.Parse(p)
		if err != nil {
			return d, err
		}
		if u.Scheme == "tcp" {
			d.TCPProbes = append(d.TCPProbes, u.Host)
		} else {
			d.HTTPProbes = append(d.HTTPProbes, p)
		}
	}
	d.Targets = cfg.Targets
	d.Integrations = integrations.Targets(cfg.Integrations)
	var err error
	r := cfg.Retention
	if d.Retention.Metrics, err = config.Hours(r.Metrics); err != nil {
		return d, err
	}
	if d.Retention.Logs, err = config.Hours(r.Logs); err != nil {
		return d, err
	}
	if d.Retention.Traces, err = config.Hours(r.Traces); err != nil {
		return d, err
	}
	d.Namespace = cfg.Kubernetes.Namespace
	return d, nil
}

// Quote renders s as a double-quoted YAML scalar (JSON strings are valid YAML).
func Quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var funcs = template.FuncMap{"q": Quote, "trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) }}

func render(name string, data any, delims [2]string, srcs ...string) ([]byte, error) {
	t := template.New(name).Funcs(funcs).Option("missingkey=error")
	if delims[0] != "" {
		t = t.Delims(delims[0], delims[1])
	}
	for _, src := range srcs {
		b, err := deploy.FS.ReadFile(src)
		if err != nil {
			return nil, err
		}
		if _, err := t.New(src).Parse(string(b)); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	if err := t.ExecuteTemplate(&out, srcs[0], data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// AlertmanagerConfig renders alertmanager.yml with every configured channel.
// Without channels, alerts are accepted but not sent anywhere.
func AlertmanagerConfig(cfg config.Config) ([]byte, error) {
	return render("alertmanager", cfg, [2]string{"[[", "]]"}, "shared/alertmanager.yml.tmpl")
}

// PrometheusConfig renders prometheus.yml for a backend.
func PrometheusConfig(b Backend, d Data) ([]byte, error) {
	src := "compose/config/prometheus.yml.tmpl"
	if b == Kubernetes {
		src = "k8s/prometheus.yml.tmpl"
		// Exporters run as Compose services; clusters use their own
		// (e.g. Helm charts) and add them as targets.
		d.Integrations = nil
	}
	return render("prometheus", d, [2]string{}, src, "shared/prometheus-jobs.tmpl")
}

func LokiConfig(d Data) ([]byte, error) {
	return render("loki", d, [2]string{}, "shared/loki.yaml.tmpl")
}

func JaegerConfig(d Data) ([]byte, error) {
	return render("jaeger", d, [2]string{}, "shared/jaeger.yaml.tmpl")
}

// Extras are the user's own alert rules and dashboards, read from
// <dir>/rules/*.yml and <dir>/dashboards/*.json.
type Extras struct {
	Rules      map[string][]byte
	Dashboards map[string][]byte
}

const (
	RulesDir      = "rules"
	DashboardsDir = "dashboards"
	// MaxDashboardBytes keeps each dashboard under the 1 MiB ConfigMap limit.
	MaxDashboardBytes = 900 << 10
)

var extraNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LoadExtras reads and checks the user's rule and dashboard files.
func LoadExtras(dir string) (Extras, error) {
	e := Extras{Rules: map[string][]byte{}, Dashboards: map[string][]byte{}}
	var errs []error

	read := func(sub string, exts ...string) map[string][]byte {
		out := map[string][]byte{}
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if errors.Is(err, os.ErrNotExist) {
			return out
		}
		if err != nil {
			errs = append(errs, err)
			return out
		}
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || !hasExt(name, exts) {
				continue
			}
			p := filepath.Join(dir, sub, name)
			if !extraNameRe.MatchString(name) {
				errs = append(errs, fmt.Errorf("%s: file name may only contain letters, digits, '.', '-' and '_'", p))
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out[name] = b
		}
		return out
	}

	for name, b := range read(RulesDir, ".yml", ".yaml") {
		var f struct {
			Groups []struct {
				Name  string `yaml:"name"`
				Rules []any  `yaml:"rules"`
			} `yaml:"groups"`
		}
		if err := yaml.Unmarshal(b, &f); err != nil || len(f.Groups) == 0 {
			errs = append(errs, fmt.Errorf("%s: not a Prometheus rules file (needs a top-level `groups:` list)", filepath.Join(dir, RulesDir, name)))
			continue
		}
		e.Rules["custom-"+strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")+".yml"] = b
	}
	for name, b := range read(DashboardsDir, ".json") {
		p := filepath.Join(dir, DashboardsDir, name)
		if len(b) > MaxDashboardBytes {
			errs = append(errs, fmt.Errorf("%s: larger than %d KiB", p, MaxDashboardBytes>>10))
			continue
		}
		if !json.Valid(b) {
			errs = append(errs, fmt.Errorf("%s: not valid JSON", p))
			continue
		}
		e.Dashboards["custom-"+name] = b
	}
	return e, errors.Join(errs...)
}

func hasExt(name string, exts []string) bool {
	for _, x := range exts {
		if strings.HasSuffix(name, x) {
			return true
		}
	}
	return false
}

// Rules returns alert rule files by name for a backend, including extras
// and an AgentDown rule per registered agent.
func Rules(b Backend, e Extras, agents []config.Agent) (map[string][]byte, error) {
	out := map[string][]byte{}
	dirs := []string{"shared/alerts"}
	if b == Kubernetes {
		dirs = append(dirs, "k8s/alerts")
	}
	for _, d := range dirs {
		if err := readDir(d, out); err != nil {
			return nil, err
		}
	}
	for k, v := range e.Rules {
		out[k] = v
	}
	if len(agents) > 0 {
		r, err := AgentRules(agents)
		if err != nil {
			return nil, err
		}
		out["agents.yml"] = r
	}
	return out, nil
}

// Dashboards returns Grafana dashboard JSON files by name, including extras.
func Dashboards(e Extras) (map[string][]byte, error) {
	out := map[string][]byte{}
	if err := readDir("shared/grafana/dashboards", out); err != nil {
		return nil, err
	}
	delete(out, "dashboard.yml") // the provider config, not a dashboard
	for k, v := range e.Dashboards {
		out[k] = v
	}
	return out, nil
}

func readDir(dir string, out map[string][]byte) error {
	entries, err := fs.ReadDir(deploy.FS, dir)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		b, err := deploy.FS.ReadFile(path.Join(dir, ent.Name()))
		if err != nil {
			return err
		}
		out[ent.Name()] = b
	}
	return nil
}

// SortedKeys returns the keys of m in order.
func SortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ProbeInstance is the Prometheus instance label a probe's series carry:
// the URL for HTTP checks, host:port for tcp:// checks.
func ProbeInstance(probe string) string {
	if u, err := url.Parse(probe); err == nil && u.Scheme == "tcp" {
		return u.Host
	}
	return probe
}

// StatusQueries returns the PromQL the public status page may run, keyed by
// endpoint name and URL-encoded. It is empty when the page shows nothing.
func StatusQueries(cfg config.Config) map[string]string {
	sp := cfg.StatusPage
	if !sp.Enabled || len(sp.Monitors) == 0 {
		return map[string]string{}
	}
	var res []string
	for _, m := range sp.Monitors {
		res = append(res, regexp.QuoteMeta(ProbeInstance(m.Probe)))
	}
	sel := `{job=~"blackbox|blackbox-tcp",instance=~` + strconv.Quote(strings.Join(res, "|")) + `}`
	q := map[string]string{
		"now":     "probe_success" + sel,
		"latency": "probe_duration_seconds" + sel,
		"day":     "avg_over_time(probe_success" + sel + "[1d])",
		"uptime":  "avg_over_time(probe_success" + sel + "[30d])",
		"history": "avg_over_time(probe_success" + sel + "[1d])[30d:1d]",
	}
	for k, v := range q {
		q[k] = url.QueryEscape(v)
	}
	return q
}

// StatusPageJSON is the status page's config.json: its title and the
// display name for each monitored instance. Nothing else is exposed.
func StatusPageJSON(cfg config.Config) ([]byte, error) {
	type monitor struct {
		Name     string `json:"name"`
		Instance string `json:"instance"`
	}
	page := struct {
		Title    string    `json:"title"`
		Monitors []monitor `json:"monitors"`
	}{Title: cfg.StatusPage.Title, Monitors: []monitor{}}
	if cfg.StatusPage.Enabled {
		for _, m := range cfg.StatusPage.Monitors {
			page.Monitors = append(page.Monitors, monitor{Name: m.Name, Instance: ProbeInstance(m.Probe)})
		}
	}
	return json.MarshalIndent(page, "", "  ")
}

// GatewayConfig renders the Caddyfile. On Compose, Caddy terminates TLS for
// the public domain itself; on Kubernetes the Ingress does, so Caddy serves
// plain HTTP inside the cluster.
func GatewayConfig(b Backend, cfg config.Config) ([]byte, error) {
	g := cfg.Gateway
	public := b == Compose && g.Domain != ""
	data := struct {
		Site, StatusSite, Email string
		HTTPS, InternalTLS      bool
		StatusPage              bool
		Queries                 map[string]string
		Agents                  []config.Agent
	}{
		Site:       ":80",
		StatusPage: cfg.StatusPage.Enabled,
		Queries:    StatusQueries(cfg),
		Agents:     cfg.Agents,
	}
	if public {
		data.Site = g.Domain
		data.HTTPS = true
		data.InternalTLS = g.TLS == "internal"
		data.Email = g.Email
	}
	if g.StatusDomain != "" {
		data.StatusSite = g.StatusDomain
		if b == Kubernetes {
			data.StatusSite = "http://" + g.StatusDomain
		}
	}
	return render("gateway", data, [2]string{}, "shared/gateway/Caddyfile.tmpl")
}

// AgentRules alerts when a registered agent stops sending metrics. The
// rule keys on the agent's own `up` series, so it also fires when the whole
// host or its network goes away.
func AgentRules(agents []config.Agent) ([]byte, error) {
	type rule struct {
		Alert       string            `yaml:"alert"`
		Expr        string            `yaml:"expr"`
		For         string            `yaml:"for"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	}
	var rules []rule
	for _, a := range agents {
		rules = append(rules, rule{
			Alert:       "AgentDown",
			Expr:        fmt.Sprintf(`absent_over_time(up{job="node",agent=%s}[5m])`, strconv.Quote(a.Name)),
			For:         "1m",
			Labels:      map[string]string{"severity": "critical", "agent": a.Name},
			Annotations: map[string]string{"summary": "Agent " + a.Name + " has not sent metrics for 5 minutes"},
		})
	}
	return yaml.Marshal(map[string]any{"groups": []map[string]any{{"name": "agents", "rules": rules}}})
}

// agentData is what the agent templates see.
type agentData struct {
	Name, Token, Host, Port, MetricsURL string
	CA                                  bool
	Outputs                             []struct{ Match, Job string }
	Integrations                        []integrations.Target
}

func newAgentData(ag config.AgentConfig, hasCA bool) (agentData, error) {
	u, err := url.Parse(strings.TrimRight(ag.Server, "/"))
	if err != nil {
		return agentData{}, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return agentData{
		Name: ag.Name, Token: ag.Token, Host: u.Hostname(), Port: port,
		MetricsURL: u.String() + "/ingest/metrics",
		CA:         hasCA,
		Outputs: []struct{ Match, Job string }{
			{"host.*", "infra-monitor"}, {"containers.*", "container-logs"},
		},
	}, nil
}

// AgentFiles renders an agent host's config directory: Prometheus (agent
// mode), Fluent Bit, the Docker log parser and the token file.
func AgentFiles(ag config.AgentConfig, ints []config.Integration, hasCA bool) (map[string][]byte, error) {
	d, err := newAgentData(ag, hasCA)
	if err != nil {
		return nil, err
	}
	d.Integrations = integrations.Targets(ints)
	files := map[string][]byte{"token": []byte(ag.Token)}
	if files["prometheus.yml"], err = render("agent-prometheus", d, [2]string{}, "agent/prometheus.yml.tmpl"); err != nil {
		return nil, err
	}
	if files["fluent-bit.conf"], err = render("agent-fluentbit", d, [2]string{}, "agent/fluent-bit.conf.tmpl"); err != nil {
		return nil, err
	}
	if files["parsers.conf"], err = deploy.FS.ReadFile("compose/config/parsers.conf"); err != nil {
		return nil, err
	}
	return files, nil
}

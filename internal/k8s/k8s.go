// Package k8s renders the stack as Kubernetes manifests and applies them
// with kubectl.
package k8s

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"text/template"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

// Manifests in apply order; the namespace must come first.
var manifests = []string{
	"namespace.yaml",
	"prometheus.yaml",
	"alertmanager.yaml",
	"grafana.yaml",
	"gateway.yaml",
	"node-exporter.yaml",
	"kube-state-metrics.yaml",
	"blackbox.yaml",
	"loki.yaml",
	"fluent-bit.yaml",
	"jaeger.yaml",
}

// Ports maps services that `port-forward` can reach to their service port.
var Ports = map[string]int{
	"grafana":      3000,
	"prometheus":   9090,
	"alertmanager": 9093,
	"jaeger":       16686,
	"loki":         3100,
	"gateway":      80,
}

type Options struct {
	// SkipNamespace leaves the Namespace object out, so deleting the
	// rendered objects keeps the namespace and its PersistentVolumeClaims.
	SkipNamespace bool
}

type dashboard struct {
	ConfigMap string // ConfigMap name
	Key       string // file name inside the ConfigMap
	Body      string
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// dashboardConfigMap derives a valid ConfigMap name from a dashboard file name.
func dashboardConfigMap(file string) string {
	n := nonDNS.ReplaceAllString(strings.ToLower(strings.TrimSuffix(file, ".json")), "-")
	n = strings.Trim(n, "-")
	sum := sha256.Sum256([]byte(file))
	// The hash keeps names unique when two files differ only in characters
	// that were replaced.
	name := "grafana-dashboard-" + n
	if len(name) > 230 {
		name = name[:230]
	}
	return name + "-" + hex.EncodeToString(sum[:])[:6]
}

// DashboardConfigMaps returns the names of the dashboard ConfigMaps Render
// creates, so stale ones can be pruned.
func DashboardConfigMaps(extras stack.Extras) ([]string, error) {
	files, err := stack.Dashboards(extras)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range stack.SortedKeys(files) {
		names = append(names, dashboardConfigMap(name))
	}
	return names, nil
}

// Render returns all manifests as one multi-document YAML stream.
func Render(cfg config.Config, extras stack.Extras, opts Options) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	d, err := stack.NewData(cfg)
	if err != nil {
		return nil, err
	}

	files := map[string]string{}
	add := func(key string, b []byte, err error) error {
		if err != nil {
			return fmt.Errorf("rendering %s: %w", key, err)
		}
		files[key] = string(b)
		return nil
	}
	read := func(p string) ([]byte, error) { return deploy.FS.ReadFile(p) }

	for _, step := range []func() error{
		func() error { b, err := stack.PrometheusConfig(stack.Kubernetes, d); return add("prometheus", b, err) },
		func() error { b, err := stack.AlertmanagerConfig(cfg); return add("alertmanager", b, err) },
		func() error { b, err := stack.LokiConfig(d); return add("loki", b, err) },
		func() error { b, err := stack.JaegerConfig(d); return add("jaeger", b, err) },
		func() error {
			b, err := read("shared/grafana/datasources/datasources.yml")
			return add("datasources", b, err)
		},
		func() error {
			b, err := read("shared/grafana/dashboards/dashboard.yml")
			return add("dashboardProvider", b, err)
		},
		func() error { b, err := read("k8s/fluent-bit.conf"); return add("fluentbit", b, err) },
		func() error { b, err := stack.GatewayConfig(stack.Kubernetes, cfg); return add("gateway", b, err) },
		func() error { b, err := stack.StatusPageJSON(cfg); return add("statusJSON", b, err) },
		func() error { b, err := read("shared/gateway/status/index.html"); return add("statusHTML", b, err) },
	} {
		if err := step(); err != nil {
			return nil, err
		}
	}

	ruleFiles, err := stack.Rules(stack.Kubernetes, extras)
	if err != nil {
		return nil, err
	}
	rules := map[string]string{}
	for k, v := range ruleFiles {
		rules[k] = string(v)
	}

	dashFiles, err := stack.Dashboards(extras)
	if err != nil {
		return nil, err
	}
	var dashboards []dashboard
	var dashSum []string
	for _, name := range stack.SortedKeys(dashFiles) {
		dashboards = append(dashboards, dashboard{ConfigMap: dashboardConfigMap(name), Key: name, Body: string(dashFiles[name])})
		dashSum = append(dashSum, name, string(dashFiles[name]))
	}
	var ruleSum []string
	for _, name := range stack.SortedKeys(ruleFiles) {
		ruleSum = append(ruleSum, name, rules[name])
	}

	k := cfg.Kubernetes
	tlsSecret := k.Ingress.TLSSecret
	if tlsSecret == "" && k.Ingress.ClusterIssuer != "" {
		tlsSecret = "grafana-tls" // created by cert-manager
	}
	scheme := "http"
	if tlsSecret != "" {
		scheme = "https"
	}

	data := map[string]any{
		"Namespace":        k.Namespace,
		"StorageClass":     k.StorageClass,
		"Storage":          k.Storage,
		"Ingress":          k.Ingress,
		"PublicHost":       cfg.PublicHost(),
		"StatusDomain":     cfg.Gateway.StatusDomain,
		"IngressHosts":     ingressHosts(cfg),
		"IngressTLSSecret": tlsSecret,
		"IngressScheme":    scheme,
		"Retention":        d.Retention,
		"Images":           stack.Images,
		"Files":            files,
		"Rules":            rules,
		"Dashboards":       dashboards,
		"GrafanaPassword":  cfg.Grafana.AdminPassword,
		// Pod-template annotations, so config changes roll the pods.
		"Checksums": map[string]string{
			"prometheus":   checksum(append([]string{files["prometheus"]}, ruleSum...)...),
			"alertmanager": checksum(files["alertmanager"]),
			"grafana":      checksum(append([]string{files["datasources"], files["dashboardProvider"], cfg.Grafana.AdminPassword, scheme, cfg.PublicHost()}, dashSum...)...),
			"gateway":      checksum(files["gateway"], files["statusJSON"], files["statusHTML"]),
			"fluentbit":    checksum(files["fluentbit"]),
			"loki":         checksum(files["loki"]),
			"jaeger":       checksum(files["jaeger"]),
		},
	}

	funcs := template.FuncMap{"indent": indent, "q": stack.Quote}
	var out bytes.Buffer
	for _, name := range manifests {
		if opts.SkipNamespace && name == "namespace.yaml" {
			continue
		}
		src, err := read(path.Join("k8s", name))
		if err != nil {
			return nil, err
		}
		t, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return nil, err
		}
		if out.Len() > 0 {
			out.WriteString("---\n")
		}
		if err := t.Execute(&out, data); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// ingressHosts lists the hostnames the gateway Ingress serves.
func ingressHosts(cfg config.Config) []string {
	var hosts []string
	if h := cfg.PublicHost(); h != "" {
		hosts = append(hosts, h)
		if sd := cfg.Gateway.StatusDomain; sd != "" {
			hosts = append(hosts, sd)
		}
	}
	return hosts
}

// indent prefixes every line of s with n spaces, for YAML block scalars.
func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

func checksum(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Kubectl builds a kubectl invocation, optionally against a specific context.
func Kubectl(kubeContext string, stdin []byte, args ...string) *exec.Cmd {
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	} else {
		cmd.Stdin = os.Stdin
	}
	return cmd
}

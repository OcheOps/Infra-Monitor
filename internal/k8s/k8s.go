// Package k8s renders the stack as Kubernetes manifests and applies them
// with kubectl.
package k8s

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
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
	"node-exporter.yaml",
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
}

type Options struct {
	Namespace    string
	StorageClass string
	// SkipNamespace leaves the Namespace object out, so deleting the
	// rendered objects keeps the namespace and its PersistentVolumeClaims.
	SkipNamespace bool
}

var (
	dnsLabelRe     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	storageClassRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

func (o Options) Validate() error {
	if !dnsLabelRe.MatchString(o.Namespace) {
		return fmt.Errorf("invalid namespace %q: must be a DNS-1123 label", o.Namespace)
	}
	if o.StorageClass != "" && !storageClassRe.MatchString(o.StorageClass) {
		return fmt.Errorf("invalid storage class %q", o.StorageClass)
	}
	return nil
}

// Render returns all manifests as one multi-document YAML stream.
func Render(cfg config.Config, opts Options) ([]byte, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	funcs := template.FuncMap{
		"indent": indent,
		// JSON strings are valid YAML double-quoted scalars.
		"quote": func(s string) (string, error) {
			b, err := json.Marshal(s)
			return string(b), err
		},
	}

	read := func(p string) (string, error) {
		b, err := deploy.FS.ReadFile(p)
		return string(b), err
	}
	files := map[string]string{}
	for key, p := range map[string]string{
		"rules":       "shared/alerts/rules.yml",
		"datasources": "shared/grafana/datasources/datasources.yml",
		"fluentbit":   "k8s/fluent-bit.conf",
	} {
		s, err := read(p)
		if err != nil {
			return nil, err
		}
		files[key] = s
	}
	am, err := stack.AlertmanagerConfig(cfg)
	if err != nil {
		return nil, err
	}
	files["alertmanager"] = string(am)

	promTmpl, err := read("k8s/prometheus.yml")
	if err != nil {
		return nil, err
	}
	var prom bytes.Buffer
	if err := template.Must(template.New("prometheus.yml").Parse(promTmpl)).Execute(&prom, opts); err != nil {
		return nil, err
	}
	files["prometheus"] = prom.String()

	dashboards := map[string]string{}
	entries, err := fs.ReadDir(deploy.FS, "shared/grafana/dashboards")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		s, err := read(path.Join("shared/grafana/dashboards", e.Name()))
		if err != nil {
			return nil, err
		}
		dashboards[e.Name()] = s
	}

	var dashAll []string
	for _, k := range sortedKeys(dashboards) {
		dashAll = append(dashAll, k, dashboards[k])
	}
	data := map[string]any{
		"Namespace":       opts.Namespace,
		"StorageClass":    opts.StorageClass,
		"Images":          stack.Images,
		"Files":           files,
		"Dashboards":      dashboards,
		"GrafanaPassword": cfg.GrafanaPassword,
		// Pod-template annotations, so config changes roll the pods.
		"Checksums": map[string]string{
			"prometheus":   checksum(files["prometheus"], files["rules"]),
			"alertmanager": checksum(files["alertmanager"]),
			"grafana":      checksum(append([]string{files["datasources"], cfg.GrafanaPassword}, dashAll...)...),
			"fluentbit":    checksum(files["fluentbit"]),
		},
	}

	var out bytes.Buffer
	for _, name := range manifests {
		if opts.SkipNamespace && name == "namespace.yaml" {
			continue
		}
		src, err := read(path.Join("k8s", name))
		if err != nil {
			return nil, err
		}
		t, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(src)
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// Package doctor checks that a machine can run the stack and explains how to
// fix whatever is missing.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Status int

const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) symbol() string {
	return [...]string{"✓", "!", "✗"}[s]
}

// Check is one result. Fix says what to do when Status isn't OK.
type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Doctor runs checks. The fields are swappable so tests can fake the host.
type Doctor struct {
	Run      Runner
	LookPath func(string) (string, error)
	// PortFree reports whether a local TCP port can be bound.
	PortFree func(port int) bool
	GOOS     string
	Timeout  time.Duration
}

func New() *Doctor {
	return &Doctor{
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		},
		LookPath: exec.LookPath,
		PortFree: func(port int) bool {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return false
			}
			l.Close()
			return true
		},
		GOOS:    runtime.GOOS,
		Timeout: 15 * time.Second,
	}
}

func (d *Doctor) run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.Timeout)
	defer cancel()
	out, err := d.Run(ctx, name, args...)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("timed out after %s", d.Timeout)
	}
	return out, err
}

// ComposePorts are the host ports the Compose stack publishes.
var ComposePorts = []struct {
	Port    int
	Service string
}{
	{3000, "Grafana"}, {9090, "Prometheus"}, {9093, "Alertmanager"}, {3100, "Loki"},
	{16686, "Jaeger UI"}, {4317, "OTLP gRPC"}, {4318, "OTLP HTTP"}, {14268, "Jaeger Thrift"},
	{8080, "cAdvisor"}, {9100, "Node Exporter"}, {9115, "Blackbox exporter"},
}

// Compose checks what `compose up` needs. stackRunning says whether the
// Infra Monitor containers are already up, in which case busy ports are ours.
func (d *Doctor) Compose(stackRunning bool) []Check {
	var checks []Check
	if _, err := d.LookPath("docker"); err != nil {
		return append(checks, Check{"Docker installed", Fail, "docker is not on your PATH",
			"Install Docker Engine: https://docs.docker.com/engine/install/"})
	}
	checks = append(checks, Check{Name: "Docker installed", Status: OK})

	if out, err := d.run("docker", "compose", "version", "--short"); err != nil {
		checks = append(checks, Check{"Docker Compose plugin", Fail, firstLine(out, err),
			"Install the Compose plugin: https://docs.docker.com/compose/install/linux/"})
	} else {
		checks = append(checks, Check{"Docker Compose plugin", OK, "version " + firstLine(out, nil), ""})
	}

	if out, err := d.run("docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		fix := "Start Docker (e.g. `sudo systemctl start docker`)"
		if strings.Contains(strings.ToLower(out), "permission denied") {
			fix = "Add yourself to the docker group (`sudo usermod -aG docker $USER`, then log in again), or run with sudo"
		}
		checks = append(checks, Check{"Docker daemon reachable", Fail, firstLine(out, err), fix})
	} else {
		checks = append(checks, Check{"Docker daemon reachable", OK, "server " + out, ""})
	}

	if d.GOOS != "linux" {
		checks = append(checks, Check{"Linux host", Warn, "running on " + d.GOOS,
			"cAdvisor and Node Exporter need a Linux host's /proc, /sys and /var/lib/docker; on Docker Desktop they may fail or report the VM, not your machine"})
	}

	if stackRunning {
		checks = append(checks, Check{"Ports", OK, "Infra Monitor is already running and using them", ""})
		return checks
	}
	var busy []string
	for _, p := range ComposePorts {
		if !d.PortFree(p.Port) {
			busy = append(busy, fmt.Sprintf("%d (%s)", p.Port, p.Service))
		}
	}
	if len(busy) > 0 {
		checks = append(checks, Check{"Ports free on 127.0.0.1", Fail, "in use: " + strings.Join(busy, ", "),
			"Stop whatever is using them (`sudo ss -ltnp` shows which process)"})
	} else {
		checks = append(checks, Check{Name: "Ports free on 127.0.0.1", Status: OK})
	}
	return checks
}

// Kubernetes checks what `k8s apply` needs against kubeContext ("" = current).
func (d *Doctor) Kubernetes(kubeContext string) []Check {
	var checks []Check
	if _, err := d.LookPath("kubectl"); err != nil {
		return append(checks, Check{"kubectl installed", Fail, "kubectl is not on your PATH",
			"Install kubectl: https://kubernetes.io/docs/tasks/tools/"})
	}
	checks = append(checks, Check{Name: "kubectl installed", Status: OK})

	kc := func(args ...string) (string, error) {
		if kubeContext != "" {
			args = append([]string{"--context", kubeContext}, args...)
		}
		return d.run("kubectl", args...)
	}

	ctxName := kubeContext
	if ctxName == "" {
		out, err := kc("config", "current-context")
		if err != nil || out == "" {
			return append(checks, Check{"kubectl context", Fail, "no current context",
				"Point kubectl at a cluster (`kubectl config use-context NAME`), or pass --context"})
		}
		ctxName = out
	}
	checks = append(checks, Check{"kubectl context", OK, ctxName, ""})

	if out, err := kc("version", "--output=json", "--request-timeout=10s"); err != nil || !strings.Contains(out, "serverVersion") {
		return append(checks, Check{"Cluster reachable", Fail, firstLine(out, err),
			"Check that the cluster is running and your credentials are valid (`kubectl get nodes`)"})
	}
	checks = append(checks, Check{Name: "Cluster reachable", Status: OK})

	for _, perm := range []struct{ verb, resource string }{
		{"create", "namespaces"}, {"create", "clusterroles"}, {"create", "clusterrolebindings"}, {"create", "daemonsets"},
	} {
		out, _ := kc("auth", "can-i", perm.verb, perm.resource, "--all-namespaces")
		if strings.TrimSpace(out) != "yes" {
			checks = append(checks, Check{"Permissions", Fail, fmt.Sprintf("cannot %s %s", perm.verb, perm.resource),
				"The stack creates cluster-wide RBAC and DaemonSets; use a cluster-admin context for the first apply"})
			break
		}
	}
	if len(checks) == 0 || checks[len(checks)-1].Name != "Permissions" {
		checks = append(checks, Check{Name: "Permissions", Status: OK})
	}

	out, err := kc("get", "storageclass", "-o",
		`jsonpath={range .items[*]}{.metadata.name}={.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}{end}`)
	switch {
	case err != nil:
		checks = append(checks, Check{"Default StorageClass", Warn, firstLine(out, err), "Pass --storage-class if volumes stay Pending"})
	default:
		def := ""
		for _, line := range strings.Split(out, "\n") {
			if name, isDefault, ok := strings.Cut(line, "="); ok && isDefault == "true" {
				def = name
			}
		}
		if def == "" {
			checks = append(checks, Check{"Default StorageClass", Warn, "none marked as default",
				"Set one with `infra-monitor k8s configure --storage-class NAME`, or volumes stay Pending"})
		} else {
			checks = append(checks, Check{"Default StorageClass", OK, def, ""})
		}
	}
	return checks
}

// Failed reports whether any check failed.
func Failed(checks []Check) bool {
	for _, c := range checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// FirstFailure returns the first failing check as an error with its fix.
func FirstFailure(checks []Check) error {
	for _, c := range checks {
		if c.Status == Fail {
			msg := c.Name + ": " + c.Detail
			if c.Fix != "" {
				msg += "\n  fix: " + c.Fix
			}
			return errors.New(msg)
		}
	}
	return nil
}

// Print writes checks under a heading.
func Print(w io.Writer, heading string, checks []Check) {
	fmt.Fprintf(w, "%s\n", heading)
	for _, c := range checks {
		line := fmt.Sprintf("  %s %s", c.Status.symbol(), c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(w, line)
		if c.Status != OK && c.Fix != "" {
			fmt.Fprintf(w, "      → %s\n", c.Fix)
		}
	}
}

func firstLine(out string, err error) string {
	s := strings.TrimSpace(out)
	if s == "" && err != nil {
		s = err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

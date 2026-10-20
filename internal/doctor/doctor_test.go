package doctor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeHost answers commands from a table keyed by "name arg1 arg2 ...".
type fakeHost struct {
	installed map[string]bool
	answers   map[string]string // prefix -> output; missing prefix -> error
	busyPorts map[int]bool
}

func (f fakeHost) doctor() *Doctor {
	return &Doctor{
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			cmd := strings.Join(append([]string{name}, args...), " ")
			for prefix, out := range f.answers {
				if strings.HasPrefix(cmd, prefix) {
					return out, nil
				}
			}
			return "boom: " + cmd, errors.New("exit status 1")
		},
		LookPath: func(n string) (string, error) {
			if f.installed[n] {
				return "/usr/bin/" + n, nil
			}
			return "", errors.New("not found")
		},
		PortFree: func(p int) bool { return !f.busyPorts[p] },
		GOOS:     "linux",
		Timeout:  time.Second,
	}
}

func byName(checks []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}

func TestComposeHealthy(t *testing.T) {
	d := fakeHost{
		installed: map[string]bool{"docker": true},
		answers: map[string]string{
			"docker compose version": "2.29.1",
			"docker info":            "27.1.1",
		},
	}.doctor()
	checks := d.Compose(false)
	if Failed(checks) {
		var b bytes.Buffer
		Print(&b, "compose", checks)
		t.Fatalf("unexpected failure:\n%s", b.String())
	}
}

func TestComposeProblems(t *testing.T) {
	if c := byName(fakeHost{}.doctor().Compose(false)); c["Docker installed"].Status != Fail || len(c) != 1 {
		t.Errorf("missing docker: %+v", c)
	}

	d := fakeHost{
		installed: map[string]bool{"docker": true},
		answers:   map[string]string{"docker compose version": "2.29.1"},
		busyPorts: map[int]bool{3000: true, 9090: true},
	}.doctor()
	c := byName(d.Compose(false))
	if c["Docker daemon reachable"].Status != Fail {
		t.Error("daemon failure not reported")
	}
	ports := c["Ports free on 127.0.0.1"]
	if ports.Status != Fail || !strings.Contains(ports.Detail, "3000 (Grafana)") || !strings.Contains(ports.Detail, "9090 (Prometheus)") {
		t.Errorf("ports = %+v", ports)
	}
	if c := byName(d.Compose(true)); c["Ports"].Status != OK {
		t.Error("busy ports should be fine when the stack is running")
	}

	d.GOOS = "darwin"
	if c := byName(d.Compose(false)); c["Linux host"].Status != Warn {
		t.Error("non-Linux host not flagged")
	}
}

func TestDaemonPermissionHint(t *testing.T) {
	d := fakeHost{installed: map[string]bool{"docker": true}, answers: map[string]string{"docker compose version": "2"}}.doctor()
	base := d.Run
	d.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "info" {
			return "permission denied while trying to connect to the Docker daemon socket", errors.New("exit 1")
		}
		return base(ctx, name, args...)
	}
	if c := byName(d.Compose(false))["Docker daemon reachable"]; !strings.Contains(c.Fix, "docker group") {
		t.Errorf("fix = %q", c.Fix)
	}
}

func TestKubernetesHealthy(t *testing.T) {
	d := fakeHost{
		installed: map[string]bool{"kubectl": true},
		answers: map[string]string{
			"kubectl config current-context": "kind-ci",
			"kubectl version":                `{"clientVersion":{},"serverVersion":{"gitVersion":"v1.30.0"}}`,
			"kubectl auth can-i":             "yes",
			"kubectl get storageclass":       "standard=true\nslow=",
		},
	}.doctor()
	checks := d.Kubernetes("")
	c := byName(checks)
	if Failed(checks) || c["kubectl context"].Detail != "kind-ci" || c["Default StorageClass"].Detail != "standard" {
		var b bytes.Buffer
		Print(&b, "k8s", checks)
		t.Fatalf("unexpected result:\n%s", b.String())
	}
}

func TestKubernetesProblems(t *testing.T) {
	if c := byName(fakeHost{}.doctor().Kubernetes("")); c["kubectl installed"].Status != Fail {
		t.Error("missing kubectl not reported")
	}

	unreachable := fakeHost{
		installed: map[string]bool{"kubectl": true},
		answers:   map[string]string{"kubectl config current-context": "prod"},
	}.doctor()
	if c := byName(unreachable.Kubernetes("")); c["Cluster reachable"].Status != Fail {
		t.Errorf("unreachable cluster not reported: %+v", c)
	}

	limited := fakeHost{
		installed: map[string]bool{"kubectl": true},
		answers: map[string]string{
			"kubectl --context dev version": `{"serverVersion":{}}`,
			"kubectl --context dev auth":    "no",
			"kubectl --context dev get":     "standard=",
		},
	}.doctor()
	checks := limited.Kubernetes("dev")
	c := byName(checks)
	if c["kubectl context"].Detail != "dev" || c["Permissions"].Status != Fail || c["Default StorageClass"].Status != Warn {
		t.Errorf("checks = %+v", c)
	}
	if err := FirstFailure(checks); err == nil || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("FirstFailure = %v", err)
	}
}

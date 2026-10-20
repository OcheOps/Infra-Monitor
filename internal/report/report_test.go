package report

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OcheOps/Infra-Monitor/internal/promapi"
	. "github.com/OcheOps/Infra-Monitor/internal/promapi/promapitest"
)

func fakeStack() *Fake {
	now := float64(time.Now().Unix())
	return &Fake{
		Vectors: map[string][]promapi.Sample{
			"probe_success":                      {S(1, "instance", "https://shop.example.com"), S(0, "instance", "tcp://db:5432")},
			"avg_over_time(probe_success":        {S(0.9995, "instance", "https://shop.example.com"), S(0.75, "instance", "tcp://db:5432")},
			"probe_duration_seconds":             {S(0.123, "instance", "https://shop.example.com")},
			`max_over_time(up{job="node"}`:       {S(1, "instance", "node-exporter:9100"), S(1, "instance", "web-2")},
			`timestamp(up{job="node"})`:          {S(now, "instance", "node-exporter:9100"), S(now-3600, "instance", "web-2")},
			`max by (instance) (up{job="node"})`: {S(1, "instance", "node-exporter:9100"), S(1, "instance", "web-2")},
			"node_cpu_seconds_total":             {S(0.25, "instance", "node-exporter:9100")},
			"node_memory_MemAvailable":           {S(0.5, "instance", "node-exporter:9100")},
			"node_filesystem_avail_bytes": {S(0.91, "instance", "node-exporter:9100", "mountpoint", "/"),
				S(0.2, "instance", "node-exporter:9100", "mountpoint", "/boot")},
			`up{job=~"integration-.*"}`: {S(1, "instance", "shop-db"), S(1, "instance", "cache")},
			"pg_up|mysql_up":            {S(0, "instance", "shop-db"), S(1, "instance", "cache")},
			"ALERTS":                    {S(42, "alertname", "ProbeFailed", "instance", "tcp://db:5432"), S(3, "alertname", "HostHighCpuLoad", "instance", "node-exporter:9100")},
		},
		Alerts: []promapi.Alert{{Name: "ProbeFailed", State: "firing", ActiveAt: time.Now().Add(-40 * time.Minute),
			Labels:      map[string]string{"alertname": "ProbeFailed", "instance": "tcp://db:5432"},
			Annotations: map[string]string{"summary": "Check of tcp://db:5432 is failing"}}},
	}
}

func TestGatherAndStatus(t *testing.T) {
	f := fakeStack()
	r, err := Gather(context.Background(), promapi.New(f.Start(t)), true)
	if err != nil {
		t.Fatal(err)
	}
	// Failing check, silent agent, service down, firing alert.
	if got := r.Problems(); got != 4 {
		t.Errorf("Problems = %d, want 4", got)
	}
	var b bytes.Buffer
	r.WriteStatus(&b)
	out := b.String()
	for _, want := range []string{
		"4 problem(s)",
		"✓ https://shop.example.com", "123ms", "99.95% up today",
		"✗ tcp://db:5432",
		"✓ node-exporter", "25%   50%   91%", "(/)",
		"✗ web-2", "last seen 1h ago",
		"✗ shop-db", "✓ cache",
		"Firing ProbeFailed for 40m: Check of tcp://db:5432 is failing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}

	d := r.Digest("Infra Monitor")
	for _, want := range []string{
		"4 problem(s) right now",
		"average uptime 87.48%",
		"✗ tcp://db:5432: 75.00% up, failing now",
		"Alerts in the last 24 hours: 2",
		"ProbeFailed on tcp://db:5432: about 42m, still firing",
		"HostHighCpuLoad on node-exporter: about 3m",
		"✗ web-2 is not reporting",
		"! node-exporter: / is 91% full",
		"✗ service shop-db is down",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("digest missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "shop.example.com") {
		t.Error("digest lists a healthy check")
	}
}

func TestGatherWithoutPrometheus(t *testing.T) {
	_, err := Gather(context.Background(), promapi.New(promapi.Endpoints{Prometheus: "http://127.0.0.1:1"}), false)
	if err == nil || !strings.Contains(err.Error(), "can't reach Prometheus") {
		t.Errorf("err = %v", err)
	}
}

func TestHuman(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "<1m", 40 * time.Minute: "40m", time.Hour: "1h",
		200 * time.Minute: "3h20m", 50 * time.Hour: "2d",
	} {
		if got := Human(d); got != want {
			t.Errorf("Human(%s) = %q, want %q", d, got, want)
		}
	}
}

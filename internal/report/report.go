// Package report gathers a summary of the stack's state (checks, hosts,
// services, alerts) for `infra-monitor status` and the daily digest.
package report

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/OcheOps/Infra-Monitor/internal/promapi"
)

// Check is a probe (a URL or TCP check).
type Check struct {
	Target  string
	Up      bool
	Latency time.Duration
	// Uptime is the fraction of the last 24 hours the check passed; NaN if
	// there's no data.
	Uptime float64
}

// Host is a machine running Node Exporter (the server or an agent).
type Host struct {
	Name              string
	Up                bool
	CPU, Memory, Disk float64 // fraction used; NaN if unknown
	FullestMount      string
	LastSeen          time.Time
}

// Service is an integration (database, cache, web server, queue).
type Service struct {
	Name string
	Up   bool
}

// Incident is an alert that fired during the report window.
type Incident struct {
	Alert, Instance string
	Firing          time.Duration // roughly how long it fired in the window
	StillFiring     bool
}

// Report is the stack's state at Time.
type Report struct {
	Time      time.Time
	Checks    []Check
	Hosts     []Host
	Services  []Service
	Alerts    []promapi.Alert
	Incidents []Incident // only filled by Gather with history
	Errors    []string   // queries that failed
}

const fsFilter = `fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs"`

// Gather queries Prometheus. With history it also lists the alerts that
// fired over the last 24 hours (for the digest).
func Gather(ctx context.Context, c *promapi.Client, history bool) (*Report, error) {
	r := &Report{Time: time.Now()}
	// The first query tells us whether Prometheus is there at all.
	up, err := c.Query(ctx, `probe_success`)
	if err != nil {
		return nil, fmt.Errorf("can't reach Prometheus at %s: %w", c.Prometheus, err)
	}
	query := func(q string) []promapi.Sample {
		s, err := c.Query(ctx, q)
		if err != nil {
			r.Errors = append(r.Errors, err.Error())
		}
		return s
	}
	byLabel := func(samples []promapi.Sample, label string) map[string]float64 {
		m := map[string]float64{}
		for _, s := range samples {
			m[s.Labels[label]] = s.Value
		}
		return m
	}

	latency := byLabel(query(`probe_duration_seconds`), "instance")
	uptime := byLabel(query(`avg_over_time(probe_success[1d])`), "instance")
	for _, s := range up {
		t := s.Labels["instance"]
		ch := Check{Target: t, Up: s.Value == 1, Uptime: math.NaN()}
		if v, ok := latency[t]; ok {
			ch.Latency = time.Duration(v * float64(time.Second))
		}
		if v, ok := uptime[t]; ok {
			ch.Uptime = v
		}
		r.Checks = append(r.Checks, ch)
	}
	sort.Slice(r.Checks, func(i, j int) bool { return r.Checks[i].Target < r.Checks[j].Target })

	nodeUp := query(`max by (instance) (max_over_time(up{job="node"}[10m]))`)
	lastSeen := byLabel(query(`max by (instance) (timestamp(up{job="node"}))`), "instance")
	current := byLabel(query(`max by (instance) (up{job="node"})`), "instance")
	cpu := byLabel(query(`1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))`), "instance")
	mem := byLabel(query(`1 - max by (instance) (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`), "instance")
	disks := query(`1 - node_filesystem_avail_bytes{` + fsFilter + `} / node_filesystem_size_bytes{` + fsFilter + `}`)
	for _, s := range nodeUp {
		name := s.Labels["instance"]
		h := Host{Name: name, Up: current[name] == 1 && time.Since(unix(lastSeen[name])) < 2*time.Minute,
			CPU: orNaN(cpu, name), Memory: orNaN(mem, name), Disk: math.NaN(), LastSeen: unix(lastSeen[name])}
		for _, d := range disks {
			if d.Labels["instance"] == name && (math.IsNaN(h.Disk) || d.Value > h.Disk) {
				h.Disk, h.FullestMount = d.Value, d.Labels["mountpoint"]
			}
		}
		r.Hosts = append(r.Hosts, h)
	}
	sort.Slice(r.Hosts, func(i, j int) bool { return r.Hosts[i].Name < r.Hosts[j].Name })

	// An integration is up when its exporter answers and the exporter can
	// reach the service.
	svcDown := byLabel(query(`{__name__=~"pg_up|mysql_up|redis_up|mongodb_up|nginx_up",job=~"integration-.*"}`), "instance")
	for _, s := range query(`up{job=~"integration-.*"}`) {
		name := s.Labels["instance"]
		ok := s.Value == 1
		if v, has := svcDown[name]; has && v != 1 {
			ok = false
		}
		r.Services = append(r.Services, Service{Name: name, Up: ok})
	}
	sort.Slice(r.Services, func(i, j int) bool { return r.Services[i].Name < r.Services[j].Name })

	if r.Alerts, err = c.Alerts(ctx); err != nil {
		r.Errors = append(r.Errors, err.Error())
	}

	if history {
		// One sample per minute the alert was firing.
		firing := map[string]bool{}
		for _, a := range r.Alerts {
			if a.State == "firing" {
				firing[a.Name+"\x00"+a.Labels["instance"]] = true
			}
		}
		for _, s := range query(`sum by (alertname, instance) (count_over_time(ALERTS{alertstate="firing"}[1d:1m]))`) {
			in := Incident{Alert: s.Labels["alertname"], Instance: s.Labels["instance"],
				Firing: time.Duration(s.Value) * time.Minute}
			in.StillFiring = firing[in.Alert+"\x00"+in.Instance]
			r.Incidents = append(r.Incidents, in)
		}
		sort.Slice(r.Incidents, func(i, j int) bool { return r.Incidents[i].Firing > r.Incidents[j].Firing })
	}
	return r, nil
}

func unix(secs float64) time.Time {
	if secs == 0 {
		return time.Time{}
	}
	return time.Unix(int64(secs), 0)
}

func orNaN(m map[string]float64, k string) float64 {
	if v, ok := m[k]; ok {
		return v
	}
	return math.NaN()
}

// Problems counts what needs attention: failing checks, hosts and services
// down, and firing alerts.
func (r *Report) Problems() int {
	n := 0
	for _, c := range r.Checks {
		if !c.Up {
			n++
		}
	}
	for _, h := range r.Hosts {
		if !h.Up {
			n++
		}
	}
	for _, s := range r.Services {
		if !s.Up {
			n++
		}
	}
	for _, a := range r.Alerts {
		if a.State == "firing" {
			n++
		}
	}
	return n
}

func pct(f float64) string {
	if math.IsNaN(f) {
		return "  –"
	}
	return fmt.Sprintf("%3.0f%%", f*100)
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// shortHost drops the default exporter port from instance names.
func shortHost(s string) string { return strings.TrimSuffix(s, ":9100") }

// WriteStatus prints the report for a terminal.
func (r *Report) WriteStatus(w io.Writer) {
	if n := r.Problems(); n == 0 {
		fmt.Fprintln(w, "All good.")
	} else {
		fmt.Fprintf(w, "%d problem(s) need attention.\n", n)
	}

	fmt.Fprintf(w, "\nChecks (%d)\n", len(r.Checks))
	if len(r.Checks) == 0 {
		fmt.Fprintln(w, "  none; add one with `infra-monitor probes add URL`")
	}
	for _, c := range r.Checks {
		lat := "–"
		if c.Up && c.Latency > 0 {
			lat = c.Latency.Round(time.Millisecond).String()
		}
		fmt.Fprintf(w, "  %s %-44s %8s   %s up today\n", mark(c.Up), c.Target, lat, uptimeStr(c.Uptime))
	}

	fmt.Fprintf(w, "\nHosts (%d)                                 CPU   Mem  Disk\n", len(r.Hosts))
	for _, h := range r.Hosts {
		line := fmt.Sprintf("  %s %-38s %s  %s  %s", mark(h.Up), shortHost(h.Name), pct(h.CPU), pct(h.Memory), pct(h.Disk))
		if !h.Up && !h.LastSeen.IsZero() {
			line += "   last seen " + Human(time.Since(h.LastSeen)) + " ago"
		} else if h.Disk > 0.85 && h.FullestMount != "" {
			line += "   (" + h.FullestMount + ")"
		}
		fmt.Fprintln(w, line)
	}

	if len(r.Services) > 0 {
		fmt.Fprintf(w, "\nServices (%d)\n", len(r.Services))
		for _, s := range r.Services {
			fmt.Fprintf(w, "  %s %s\n", mark(s.Up), s.Name)
		}
	}

	fmt.Fprintf(w, "\nAlerts\n")
	if len(r.Alerts) == 0 {
		fmt.Fprintln(w, "  none firing")
	}
	for _, a := range r.Alerts {
		since := ""
		if !a.ActiveAt.IsZero() {
			since = " for " + Human(time.Since(a.ActiveAt))
		}
		fmt.Fprintf(w, "  %s %s%s: %s\n", strings.ToUpper(a.State[:1])+a.State[1:], a.Name, since, summary(a))
	}
	for _, e := range r.Errors {
		fmt.Fprintf(w, "\n! %s\n", e)
	}
}

func uptimeStr(u float64) string {
	if math.IsNaN(u) {
		return "  –  "
	}
	return fmt.Sprintf("%6.2f%%", u*100)
}

func summary(a promapi.Alert) string {
	if s := a.Annotations["summary"]; s != "" {
		return s
	}
	if i := a.Labels["instance"]; i != "" {
		return i
	}
	return a.Labels["job"]
}

// Digest renders the daily summary as plain text (it's sent to Telegram,
// Slack, email and webhooks, so no markup).
func (r *Report) Digest(title string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: daily summary for %s\n\n", title, r.Time.Format("Mon 2 Jan"))
	if n := r.Problems(); n == 0 {
		b.WriteString("Everything is up right now.\n")
	} else {
		fmt.Fprintf(&b, "%d problem(s) right now.\n", n)
	}

	if len(r.Checks) > 0 {
		var worst []string
		total := 0.0
		counted := 0
		for _, c := range r.Checks {
			if !math.IsNaN(c.Uptime) {
				total += c.Uptime
				counted++
			}
			if !c.Up || (!math.IsNaN(c.Uptime) && c.Uptime < 0.999) {
				worst = append(worst, fmt.Sprintf("  %s %s: %s up%s", mark(c.Up), c.Target, strings.TrimSpace(uptimeStr(c.Uptime)), map[bool]string{true: "", false: ", failing now"}[c.Up]))
			}
		}
		if counted > 0 {
			fmt.Fprintf(&b, "\nChecks: %d, average uptime %.2f%% over 24 hours\n", len(r.Checks), total/float64(counted)*100)
		}
		for _, l := range worst {
			b.WriteString(l + "\n")
		}
	}

	fmt.Fprintf(&b, "\nAlerts in the last 24 hours: %d\n", len(r.Incidents))
	for i, in := range r.Incidents {
		if i == 10 {
			fmt.Fprintf(&b, "  …and %d more\n", len(r.Incidents)-10)
			break
		}
		state := ""
		if in.StillFiring {
			state = ", still firing"
		}
		where := ""
		if in.Instance != "" {
			where = " on " + shortHost(in.Instance)
		}
		fmt.Fprintf(&b, "  %s%s: about %s%s\n", in.Alert, where, Human(in.Firing), state)
	}

	var hostNotes []string
	for _, h := range r.Hosts {
		switch {
		case !h.Up:
			hostNotes = append(hostNotes, fmt.Sprintf("  ✗ %s is not reporting", shortHost(h.Name)))
		case h.Disk > 0.8:
			hostNotes = append(hostNotes, fmt.Sprintf("  ! %s: %s is %.0f%% full", shortHost(h.Name), h.FullestMount, h.Disk*100))
		}
	}
	fmt.Fprintf(&b, "\nHosts: %d\n", len(r.Hosts))
	for _, l := range hostNotes {
		b.WriteString(l + "\n")
	}
	for _, s := range r.Services {
		if !s.Up {
			fmt.Fprintf(&b, "  ✗ service %s is down\n", s.Name)
		}
	}
	return b.String()
}

// Human formats a duration the way people say it: 40m, 1h, 3h20m, 2d.
func Human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	case d < 48*time.Hour:
		d = d.Round(time.Minute)
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

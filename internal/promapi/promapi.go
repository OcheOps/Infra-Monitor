// Package promapi is a small client for the parts of the Prometheus,
// Alertmanager and Loki HTTP APIs that the CLI reads.
package promapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Endpoints are the base URLs of the stack's APIs.
type Endpoints struct {
	Prometheus, Loki string
}

// Local is where the Compose stack publishes them.
var Local = Endpoints{Prometheus: "http://localhost:9090", Loki: "http://localhost:3100"}

// Client queries the stack.
type Client struct {
	Endpoints
	HTTP *http.Client
}

func New(e Endpoints) *Client {
	return &Client{Endpoints: e, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Sample is one series of an instant query.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Series is one series of a range query.
type Series struct {
	Labels map[string]string
	Points []Point
}

type Point struct {
	Time  time.Time
	Value float64
}

func (c *Client) get(ctx context.Context, base, path string, params url.Values, out any) error {
	u := strings.TrimRight(base, "/") + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return fmt.Errorf("%s: %s: %s", u, resp.Status, msg)
	}
	return json.Unmarshal(body, out)
}

func parseValue(v any) float64 {
	s, _ := v.(string)
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseTime(v any) time.Time {
	f, _ := v.(float64)
	return time.Unix(0, int64(f*float64(time.Second)))
}

// Query runs an instant PromQL query.
func (c *Client) Query(ctx context.Context, q string) ([]Sample, error) {
	var body struct {
		Data struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := c.get(ctx, c.Prometheus, "/api/v1/query", url.Values{"query": {q}}, &body); err != nil {
		return nil, err
	}
	switch body.Data.ResultType {
	case "vector":
		var res []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(body.Data.Result, &res); err != nil {
			return nil, err
		}
		out := make([]Sample, len(res))
		for i, r := range res {
			out[i] = Sample{Labels: r.Metric, Value: parseValue(r.Value[1])}
		}
		return out, nil
	case "scalar":
		var v [2]any
		if err := json.Unmarshal(body.Data.Result, &v); err != nil {
			return nil, err
		}
		return []Sample{{Labels: map[string]string{}, Value: parseValue(v[1])}}, nil
	default:
		return nil, fmt.Errorf("unexpected result type %q", body.Data.ResultType)
	}
}

// QueryRange runs a range query.
func (c *Client) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	params := url.Values{
		"query": {q},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(step.Seconds()))},
	}
	if err := c.get(ctx, c.Prometheus, "/api/v1/query_range", params, &body); err != nil {
		return nil, err
	}
	out := make([]Series, len(body.Data.Result))
	for i, r := range body.Data.Result {
		s := Series{Labels: r.Metric}
		for _, v := range r.Values {
			s.Points = append(s.Points, Point{Time: parseTime(v[0]), Value: parseValue(v[1])})
		}
		out[i] = s
	}
	return out, nil
}

// Alert is an alert as Prometheus reports it.
type Alert struct {
	Name        string
	State       string // pending or firing
	Labels      map[string]string
	Annotations map[string]string
	ActiveAt    time.Time
	Value       string
}

// Alerts lists pending and firing alerts, firing first, then by name.
func (c *Client) Alerts(ctx context.Context) ([]Alert, error) {
	var body struct {
		Data struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    time.Time         `json:"activeAt"`
				Value       string            `json:"value"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := c.get(ctx, c.Prometheus, "/api/v1/alerts", nil, &body); err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(body.Data.Alerts))
	for _, a := range body.Data.Alerts {
		out = append(out, Alert{Name: a.Labels["alertname"], State: a.State, Labels: a.Labels,
			Annotations: a.Annotations, ActiveAt: a.ActiveAt, Value: a.Value})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == "firing"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Rule is an alerting rule.
type Rule struct {
	Name, Query string
	Duration    time.Duration
	Labels      map[string]string
	Annotations map[string]string
}

// Rules lists the alerting rules.
func (c *Client) Rules(ctx context.Context) ([]Rule, error) {
	var body struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Type        string            `json:"type"`
					Name        string            `json:"name"`
					Query       string            `json:"query"`
					Duration    float64           `json:"duration"`
					Labels      map[string]string `json:"labels"`
					Annotations map[string]string `json:"annotations"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := c.get(ctx, c.Prometheus, "/api/v1/rules", url.Values{"type": {"alert"}}, &body); err != nil {
		return nil, err
	}
	var out []Rule
	for _, g := range body.Data.Groups {
		for _, r := range g.Rules {
			out = append(out, Rule{Name: r.Name, Query: r.Query, Duration: time.Duration(r.Duration * float64(time.Second)),
				Labels: r.Labels, Annotations: r.Annotations})
		}
	}
	return out, nil
}

// LogLine is one log entry.
type LogLine struct {
	Time   time.Time
	Labels map[string]string
	Line   string
}

// Logs runs a LogQL query over [start, end], newest first, up to limit lines.
func (c *Client) Logs(ctx context.Context, q string, start, end time.Time, limit int) ([]LogLine, error) {
	var body struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	params := url.Values{
		"query":     {q},
		"start":     {strconv.FormatInt(start.UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {strconv.Itoa(limit)},
		"direction": {"backward"},
	}
	if err := c.get(ctx, c.Loki, "/loki/api/v1/query_range", params, &body); err != nil {
		return nil, err
	}
	var out []LogLine
	for _, r := range body.Data.Result {
		for _, v := range r.Values {
			ns, _ := strconv.ParseInt(v[0], 10, 64)
			out = append(out, LogLine{Time: time.Unix(0, ns), Labels: r.Stream, Line: v[1]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

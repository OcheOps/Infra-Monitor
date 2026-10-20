// Package promapitest is a fake Prometheus and Loki for tests.
package promapitest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OcheOps/Infra-Monitor/internal/promapi"
)

// Fake answers instant queries from Vectors: the first key contained in the
// query wins, and queries matching nothing return an empty vector.
type Fake struct {
	Vectors map[string][]promapi.Sample
	Ranges  map[string][]promapi.Series
	Alerts  []promapi.Alert
	Rules   []promapi.Rule
	Logs    []promapi.LogLine
	// Queries records every PromQL and LogQL query received.
	Queries []string
}

// Start serves the fake and returns its endpoints.
func (f *Fake) Start(t *testing.T) promapi.Endpoints {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return promapi.Endpoints{Prometheus: srv.URL, Loki: srv.URL}
}

func match[T any](m map[string]T, q string) T {
	best := ""
	for k := range m {
		if strings.Contains(q, k) && len(k) > len(best) {
			best = k
		}
	}
	return m[best]
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q != "" {
		f.Queries = append(f.Queries, q)
	}
	var data any
	switch r.URL.Path {
	case "/api/v1/query":
		var res []map[string]any
		for _, s := range match(f.Vectors, q) {
			res = append(res, map[string]any{"metric": s.Labels, "value": []any{float64(time.Now().Unix()), fmt.Sprint(s.Value)}})
		}
		data = map[string]any{"resultType": "vector", "result": orEmpty(res)}
	case "/api/v1/query_range":
		var res []map[string]any
		for _, s := range match(f.Ranges, q) {
			var vals [][]any
			for _, p := range s.Points {
				vals = append(vals, []any{float64(p.Time.Unix()), fmt.Sprint(p.Value)})
			}
			res = append(res, map[string]any{"metric": s.Labels, "values": vals})
		}
		data = map[string]any{"resultType": "matrix", "result": orEmpty(res)}
	case "/api/v1/alerts":
		var res []map[string]any
		for _, a := range f.Alerts {
			res = append(res, map[string]any{"labels": a.Labels, "annotations": a.Annotations, "state": a.State, "activeAt": a.ActiveAt, "value": a.Value})
		}
		data = map[string]any{"alerts": orEmpty(res)}
	case "/api/v1/rules":
		var rules []map[string]any
		for _, rl := range f.Rules {
			rules = append(rules, map[string]any{"type": "alerting", "name": rl.Name, "query": rl.Query,
				"duration": rl.Duration.Seconds(), "labels": rl.Labels, "annotations": rl.Annotations})
		}
		data = map[string]any{"groups": []any{map[string]any{"name": "g", "rules": orEmpty(rules)}}}
	case "/loki/api/v1/query_range":
		var res []map[string]any
		for _, l := range f.Logs {
			res = append(res, map[string]any{"stream": l.Labels, "values": [][]string{{fmt.Sprint(l.Time.UnixNano()), l.Line}}})
		}
		data = map[string]any{"resultType": "streams", "result": orEmpty(res)}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// S is shorthand for a sample: S(1, "instance", "x").
func S(v float64, labels ...string) promapi.Sample {
	l := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		l[labels[i]] = labels[i+1]
	}
	return promapi.Sample{Labels: l, Value: v}
}

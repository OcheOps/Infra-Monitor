package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OcheOps/Infra-Monitor/internal/promapi"
	. "github.com/OcheOps/Infra-Monitor/internal/promapi/promapitest"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		`login failed password=hunter2 user=bob`:      `login failed password=[redacted] user=bob`,
		`{"api_key": "sk-123"}`:                       `{"api_key": "[redacted]"}`,
		`Authorization: Bearer abc.def`:               `Authorization: [redacted] abc.def`,
		`curl -H "x" Bearer abc.def`:                  `curl -H "x" Bearer [redacted]`,
		`dial postgres://app:s3cret@db:5432/x failed`: `dial postgres://app:[redacted]@db:5432/x failed`,
		`connection refused`:                          `connection refused`,
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGatherAndPrompt(t *testing.T) {
	now := time.Now()
	f := &Fake{
		Alerts: []promapi.Alert{
			{Name: "HostOutOfMemory", State: "firing", ActiveAt: now.Add(-20 * time.Minute), Value: "4.2",
				Labels:      map[string]string{"alertname": "HostOutOfMemory", "instance": "web-2", "agent": "web-2", "severity": "critical"},
				Annotations: map[string]string{"summary": "web-2 has 4% memory left"}},
			{Name: "HostOutOfMemory", State: "firing", Labels: map[string]string{"alertname": "HostOutOfMemory", "instance": "web-3"}},
		},
		Rules: []promapi.Rule{{Name: "HostOutOfMemory", Query: "node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes * 100 < 10", Duration: 5 * time.Minute}},
		Ranges: map[string][]promapi.Series{"node_memory": {
			{Labels: map[string]string{"instance": "web-2"}, Points: []promapi.Point{{Time: now, Value: 9.5}, {Time: now, Value: 4.25}}},
			{Labels: map[string]string{"instance": "web-3"}, Points: []promapi.Point{{Time: now, Value: 8}}},
		}},
		Logs: []promapi.LogLine{{Time: now, Labels: map[string]string{"host": "web-2", "job": "infra-monitor"},
			Line: "kernel: Out of memory: Killed process 123 (java) token=abc123"}},
	}
	c := promapi.New(f.Start(t))
	ec, err := Gather(context.Background(), c, "HostOutOfMemory", "web-2", "1 problem(s)\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(ec.Series) != 1 || ec.Series[0].Labels["instance"] != "web-2" {
		t.Errorf("series = %+v", ec.Series)
	}
	if q := f.Queries[len(f.Queries)-1]; !strings.HasPrefix(q, `{agent="web-2"} |~ "(?i)(error|`) {
		t.Errorf("log query = %q", q)
	}
	p := ec.Prompt()
	for _, want := range []string{
		"Alert: HostOutOfMemory", "State: firing since", "(20m0s ago)", `instance="web-2"`, "Summary: web-2 has 4% memory left",
		"Value when it fired: 4.2", "Must hold for: 5m0s", "node_memory_MemAvailable_bytes",
		`{instance="web-2"}: 9.5 4.25`, "web-2/infra-monitor kernel: Out of memory", "token=[redacted]", "1 problem(s)",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "abc123") || strings.Contains(p, "web-3") {
		t.Errorf("prompt leaks a secret or another instance:\n%s", p)
	}

	if _, err := Gather(context.Background(), c, "Nope", "", ""); err == nil || !strings.Contains(err.Error(), "known: HostOutOfMemory") {
		t.Errorf("unknown alert: %v", err)
	}
}

// fakeClaude streams a canned answer and records the request.
func fakeClaude(t *testing.T, stopReason string, status int) (*httptest.Server, *map[string]any, *http.Header) {
	var body map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		body = nil
		json.Unmarshal(b, &body)
		if status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(name, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
		ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		for _, s := range []string{"Memory is ", "nearly exhausted."} {
			ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, s))
		}
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":5}}`, stopReason))
		ev("message_stop", `{"type":"message_stop"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &body, &hdr
}

func TestAsk(t *testing.T) {
	srv, body, hdr := fakeClaude(t, "end_turn", 200)
	var out strings.Builder
	if err := Ask(context.Background(), Options{APIKey: "sk-ant-fake", BaseURL: srv.URL}, "the prompt", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Memory is nearly exhausted.\n" {
		t.Errorf("output = %q", out.String())
	}
	b := *body
	if b["model"] != "claude-opus-5-5" || b["fallbacks"] != "default" || b["stream"] != true {
		t.Errorf("request = %v", b)
	}
	if oc, _ := b["output_config"].(map[string]any); oc["effort"] != "medium" {
		t.Errorf("output_config = %v", b["output_config"])
	}
	if _, ok := b["thinking"]; ok {
		t.Error("thinking should be left to the model's default")
	}
	if !strings.Contains(hdr.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") || hdr.Get("X-Api-Key") != "sk-ant-fake" {
		t.Errorf("headers = %v", *hdr)
	}

	// A custom model gets no fallback settings.
	Ask(context.Background(), Options{APIKey: "k", Model: "claude-sonnet-5-5", BaseURL: srv.URL}, "p", io.Discard)
	if _, ok := (*body)["fallbacks"]; ok || (*body)["model"] != "claude-sonnet-5-5" {
		t.Errorf("custom model request = %v", *body)
	}
}

func TestAskErrors(t *testing.T) {
	srv, _, _ := fakeClaude(t, "refusal", 200)
	if err := Ask(context.Background(), Options{APIKey: "k", BaseURL: srv.URL}, "p", io.Discard); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("refusal: %v", err)
	}
	srv401, _, _ := fakeClaude(t, "", 401)
	if err := Ask(context.Background(), Options{APIKey: "k", BaseURL: srv401.URL}, "p", io.Discard); err == nil || !strings.Contains(err.Error(), "ai enable") {
		t.Errorf("401: %v", err)
	}
}

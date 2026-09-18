package aiprotocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Test fixtures and JSON accessors. The protocol traps these tests pin down
// (design.md §12.5) are all *wire shape*, so every fixture asserts on the bytes
// that actually reached the server rather than on an encoder's internals.

// testAPIKey is deliberately key-shaped and distinctive: the redaction tests
// look for it in error strings.
const testAPIKey = "sk-test-secret-0123456789"

type captured struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// fixture is an httptest server that records every request it receives.
type fixture struct {
	*httptest.Server

	mu  sync.Mutex
	got []captured
}

func newFixture(t *testing.T, respond func(http.ResponseWriter, *http.Request)) *fixture {
	t.Helper()
	f := &fixture{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, captured{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Header: r.Header.Clone(),
			Body:   body,
		})
		f.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// newSSEFixture answers every request with one event-stream body.
func newSSEFixture(t *testing.T, body string) *fixture {
	t.Helper()
	return newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
}

// newJSONFixture answers every request with a status and a JSON body.
func newJSONFixture(t *testing.T, status int, body string) *fixture {
	t.Helper()
	return newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func (f *fixture) requests() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.got...)
}

func (f *fixture) last(t *testing.T) captured {
	t.Helper()
	got := f.requests()
	if len(got) == 0 {
		t.Fatalf("no request reached the fixture")
	}
	return got[len(got)-1]
}

// stopStream is the smallest body that makes each dialect finish cleanly, so a
// request-shape test can call Stream end to end.
func stopStream(protocol string) string {
	switch protocol {
	case ProtocolOpenAICompletions:
		return "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	case ProtocolOpenAIResponses:
		return "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
	default:
		return "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
}

// baseRequest builds a request pointed at the fixture, with the fixture's root
// base_url semantics (f.URL is the host, /v1 is the provider root).
func baseRequest(f *fixture, protocol string) Request {
	return Request{
		BaseURL:  f.URL + "/v1",
		APIKey:   testAPIKey,
		Protocol: protocol,
		Model:    "test-model",
		Messages: []Message{{Role: roleUser, Text: "hi"}},
	}
}

func runStream(t *testing.T, f *fixture, req Request) []Event {
	t.Helper()
	rec := &eventRecorder{}
	if err := Stream(context.Background(), f.Client(), req, rec.collect); err != nil {
		t.Fatalf("Stream(%s): %v", req.Protocol, err)
	}
	return rec.events
}

type eventRecorder struct {
	events []Event
}

func (r *eventRecorder) collect(ev Event) error {
	r.events = append(r.events, ev)
	return nil
}

func (r *eventRecorder) kinds() []string {
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Kind)
	}
	return out
}

func (r *eventRecorder) first(kind string) *Event {
	for i := range r.events {
		if r.events[i].Kind == kind {
			return &r.events[i]
		}
	}
	return nil
}

func (r *eventRecorder) text() string {
	var b strings.Builder
	for _, ev := range r.events {
		if ev.Kind == KindText {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func (r *eventRecorder) thinking() string {
	var b strings.Builder
	for _, ev := range r.events {
		if ev.Kind == KindThinking {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func (r *eventRecorder) calls() []ToolCall {
	out := []ToolCall{}
	for _, ev := range r.events {
		if ev.Kind == KindToolCall && ev.ToolCall != nil {
			out = append(out, *ev.ToolCall)
		}
	}
	return out
}

// --- JSON accessors -------------------------------------------------------

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode JSON %s: %v", raw, err)
	}
	return m
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func field(t *testing.T, m map[string]any, key string) any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing from %v", key, m)
	}
	return v
}

func object(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: want a JSON object, got %T (%v)", what, v, v)
	}
	return m
}

func array(t *testing.T, v any, what string) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want a JSON array, got %T (%v)", what, v, v)
	}
	return a
}

func stringOf(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("want a JSON string, got %T (%v)", v, v)
	}
	return s
}

func objectField(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	return object(t, field(t, m, key), key)
}

func arrayField(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	return array(t, field(t, m, key), key)
}

func stringField(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	return stringOf(t, field(t, m, key))
}

// countField reads a JSON number as an int.
func countField(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	n, ok := field(t, m, key).(float64)
	if !ok {
		t.Fatalf("%s: want a JSON number, got %T", key, m[key])
	}
	return int(n)
}

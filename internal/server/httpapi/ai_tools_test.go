package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- the mock upstream ---
//
// §12.6 makes the assistant streaming-only: the adapter rejects a 2xx answer
// whose Content-Type is not text/event-stream, and it treats a stream that ends
// without finish_reason/[DONE] as truncated. So every fixture here is a real
// event stream — serving the old non-streaming JSON body is what made these
// tests fail. The request bodies are still captured, because "what did the model
// actually receive" is half of what they assert.

// aiUpstreamRequest is the test-side view of one completions request body.
// aiOpenAIRequest (the production type) has no tool_call_id, and pairing a tool
// result with the assistant's own call id is exactly what the loop tests must
// observe (§12.6: resolveAIConfirmation reuses action.CallID).
type aiUpstreamRequest struct {
	Model    string              `json:"model"`
	Stream   bool                `json:"stream"`
	Messages []aiUpstreamMessage `json:"messages"`
	Tools    []aiOpenAIToolSpec  `json:"tools"`
}

type aiUpstreamMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	ToolCalls  []aiOpenAIToolCall `json:"tool_calls"`
	ToolCallID string             `json:"tool_call_id"`
}

// toolMessageFor returns the dialect's tool message carrying callID — the proof
// that a tool result was fed back for the RIGHT call.
func toolMessageFor(request aiUpstreamRequest, callID string) *aiUpstreamMessage {
	for i := range request.Messages {
		if request.Messages[i].Role == "tool" && request.Messages[i].ToolCallID == callID {
			return &request.Messages[i]
		}
	}
	return nil
}

// mockAIUpstream stands in for an OpenAI-compatible gateway that honours
// stream:true. It records every request body it is handed.
type mockAIUpstream struct {
	t      *testing.T
	server *httptest.Server
	// check inspects each request (auth header, path, ...). Optional; set it
	// before the first call.
	check func(*http.Request)

	seq func(call int) string

	mu       sync.Mutex
	requests []aiUpstreamRequest
}

// newMockAIUpstream answers with one canned body per call and repeats the last
// one if the loop asks for more.
func newMockAIUpstream(t *testing.T, canned ...string) *mockAIUpstream {
	t.Helper()
	if len(canned) == 0 {
		t.Fatal("newMockAIUpstream needs at least one canned body")
	}
	return newMockAIUpstreamFunc(t, func(call int) string {
		if call > len(canned) {
			call = len(canned)
		}
		return canned[call-1]
	})
}

// newMockAIUpstreamFunc answers the n-th call (1-based) with fn(n), for a model
// that reacts to what it was told.
func newMockAIUpstreamFunc(t *testing.T, fn func(call int) string) *mockAIUpstream {
	t.Helper()
	mock := &mockAIUpstream{t: t, seq: fn}
	mock.server = httptest.NewServer(http.HandlerFunc(mock.handle))
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *mockAIUpstream) handle(w http.ResponseWriter, r *http.Request) {
	var body aiUpstreamRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.t.Errorf("decode upstream body: %v", err)
	}
	if m.check != nil {
		m.check(r)
	}
	m.mu.Lock()
	m.requests = append(m.requests, body)
	call := len(m.requests)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, m.seq(call))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (m *mockAIUpstream) URL() string { return m.server.URL }

func (m *mockAIUpstream) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *mockAIUpstream) Requests() []aiUpstreamRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]aiUpstreamRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

func (m *mockAIUpstream) Request(t *testing.T, index int) aiUpstreamRequest {
	t.Helper()
	requests := m.Requests()
	if index >= len(requests) {
		t.Fatalf("upstream call %d missing: only %d arrived", index+1, len(requests))
	}
	return requests[index]
}

// --- minimal legal completions streams ---

// openAIData renders one `data:` frame of the completions dialect.
func openAIData(payload string) string { return "data: " + payload + "\n\n" }

// openAIStreamText ends a turn with text and no tool call.
func openAIStreamText(t *testing.T, text string) string {
	t.Helper()
	return openAIStreamChunks(t, text, nil, "stop")
}

// openAIStreamToolCall ends a turn with one fully assembled tool call.
func openAIStreamToolCall(t *testing.T, id, name, arguments string) string {
	t.Helper()
	return openAIStreamChunks(t, "", map[string]any{
		"index": 0, "id": id,
		"function": map[string]any{"name": name, "arguments": arguments},
	}, "tool_calls")
}

func openAIStreamChunks(t *testing.T, text string, toolCall map[string]any, finish string) string {
	t.Helper()
	var body strings.Builder
	body.WriteString(openAIData(openAIChoice(t, map[string]any{"role": "assistant"}, "")))
	if text != "" {
		body.WriteString(openAIData(openAIChoice(t, map[string]any{"content": text}, "")))
	}
	if toolCall != nil {
		body.WriteString(openAIData(openAIChoice(t, map[string]any{"tool_calls": []any{toolCall}}, "")))
	}
	if finish == "" {
		finish = "stop"
	}
	body.WriteString(openAIData(openAIChoice(t, map[string]any{}, finish)))
	// [DONE] is the dialect's terminal marker: without it (or a finish_reason)
	// the adapter calls the stream truncated and turns it into an error.
	body.WriteString(openAIData("[DONE]"))
	return body.String()
}

func openAIChoice(t *testing.T, delta map[string]any, finish string) string {
	t.Helper()
	choice := map[string]any{"delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	raw, err := json.Marshal(map[string]any{"choices": []any{choice}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// --- helpers shared by the tool and loop tests ---

// shrinkAICommandWait shortens the loop's wait for a queued command's result
// (§12.1 caps it at 8s in production). Tests that leave a command pending have
// no eight seconds to spare.
func shrinkAICommandWait(t *testing.T, wait time.Duration) {
	t.Helper()
	previous := aiQueuedCommandWait
	aiQueuedCommandWait = wait
	t.Cleanup(func() { aiQueuedCommandWait = previous })
}

// toolResultMessageHas reports whether a persisted tool message carries needle.
// The transcript is where an outcome the stream does not spell out (a refusal
// reason, say) is still observable.
func toolResultMessageHas(t *testing.T, api *Server, sessionID, needle string) bool {
	t.Helper()
	if sessionID == "" {
		t.Fatal("no session id: the session event never arrived")
	}
	messages, err := api.Store.ListAIMessages(sessionID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == "tool" && strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

// confirmationAuditCount counts the records a held-back action leaves behind.
// It is the only test-visible way to assert "nothing was staged at all".
func confirmationAuditCount(t *testing.T, api *Server) int {
	t.Helper()
	audit, err := api.Store.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range audit {
		if entry.Action == "ai_action_confirmation_required" {
			count++
		}
	}
	return count
}

func nodeCommandCount(t *testing.T, api *Server, nodeID string) int {
	t.Helper()
	cmds, err := api.Store.ListCommands(nodeID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(cmds)
}

func TestAIContextIncludesNodesAndLatency(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-ctx-primary")
	otherID := createAINode(t, api, "ai-ctx-other")
	if err := api.Store.CreateNode(&store.Node{
		ID: otherID + "-2", Name: "second node", MachineID: "second-machine",
		PrimaryIP: "203.0.113.10", AgentVersion: "0.4.1", TZ: "UTC",
	}, "hash"); err != nil {
		t.Fatal(err)
	}
	targetID, err := api.Store.CreateLatencyTarget("google", "tcp", "8.8.8.8", 443)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetNodeLatencyTargets(nodeID, []int64{targetID}); err != nil {
		t.Fatal(err)
	}
	ts := nowUnix()
	if err := api.Store.InsertLatencySamples(nodeID, []store.LatencySampleRow{
		{TargetID: targetID, TS: ts - 60, ICMPMs: 10, TCPMs: 12.5, Loss: 0},
		{TargetID: targetID, TS: ts - 30, ICMPMs: 20, TCPMs: 22.5, Loss: 0},
	}); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, server.URL)
	upstream := newMockAIUpstream(t, openAIStreamText(t, "ok"))
	configureAI(t, api, upstream.URL(), "k", "m")

	result := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "overview please"})
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d", result.Status)
	}
	system := upstream.Request(t, 0).Messages[0].Content
	for _, want := range []string{
		`"nodes"`, `"current":true`, `"agent_version"`,
		`"latency"`, `"8.8.8.8"`, `"icmp_p95_ms":20`, `"tcp_p95_ms":22.5`,
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system context missing %s:\n%s", want, system)
		}
	}
}

// answerBrowserQueries stands in for the browser that holds a terminal session
// (design §12.7.1): every buffer query is answered with the same window.
//
// The server keeps NO terminal state — that is the whole point of the design —
// so a test cannot invent a screen any other way. This helper is the test-side
// equivalent of the xterm instance.
func answerBrowserQueries(t *testing.T, api *Server, sessionID, screen string) {
	t.Helper()
	lines := strings.Split(screen, "\n")
	cleanup := api.Hub.RegisterTerminalPush(sessionID, func(env protocol.Envelope) bool {
		var query protocol.TerminalQuery
		if err := json.Unmarshal(env.Payload, &query); err != nil {
			return false
		}
		return api.Hub.DeliverTerminalBuffer(protocol.TerminalBuffer{
			ID: query.ID, OK: true, Cols: 80, Rows: len(lines),
			Length: len(lines), Lines: lines,
		})
	})
	t.Cleanup(cleanup)
}

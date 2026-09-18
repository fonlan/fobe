package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

func TestAIChatRequiresSession(t *testing.T) {
	server, _ := newTestServer(t)
	defer server.Close()

	resp, body := postJSON(t, http.DefaultClient, server.URL+"/api/ai/chat", map[string]any{
		"node_id": "node-1", "terminal_session_id": "term-1", "message": "hello",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (%v)", resp.StatusCode, http.StatusUnauthorized, body)
	}
	if code := errorCode(body); code != "unauthorized" {
		t.Fatalf("error code = %q, want unauthorized", code)
	}
}

func TestAIChatNotConfigured(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-node-not-configured")
	cookie := loginCookie(t, server.URL)

	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, TerminalSessionID: "term-1", Message: "hello",
	})
	if result.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (%v)", result.Status, http.StatusServiceUnavailable, result.JSONBody)
	}
	if code := result.errorCode(); code != "ai_not_configured" {
		t.Fatalf("error code = %q, want ai_not_configured", code)
	}
}

func TestAIChatNodeNotFound(t *testing.T) {
	server, _ := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)

	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: "missing-node", TerminalSessionID: "term-1", Message: "hello",
	})
	if result.Status != http.StatusNotFound || result.errorCode() != "not_found" {
		t.Fatalf("response = %d %v", result.Status, result.JSONBody)
	}
}

func TestAIChatUpstreamMockPersistsConversationAndContext(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-node-context")
	cookie := loginCookie(t, server.URL)

	upstream := newMockAIUpstream(t, openAIStreamText(t, "hello from mock"))
	upstream.check = func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-ai-key" {
			t.Errorf("authorization = %q", got)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
	}
	configureAI(t, api, upstream.URL()+"/v1", "test-ai-key", "test-model")

	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, TerminalSessionID: "term-1", Message: "why is this node unhealthy?",
	})
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", result.Status, result.JSONBody)
	}
	if result.SessionID == "" || result.Text != "hello from mock" {
		t.Fatalf("response = %+v", result)
	}
	if result.TurnEnd == nil || result.TurnEnd.Reason != "completed" {
		t.Fatalf("turn end = %+v, want reason=completed", result.TurnEnd)
	}

	if upstream.Calls() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a plain answer ends the turn)", upstream.Calls())
	}
	upstreamBody := upstream.Request(t, 0)
	if upstreamBody.Model != "test-model" || len(upstreamBody.Messages) < 2 {
		t.Fatalf("upstream request = %+v", upstreamBody)
	}
	// §12.5: the assistant loop is streaming-only; a non-stream request would
	// make the adapter reject the answer.
	if !upstreamBody.Stream {
		t.Fatal("upstream request did not ask for stream:true")
	}
	if upstreamBody.Messages[0].Role != "system" || !strings.Contains(upstreamBody.Messages[0].Content, nodeID) {
		t.Fatalf("missing node context: %+v", upstreamBody.Messages[0])
	}
	if strings.Contains(upstreamBody.Messages[0].Content, `"include_logs"`) {
		t.Fatalf("unexpected log context: %s", upstreamBody.Messages[0].Content)
	}
	if upstreamBody.Messages[len(upstreamBody.Messages)-1].Content != "why is this node unhealthy?" {
		t.Fatalf("last message = %+v", upstreamBody.Messages[len(upstreamBody.Messages)-1])
	}

	messages, err := api.Store.ListAIMessages(result.SessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The turn now also records a `turn_end` marker row (§12.6: the reason a turn
	// stopped is persisted so a reload can explain it). It is panel metadata, so
	// the conversational rows are asserted first and the marker separately.
	if len(messages) != 3 || messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Fatalf("persisted messages = %+v", messages)
	}
	if messages[1].Content != "hello from mock" {
		t.Fatalf("assistant message = %+v", messages[1])
	}
	if messages[2].Role != "turn_end" || messages[2].Content != "completed" {
		t.Fatalf("turn end marker = %+v, want role=turn_end reason=completed", messages[2])
	}
}

func TestAIRiskyOrPolicyGatedActionNeverExecutes(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-node-safety")
	cookie := loginCookie(t, server.URL)

	// A model that always proposes the same risky command: the run ends at the
	// confirmation, so no further upstream answer is needed.
	upstream := newMockAIUpstream(t, openAIStreamToolCall(t, "call-1", "run_shell",
		`{"command":"reboot","reason":"restart required","risky":true}`))
	configureAI(t, api, upstream.URL(), "test-ai-key", "test-model")

	result := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "reboot it"})
	if result.Status != http.StatusOK {
		t.Fatalf("risky status = %d (%v)", result.Status, result.JSONBody)
	}
	// §12.6: a risky action is expressed as a needs_confirmation event plus a
	// pending row — the stream ends and the decision is the operator's.
	confirm := result.Confirmation
	if confirm == nil || confirm.ActionID == "" || confirm.Name != "run_shell" {
		t.Fatalf("needs_confirmation = %+v", confirm)
	}
	if confirm.Command != "reboot" || confirm.Reason != "restart required" {
		t.Fatalf("confirmation payload = %+v", confirm)
	}
	if result.TurnEnd == nil || result.TurnEnd.Reason != "needs_confirmation" {
		t.Fatalf("turn end = %+v, want reason=needs_confirmation", result.TurnEnd)
	}
	action, err := api.Store.GetAIPendingAction(confirm.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if action.Status != "pending" || action.Kind != "run_shell" || action.Risk != "risky" {
		t.Fatalf("pending action = %+v", action)
	}
	if action.CallID != "call-1" {
		t.Fatalf("pending action call id = %q, want call-1", action.CallID)
	}
	commands, err := api.Store.ListCommands(nodeID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("risky action executed commands = %+v", commands)
	}
	assertAIConfirmationAudit(t, api, result.SessionID, "restart required", "risky")

	// A panel SETTING can also close the gate (§12.3's default_policy): the
	// proposal is still recorded, so the operator sees what was asked, but
	// nothing executes until they answer. This replaced the kill-switch half of
	// this test — the freeze was removed because an agent whose only useful
	// capability is running commands cannot be made "read-only" and still work.
	if err := api.Store.SetSetting("ai.default_policy", "confirm", false); err != nil {
		t.Fatal(err)
	}
	result2 := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, SessionID: result.SessionID, Message: "reboot it again",
	})
	if result2.Status != http.StatusOK {
		t.Fatalf("policy status = %d (%v)", result2.Status, result2.JSONBody)
	}
	if result2.Confirmation == nil || result2.Confirmation.ActionID == "" {
		t.Fatalf("policy confirmation = %+v", result2.Confirmation)
	}
	if result2.Confirmation.ActionID == confirm.ActionID {
		t.Fatal("the policy prompt reused the previous pending action")
	}
	action2, err := api.Store.GetAIPendingAction(result2.Confirmation.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if action2.Status != "pending" || action2.Risk != "policy" {
		t.Fatalf("policy pending action = %+v", action2)
	}
	commands, err = api.Store.ListCommands(nodeID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("policy-gated action executed commands = %+v", commands)
	}
	assertAIConfirmationAudit(t, api, result2.SessionID, "restart required", "policy")
}

// assertAIConfirmationAudit pins the §12.3 audit trail a held-back action
// leaves behind: the risk label and the reason travel with it, so a reviewer can
// tell a model-marked risky action from one the panel's policy held back.
func assertAIConfirmationAudit(t *testing.T, api *Server, sessionID, reason, risk string) {
	t.Helper()
	audit, err := api.Store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range audit {
		if entry.Actor == "ai" && entry.AISessionID == sessionID && entry.Reason == reason && entry.Risk == risk {
			return
		}
	}
	t.Fatalf("audit missing actor=ai session=%s reason=%q risk=%q: %+v", sessionID, reason, risk, audit)
}

func TestAIChatUpstreamErrorCode(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-node-upstream-error")
	cookie := loginCookie(t, server.URL)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream failure", http.StatusBadGateway)
	}))
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "test-ai-key", "test-model")

	result := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "hello"})
	// Once the SSE response has started a JSON error is impossible: the failure
	// arrives as an `error` event, and the HTTP status stays 200.
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", result.Status, result.JSONBody)
	}
	if code := result.errorCode(); code != "ai_upstream_error" {
		t.Fatalf("error code = %q, want ai_upstream_error", code)
	}
	if result.Err == nil || strings.TrimSpace(result.Err.Message) == "" {
		t.Fatalf("error event = %+v, want the redacted upstream message", result.Err)
	}
	if result.TurnEnd == nil || result.TurnEnd.Reason != "upstream_error" {
		t.Fatalf("turn end = %+v, want reason=upstream_error", result.TurnEnd)
	}
}

func TestGetSettingsExposesAIConfigured(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)

	read := func() bool {
		t.Helper()
		resp, raw := doAuthed(t, http.MethodGet, server.URL+"/api/settings", cookie, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d (%s)", resp.StatusCode, raw)
		}
		var body struct {
			AIConfigured bool `json:"ai_configured"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return body.AIConfigured
	}

	if read() {
		t.Fatal("ai_configured = true before any AI settings were stored")
	}

	// Partially configured (provider present, key missing) must stay false: it
	// is exactly the state in which POST /api/ai/chat answers 503, and the
	// terminal page hides the sidebar off this flag.
	configureAI(t, api, "https://ai.example.com/v1", "test-ai-key", "gpt-test")
	provider, err := api.Store.GetAIProvider(testAIProviderID)
	if err != nil {
		t.Fatal(err)
	}
	provider.APIKeyEnc = ""
	if err := api.Store.UpsertAIProvider(provider); err != nil {
		t.Fatal(err)
	}
	if read() {
		t.Fatal("ai_configured = true without an api_key")
	}

	// Disabled models and disabled providers are equally unusable: the gate is
	// about "there is something the picker could run", not "rows exist".
	configureAI(t, api, "https://ai.example.com/v1", "test-ai-key", "gpt-test")
	if !read() {
		t.Fatal("ai_configured = false after a usable provider+model pair was stored")
	}
	model, err := api.Store.GetAIModel("gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	model.Enabled = false
	if err := api.Store.UpsertAIModel(model); err != nil {
		t.Fatal(err)
	}
	if read() {
		t.Fatal("ai_configured = true with every model disabled")
	}
}

// testAIProviderID is the provider configureAI seeds; tests that need to flip
// its key off reach for it by name.
const testAIProviderID = "aip-test"

// configureAI seeds one usable (provider, model) pair and pins it as the panel
// default — the shape the multi-provider rewrite stores (§12.1). The three
// legacy settings keys no longer exist, so a test that writes them would sail
// past with ai_configured=false and fail much later, which is why this helper
// is the only place that knows how to "configure AI".
func configureAI(t *testing.T, api *Server, baseURL, key, model string) {
	t.Helper()
	ciphertext, err := api.Crypt.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	provider := &store.AIProvider{
		ID: testAIProviderID, Name: "test upstream",
		Protocol: store.ProtocolOpenAICompletions,
		BaseURL:  strings.TrimRight(baseURL, "/"), APIKeyEnc: ciphertext,
		Enabled: true, CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}
	if err := api.Store.UpsertAIProvider(provider); err != nil {
		t.Fatal(err)
	}
	row := &store.AIModel{
		ID: model, DisplayName: model, ContextWindow: 200000, MaxOutputTokens: 8192,
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		ReasoningLevels: []string{"off", "low", "medium", "high"},
		Source:          "manual", Enabled: true, CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}
	if err := api.Store.UpsertAIModel(row); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.LinkAIProviderModel(provider.ID, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetSetting("ai.default_provider_id", provider.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetSetting("ai.default_model_id", row.ID, false); err != nil {
		t.Fatal(err)
	}
}

func createAINode(t *testing.T, api *Server, id string) string {
	t.Helper()
	err := api.Store.CreateNode(&store.Node{
		ID: id, Name: "AI test node", MachineID: id + "-machine", TZ: "UTC",
	}, "hash")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func loginCookie(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Post(baseURL+"/api/login", "application/json", bytes.NewBufferString(`{"password":"test-password-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == security.SessionCookieName {
			return cookie.Name + "=" + cookie.Value
		}
	}
	t.Fatal("session cookie missing")
	return ""
}

// --- the client side of the §12.6 streaming contract ---
//
// POST /api/ai/chat answers with text/event-stream, so a test cannot decode the
// body as JSON any more (that is what made the old tests fail with "invalid
// character 'e' looking for beginning of value" — they were parsing the
// `event: ...` framing). EventSource is not an option either: it cannot POST.

type aiToolResultEvent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
	Reason   string `json:"reason"`
}

type aiConfirmEvent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ActionID string `json:"action_id"`
	Command  string `json:"command"`
	Reason   string `json:"reason"`
	Risk     string `json:"risk"`
}

type aiTurnEndEvent struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
	Changes   int    `json:"changes"`
	ActionID  string `json:"action_id"`
}

type aiErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// aiChatResult is the test-side view of one chat exchange: either a decoded
// event stream (a turn that started, whatever it ended with) or the JSON error
// body of a request refused before the stream began.
type aiChatResult struct {
	Status       int
	JSONBody     map[string]any
	SessionID    string
	Text         string
	Thinking     string
	ToolResults  []aiToolResultEvent
	Confirmation *aiConfirmEvent
	TurnEnd      *aiTurnEndEvent
	Err          *aiErrorEvent
	Raw          []aiSSEEvent
}

// errorCode reads the machine code from whichever surface carried it: an
// `error` event mid-stream, or the JSON error of an early refusal.
func (r *aiChatResult) errorCode() string {
	if r.Err != nil {
		return r.Err.Code
	}
	return errorCode(r.JSONBody)
}

// toolResult returns the first tool_result event for a tool, or nil.
func (r *aiChatResult) toolResult(name string) *aiToolResultEvent {
	for i := range r.ToolResults {
		if r.ToolResults[i].Name == name {
			return &r.ToolResults[i]
		}
	}
	return nil
}

type aiSSEEvent struct {
	Name string
	Data string
}

// aiSSEReader frames one event stream: `event:`/`data:` lines, events separated
// by a blank line. Deliberately hand-rolled — the server's exact framing is part
// of what these tests verify.
type aiSSEReader struct {
	sc *bufio.Scanner
}

func newAISSEReader(r io.Reader) *aiSSEReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return &aiSSEReader{sc: sc}
}

// Next returns the next complete event, or false once the stream ends.
func (r *aiSSEReader) Next() (aiSSEEvent, bool) {
	var (
		name string
		data []string
	)
	for r.sc.Scan() {
		line := strings.TrimRight(r.sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) == 0 {
				name = ""
				continue
			}
			return aiSSEEvent{Name: name, Data: strings.Join(data, "\n")}, true
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive.
		default:
			field, value, _ := strings.Cut(line, ":")
			switch field {
			case "event":
				name = strings.TrimPrefix(value, " ")
			case "data":
				data = append(data, strings.TrimPrefix(value, " "))
			}
		}
	}
	if len(data) > 0 {
		return aiSSEEvent{Name: name, Data: strings.Join(data, "\n")}, true
	}
	return aiSSEEvent{}, false
}

// aiChatStream is one in-flight response. Keeping the un-consumed stream around
// is what lets the stop-button test cut a turn short instead of only inspecting
// finished turns.
type aiChatStream struct {
	status  int
	resp    *http.Response
	reader  *aiSSEReader
	jsonErr map[string]any
}

func (s *aiChatStream) close() {
	if s.resp != nil {
		_ = s.resp.Body.Close()
	}
}

func (s *aiChatStream) next() (aiSSEEvent, bool) {
	if s.reader == nil {
		return aiSSEEvent{}, false
	}
	return s.reader.Next()
}

// collect drains what is left of the stream into a result.
func (s *aiChatStream) collect(t *testing.T) *aiChatResult {
	t.Helper()
	result := &aiChatResult{Status: s.status, JSONBody: s.jsonErr}
	for {
		event, ok := s.next()
		if !ok {
			return result
		}
		result.Raw = append(result.Raw, event)
		applyAIChatEvent(t, result, event)
	}
}

// openAIChatStream POSTs a chat body and returns the open response. A JSON body
// (a refusal that never reached the SSE writer) is decoded up front; an event
// stream is left for the caller to consume.
func openAIChatStream(t *testing.T, ctx context.Context, url, cookie string, payload any) *aiChatStream {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	stream := &aiChatStream{status: resp.StatusCode, resp: resp}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		stream.reader = newAISSEReader(resp.Body)
		return stream
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	stream.jsonErr = body
	return stream
}

func postAIChat(t *testing.T, baseURL, cookie string, request aiChatRequest) *aiChatResult {
	t.Helper()
	return postAIEndpoint(t, baseURL+"/api/ai/chat", cookie, request)
}

// postAIChatContinue answers a confirmation (§12.6): the same streaming contract
// as /api/ai/chat, entered through the action instead of a new message.
func postAIChatContinue(t *testing.T, baseURL, cookie string, request aiContinueRequest) *aiChatResult {
	t.Helper()
	return postAIEndpoint(t, baseURL+"/api/ai/chat/continue", cookie, request)
}

func postAIEndpoint(t *testing.T, url, cookie string, payload any) *aiChatResult {
	t.Helper()
	stream := openAIChatStream(t, context.Background(), url, cookie, payload)
	defer stream.close()
	return stream.collect(t)
}

// applyAIChatEvent folds one event into the result. The switch is exhaustive on
// purpose: a new event name the panel was never taught fails the test loudly.
func applyAIChatEvent(t *testing.T, out *aiChatResult, event aiSSEEvent) {
	t.Helper()
	switch event.Name {
	case aiEventSession:
		var payload struct {
			SessionID string `json:"session_id"`
		}
		decodeAIEvent(t, event, &payload)
		out.SessionID = payload.SessionID
	case aiEventText:
		var payload struct {
			Text string `json:"text"`
		}
		decodeAIEvent(t, event, &payload)
		out.Text += payload.Text
	case aiEventThinking:
		var payload struct {
			Text string `json:"text"`
		}
		decodeAIEvent(t, event, &payload)
		out.Thinking += payload.Text
	case aiEventToolResult:
		var payload aiToolResultEvent
		decodeAIEvent(t, event, &payload)
		out.ToolResults = append(out.ToolResults, payload)
	case aiEventNeedsConfirm:
		var payload aiConfirmEvent
		decodeAIEvent(t, event, &payload)
		out.Confirmation = &payload
	case aiEventTurnEnd:
		var payload aiTurnEndEvent
		decodeAIEvent(t, event, &payload)
		out.TurnEnd = &payload
	case aiEventError:
		var payload aiErrorEvent
		decodeAIEvent(t, event, &payload)
		out.Err = &payload
	default:
		t.Errorf("unknown SSE event %q (%s)", event.Name, event.Data)
	}
}

func decodeAIEvent(t *testing.T, event aiSSEEvent, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(event.Data), out); err != nil {
		t.Fatalf("decode %s event %q: %v", event.Name, event.Data, err)
	}
}

func errorCode(body map[string]any) string {
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errorBody["code"].(string)
	return code
}

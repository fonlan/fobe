package httpapi

import (
	"bytes"
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

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, TerminalSessionID: "term-1", Message: "hello",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (%v)", resp.StatusCode, http.StatusServiceUnavailable, body)
	}
	if code := errorCode(body); code != "ai_not_configured" {
		t.Fatalf("error code = %q, want ai_not_configured", code)
	}
}

func TestAIChatNodeNotFound(t *testing.T) {
	server, _ := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: "missing-node", TerminalSessionID: "term-1", Message: "hello",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || errorCode(body) != "not_found" {
		t.Fatalf("response = %d %v", resp.StatusCode, body)
	}
}

func TestAIChatUpstreamMockPersistsConversationAndContext(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-node-context")
	cookie := loginCookie(t, server.URL)

	var upstreamBody aiOpenAIRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-ai-key" {
			t.Errorf("authorization = %q", got)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello from mock"}}]}`)
	}))
	defer upstream.Close()
	configureAI(t, api, upstream.URL+"/v1", "test-ai-key", "test-model")

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, TerminalSessionID: "term-1", Message: "why is this node unhealthy?",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["session_id"] == "" || body["message"] != "hello from mock" {
		t.Fatalf("response = %v", body)
	}
	if upstreamBody.Model != "test-model" || len(upstreamBody.Messages) < 2 {
		t.Fatalf("upstream request = %+v", upstreamBody)
	}
	if upstreamBody.Messages[0].Role != "system" || !strings.Contains(upstreamBody.Messages[0].Content, nodeID) {
		t.Fatalf("missing node context: %+v", upstreamBody.Messages[0])
	}
	if !strings.Contains(upstreamBody.Messages[0].Content, `"include_logs":false`) {
		t.Fatalf("unexpected log context: %s", upstreamBody.Messages[0].Content)
	}
	if upstreamBody.Messages[len(upstreamBody.Messages)-1].Content != "why is this node unhealthy?" {
		t.Fatalf("last message = %+v", upstreamBody.Messages[len(upstreamBody.Messages)-1])
	}

	sessionID := body["session_id"].(string)
	messages, err := api.Store.ListAIMessages(sessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Fatalf("persisted messages = %+v", messages)
	}
}

func TestAIRiskyAndKillSwitchNeverExecute(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-node-safety")
	cookie := loginCookie(t, server.URL)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"run_shell","arguments":"{\"command\":\"reboot\",\"reason\":\"restart required\",\"risky\":true}"}}]}}]}`)
	}))
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "test-ai-key", "test-model")

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "reboot it"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("risky status = %d (%v)", resp.StatusCode, body)
	}
	tool, ok := body["tool_call"].(map[string]any)
	if !ok || tool["status"] != "needs_confirmation" || tool["action_id"] == "" {
		t.Fatalf("risky tool call = %v", body["tool_call"])
	}
	commands, err := api.Store.ListCommands(nodeID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("risky action executed commands = %+v", commands)
	}
	audit, err := api.Store.ListAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	foundAudit := false
	for _, entry := range audit {
		if entry.Actor == "ai" && entry.AISessionID == body["session_id"] && entry.Reason == "restart required" && entry.Risk == "risky" {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Fatalf("AI confirmation audit missing: %+v", audit)
	}

	if err := api.Store.SetSetting("ai.kill_switch", "1", false); err != nil {
		t.Fatal(err)
	}
	resp2, body2 := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, SessionID: body["session_id"].(string), Message: "reboot it again",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("kill switch status = %d (%v)", resp2.StatusCode, body2)
	}
	tool2, ok := body2["tool_call"].(map[string]any)
	if !ok || tool2["status"] != "needs_confirmation" || tool2["action_id"] == "" {
		t.Fatalf("kill switch tool call = %v", body2["tool_call"])
	}
	commands, err = api.Store.ListCommands(nodeID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("kill switch executed commands = %+v", commands)
	}
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

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "hello"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || errorCode(body) != "ai_upstream_error" {
		t.Fatalf("response = %d %v", resp.StatusCode, body)
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

	// Partially configured (no api_key) must stay false: it is exactly the
	// state in which POST /api/ai/chat answers 503, and the terminal page
	// hides the sidebar off this flag.
	configureAI(t, api, "https://ai.example.com/v1", "test-ai-key", "gpt-test")
	if err := api.Store.SetSetting("ai.api_key", "", false); err != nil {
		t.Fatal(err)
	}
	if read() {
		t.Fatal("ai_configured = true without an api_key")
	}

	configureAI(t, api, "https://ai.example.com/v1", "test-ai-key", "gpt-test")
	if !read() {
		t.Fatal("ai_configured = false after base_url/api_key/model were stored")
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

func configureAI(t *testing.T, api *Server, baseURL, key, model string) {
	t.Helper()
	ciphertext, err := api.Crypt.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	for setting, item := range map[string]struct {
		value     string
		encrypted bool
	}{
		"ai.base_url": {value: baseURL, encrypted: false},
		"ai.api_key":  {value: ciphertext, encrypted: true},
		"ai.model":    {value: model, encrypted: false},
	} {
		if err := api.Store.SetSetting(setting, item.value, item.encrypted); err != nil {
			t.Fatal(err)
		}
	}
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

func postAIChat(t *testing.T, baseURL, cookie string, request aiChatRequest) (*http.Response, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/ai/chat", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	return resp, body
}

func errorCode(body map[string]any) string {
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errorBody["code"].(string)
	return code
}

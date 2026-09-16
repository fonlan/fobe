package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/store"
)

// upstreamAI returns a mock OpenAI-compatible upstream that answers with a
// canned body and records every request it receives.
func upstreamAI(t *testing.T, canned string, captured *[]aiOpenAIRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			var body aiOpenAIRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upstream body: %v", err)
			}
			*captured = append(*captured, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, canned)
	}))
}

// chatToolCall builds an upstream reply carrying one OpenAI tool_call.
func chatToolCall(id, name, arguments string) string {
	reply := map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": "",
				"tool_calls": []map[string]any{{
					"id":       id,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": arguments},
				}},
			},
		}},
	}
	raw, _ := json.Marshal(reply)
	return string(raw)
}

// finishTailLogsAsync simulates the agent answering a queued tail_logs
// command. It runs on its own goroutine (the chat handler blocks on the
// result) and reports the outcome on the returned channel.
func finishTailLogsAsync(api *Server, nodeID, stdout string) <-chan error {
	done := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			cmds, err := api.Store.ListCommands(nodeID, 50)
			if err != nil {
				done <- err
				return
			}
			for _, c := range cmds {
				if c.Kind != "tail_logs" || c.Status != "pending" {
					continue
				}
				result, _ := json.Marshal(protocol.CmdResult{ID: c.ID, ExitCode: 0, Stdout: stdout})
				if err := api.Store.FinishCommand(c.ID, "ok", string(result)); err != nil {
					done <- err
					return
				}
				done <- nil
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		done <- errors.New("tail_logs command never appeared in the queue")
	}()
	return done
}

func nodeCommandCount(t *testing.T, api *Server, nodeID string) int {
	t.Helper()
	cmds, err := api.Store.ListCommands(nodeID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(cmds)
}

func TestAIMetaToolsForceConfirmation(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		arguments  string
		kind       string
		payloadHas string
	}{
		{
			name:       "install_singbox",
			tool:       "install_singbox",
			arguments:  `{"version":"1.10.0","reason":"upgrade attempt"}`,
			kind:       "install_singbox",
			payloadHas: `"version":"1.10.0"`,
		},
		{
			name:       "set_singbox_port",
			tool:       "set_singbox_port",
			arguments:  `{"port":24443,"reason":"port move"}`,
			kind:       "set_singbox_port",
			payloadHas: `"port":24443`,
		},
		{
			name:       "set_anytls_password",
			tool:       "set_anytls_password",
			arguments:  `{"password":"super-secret-pw","reason":"rotation"}`,
			kind:       "set_anytls_password",
			payloadHas: `"password_encrypted"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, api := newTestServer(t)
			defer server.Close()
			nodeID := createAINode(t, api, "ai-meta-"+test.tool)
			cookie := loginCookie(t, server.URL)
			upstream := upstreamAI(t, chatToolCall("call-meta", test.tool, test.arguments), nil)
			defer upstream.Close()
			configureAI(t, api, upstream.URL, "k", "m")

			resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "do it"})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			tool, ok := body["tool_call"].(map[string]any)
			if !ok || tool["status"] != "needs_confirmation" || tool["action_id"] == "" {
				t.Fatalf("tool_call = %v", body["tool_call"])
			}

			// nothing may enter the commands queue before confirmation
			if n := nodeCommandCount(t, api, nodeID); n != 0 {
				t.Fatalf("commands queued = %d, want 0", n)
			}
			action, err := api.Store.GetAIPendingAction(tool["action_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if action.Kind != test.kind || action.Status != "pending" || action.Risk != "forced" {
				t.Fatalf("pending action = %+v", action)
			}
			if !strings.Contains(action.Payload, test.payloadHas) {
				t.Fatalf("payload %q missing %q", action.Payload, test.payloadHas)
			}
			if test.kind == "set_anytls_password" && strings.Contains(action.Payload, "super-secret-pw") {
				t.Fatalf("plaintext password leaked into pending action: %q", action.Payload)
			}
			audit, err := api.Store.ListAudit(50)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range audit {
				if entry.Actor == "ai" && entry.Action == "ai_action_confirmation_required" && entry.Risk == "forced" {
					found = true
				}
				if test.kind == "set_anytls_password" && strings.Contains(entry.Command, "super-secret-pw") {
					t.Fatalf("plaintext password in audit entry: %+v", entry)
				}
			}
			if !found {
				t.Fatalf("forced confirmation audit missing: %+v", audit)
			}
		})
	}
}

func TestAIInstallSingboxInvalidVersionRejected(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-install-invalid")
	cookie := loginCookie(t, server.URL)
	upstream := upstreamAI(t, chatToolCall("call-bad", "install_singbox", `{"version":"bad version!"}`), nil)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "install"})
	defer resp.Body.Close()
	tool, ok := body["tool_call"].(map[string]any)
	if !ok || tool["status"] != "invalid" {
		t.Fatalf("tool_call = %v, want invalid", body["tool_call"])
	}
	if n := nodeCommandCount(t, api, nodeID); n != 0 {
		t.Fatalf("commands queued = %d, want 0", n)
	}
}

func TestAIRestartSingboxQueuesCommand(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-restart")
	cookie := loginCookie(t, server.URL)
	upstream := upstreamAI(t, chatToolCall("call-r", "restart_singbox", `{"reason":"config apply"}`), nil)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")

	// without a singbox desired state the action is refused up front
	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "restart"})
	defer resp.Body.Close()
	tool := body["tool_call"].(map[string]any)
	if tool["status"] != "blocked" || tool["reason"] != "singbox_not_installed" {
		t.Fatalf("tool_call = %v, want blocked/singbox_not_installed", tool)
	}

	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: nodeID, DesiredVersion: "1.10.0", Status: "absent",
	}); err != nil {
		t.Fatal(err)
	}
	resp2, body2 := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, SessionID: body["session_id"].(string), Message: "restart again",
	})
	defer resp2.Body.Close()
	tool2 := body2["tool_call"].(map[string]any)
	if tool2["status"] != "queued" || tool2["command_id"] == "" {
		t.Fatalf("tool_call = %v, want queued", tool2)
	}
	cmds, err := api.Store.ListCommands(nodeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Kind != "restart_singbox" || cmds[0].Actor != "ai" || cmds[0].Payload != "{}" {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestAIStructuredContentSetPort(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-structured")
	cookie := loginCookie(t, server.URL)
	// model answers with structured JSON instead of tool_calls
	upstream := upstreamAI(t,
		`{"choices":[{"message":{"content":"{\"tool\":\"set_singbox_port\",\"arguments\":{\"port\":70000}}"}}]}`, nil)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "move the port"})
	defer resp.Body.Close()
	tool := body["tool_call"].(map[string]any)
	if tool["status"] != "invalid" { // 70000 is outside the valid port range
		t.Fatalf("tool_call = %v, want invalid", tool)
	}

	// valid port, nested tool_call shape this time
	upstream2 := upstreamAI(t,
		`{"choices":[{"message":{"content":"{\"tool_call\":{\"name\":\"set_singbox_port\",\"arguments\":{\"port\":24443}}}"}}]}`, nil)
	defer upstream2.Close()
	configureAI(t, api, upstream2.URL, "k", "m")
	resp2, body2 := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, SessionID: body["session_id"].(string), Message: "move the port for real",
	})
	defer resp2.Body.Close()
	tool2 := body2["tool_call"].(map[string]any)
	if tool2["status"] != "needs_confirmation" || tool2["action_id"] == "" {
		t.Fatalf("tool_call = %v, want needs_confirmation", tool2)
	}
	if n := nodeCommandCount(t, api, nodeID); n != 0 {
		t.Fatalf("commands queued = %d, want 0", n)
	}
}

func TestAIIncludeLogsInjectsTailLogs(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-includelogs")
	cookie := loginCookie(t, server.URL)

	var captured []aiOpenAIRequest
	upstream := upstreamAI(t, `{"choices":[{"message":{"content":"logs received"}}]}`, &captured)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")

	oldWait := aiTailLogsWait
	aiTailLogsWait = 3 * time.Second
	defer func() { aiTailLogsWait = oldWait }()

	agentDone := finishTailLogsAsync(api, nodeID, "fobe-tail-marker-42 ERROR sing-box exited\n")

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "why did sing-box die?", IncludeLogs: true,
	})
	defer resp.Body.Close()
	if err := <-agentDone; err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || body["message"] != "logs received" {
		t.Fatalf("response = %d %v", resp.StatusCode, body)
	}
	if len(captured) == 0 {
		t.Fatal("upstream never called")
	}
	system := captured[0].Messages[0].Content
	if !strings.Contains(system, `"node_logs"`) || !strings.Contains(system, "fobe-tail-marker-42") {
		t.Fatalf("system context missing log section: %s", system)
	}
	// the read-only fetch went through the commands queue with actor=ai
	cmds, err := api.Store.ListCommands(nodeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Kind != "tail_logs" || cmds[0].Actor != "ai" {
		t.Fatalf("commands = %+v", cmds)
	}
}

func TestAITailLogsToolCallReturnsStdout(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-taillogs")
	cookie := loginCookie(t, server.URL)
	upstream := upstreamAI(t, chatToolCall("call-tail", "tail_logs", `{"lines":5000}`), nil)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")

	oldWait := aiTailLogsWait
	aiTailLogsWait = 3 * time.Second
	defer func() { aiTailLogsWait = oldWait }()

	agentDone := finishTailLogsAsync(api, nodeID, "fobe-toolcall-marker-7\n")

	resp, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "show me the logs"})
	defer resp.Body.Close()
	if err := <-agentDone; err != nil {
		t.Fatal(err)
	}
	tool := body["tool_call"].(map[string]any)
	if tool["status"] != "ok" {
		t.Fatalf("tool_call = %v, want ok", tool)
	}
	if stdout, _ := tool["stdout"].(string); !strings.Contains(stdout, "fobe-toolcall-marker-7") {
		t.Fatalf("stdout = %v", tool["stdout"])
	}
	cmds, err := api.Store.ListCommands(nodeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 5000 requested → clamped to the 500-line cap in the queued payload
	if len(cmds) != 1 || cmds[0].Payload != `{"lines":500}` {
		t.Fatalf("commands = %+v", cmds)
	}
	sessionID := body["session_id"].(string)
	messages, err := api.Store.ListAIMessages(sessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	hasTool := false
	for _, m := range messages {
		if m.Role == "tool" && strings.Contains(m.Content, "fobe-toolcall-marker-7") {
			hasTool = true
		}
	}
	if !hasTool {
		t.Fatalf("tool message not persisted: %+v", messages)
	}
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
	var captured []aiOpenAIRequest
	upstream := upstreamAI(t, `{"choices":[{"message":{"content":"ok"}}]}`, &captured)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")

	resp, _ := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "overview please"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	system := captured[0].Messages[0].Content
	for _, want := range []string{
		`"nodes"`, `"current":true`, `"agent_version"`,
		`"latency"`, `"8.8.8.8"`, `"icmp_p95_ms":20`, `"tcp_p95_ms":22.5`,
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system context missing %s:\n%s", want, system)
		}
	}
}

func TestAIConfirmSetPortAppliesDesired(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-confirm-port")
	cookie := loginCookie(t, server.URL)
	upstream := upstreamAI(t, chatToolCall("call-port", "set_singbox_port", `{"port":24443}`), nil)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")
	password, err := api.Crypt.Encrypt("ctx-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetSetting("anytls_password", password, true); err != nil {
		t.Fatal(err)
	}

	_, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "change port"})
	actionID := body["tool_call"].(map[string]any)["action_id"].(string)

	confirmResp, confirmBody := postJSONCookie(t, server.URL, cookie, "POST", "/api/ai/actions/"+actionID+"/confirm", nil)
	defer confirmResp.Body.Close()
	if confirmResp.StatusCode != http.StatusOK {
		t.Fatalf("confirm status = %d (%v)", confirmResp.StatusCode, confirmBody)
	}
	if confirmBody["status"] != "applied" {
		t.Fatalf("confirm body = %v", confirmBody)
	}
	sb, err := api.Store.GetNodeSingbox(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Port != 24443 || sb.DesiredVersion != "" {
		t.Fatalf("node singbox = %+v", sb)
	}
	if cfg, err := api.Store.GetSetting("singbox_config:" + nodeID); err != nil || !strings.Contains(cfg, "24443") {
		t.Fatalf("singbox_config = %q err=%v", cfg, err)
	}
	if n := nodeCommandCount(t, api, nodeID); n != 0 {
		t.Fatalf("commands queued = %d, want 0", n)
	}
}

func TestAIConfirmAnytlsPasswordRepublishesConfig(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-confirm-pw")
	cookie := loginCookie(t, server.URL)
	upstream := upstreamAI(t, chatToolCall("call-pw", "set_anytls_password", `{"password":"brand-new-pw"}`), nil)
	defer upstream.Close()
	configureAI(t, api, upstream.URL, "k", "m")
	oldPW, err := api.Crypt.Encrypt("old-pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetSetting("anytls_password", oldPW, true); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: nodeID, DesiredVersion: "1.10.0", Port: 24443, Status: "running",
	}); err != nil {
		t.Fatal(err)
	}

	_, body := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "rotate the password"})
	actionID := body["tool_call"].(map[string]any)["action_id"].(string)

	confirmResp, confirmBody := postJSONCookie(t, server.URL, cookie, "POST", "/api/ai/actions/"+actionID+"/confirm", nil)
	defer confirmResp.Body.Close()
	if confirmResp.StatusCode != http.StatusOK || confirmBody["status"] != "applied" {
		t.Fatalf("confirm = %d (%v)", confirmResp.StatusCode, confirmBody)
	}
	stored, ok := api.GetDecryptedSetting("anytls_password")
	if !ok || stored != "brand-new-pw" {
		t.Fatalf("anytls_password = %q ok=%v", stored, ok)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + nodeID)
	if err != nil || !strings.Contains(cfg, "brand-new-pw") {
		t.Fatalf("regenerated config = %q err=%v", cfg, err)
	}
}

// postJSONCookie performs an authenticated JSON request and decodes the body.
func postJSONCookie(t *testing.T, baseURL, cookie, method, path string, payload any) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
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

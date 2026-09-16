package httpapi

// §21 end-to-end: panel HTTP → commands table → agent WS → nftables result →
// snapshot → panel HTTP again. The agent here is simulated (a WS client that
// answers nft_forwards with a canned ForwardsResult), which is what proves the
// transport contract; the real nft behaviour is covered by the agent's own
// kernel test (internal/agent/forwards_kernel_test.go).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/gorilla/websocket"
)

// forwardAgent is a minimal agent: registers, keeps one WSS session, and
// answers nft_forwards commands with a scripted result.
type forwardAgent struct {
	t    *testing.T
	ws   *websocket.Conn
	node string

	mu      sync.Mutex
	handler func(cmd protocol.Cmd) protocol.CmdResult
}

// setHandler swaps what the simulated probe answers; safe to call while the
// read loop is running.
func (a *forwardAgent) setHandler(h func(cmd protocol.Cmd) protocol.CmdResult) {
	a.mu.Lock()
	a.handler = h
	a.mu.Unlock()
}

// setReply scripts the probe's nft_forwards answer.
func (a *forwardAgent) setReply(r func(req protocol.ForwardsRequest) protocol.ForwardsResult) {
	a.setHandler(func(cmd protocol.Cmd) protocol.CmdResult {
		var req protocol.ForwardsRequest
		_ = json.Unmarshal(cmd.Payload, &req)
		out := r(req)
		raw, _ := json.Marshal(out)
		return protocol.CmdResult{ID: cmd.ID, Stdout: string(raw)}
	})
}

func startForwardAgent(t *testing.T, srv *httptest.Server, s *Server, cookie, name string,
	report *protocol.ForwardsState) *forwardAgent {
	t.Helper()
	token := freshToken(t, srv, cookie, name)
	_, reg := postJSON(t, http.DefaultClient, srv.URL+"/api/agent/register", map[string]any{
		"token": token, "machine_id": "machine-" + name, "hostname": name,
		"os": "linux", "arch": "amd64", "version": "dev", "tz": "UTC",
	})
	nodeID, _ := reg["node_id"].(string)
	secret, _ := reg["node_secret"].(string)
	if nodeID == "" || secret == "" {
		t.Fatalf("register %s: %v", name, reg)
	}
	agent := &forwardAgent{t: t, node: nodeID}
	// The scripted probe state: what the operator will see before any edit.
	agent.setReply(func(protocol.ForwardsRequest) protocol.ForwardsResult {
		return protocol.ForwardsResult{OK: true, State: *report}
	})

	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
	})
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	agent.ws = ws
	t.Cleanup(func() { ws.Close() })
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{
		MachineID: "machine-" + name, Hostname: name, Version: "dev", OS: "linux", Arch: "amd64", TZ: "UTC",
	})); err != nil {
		t.Fatal(err)
	}
	// hello_ack, then the state report that carries §21.
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{Forwards: report})); err != nil {
		t.Fatal(err)
	}
	go agent.serve()
	// Wait for the state frame to be persisted: the panel's first GET must see
	// `agent_supported: true`, not "never reported".
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Store.GetNodeForwardStatus(nodeID)
		if err == nil && st.ReportedAt > 0 {
			return agent
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent state report never landed")
	return nil
}

func (a *forwardAgent) serve() {
	for {
		var env protocol.Envelope
		if err := a.ws.ReadJSON(&env); err != nil {
			return
		}
		if env.Type != protocol.TypeCmd {
			continue
		}
		var cmd protocol.Cmd
		if err := json.Unmarshal(env.Payload, &cmd); err != nil || cmd.Kind != protocol.CmdKindNftForwards {
			continue
		}
		a.mu.Lock()
		handler := a.handler
		a.mu.Unlock()
		if handler == nil {
			continue
		}
		_ = a.ws.WriteJSON(protocol.NewEnvelope(protocol.TypeCmdResult, cmd.ID, handler(cmd)))
	}
}

// fwdDo sends one authed JSON request and decodes the response body.
func fwdDo(t *testing.T, method, url, cookie string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func forwardsList(t *testing.T, url, cookie string, out map[string]any) []map[string]any {
	t.Helper()
	raw, _ := out["forwards"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

func TestForwardsPanelFlow(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)

	// 1. The probe reports what nfpf.sh left on it.
	initial := protocol.ForwardsState{
		Supported: true, Initialized: true,
		Rules: []protocol.ForwardRule{
			{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Handle: 3},
			{Proto: "udp", SrcPort: 5353, DstIP: "10.0.0.2", DstPort: 5353, Handle: 4},
		},
	}
	agent := startForwardAgent(t, srv, api, cookie, "probe-a", &initial)

	// 2. The panel sees the external rules, tagged with their handles.
	resp, out := fwdDo(t, "GET", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET forwards: %d %v", resp.StatusCode, out)
	}
	if out["agent_supported"] != true || out["supported"] != true {
		t.Fatalf("status = %v", out)
	}
	rows := forwardsList(t, "", cookie, out)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0]["src_port"].(float64) != 8080 || rows[0]["iface"] != "eth0" || rows[0]["handle"].(float64) != 3 {
		t.Fatalf("nfpf rule misread by the panel: %v", rows[0])
	}

	// 3. Add: the agent applies and answers with the new set.
	var gotAdd protocol.ForwardsRequest
	agent.setReply(func(req protocol.ForwardsRequest) protocol.ForwardsResult {
		gotAdd = req
		st := initial
		st.Rules = append(append([]protocol.ForwardRule{}, initial.Rules...),
			protocol.ForwardRule{Proto: "tcp", SrcPort: 9090, DstIP: "10.0.0.9", DstPort: 9090, Handle: 7})
		return protocol.ForwardsResult{OK: true, State: st, Warnings: []string{"nftables_service_disabled: x"}}
	})
	resp, out = fwdDo(t, "POST", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, map[string]any{
		"rule": map[string]any{"proto": "tcp", "src_port": 9090, "dst_ip": "10.0.0.9", "dst_port": 9090,
			"comment": "面板备注"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("POST forwards: %d %v", resp.StatusCode, out)
	}
	if gotAdd.Action != protocol.ForwardsAdd || gotAdd.Rule == nil || gotAdd.Rule.SrcPort != 9090 {
		t.Fatalf("agent got %+v", gotAdd)
	}
	// The comment rides with the rule to the probe (nfpf.sh's own position).
	if gotAdd.Rule.Comment != "面板备注" {
		t.Fatalf("comment not forwarded: %+v", gotAdd.Rule)
	}
	if out["live"] != true || out["queued"] != false {
		t.Fatalf("live/queued = %v/%v", out["live"], out["queued"])
	}
	if rows = forwardsList(t, "", cookie, out); len(rows) != 3 {
		t.Fatalf("rows after add = %v", rows)
	}
	if warn, _ := out["warnings"].([]any); len(warn) != 1 {
		t.Fatalf("warnings = %v", out["warnings"])
	}
	// The snapshot is persisted: a plain GET agrees without another round trip.
	_, out = fwdDo(t, "GET", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, nil)
	if rows = forwardsList(t, "", cookie, out); len(rows) != 3 {
		t.Fatalf("stored snapshot = %v", rows)
	}
	if out["live"] != false {
		t.Fatalf("a plain GET should not be live: %v", out["live"])
	}

	// 3b. A probe that applies the rule but drops the note (an agent from before
	// §21 comment support) must say so: the panel renders warnings as hints.
	agent.setReply(func(req protocol.ForwardsRequest) protocol.ForwardsResult {
		st := initial
		st.Rules = append(append([]protocol.ForwardRule{}, initial.Rules...), protocol.ForwardRule{
			Proto: "tcp", SrcPort: 7070, DstIP: "10.0.0.4", DstPort: 7070, Handle: 8,
		}) // note deliberately missing
		return protocol.ForwardsResult{OK: true, State: st, Warnings: []string{"comment_not_applied"}}
	})
	_, out = fwdDo(t, "POST", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, map[string]any{
		"rule": map[string]any{"proto": "tcp", "src_port": 7070, "dst_ip": "10.0.0.4", "dst_port": 7070,
			"comment": "带备注"},
	})
	warns, _ := out["warnings"].([]any)
	if len(warns) != 1 || warns[0] != "comment_not_applied" {
		t.Fatalf("warnings = %v, want comment_not_applied", out["warnings"])
	}

	// 4. Update: the panel sends the old rule (handle included) and the new one.
	var gotUpd protocol.ForwardsRequest
	agent.setReply(func(req protocol.ForwardsRequest) protocol.ForwardsResult {
		gotUpd = req
		return protocol.ForwardsResult{OK: true, State: initial}
	})
	resp, out = fwdDo(t, "PUT", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, map[string]any{
		"old":  map[string]any{"proto": "tcp", "src_port": 8080, "iface": "eth0", "dst_ip": "10.0.0.1", "dst_port": 80, "handle": 3},
		"rule": map[string]any{"proto": "tcp", "src_port": 8081, "iface": "eth0", "dst_ip": "10.0.0.1", "dst_port": 80},
	})
	if resp.StatusCode != 200 || gotUpd.Action != protocol.ForwardsUpdate {
		t.Fatalf("PUT: %d %v / agent %+v", resp.StatusCode, out, gotUpd)
	}
	if gotUpd.Old == nil || gotUpd.Old.Handle != 3 || gotUpd.Rule.SrcPort != 8081 {
		t.Fatalf("agent got %+v", gotUpd)
	}

	// 5. Delete.
	var gotDel protocol.ForwardsRequest
	agent.setReply(func(req protocol.ForwardsRequest) protocol.ForwardsResult {
		gotDel = req
		return protocol.ForwardsResult{OK: true, State: protocol.ForwardsState{Supported: true, Initialized: true,
			Rules: []protocol.ForwardRule{initial.Rules[1]}}}
	})
	resp, out = fwdDo(t, "DELETE", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, map[string]any{
		"rule": map[string]any{"proto": "tcp", "src_port": 8080, "iface": "eth0", "dst_ip": "10.0.0.1", "dst_port": 80, "handle": 3},
	})
	if resp.StatusCode != 200 || gotDel.Action != protocol.ForwardsDelete {
		t.Fatalf("DELETE: %d %v / agent %+v", resp.StatusCode, out, gotDel)
	}
	if rows = forwardsList(t, "", cookie, out); len(rows) != 1 {
		t.Fatalf("rows after delete = %v", rows)
	}
}

func TestForwardsPanelErrors(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	initial := protocol.ForwardsState{Supported: true, Initialized: true,
		Rules: []protocol.ForwardRule{{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80, Handle: 3}}}
	agent := startForwardAgent(t, srv, api, cookie, "probe-b", &initial)

	body := map[string]any{"rule": map[string]any{"proto": "tcp", "src_port": 8080, "dst_ip": "10.0.0.1", "dst_port": 80}}

	// The agent refuses: its token must reach the panel as a stable code.
	cases := []struct {
		agentCode string
		want      string
		status    int
	}{
		{"conflict", "forward_conflict", http.StatusConflict},
		{"not_found", "forward_not_found", http.StatusNotFound},
		{"ambiguous", "forward_ambiguous", http.StatusConflict},
		{"bad_port", "bad_forward", http.StatusBadRequest},
		{"need_root", "forward_need_root", http.StatusBadRequest},
		{"nft_missing", "forward_nft_missing", http.StatusBadRequest},
		{"chain_mismatch", "forward_chain_mismatch", http.StatusBadRequest},
		{"nft_failed", "forward_failed", http.StatusBadGateway},
	}
	for _, tc := range cases {
		agent.setReply(func(protocol.ForwardsRequest) protocol.ForwardsResult {
			return protocol.ForwardsResult{Error: tc.agentCode, Message: "x", State: initial}
		})
		resp, out := fwdDo(t, "POST", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, body)
		code, _ := out["error"].(map[string]any)["code"].(string)
		if resp.StatusCode != tc.status || code != tc.want {
			t.Errorf("%s: got %d %q, want %d %q", tc.agentCode, resp.StatusCode, code, tc.status, tc.want)
		}
	}

	// An agent that predates §21 answers every unknown kind the same way.
	agent.setHandler(func(cmd protocol.Cmd) protocol.CmdResult {
		return protocol.CmdResult{ID: cmd.ID, Error: "unsupported command kind: " + cmd.Kind}
	})
	resp, out := fwdDo(t, "POST", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, body)
	code, _ := out["error"].(map[string]any)["code"].(string)
	if resp.StatusCode != http.StatusBadRequest || code != "forward_agent_unsupported" {
		t.Fatalf("old agent: %d %v", resp.StatusCode, out)
	}

	// Server-side validation rejects garbage before it reaches the queue.
	for _, bad := range []map[string]any{
		{"rule": map[string]any{"proto": "sctp", "src_port": 1, "dst_ip": "10.0.0.1", "dst_port": 1}},
		{"rule": map[string]any{"proto": "tcp", "src_port": 0, "dst_ip": "10.0.0.1", "dst_port": 1}},
		{"rule": map[string]any{"proto": "tcp", "src_port": 1, "dst_ip": "2001:db8::1", "dst_port": 1}},
		{"rule": map[string]any{"proto": "tcp", "src_port": 1, "dst_ip": "nope", "dst_port": 1}},
	} {
		resp, out := fwdDo(t, "POST", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, bad)
		if code, _ := out["error"].(map[string]any)["code"].(string); resp.StatusCode != http.StatusBadRequest || code != "bad_forward" {
			t.Errorf("bad rule %v: %d %v", bad, resp.StatusCode, out)
		}
	}

	// A quote in a comment is unwritable in nft's string syntax: refused here,
	// with its own code, before it ever reaches the queue.
	resp, out = fwdDo(t, "POST", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, map[string]any{
		"rule": map[string]any{"proto": "tcp", "src_port": 1234, "dst_ip": "10.0.0.1", "dst_port": 80,
			"comment": `a "b"`},
	})
	if code, _ := out["error"].(map[string]any)["code"].(string); resp.StatusCode != http.StatusBadRequest || code != "bad_comment" {
		t.Fatalf("quoted comment: %d %v", resp.StatusCode, out)
	}

	// Unknown node.
	resp, out = fwdDo(t, "GET", srv.URL+"/api/nodes/nope/forwards", cookie, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown node: %d %v", resp.StatusCode, out)
	}
}

func TestForwardsPanelOfflineProbe(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	agent := startForwardAgent(t, srv, api, cookie, "probe-c", &protocol.ForwardsState{Supported: true})
	agent.ws.Close() // probe goes away: the command is queued, not lost
	waitOffline(t, api, agent.node)

	resp, out := fwdDo(t, "POST", srv.URL+"/api/nodes/"+agent.node+"/forwards", cookie, map[string]any{
		"rule": map[string]any{"proto": "tcp", "src_port": 1234, "dst_ip": "10.0.0.1", "dst_port": 80},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("offline add: %d %v", resp.StatusCode, out)
	}
	if out["queued"] != true || out["live"] != false {
		t.Fatalf("queued/live = %v/%v", out["queued"], out["live"])
	}
	// The command is in the queue with its TTL, and the panel still shows the
	// last known rules (none here) instead of an error.
	cmds, err := api.Store.ListCommands(agent.node, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Kind != protocol.CmdKindNftForwards || cmds[0].Status != "pending" {
		t.Fatalf("queue = %+v", cmds)
	}
}

func waitOffline(t *testing.T, api *Server, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !api.Hub.IsOnline(nodeID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("probe never went offline")
}

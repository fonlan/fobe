package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/agentupdate"
	"github.com/gorilla/websocket"
)

func authedGet(t *testing.T, srv *httptest.Server, cookie, path string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func authedPost(t *testing.T, srv *httptest.Server, cookie, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

// registerNode runs the add-node flow far enough to have a usable node.
func registerNode(t *testing.T, srv *httptest.Server, cookie, name, machineID string) (nodeID, secret string) {
	t.Helper()
	_, reg := postJSON(t, &http.Client{}, srv.URL+"/api/agent/register", map[string]any{
		"token": freshToken(t, srv, cookie, name), "machine_id": machineID, "hostname": name,
		"os": "linux", "arch": "amd64", "version": "old", "tz": "UTC", "cpu_cores": 2,
	})
	nodeID, _ = reg["node_id"].(string)
	secret, _ = reg["node_secret"].(string)
	if nodeID == "" || secret == "" {
		t.Fatalf("register failed: %v", reg)
	}
	return nodeID, secret
}

// agentHello dials /ws/agent, sends one hello and returns the hello_ack (plus
// the open socket so the caller can keep reading).
func agentHello(t *testing.T, srv *httptest.Server, nodeID, secret string, hello protocol.Hello) (protocol.HelloAck, *websocket.Conn) {
	t.Helper()
	hdr := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
	}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", hello)); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var env protocol.Envelope
	if err := ws.ReadJSON(&env); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	if env.Type != protocol.TypeHelloAck {
		t.Fatalf("first frame = %q, want hello_ack", env.Type)
	}
	var ack protocol.HelloAck
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	return ack, ws
}

// wireAgentUpdate installs a §5.5 manager on the test server with a release
// version and a planted artifact.
func wireAgentUpdate(t *testing.T, api *Server, version string) *agentupdate.Manager {
	t.Helper()
	dl := t.TempDir()
	dir := filepath.Join(dl, "agent", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "linux-amd64"), []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "linux-amd64.sha256"), []byte("sum"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := agentupdate.New(agentupdate.Config{
		Store: api.Store, Log: api.Log, ServerVersion: version, DLDir: dl,
		// wired exactly like cmd/server, so the panel switch is under test too
		Enabled: func() bool { return agentupdate.AutoUpdateEnabled(api.Store) },
	})
	api.AgentUpdate = m
	api.Hub.SetAgentUpdater(m)
	return m
}

func TestAgentUpdateStatusEndpoint(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	registerNode(t, srv, cookie, "probe-status", "m-status")

	resp, out := authedGet(t, srv, cookie, "/api/agent/update")
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %v", resp.StatusCode, out)
	}
	status, _ := out["status"].(map[string]any)
	if status == nil {
		t.Fatalf("no status in %v", out)
	}
	if status["enabled"] != true || status["server_version"] != "20260915.000000" {
		t.Fatalf("status = %v", status)
	}
	if status["nodes_behind"] != float64(1) {
		t.Fatalf("nodes_behind = %v, want 1", status["nodes_behind"])
	}
}

// With no manager wired the endpoint must explain itself instead of failing.
func TestAgentUpdateStatusWithoutManager(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	resp, out := authedGet(t, srv, cookie, "/api/agent/update")
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	status, _ := out["status"].(map[string]any)
	if status["enabled"] != false || status["reason"] != "not_wired" {
		t.Fatalf("status = %v", status)
	}
}

// The hello_ack is the only place a probe learns its target, and the flat
// fields must agree with the desired-state copy (§7/§5.5).
func TestHelloAckCarriesAgentTargetAndStagger(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	nodeID, secret := registerNode(t, srv, cookie, "probe-target", "m-target")

	ack, ws := agentHello(t, srv, nodeID, secret, protocol.Hello{
		MachineID: "m-target", Version: "old", OS: "linux", Arch: "amd64",
	})
	defer ws.Close()

	if ack.AgentTargetVersion != "20260915.000000" {
		t.Fatalf("ack target = %q", ack.AgentTargetVersion)
	}
	if ack.Desired.AgentTargetVersion != ack.AgentTargetVersion || ack.Desired.AgentUpdateAfter != ack.AgentUpdateAfter {
		t.Fatalf("carriers disagree: flat=%q/%d desired=%q/%d",
			ack.AgentTargetVersion, ack.AgentUpdateAfter,
			ack.Desired.AgentTargetVersion, ack.Desired.AgentUpdateAfter)
	}
	if ack.AgentUpdateAfter == 0 {
		t.Fatal("the stagger deadline must be set: without it every probe starts at once")
	}
	// The plan is visible to the panel.
	n, err := api.Store.GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.AgentTargetVersion != "20260915.000000" || n.AgentUpdatePlannedAt == 0 {
		t.Fatalf("plan not recorded: %+v", n)
	}
}

// A dev/compose server has nothing to hand out; offering a target anyway would
// turn every probe into a 404 retry loop.
// A probe that converged by reinstalling itself (§5.5 存量探针路径) reports its
// new build in the hello that follows — which arrives *after* hello_ack was
// already assembled against the previous version. Without a reconcile the panel
// keeps the old binary's verdict ("unsupported" + its reason) until the node's
// next reconnect, which reads exactly like "the reinstall did nothing".
func TestHelloReconcilesStaleAgentUpdateState(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	nodeID, secret := registerNode(t, srv, cookie, "probe-reconcile", "m-reconcile")

	// What the panel looked like right before the reinstall: the old fallback
	// verdict plus the plan that was handed out to it.
	if err := api.Store.SetAgentUpdatePlanned(nodeID, "20260915.000000", 111); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.RecordAgentUpdate(nodeID, "20260915.000000", "unsupported", 0,
		"no service manager to restart the agent", 0); err != nil {
		t.Fatal(err)
	}

	ack, ws := agentHello(t, srv, nodeID, secret, protocol.Hello{
		MachineID: "m-reconcile", Version: "20260915.000000", OS: "linux", Arch: "amd64",
		Caps: protocol.Caps{Systemd: true, SelfUpdate: true},
	})
	defer ws.Close()
	// The ack is assembled before this hello is read, so it may still name the
	// version the node is *already* running — the agent treats target==own
	// build as "nothing to do", which is why that is harmless. What must not
	// survive is the stored verdict.
	_ = ack

	// The hello is handled by the read pump, so the reconcile lands asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		n, err := api.Store.GetNode(nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if n.AgentUpdateState == "committed" {
			if n.AgentUpdateError != "" || n.AgentUpdatePlannedAt != 0 || n.AgentUpdateDoneAt == 0 {
				t.Fatalf("stale bookkeeping survived: %+v", n)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale verdict survived the hello: %+v", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHelloAckOmitsTargetForNonReleaseVersion(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "dev")
	nodeID, secret := registerNode(t, srv, cookie, "probe-dev", "m-dev")

	ack, ws := agentHello(t, srv, nodeID, secret, protocol.Hello{
		MachineID: "m-dev", Version: "old", OS: "linux", Arch: "amd64",
	})
	defer ws.Close()
	if ack.AgentTargetVersion != "" || ack.Desired.AgentTargetVersion != "" {
		t.Fatalf("non-release server offered a target: %q", ack.AgentTargetVersion)
	}
}

// The §5.5 bypass handshake must not register the connection, must not close
// the live one, and must not write a version the probe is not running.
func TestSelfCheckHandshakeDoesNotTouchTheLiveAgent(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	nodeID, secret := registerNode(t, srv, cookie, "probe-selfcheck", "m-selfcheck")

	// A healthy agent is connected and has said hello.
	_, live := agentHello(t, srv, nodeID, secret, protocol.Hello{
		MachineID: "m-selfcheck", Version: "old", OS: "linux", Arch: "amd64",
	})
	defer live.Close()

	// Now the freshly downloaded candidate checks itself in.
	hdr := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
		"X-Fobe-Selfcheck":   {"1"},
	}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	check, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("self-check dial: %v", err)
	}
	defer check.Close()
	if err := check.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{
		MachineID: "m-selfcheck", Version: "20260915.000000", OS: "linux", Arch: "amd64",
		SelfCheck: true,
	})); err != nil {
		t.Fatal(err)
	}
	check.SetReadDeadline(time.Now().Add(5 * time.Second))
	var env protocol.Envelope
	if err := check.ReadJSON(&env); err != nil {
		t.Fatalf("self-check read: %v", err)
	}
	var ack protocol.HelloAck
	_ = json.Unmarshal(env.Payload, &ack)
	if ack.AgentTargetVersion != "20260915.000000" {
		t.Fatalf("self-check ack target = %q, want the server version", ack.AgentTargetVersion)
	}

	// The live socket must still be the registered one: the self-check did not
	// replace it, so the agent is not kicked offline by its own update.
	if !api.Hub.IsOnline(nodeID) {
		t.Fatal("self-check knocked the live agent offline")
	}
	live.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var probe protocol.Envelope
	if err := live.ReadJSON(&probe); err == nil {
		t.Fatalf("live connection unexpectedly received %q", probe.Type)
	}

	// The version written by a self-checking binary must not leak into the node.
	n, err := api.Store.GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.AgentVersion != "old" {
		t.Fatalf("agent_version = %q: the panel would claim an update that never committed", n.AgentVersion)
	}
}

// Retry unlocks a circuit-broken probe and nudges it without waiting for the
// next handshake.
func TestAgentUpdateRetryUnlocksAndPushes(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	m := wireAgentUpdate(t, api, "20260915.000000")
	nodeID, secret := registerNode(t, srv, cookie, "probe-retry", "m-retry")

	if err := api.Store.RecordAgentUpdate(nodeID, "20260915.000000", "suppressed", 3, "gave up", 0); err != nil {
		t.Fatal(err)
	}
	_ = m

	_, live := agentHello(t, srv, nodeID, secret, protocol.Hello{
		MachineID: "m-retry", Version: "old", OS: "linux", Arch: "amd64",
	})
	defer live.Close()

	resp, out := authedPost(t, srv, cookie, "/api/nodes/"+nodeID+"/agent/retry", map[string]any{})
	if resp.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("retry: %d %v", resp.StatusCode, out)
	}
	n, _ := api.Store.GetNode(nodeID)
	if n.AgentUpdateAttempts != 0 || n.AgentUpdateError != "" {
		t.Fatalf("retry did not clear the counters: %+v", n)
	}
	if n.AgentUpdatePlannedAt == 0 {
		t.Fatal("retry did not re-anchor the plan")
	}

	// the online agent gets the fresh plan as a desired frame
	live.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var env protocol.Envelope
		if err := live.ReadJSON(&env); err != nil {
			t.Fatalf("no desired frame after retry: %v", err)
		}
		if env.Type != protocol.TypeDesired {
			continue
		}
		var desired protocol.DesiredState
		if err := json.Unmarshal(env.Payload, &desired); err != nil {
			t.Fatal(err)
		}
		if desired.AgentTargetVersion != "20260915.000000" {
			t.Fatalf("desired target = %q", desired.AgentTargetVersion)
		}
		// The push must carry a *fresh* plan deadline: that re-anchoring is the
		// only signal the probe has to clear its own attempt budget.
		if desired.AgentUpdateAfter == 0 {
			t.Fatal("retry push carried no plan deadline: the probe would stay circuit-broken")
		}
		break
	}
}

func TestAgentUpdateRetryUnknownNode(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	resp, out := authedPost(t, srv, cookie, "/api/nodes/nope/agent/retry", map[string]any{})
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d %v", resp.StatusCode, out)
	}
}

// A probe whose binary predates §5.5 needs one manual reinstall; the panel gets
// a ready-to-paste command and the server records a fresh single-use token.
func TestAgentReinstallCommand(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	nodeID, _ := registerNode(t, srv, cookie, "probe-old", "m-old")
	if err := api.Store.SetSetting("server.public_url", "https://panel.example.com", false); err != nil {
		t.Fatal(err)
	}

	resp, out := authedPost(t, srv, cookie, "/api/nodes/"+nodeID+"/agent/reinstall-command", map[string]any{})
	if resp.StatusCode != 200 {
		t.Fatalf("reinstall: %d %v", resp.StatusCode, out)
	}
	cmd, _ := out["install_command"].(string)
	if !strings.Contains(cmd, "https://panel.example.com/install.sh") || !strings.Contains(cmd, "--token") {
		t.Fatalf("unusable install command: %q", cmd)
	}
	tokens, err := api.Store.ListRegTokens(true)
	if err != nil || len(tokens) == 0 {
		t.Fatalf("no registration token was minted: %v %v", tokens, err)
	}
	if tokens[0].Name != "probe-old" {
		t.Fatalf("token name = %q, want the node name so the rebind is obvious", tokens[0].Name)
	}
}

// The panel decides "needs a reinstall" from the capability bit, never from a
// version guess.
func TestNodeViewExposesSelfUpdateCapability(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	nodeID, _ := registerNode(t, srv, cookie, "probe-caps", "m-caps")

	// old agent: no capability bit
	_, out := authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	node, _ := out["node"].(map[string]any)
	if node["agent_self_update"] != false {
		t.Fatalf("agent_self_update = %v, want false for a pre-§5.5 agent", node["agent_self_update"])
	}

	// new agent: reports it in hello caps
	caps, _ := json.Marshal(protocol.Caps{SelfUpdate: true, Systemd: true})
	if err := api.Store.SetNodeCaps(nodeID, caps); err != nil {
		t.Fatal(err)
	}
	_, out2 := authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	node2, _ := out2["node"].(map[string]any)
	if node2["agent_self_update"] != true {
		t.Fatalf("agent_self_update = %v, want true after the capability bit is reported", node2["agent_self_update"])
	}
}

func TestSettingsAcceptAgentSwitchAndRejectGarbage(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	m := wireAgentUpdate(t, api, "20260915.000000")

	req, _ := http.NewRequest("PUT", srv.URL+"/api/settings",
		bytes.NewReader([]byte(`{"settings":{"agent.auto_update":"0"}}`)))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("put settings: %v %d", err, statusCode(resp))
	}
	resp.Body.Close()
	if got, _, _ := api.Store.GetSettingValue(agentupdate.SettingAutoUpdate); got != "0" {
		t.Fatalf("stored switch = %q, want 0", got)
	}
	if m.Status().Enabled {
		t.Fatal("the switch must actually disable the feature")
	}

	req2, _ := http.NewRequest("PUT", srv.URL+"/api/settings",
		bytes.NewReader([]byte(`{"settings":{"agent.auto_update":"maybe"}}`)))
	req2.Header.Set("Cookie", cookie)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage switch accepted: %v %d", err, statusCode(resp2))
	}
	resp2.Body.Close()
}

// A node that never said hello has no caps; the panel must not read that as
// "outdated binary" (§5.5).
func TestNodeViewDistinguishesNoCapsFromNoSupport(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	wireAgentUpdate(t, api, "20260915.000000")
	nodeID, _ := registerNode(t, srv, cookie, "probe-nocaps", "m-nocaps")

	_, out := authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	node, _ := out["node"].(map[string]any)
	if node["agent_caps_seen"] != false || node["agent_self_update"] != false {
		t.Fatalf("a fresh node must read as caps-unknown, got seen=%v supported=%v",
			node["agent_caps_seen"], node["agent_self_update"])
	}

	// An old agent that HAS handshaken reports caps without the bit.
	caps, _ := json.Marshal(protocol.Caps{Systemd: true})
	if err := api.Store.SetNodeCaps(nodeID, caps); err != nil {
		t.Fatal(err)
	}
	_, out2 := authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	node2, _ := out2["node"].(map[string]any)
	if node2["agent_caps_seen"] != true || node2["agent_self_update"] != false {
		t.Fatalf("a pre-§5.5 agent must read as seen+unsupported, got seen=%v supported=%v",
			node2["agent_caps_seen"], node2["agent_self_update"])
	}
}

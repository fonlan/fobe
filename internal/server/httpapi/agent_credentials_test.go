package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/gorilla/websocket"
)

// tokenFromInstallCommand pulls the single-use reg token out of the command the
// panel shows (`curl ... | sh -s -- --token 'x' --server 'y'`).
func tokenFromInstallCommand(t *testing.T, cmd string) string {
	t.Helper()
	i := strings.Index(cmd, "--token ")
	if i < 0 {
		t.Fatalf("no --token in install command %q", cmd)
	}
	rest := strings.TrimPrefix(cmd[i+len("--token "):], "'")
	if j := strings.IndexAny(rest, "' "); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatalf("empty token in install command %q", cmd)
	}
	return rest
}

// reissueToken mints the node-bound token through the panel API.
func reissueToken(t *testing.T, srv *httptest.Server, cookie, nodeID string) string {
	t.Helper()
	resp, out := authedPost(t, srv, cookie, "/api/nodes/"+nodeID+"/agent/reinstall-command", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reinstall-command status = %d (%v)", resp.StatusCode, out)
	}
	if out["reissues"] != true {
		t.Fatalf("reinstall-command should report reissues=true: %v", out)
	}
	cmd, _ := out["install_command"].(string)
	return tokenFromInstallCommand(t, cmd)
}

// registerRaw posts a registration body and returns the status + decoded body.
func registerRaw(t *testing.T, srv *httptest.Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	resp, out := postJSON(t, &http.Client{}, srv.URL+"/api/agent/register", body)
	return resp.StatusCode, out
}

// wsHandshakeStatus dials /ws/agent and reports the status of the handshake
// (200 = accepted). A refused dial is exactly how a stale secret shows up.
func wsHandshakeStatus(t *testing.T, srv *httptest.Server, nodeID, secret string) int {
	t.Helper()
	hdr := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
	}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		if resp == nil {
			t.Fatalf("dial failed without a response: %v", err)
		}
		return resp.StatusCode
	}
	conn.Close()
	return http.StatusOK
}

// TestReinstallTokenReissuesCredentialsWithoutOldSecret is the scenario this
// feature exists for: the probe's config.json was overwritten or wiped, so it
// can present nothing but the token — the server only kept the hash of the old
// secret and could never hand it back (§4.2 revision 2026-09-17).
func TestReinstallTokenReissuesCredentialsWithoutOldSecret(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, oldSecret := registerNode(t, srv, cookie, "probe-reissue", "machine-reissue")

	token := reissueToken(t, srv, cookie, nodeID)

	// No node_id, no node_secret: exactly what a wiped probe sends.
	status, reg := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-reissue", "hostname": "probe-reissue",
		"os": "linux", "arch": "amd64", "version": "0.1.0", "tz": "UTC",
	})
	if status != http.StatusOK {
		t.Fatalf("reissue register status = %d, body %v", status, reg)
	}
	if got, _ := reg["node_id"].(string); got != nodeID {
		t.Fatalf("reissue changed the node id: got %q want %q", got, nodeID)
	}
	if reused, _ := reg["reused"].(bool); !reused {
		t.Fatalf("reissue should report reused=true: %v", reg)
	}
	newSecret, _ := reg["node_secret"].(string)
	if newSecret == "" || newSecret == oldSecret {
		t.Fatalf("expected a fresh secret, got %q (old %q)", newSecret, oldSecret)
	}

	// The new secret works, the old one is dead.
	if code := wsHandshakeStatus(t, srv, nodeID, newSecret); code != http.StatusOK {
		t.Fatalf("new secret rejected by /ws/agent: %d", code)
	}
	if code := wsHandshakeStatus(t, srv, nodeID, oldSecret); code != http.StatusUnauthorized {
		t.Fatalf("old secret still accepted by /ws/agent: %d", code)
	}

	// And the node is still the same node — that is the whole point (deleting it
	// would have taken its forwards, billing and settings with it).
	_, node := authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	n, _ := node["node"].(map[string]any)
	if n == nil || n["id"] != nodeID {
		t.Fatalf("node gone or replaced after reissue: %v", node)
	}

	// The panel can tell reissue tokens apart from add-node tokens.
	_, list := authedGet(t, srv, cookie, "/api/reg-tokens?all=1")
	seen := false
	if arr, ok := list["tokens"].([]any); ok {
		for _, raw := range arr {
			if m, ok := raw.(map[string]any); ok && m["node_id"] == nodeID {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatalf("reissue token not listed with its node id: %v", list)
	}
}

// The incident's exact shape: the probe *does* carry credentials, but they
// belong to a dead test panel (different node id, unknown secret). The bound
// token must win over whatever the probe thinks it is.
func TestReinstallTokenIgnoresStaleProbeCredentials(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, _ := registerNode(t, srv, cookie, "probe-stale", "machine-stale")

	token := reissueToken(t, srv, cookie, nodeID)
	status, reg := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-stale", "hostname": "probe-stale", "tz": "UTC",
		"node_id": "peK5KywBWVk", "node_secret": "secret-from-a-dead-panel",
	})
	if status != http.StatusOK {
		t.Fatalf("stale-credential reissue: status %d body %v", status, reg)
	}
	if got, _ := reg["node_id"].(string); got != nodeID {
		t.Fatalf("stale credentials won: got node %q want %q", got, nodeID)
	}
}

// A node-bound token can only touch the node it was minted for: it must never
// steal a machine that is already registered as a different node.
func TestReinstallTokenRejectsAnotherMachine(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, _ := registerNode(t, srv, cookie, "probe-a", "machine-a")
	otherID, _ := registerNode(t, srv, cookie, "probe-b", "machine-b")

	// A machine that already belongs to another node: a real conflict, refused.
	token := reissueToken(t, srv, cookie, nodeID)
	status, body := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-b", "hostname": "probe-b", "tz": "UTC",
	})
	if status != http.StatusConflict || errorCode(body) != "machine_mismatch" {
		t.Fatalf("bound token on another node: status %d body %v", status, body)
	}

	// Neither node is damaged.
	if _, n := authedGet(t, srv, cookie, "/api/nodes/"+otherID); n["node"] == nil {
		t.Fatalf("unrelated node damaged: %v", n)
	}
	if _, n := authedGet(t, srv, cookie, "/api/nodes/"+nodeID); n["node"] == nil {
		t.Fatalf("target node damaged: %v", n)
	}
}

// An unknown machine id means the probe lost its whole state directory
// (machine-id included) or moved to new hardware. The panel never exposes
// machine_id, so refusing here would leave no way back — the reissue token
// adopts the new identity instead.
func TestReinstallTokenAdoptsNewMachineID(t *testing.T) {
	srv, st := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, _ := registerNode(t, srv, cookie, "probe-moved", "machine-old")

	token := reissueToken(t, srv, cookie, nodeID)
	status, reg := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-new", "hostname": "probe-moved", "tz": "UTC",
	})
	if status != http.StatusOK {
		t.Fatalf("adopt register status = %d, body %v", status, reg)
	}
	if got, _ := reg["node_id"].(string); got != nodeID {
		t.Fatalf("adopt created a new node: got %q want %q", got, nodeID)
	}
	n, err := st.Store.GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.MachineID != "machine-new" {
		t.Fatalf("machine id not adopted: %q", n.MachineID)
	}
	if n.Name != "probe-moved" {
		t.Fatalf("adopt renamed the node: %q", n.Name)
	}
	secret, _ := reg["node_secret"].(string)
	if code := wsHandshakeStatus(t, srv, nodeID, secret); code != http.StatusOK {
		t.Fatalf("adopted credentials rejected: %d", code)
	}
	// The adopted identity is the one dedupe knows from now on.
	token2 := reissueToken(t, srv, cookie, nodeID)
	if status, body := registerRaw(t, srv, map[string]any{
		"token": token2, "machine_id": "machine-new", "hostname": "probe-moved", "tz": "UTC",
	}); status != http.StatusOK {
		t.Fatalf("second reissue with the adopted id: %d %v", status, body)
	}
}

func TestReinstallTokenIsSingleUse(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, _ := registerNode(t, srv, cookie, "probe-once", "machine-once")

	token := reissueToken(t, srv, cookie, nodeID)
	if status, body := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-once", "hostname": "probe-once", "tz": "UTC",
	}); status != http.StatusOK {
		t.Fatalf("first use failed: %d %v", status, body)
	}
	status, body := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-once", "hostname": "probe-once", "tz": "UTC",
	})
	if status != http.StatusUnauthorized || errorCode(body) != "invalid_token" {
		t.Fatalf("replayed token: status %d body %v", status, body)
	}
}

// If the node was deleted in the meantime the token is dead too — it must not
// silently resurrect it as a brand-new node.
func TestReinstallTokenAfterNodeDeleted(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, _ := registerNode(t, srv, cookie, "probe-gone", "machine-gone")

	token := reissueToken(t, srv, cookie, nodeID)
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/nodes/"+nodeID, nil)
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("delete node: %v %d", err, statusCode(resp))
	}
	resp.Body.Close()

	status, body := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-gone", "hostname": "probe-gone", "tz": "UTC",
	})
	if status != http.StatusNotFound || errorCode(body) != "node_not_found" {
		t.Fatalf("token for a deleted node: status %d body %v", status, body)
	}
}

// Regression: the pre-existing paths must behave exactly as before — a generic
// token cannot rebind without credentials, and the old-secret path still does.
func TestGenericRegTokenPathsUnchanged(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, secret := registerNode(t, srv, cookie, "probe-legacy", "machine-legacy")

	// Generic token, same machine, no credentials → suspected duplicate install.
	status, body := registerRaw(t, srv, map[string]any{
		"token": freshToken(t, srv, cookie, "probe-legacy"), "machine_id": "machine-legacy",
		"hostname": "probe-legacy", "tz": "UTC",
	})
	if status != http.StatusConflict || errorCode(body) != "duplicate_machine" {
		t.Fatalf("generic token without credentials: status %d body %v", status, body)
	}

	// Generic token + the node's own valid credentials → reuse (rebind).
	status, body = registerRaw(t, srv, map[string]any{
		"token": freshToken(t, srv, cookie, "probe-legacy"), "machine_id": "machine-legacy",
		"hostname": "probe-legacy", "tz": "UTC", "node_id": nodeID, "node_secret": secret,
	})
	if status != http.StatusOK {
		t.Fatalf("legacy rebind failed: %d %v", status, body)
	}
	if got, _ := body["node_id"].(string); got != nodeID {
		t.Fatalf("legacy rebind changed the node id: %q", got)
	}
	if got, _ := body["reused"].(bool); !got {
		t.Fatalf("legacy rebind should report reused=true: %v", body)
	}

	// A generic token still creates a node for a machine nobody knows.
	status, body = registerRaw(t, srv, map[string]any{
		"token": freshToken(t, srv, cookie, "probe-new"), "machine_id": "machine-new",
		"hostname": "probe-new", "tz": "UTC",
	})
	if status != http.StatusOK || body["node_id"] == "" {
		t.Fatalf("fresh registration broken: %d %v", status, body)
	}
}

// The reissued credentials must be enough to complete a §5.5-style hello, i.e.
// the hub accepts the connection for the same node.
func TestReissuedCredentialsCompleteHello(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)
	nodeID, _ := registerNode(t, srv, cookie, "probe-hello", "machine-hello")
	token := reissueToken(t, srv, cookie, nodeID)
	_, reg := registerRaw(t, srv, map[string]any{
		"token": token, "machine_id": "machine-hello", "hostname": "probe-hello", "tz": "UTC",
	})
	secret, _ := reg["node_secret"].(string)
	if secret == "" {
		t.Fatalf("no secret issued: %v", reg)
	}
	ack, ws := agentHello(t, srv, nodeID, secret, protocol.Hello{Version: "0.1.1"})
	defer ws.Close()
	if ack.NodeID != "" && ack.NodeID != nodeID {
		t.Fatalf("hello_ack for %q, want %q", ack.NodeID, nodeID)
	}
}

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

// createSSHTestNode registers a node and connects its (fake) agent, leaving
// the hello/hello_ack handshake done so the node counts as online.
func createSSHTestNode(t *testing.T, srv *httptest.Server, cookie, machineID string) (*websocket.Conn, string) {
	t.Helper()
	token := freshToken(t, srv, cookie, "node-"+machineID)
	_, reg := postJSON(t, http.DefaultClient, srv.URL+"/api/agent/register", map[string]any{
		"token": token, "machine_id": machineID, "hostname": "host-" + machineID,
		"os": "linux", "arch": "amd64", "version": "dev", "tz": "UTC", "cpu_cores": 2,
	})
	nodeID, _ := reg["node_id"].(string)
	nodeSecret, _ := reg["node_secret"].(string)
	if nodeID == "" || nodeSecret == "" {
		t.Fatalf("agent register failed: %v", reg)
	}
	hdr := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {nodeSecret},
	}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, hs, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		code := 0
		if hs != nil {
			code = hs.StatusCode
		}
		t.Fatalf("agent dial: %v (status %d)", err, code)
	}
	t.Cleanup(func() { ws.Close() })

	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{
		MachineID: machineID, Hostname: "host-" + machineID, Version: "dev",
		OS: "linux", Arch: "amd64", CPUCores: 2, TZ: "UTC",
	})); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var env protocol.Envelope
	if err := ws.ReadJSON(&env); err != nil || env.Type != protocol.TypeHelloAck {
		t.Fatalf("hello_ack: %v (type %s)", err, env.Type)
	}
	return ws, nodeID
}

func dialBrowserTerminal(t *testing.T, srv *httptest.Server, cookie, nodeID string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/terminal?node=" + nodeID
	hdr := http.Header{"Cookie": {cookie}}
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("terminal dial: %v (status %d)", err, code)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func readTerminalFrame(t *testing.T, ws *websocket.Conn, want ...string) protocol.Envelope {
	t.Helper()
	for {
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var env protocol.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatalf("read (want %v): %v", want, err)
		}
		for _, w := range want {
			if env.Type == w {
				return env
			}
		}
	}
}

func terminalLoginCookie(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, _ := postJSON(t, http.DefaultClient, srv.URL+"/api/login", map[string]string{"password": "test-password-123"})
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	cookie := ""
	for _, c := range readCookies(resp) {
		if c.Name == security.SessionCookieName {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	return cookie
}

func terminalOpenEnvelope(mode string) protocol.Envelope {
	return protocol.NewEnvelope(protocol.TypeTermOpen, "", protocol.TerminalOpen{Mode: mode, Cols: 100, Rows: 30})
}

func authRequest(t *testing.T, method, url, cookie string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, reader)
	req.Header.Set("Cookie", cookie)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

// PUT then GET: only set-flags come back, never the secrets; the empty
// string keeps a stored value and the "!" prefix clears it.
func TestNodeSSHCredentialsRoundTrip(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := terminalLoginCookie(t, srv)
	if err := api.Store.CreateNode(&store.Node{ID: "n-ssh", Name: "n-ssh", MachineID: "m-ssh", TZ: "UTC"}, "h"); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/nodes/n-ssh/ssh"

	// node without credentials: defaults, nothing set
	resp, out := authRequest(t, "GET", base, cookie, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET fresh: %d %v", resp.StatusCode, out)
	}
	if out["user"] != "root" || out["port"].(float64) != 22 || out["password_set"] != false || out["privkey_set"] != false {
		t.Fatalf("fresh view = %v, want root/22/none set", out)
	}

	resp, out = authRequest(t, "PUT", base, cookie, map[string]any{
		"user": "admin", "port": 2222, "password": "hunter2", "private_key": "KEYDATA",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("PUT: %d %v", resp.StatusCode, out)
	}
	resp, out = authRequest(t, "GET", base, cookie, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET: %d %v", resp.StatusCode, out)
	}
	if out["user"] != "admin" || out["port"].(float64) != 2222 || out["password_set"] != true || out["privkey_set"] != true {
		t.Fatalf("view after PUT = %v", out)
	}
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "KEYDATA") {
		t.Fatalf("GET leaked secret material: %s", raw)
	}

	// empty strings keep the stored secrets
	resp, out = authRequest(t, "PUT", base, cookie, map[string]any{"password": "", "private_key": ""})
	if resp.StatusCode != 200 || out["password_set"] != true || out["privkey_set"] != true {
		t.Fatalf("empty-string keep failed: %d %v", resp.StatusCode, out)
	}

	// "!" clears
	resp, out = authRequest(t, "PUT", base, cookie, map[string]any{"password": "!"})
	if resp.StatusCode != 200 || out["password_set"] != false || out["privkey_set"] != true {
		t.Fatalf("'!' clear failed: %d %v", resp.StatusCode, out)
	}

	// invalid port is rejected without touching the row
	resp, _ = authRequest(t, "PUT", base, cookie, map[string]any{"port": 70000})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad port: got %d, want 400", resp.StatusCode)
	}

	// unknown node 404s
	resp, _ = authRequest(t, "GET", srv.URL+"/api/nodes/none/ssh", cookie, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown node: got %d, want 404", resp.StatusCode)
	}
}

// The testable injection core: stored ciphertext row → wire-open with
// loopback host and decrypted secrets.
func TestApplyNodeSSHInjection(t *testing.T) {
	crypt, err := security.NewCryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	pwEnc, _ := crypt.Encrypt("hunter2")
	row := &store.NodeSSH{NodeID: "n1", User: "admin", Port: 2222, PasswordEnc: pwEnc}

	open := &protocol.TerminalOpen{Mode: "ssh"}
	if err := applyNodeSSH(open, row, crypt); err != nil {
		t.Fatalf("applyNodeSSH: %v", err)
	}
	if open.Host != "127.0.0.1" || open.Port != 2222 || open.User != "admin" || open.Password != "hunter2" || open.PrivKey != "" {
		t.Fatalf("injected open = %+v", open)
	}

	// defaults apply together with a usable secret (port/user fall back when
	// the stored row omits them)
	keyEnc, _ := crypt.Encrypt("KEYDATA")
	open = &protocol.TerminalOpen{Mode: "ssh"}
	if err := applyNodeSSH(open, &store.NodeSSH{PasswordEnc: pwEnc}, crypt); err != nil {
		t.Fatalf("applyNodeSSH defaults: %v", err)
	}
	if open.Port != 22 || open.User != "root" || open.Password != "hunter2" {
		t.Fatalf("defaults not applied: %+v", open)
	}
	open = &protocol.TerminalOpen{Mode: "ssh"}
	if err := applyNodeSSH(open, &store.NodeSSH{PrivKeyEnc: keyEnc}, crypt); err != nil {
		t.Fatalf("applyNodeSSH key-only: %v", err)
	}
	if open.PrivKey != "KEYDATA" {
		t.Fatalf("key not injected: %+v", open)
	}

	// corrupt ciphertext is an error, never a silent fallback
	if err := applyNodeSSH(&protocol.TerminalOpen{Mode: "ssh"}, &store.NodeSSH{PasswordEnc: "garbage"}, crypt); err == nil {
		t.Fatal("corrupt ciphertext should fail")
	}

	// a row without any usable secret counts as missing credentials
	if err := applyNodeSSH(&protocol.TerminalOpen{Mode: "ssh"}, &store.NodeSSH{User: "root", Port: 22}, crypt); err != errSSHCredentialsMissing {
		t.Fatalf("secretless row: got %v, want errSSHCredentialsMissing", err)
	}
}

// Full relay check: an ssh-mode terminal_open only reaches the agent with
// injected credentials, and a missing-credentials open is answered directly
// to the browser without reaching the agent.

// pumpAgentFrames reads agent frames on a background goroutine. Needed because
// a websocket read deadline error is sticky in gorilla/websocket: timing out a
// direct read in the negative check would poison the connection for the later
// positive steps.
func pumpAgentFrames(ws *websocket.Conn) <-chan protocol.Envelope {
	ch := make(chan protocol.Envelope, 16)
	go func() {
		for {
			var env protocol.Envelope
			if err := ws.ReadJSON(&env); err != nil {
				close(ch)
				return
			}
			ch <- env
		}
	}()
	return ch
}

// expectNoAgentOpen gives the relay a short window to (wrongly) forward the
// open; any terminal_open in that window fails the test.
func expectNoAgentOpen(t *testing.T, ch <-chan protocol.Envelope) {
	t.Helper()
	for {
		select {
		case env, ok := <-ch:
			if !ok {
				return
			}
			if env.Type == protocol.TypeTermOpen {
				t.Fatal("agent received terminal_open while credentials were missing")
			}
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

func expectAgentOpen(t *testing.T, ch <-chan protocol.Envelope) protocol.TerminalOpen {
	t.Helper()
	for {
		select {
		case env, ok := <-ch:
			if !ok {
				t.Fatal("agent connection closed before terminal_open arrived")
			}
			if env.Type != protocol.TypeTermOpen {
				continue
			}
			var open protocol.TerminalOpen
			if err := json.Unmarshal(env.Payload, &open); err != nil {
				t.Fatal(err)
			}
			return open
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for agent terminal_open")
		}
	}
}

func TestTerminalRelaySSHInjection(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := terminalLoginCookie(t, srv)
	agentWS, nodeID := createSSHTestNode(t, srv, cookie, "m-relay")
	agentFrames := pumpAgentFrames(agentWS)

	browser := dialBrowserTerminal(t, srv, cookie, nodeID)

	// 1. no credentials yet: browser gets terminal_closed, agent sees nothing
	if err := browser.WriteJSON(terminalOpenEnvelope("ssh")); err != nil {
		t.Fatal(err)
	}
	env := readTerminalFrame(t, browser, "terminal_closed")
	var closed protocol.TerminalClosed
	if err := json.Unmarshal(env.Payload, &closed); err != nil {
		t.Fatal(err)
	}
	if closed.Reason != "ssh_credentials_missing" {
		t.Fatalf("closed.Reason = %q", closed.Reason)
	}
	if closed.SessionID == "" {
		t.Fatal("server must stamp the session id on the injected frame")
	}
	expectNoAgentOpen(t, agentFrames)

	// 2. store credentials, then re-open: the agent gets the injected frame
	resp, out := authRequest(t, "PUT", srv.URL+"/api/nodes/"+nodeID+"/ssh", cookie, map[string]any{
		"user": "admin", "port": 2222, "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("PUT creds: %d %v", resp.StatusCode, out)
	}
	if err := browser.WriteJSON(terminalOpenEnvelope("ssh")); err != nil {
		t.Fatal(err)
	}
	open := expectAgentOpen(t, agentFrames)
	if open.Host != "127.0.0.1" || open.Port != 2222 || open.User != "admin" || open.Password != "hunter2" {
		t.Fatalf("agent open = %+v", open)
	}
	if open.SessionID == "" || open.SessionID != closed.SessionID {
		t.Fatalf("session id not stamped consistently: %q vs %q", open.SessionID, closed.SessionID)
	}
	if open.Mode != "ssh" {
		t.Fatalf("open.Mode = %q", open.Mode)
	}

	// 3. pty mode passes through without any injected credentials
	if err := browser.WriteJSON(terminalOpenEnvelope("pty")); err != nil {
		t.Fatal(err)
	}
	ptyOpen := expectAgentOpen(t, agentFrames)
	if ptyOpen.Host != "" || ptyOpen.Password != "" || ptyOpen.PrivKey != "" || ptyOpen.Mode != "pty" {
		t.Fatalf("pty open must not be injected: %+v", ptyOpen)
	}
}

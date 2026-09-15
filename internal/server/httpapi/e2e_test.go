// Package httpapi integration tests: the full M1/M2 agent↔server path —
// login, registration, WSS hello, metrics/traffic ingestion with counter
// reset detection, command round-trip. Run: go test ./internal/server/...
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/hub"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hash, err := security.HashPassword("test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(hash, false); err != nil {
		t.Fatal(err)
	}
	trust, err := security.NewTrustChain("")
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := security.NewCryptor([]byte(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	log := testLogger()
	h := hub.New(st, trust, log)
	api := NewServer(st, h, trust, crypt, log)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	go h.PumpCommands(time.Second)
	return srv, api
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func postJSON(t *testing.T, client *http.Client, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestFullAgentPath(t *testing.T) {
	srv, _ := newTestServer(t)
	client := &http.Client{}

	// 1. login
	resp, out := postJSON(t, client, srv.URL+"/api/login", map[string]string{"password": "test-password-123"})
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d %v", resp.StatusCode, out)
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
	authClient := &http.Client{}
	req, _ := http.NewRequest("GET", srv.URL+"/api/nodes", nil)
	req.Header.Set("Cookie", cookie)
	r2, err := authClient.Do(req)
	if err != nil || r2.StatusCode != 200 {
		t.Fatalf("authed /api/nodes: %v %d", err, statusCode(r2))
	}
	r2.Body.Close()

	// 2. creating without a name is rejected
	missingNameReq, _ := http.NewRequest("POST", srv.URL+"/api/reg-tokens", bytes.NewReader([]byte(`{"note":"t"}`)))
	missingNameReq.Header.Set("Cookie", cookie)
	missingNameResp, err := authClient.Do(missingNameReq)
	if err != nil {
		t.Fatalf("missing-name request: %v", err)
	}
	if missingNameResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing-name request: got %d, want 400", missingNameResp.StatusCode)
	}
	var missingNameBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(missingNameResp.Body).Decode(&missingNameBody)
	missingNameResp.Body.Close()
	if missingNameBody.Error.Code != "name_required" {
		t.Fatalf("missing-name code: got %q, want name_required", missingNameBody.Error.Code)
	}

	// 3. create a registration token with the required node name
	raw, _ := http.NewRequest("POST", srv.URL+"/api/reg-tokens", bytes.NewReader([]byte(`{"name":"probe-alpha","note":"t"}`)))
	raw.Header.Set("Cookie", cookie)
	rt, err := authClient.Do(raw)
	if err != nil || rt.StatusCode != 200 {
		t.Fatalf("reg-token create: %v %d", err, statusCode(rt))
	}
	var tokenResp struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(rt.Body).Decode(&tokenResp)
	rt.Body.Close()
	if tokenResp.Token == "" {
		t.Fatal("empty token")
	}

	// 3. agent registration
	_, reg := postJSON(t, client, srv.URL+"/api/agent/register", map[string]any{
		"token": tokenResp.Token, "machine_id": "m-test-1", "hostname": "testhost",
		"os": "linux", "arch": "amd64", "version": "dev", "tz": "Asia/Shanghai", "cpu_cores": 4,
	})
	nodeID, _ := reg["node_id"].(string)
	nodeSecret, _ := reg["node_secret"].(string)
	if nodeID == "" || nodeSecret == "" {
		t.Fatalf("register failed: %v", reg)
	}
	// The required panel name is persisted as the node name, not replaced by hostname.
	nodeReq, _ := http.NewRequest("GET", srv.URL+"/api/nodes/"+nodeID, nil)
	nodeReq.Header.Set("Cookie", cookie)
	nodeResp, err := authClient.Do(nodeReq)
	if err != nil {
		t.Fatalf("get registered node: %v", err)
	}
	var nodeBody struct {
		Node struct {
			Name string `json:"name"`
		} `json:"node"`
	}
	_ = json.NewDecoder(nodeResp.Body).Decode(&nodeBody)
	nodeResp.Body.Close()
	if nodeBody.Node.Name != "probe-alpha" {
		t.Fatalf("registered node name: got %q, want %q", nodeBody.Node.Name, "probe-alpha")
	}

	// duplicate registration without credentials must be refused (§4.2)
	_, tok2 := postJSON(t, client, srv.URL+"/api/agent/register", map[string]any{
		"token": freshToken(t, srv, cookie, "duplicate-test"), "machine_id": "m-test-1", "hostname": "x",
	})
	if code, _ := tok2["error"].(map[string]any)["code"].(string); code != "duplicate_machine" {
		t.Fatalf("expected duplicate_machine, got %v", tok2)
	}

	// 4. agent WSS: hello → hello_ack
	hdr := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {nodeSecret},
	}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, hsResp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		code := 0
		if hsResp != nil {
			code = hsResp.StatusCode
		}
		t.Fatalf("agent dial: %v (status %d)", err, code)
	}
	defer ws.Close()

	sendPayload := func(typ, id string, payload any) {
		env := protocol.NewEnvelope(typ, id, payload)
		if err := ws.WriteJSON(env); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
	}
	var readType func(want ...string) protocol.Envelope
	readType = func(want ...string) protocol.Envelope {
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
		return readType(want...) // tolerate unrelated frames
	}

	sendPayload(protocol.TypeHello, "", protocol.Hello{
		MachineID: "m-test-1", Hostname: "testhost", Version: "dev",
		OS: "linux", Arch: "amd64", CPUCores: 4, TZ: "Asia/Shanghai",
		IPs: []protocol.IPInfo{{IP: "203.0.113.10", Family: 4, Scope: "public", IsPrimary: true}},
	})
	readType(protocol.TypeHelloAck)

	// 5. node shows online
	req2, _ := http.NewRequest("GET", srv.URL+"/api/nodes", nil)
	req2.Header.Set("Cookie", cookie)
	lr, _ := authClient.Do(req2)
	var listResp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	_ = json.NewDecoder(lr.Body).Decode(&listResp)
	lr.Body.Close()
	if len(listResp.Nodes) != 1 || listResp.Nodes[0]["status"] != "online" {
		t.Fatalf("node not online: %+v", listResp)
	}

	// 6. traffic accounting — configure the interface FIRST so reports land
	updReq, _ := http.NewRequest("PATCH", srv.URL+"/api/nodes/"+nodeID, bytes.NewReader([]byte(
		`{"network":{"iface":"eth0","mode":"both","quota_bytes":1000000,"tz":"Asia/Shanghai"}}`)))
	updReq.Header.Set("Cookie", cookie)
	if ur, err := authClient.Do(updReq); err != nil || ur.StatusCode != 200 {
		t.Fatalf("network update: %v %d", err, statusCode(ur))
	} else {
		ur.Body.Close()
	}

	sendPayload(protocol.TypeMetrics, "", protocol.Metrics{
		CPU: 12.5, Load1: 0.4, MemUsed: 2 << 30, MemTotal: 8 << 30,
		Disks: []protocol.Disk{{Path: "/", Total: 100 << 30, Used: 40 << 30}},
	})
	// baseline (no delta), accumulate, then a counter drop (reboot/wrap, §8.2.2)
	sendPayload(protocol.TypeTraffic, "", protocol.Traffic{Iface: "eth0", Rx: 1000, Tx: 500})
	sendPayload(protocol.TypeTraffic, "", protocol.Traffic{Iface: "eth0", Rx: 3000, Tx: 2000})
	sendPayload(protocol.TypeTraffic, "", protocol.Traffic{Iface: "eth0", Rx: 100, Tx: 50})
	// new baseline after the reset, then accumulate again
	sendPayload(protocol.TypeTraffic, "", protocol.Traffic{Iface: "eth0", Rx: 3100, Tx: 2100})
	sendPayload(protocol.TypeTraffic, "", protocol.Traffic{Iface: "eth0", Rx: 6100, Tx: 3600})

	time.Sleep(200 * time.Millisecond)

	req3, _ := http.NewRequest("GET", srv.URL+"/api/nodes/"+nodeID+"/traffic", nil)
	req3.Header.Set("Cookie", cookie)
	tr, _ := authClient.Do(req3)
	var trafficResp struct {
		Daily []struct {
			RxBytes int64 `json:"rx_bytes"`
			TxBytes int64 `json:"tx_bytes"`
		} `json:"daily"`
	}
	_ = json.NewDecoder(tr.Body).Decode(&trafficResp)
	tr.Body.Close()
	var rx, tx int64
	for _, d := range trafficResp.Daily {
		rx += d.RxBytes
		tx += d.TxBytes
	}
	// accumulated: (3000-1000) + (3100-100) + (6100-3100) = 8000 rx
	// tx likewise 5050. The reset frame itself (100/50) contributes 0:
	// a negative delta is never counted, it re-baselines (§8.2.2).
	if rx != 8000 || tx != 5050 {
		t.Fatalf("traffic rx=%d tx=%d, want 8000/5050", rx, tx)
	}

	// counter reset alert exists
	req4, _ := http.NewRequest("GET", srv.URL+"/api/alerts", nil)
	req4.Header.Set("Cookie", cookie)
	ar, _ := authClient.Do(req4)
	var alertsResp struct {
		Alerts []struct {
			Kind string `json:"kind"`
		} `json:"alerts"`
	}
	_ = json.NewDecoder(ar.Body).Decode(&alertsResp)
	ar.Body.Close()
	foundReset := false
	for _, a := range alertsResp.Alerts {
		if a.Kind == "counter_reset" {
			foundReset = true
		}
	}
	if !foundReset {
		t.Fatalf("counter_reset alert missing: %+v", alertsResp)
	}

	// 7. heartbeat: ping → pong
	sendPayload(protocol.TypePing, "", nil)
	readType("pong")

	// 8. command round-trip: enqueue via API, agent executes, result recorded
	cmdRaw, _ := http.NewRequest("POST", srv.URL+"/api/nodes/"+nodeID+"/commands",
		bytes.NewReader([]byte(`{"kind":"run_shell","payload":{"command":"echo hello-fobe"},"risky":false}`)))
	cmdRaw.Header.Set("Cookie", cookie)
	cr, err := authClient.Do(cmdRaw)
	if err != nil || cr.StatusCode != 200 {
		t.Fatalf("enqueue: %v %d", err, statusCode(cr))
	}
	cr.Body.Close()

	// agent receives the cmd and answers
	cmdEnv := readType(protocol.TypeCmd)
	var cmd protocol.Cmd
	_ = json.Unmarshal(cmdEnv.Payload, &cmd)
	if cmd.Kind != "run_shell" {
		t.Fatalf("unexpected cmd kind %q", cmd.Kind)
	}
	var shellPayload struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(cmd.Payload, &shellPayload)
	res := protocol.CmdResult{ID: cmd.ID, ExitCode: 0, Stdout: shellPayload.Command + "\n"}
	sendPayload(protocol.TypeCmdResult, cmd.ID, res)

	time.Sleep(300 * time.Millisecond)
	req5, _ := http.NewRequest("GET", srv.URL+"/api/nodes/"+nodeID+"/commands", nil)
	req5.Header.Set("Cookie", cookie)
	cmr, _ := authClient.Do(req5)
	var cmdsResp struct {
		Commands []struct {
			Status string `json:"status"`
			Result string `json:"result"`
		} `json:"commands"`
	}
	_ = json.NewDecoder(cmr.Body).Decode(&cmdsResp)
	cmr.Body.Close()
	if len(cmdsResp.Commands) != 1 || cmdsResp.Commands[0].Status != "ok" {
		t.Fatalf("command not completed: %+v", cmdsResp)
	}
	if !bytes.Contains([]byte(cmdsResp.Commands[0].Result), []byte("hello-fobe")) {
		t.Fatalf("command result missing stdout: %s", cmdsResp.Commands[0].Result)
	}

	// 9. anti-lockout (§4.3 rule 1): loopback failures NEVER blacklist —
	// even five wrong passwords stay 401 bad_credentials and never 403
	bad := &http.Client{}
	for i := 0; i < 5; i++ {
		br, _ := bad.Post(srv.URL+"/api/login", "application/json",
			bytes.NewReader([]byte(`{"password":"nope"}`)))
		br.Body.Close()
		if br.StatusCode != http.StatusUnauthorized {
			t.Fatalf("loopback wrong login: got %d, want 401 (never blacklisted)", br.StatusCode)
		}
	}
	okBr, _ := postJSON(t, bad, srv.URL+"/api/login", map[string]string{"password": "test-password-123"})
	if okBr.StatusCode != 200 {
		t.Fatalf("loopback correct login blocked despite anti-lockout: %d", okBr.StatusCode)
	}

	// 9b. blacklist mechanics for a REMOTE ip: spoofed via X-Forwarded-For
	// (socket 127.0.0.1 is a trusted proxy, so the leftmost XFF is honored).
	// 3 failures block the address; even the right password gets 403 until
	// the panel unblock API clears it.
	remote := &http.Client{}
	loginAs := func(ip, password string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/api/login",
			bytes.NewReader([]byte(`{"password":"`+password+`"}`)))
		req.Header.Set("X-Forwarded-For", ip)
		br, err := remote.Do(req)
		if err != nil {
			t.Fatalf("login as %s: %v", ip, err)
		}
		return br
	}
	for i := 0; i < 3; i++ {
		br := loginAs("203.0.113.77", "nope")
		br.Body.Close()
	}
	br := loginAs("203.0.113.77", "test-password-123")
	br.Body.Close()
	if br.StatusCode != http.StatusForbidden {
		t.Fatalf("expected remote blacklist 403, got %d", br.StatusCode)
	}
	// panel unblock (via authed API)
	delReq, _ := http.NewRequest("DELETE", srv.URL+"/api/blacklist/203.0.113.77", nil)
	delReq.Header.Set("Cookie", cookie)
	ur, err := authClient.Do(delReq)
	if err != nil || ur.StatusCode != 200 {
		t.Fatalf("unblock: %v %d", err, statusCode(ur))
	}
	ur.Body.Close()
	br2 := loginAs("203.0.113.77", "test-password-123")
	br2.Body.Close()
	if br2.StatusCode != 200 {
		t.Fatalf("login after unblock failed: %d", br2.StatusCode)
	}
}

func freshToken(t *testing.T, srv *httptest.Server, cookie, name string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/api/reg-tokens", bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Token
}

func readCookies(resp *http.Response) []*http.Cookie {
	// resp.Body was drained by postJSON; use resp.Header instead
	var out []*http.Cookie
	for _, v := range resp.Header.Values("Set-Cookie") {
		if c, err := parseSetCookie(v); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func parseSetCookie(v string) (*http.Cookie, error) {
	parts := bytes.SplitN([]byte(v), []byte(";"), 2)
	kv := bytes.SplitN(parts[0], []byte("="), 2)
	if len(kv) != 2 {
		return nil, fmt.Errorf("bad cookie %q", v)
	}
	return &http.Cookie{Name: string(kv[0]), Value: string(kv[1])}, nil
}

func statusCode(r *http.Response) int {
	if r == nil {
		return -1
	}
	return r.StatusCode
}

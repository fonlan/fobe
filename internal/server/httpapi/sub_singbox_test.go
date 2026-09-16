// Tests for design.md §9/§10: sing-box desired state management and the
// subscription/template pipeline (render, UA sniffing, rotation, access log).
package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

const testAnytlsPassword = "shared-proxy-pw"

const testCertPEM = "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIUEeTest\n-----END CERTIFICATE-----\n"

// httpResult captures status + headers + body for assertions.
type httpResult struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r httpResult) JSONMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, r.Body)
	}
	return m
}

func (r httpResult) errCode(t *testing.T) string {
	m := r.JSONMap(t)
	e, _ := m["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// doReq performs an authenticated panel request (empty cookie = anonymous).
func doReq(t *testing.T, client *http.Client, method, url, cookie string, body any) httpResult {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return httpResult{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

func panelCookie(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, out := postJSON(t, &http.Client{}, srv.URL+"/api/login", map[string]string{"password": "test-password-123"})
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d %v", resp.StatusCode, out)
	}
	for _, c := range readCookies(resp) {
		if c.Name == security.SessionCookieName {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

// seedNode inserts a node directly (bypassing the reg-token dance).
func seedNode(t *testing.T, api *Server, name, machineID, primaryIP string) (string, string) {
	t.Helper()
	id, err := security.RandomToken(8)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := security.RandomToken(16)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := security.HashPassword(secret)
	if err != nil {
		t.Fatal(err)
	}
	n := &store.Node{ID: id, Name: name, MachineID: machineID, PrimaryIP: primaryIP}
	if err := api.Store.CreateNode(n, hash); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return id, secret
}

func setAnytlsPassword(t *testing.T, srv *httptest.Server, cookie string) {
	t.Helper()
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
		map[string]any{"settings": map[string]string{"anytls_password": testAnytlsPassword}})
	if r.Status != 200 {
		t.Fatalf("set anytls_password: %d %s", r.Status, r.Body)
	}
}

// seedSingbox fakes an agent-reported sing-box state (running, cert reported).
func seedSingbox(t *testing.T, api *Server, nodeID string, port int) {
	t.Helper()
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: nodeID, Version: "1.10.0", Status: "running", Port: port,
		CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}
}

// createSubscription exercises POST /api/subscriptions and returns id+token.
func createSubscription(t *testing.T, srv *httptest.Server, cookie, name string) (string, string) {
	t.Helper()
	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/subscriptions", cookie, map[string]string{"name": name})
	if r.Status != 200 {
		t.Fatalf("create subscription: %d %s", r.Status, r.Body)
	}
	m := r.JSONMap(t)
	id, _ := m["id"].(string)
	token, _ := m["token"].(string)
	if id == "" || token == "" {
		t.Fatalf("subscription create missing id/token: %v", m)
	}
	if u, _ := m["url"].(string); !strings.HasSuffix(u, "/sub/"+token) {
		t.Fatalf("subscription url = %q, want suffix /sub/%s", u, token)
	}
	return id, token
}

func fetchSub(t *testing.T, srv *httptest.Server, token, query, ua string) httpResult {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/sub/"+token+query, nil)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sub: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return httpResult{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// --- templates ---

func TestTemplatePlaceholderValidation(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	// missing {{rules}} → 400
	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/templates", cookie, map[string]string{
		"name": "bad", "format": "singbox", "content": `{"outbounds": [{{nodes}}]}`,
	})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "missing_placeholder" {
		t.Fatalf("missing {{rules}}: %d %s", r.Status, r.Body)
	}
	// missing {{nodes}} → 400
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/templates", cookie, map[string]string{
		"name": "bad", "format": "clash", "content": "rules:\n  - {{rules}}\n",
	})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "missing_placeholder" {
		t.Fatalf("missing {{nodes}}: %d %s", r.Status, r.Body)
	}
	// bad format → 400
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/templates", cookie, map[string]string{
		"name": "bad", "format": "surge", "content": "{{nodes}} {{rules}}",
	})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_format" {
		t.Fatalf("bad format: %d %s", r.Status, r.Body)
	}

	// valid template lands
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/templates", cookie, map[string]string{
		"name": "ok", "format": "singbox",
		"content": "{\n  \"outbounds\": [\n{{nodes}}\n  ],\n  \"route\": {\"rules\": [{{rules}}]}\n}",
	})
	if r.Status != 200 {
		t.Fatalf("valid template rejected: %d %s", r.Status, r.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body, &created)
	if created.ID == "" {
		t.Fatal("template id empty")
	}

	// updating it to remove a placeholder is refused too
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/templates/"+created.ID, cookie, map[string]string{
		"name": "ok", "format": "singbox", "content": `{"outbounds": []}`,
	})
	if r.Status != http.StatusBadRequest {
		t.Fatalf("update without placeholders: %d %s", r.Status, r.Body)
	}
	_ = api
}

// --- subscription rendering ---

func TestSubscriptionRenderDefaultsAndSniffing(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	nodeID, _ := seedNode(t, api, "probe-1", "m-sub-1", "203.0.113.10")
	seedSingbox(t, api, nodeID, 23456)

	subID, token := createSubscription(t, srv, cookie, "main")
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}})
	if r.Status != 200 {
		t.Fatalf("set sub nodes: %d %s", r.Status, r.Body)
	}

	// explicit singbox render
	r = fetchSub(t, srv, token, "?format=singbox", "")
	if r.Status != 200 {
		t.Fatalf("singbox render: %d %s", r.Status, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("singbox content-type %q", ct)
	}
	if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control %q", cc)
	}
	var cfg struct {
		Outbounds []struct {
			Type       string `json:"type"`
			Tag        string `json:"tag"`
			Server     string `json:"server"`
			ServerPort int    `json:"server_port"`
			Password   string `json:"password"`
			TLS        struct {
				Enabled     bool   `json:"enabled"`
				ServerName  string `json:"server_name"`
				Insecure    bool   `json:"insecure"`
				Certificate string `json:"certificate"`
			} `json:"tls"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(r.Body, &cfg); err != nil {
		t.Fatalf("singbox render not JSON: %v\n%s", err, r.Body)
	}
	if len(cfg.Outbounds) != 1 {
		t.Fatalf("outbounds = %d, want 1", len(cfg.Outbounds))
	}
	ob := cfg.Outbounds[0]
	if ob.Type != "anytls" || ob.Server != "203.0.113.10" || ob.ServerPort != 23456 {
		t.Fatalf("bad outbound: %+v", ob)
	}
	if ob.Password != testAnytlsPassword {
		t.Fatalf("password not injected: %q", ob.Password)
	}
	if !ob.TLS.Enabled || ob.TLS.Insecure || ob.TLS.ServerName != "www.bing.com" {
		t.Fatalf("tls block wrong: %+v", ob.TLS)
	}
	if !strings.Contains(ob.TLS.Certificate, "BEGIN CERTIFICATE") {
		t.Fatalf("certificate not pinned: %q", ob.TLS.Certificate)
	}

	// explicit clash render
	r = fetchSub(t, srv, token, "?format=clash", "")
	if r.Status != 200 {
		t.Fatalf("clash render: %d %s", r.Status, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/yaml") {
		t.Fatalf("clash content-type %q", ct)
	}
	y := string(r.Body)
	for _, want := range []string{"type: anytls", `"203.0.113.10"`, "port: 23456", "skip-cert-verify: false", "ca-str: |", "BEGIN CERTIFICATE", "sni: www.bing.com"} {
		if !strings.Contains(y, want) {
			t.Fatalf("clash output missing %q:\n%s", want, y)
		}
	}

	// UA sniffing: clash UA → yaml, sing-box UA → json, unknown → default singbox
	r = fetchSub(t, srv, token, "", "clash-verge/2.0.0 mihomo")
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/yaml") {
		t.Fatalf("clash UA sniff failed: %q", r.Header.Get("Content-Type"))
	}
	r = fetchSub(t, srv, token, "", "sing-box/1.10.0 (linux-amd64)")
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("sing-box UA sniff failed: %q", r.Header.Get("Content-Type"))
	}
	r = fetchSub(t, srv, token, "", "curl/8.4.0")
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("default sniff should be singbox: %q", r.Header.Get("Content-Type"))
	}
	// explicit format beats UA
	r = fetchSub(t, srv, token, "?format=clash", "sing-box/1.10.0")
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/yaml") {
		t.Fatalf("explicit format must beat UA: %q", r.Header.Get("Content-Type"))
	}

	// access log recorded for each hit
	r = doReq(t, &http.Client{}, "GET", srv.URL+"/api/subscriptions/"+subID+"/access", cookie, nil)
	var al struct {
		Logs []struct {
			TS int64  `json:"ts"`
			IP string `json:"ip"`
			UA string `json:"ua"`
		} `json:"logs"`
	}
	_ = json.Unmarshal(r.Body, &al)
	if len(al.Logs) < 5 {
		t.Fatalf("access logs = %d, want >= 5: %s", len(al.Logs), r.Body)
	}
	if al.Logs[0].IP == "" || al.Logs[0].TS == 0 {
		t.Fatalf("access log row incomplete: %+v", al.Logs[0])
	}
}

func TestSubscriptionDisableAndRotate(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	nodeID, _ := seedNode(t, api, "probe-2", "m-sub-2", "203.0.113.11")
	seedSingbox(t, api, nodeID, 30000)
	subID, token := createSubscription(t, srv, cookie, "rot")
	doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}})

	if r := fetchSub(t, srv, token, "?format=singbox", ""); r.Status != 200 {
		t.Fatalf("initial fetch: %d %s", r.Status, r.Body)
	}

	// disable → the URL stops resolving with 404 (§10)
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie, map[string]any{"enabled": false})
	if r.Status != 200 {
		t.Fatalf("disable: %d %s", r.Status, r.Body)
	}
	if r := fetchSub(t, srv, token, "?format=singbox", ""); r.Status != http.StatusNotFound {
		t.Fatalf("disabled subscription: got %d, want 404", r.Status)
	}
	doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie, map[string]any{"enabled": true})
	if r := fetchSub(t, srv, token, "?format=singbox", ""); r.Status != 200 {
		t.Fatalf("re-enabled fetch: %d %s", r.Status, r.Body)
	}

	// rotate: old token 404s, new token works
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/subscriptions/"+subID+"/rotate", cookie, nil)
	if r.Status != 200 {
		t.Fatalf("rotate: %d %s", r.Status, r.Body)
	}
	var rot struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(r.Body, &rot)
	if rot.Token == "" || rot.Token == token {
		t.Fatalf("rotate returned same/empty token: %q", rot.Token)
	}
	if r := fetchSub(t, srv, token, "?format=singbox", ""); r.Status != http.StatusNotFound {
		t.Fatalf("old token after rotate: got %d, want 404", r.Status)
	}
	if r := fetchSub(t, srv, rot.Token, "?format=singbox", ""); r.Status != 200 {
		t.Fatalf("new token after rotate: %d %s", r.Status, r.Body)
	}

	// unknown tokens 404 without leaking anything
	if r := fetchSub(t, srv, "totally-unknown-token", "", ""); r.Status != http.StatusNotFound {
		t.Fatalf("unknown token: got %d, want 404", r.Status)
	}

	// listing shows node binding + enabled flag
	r = doReq(t, &http.Client{}, "GET", srv.URL+"/api/subscriptions", cookie, nil)
	var list struct {
		Subscriptions []subscriptionView `json:"subscriptions"`
	}
	_ = json.Unmarshal(r.Body, &list)
	if len(list.Subscriptions) != 1 || !list.Subscriptions[0].Enabled || len(list.Subscriptions[0].NodeIDs) != 1 {
		t.Fatalf("subscription list wrong: %s", r.Body)
	}
}

// --- sing-box desired state (§9) ---

func TestSingboxInstallAndDesiredPush(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	nodeID, secret := seedNode(t, api, "probe-sb", "m-sb-1", "203.0.113.12")

	// fake agent connects first: install must push the desired frame live
	hdr := http.Header{"X-Fobe-Node-ID": {nodeID}, "X-Fobe-Node-Secret": {secret}}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()
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
		return readType(want...)
	}
	ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{MachineID: "m-sb-1", Version: "dev"}))
	readType(protocol.TypeHelloAck)

	// install without anytls_password would fail; already set above.
	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/install", cookie,
		map[string]string{"version": "1.11.5"})
	if r.Status != 200 {
		t.Fatalf("install: %d %s", r.Status, r.Body)
	}
	var inst struct {
		Port int `json:"port"`
	}
	_ = json.Unmarshal(r.Body, &inst)
	if inst.Port < 10000 || inst.Port > 60000 {
		t.Fatalf("random port out of range: %d", inst.Port)
	}

	// the online agent receives the desired frame with config + version + port
	env := readType(protocol.TypeDesired)
	var desired protocol.DesiredState
	if err := json.Unmarshal(env.Payload, &desired); err != nil {
		t.Fatalf("desired payload: %v", err)
	}
	if desired.Singbox == nil || desired.Singbox.Version != "1.11.5" || desired.Singbox.Port != inst.Port {
		t.Fatalf("desired singbox wrong: %+v", desired.Singbox)
	}
	if !strings.Contains(desired.Singbox.ConfigJSON, `"listen_port": `+strconv.Itoa(inst.Port)) {
		t.Fatalf("desired config incomplete:\n%s", desired.Singbox.ConfigJSON)
	}
	if !strings.Contains(desired.Singbox.ConfigJSON, `"password": "`+testAnytlsPassword+`"`) {
		t.Fatalf("desired config missing shared password:\n%s", desired.Singbox.ConfigJSON)
	}

	// node_singbox carries the desired version, and the settings key holds the
	// exact config hub.buildDesiredState reads
	sb, err := api.Store.GetNodeSingbox(nodeID)
	if err != nil || sb.DesiredVersion != "1.11.5" || sb.Port != inst.Port || sb.ConfigHash == "" {
		t.Fatalf("node_singbox after install: %+v err=%v", sb, err)
	}
	cfgSetting, err := api.Store.GetSetting("singbox_config:" + nodeID)
	if err != nil || !strings.Contains(cfgSetting, "www.bing.com") {
		t.Fatalf("singbox_config setting: %q err=%v", cfgSetting, err)
	}

	// GET endpoint reports the state (no cert body)
	r = doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID+"/singbox", cookie, nil)
	m := r.JSONMap(t)
	sbMap, _ := m["singbox"].(map[string]any)
	if sbMap == nil || sbMap["desired_version"] != "1.11.5" || sbMap["port"].(float64) != float64(inst.Port) {
		t.Fatalf("GET singbox: %s", r.Body)
	}

	// port change regenerates the config and re-pushes desired
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/singbox/port", cookie, map[string]any{"port": 34567})
	if r.Status != 200 {
		t.Fatalf("port change: %d %s", r.Status, r.Body)
	}
	env = readType(protocol.TypeDesired)
	var desired2 protocol.DesiredState
	_ = json.Unmarshal(env.Payload, &desired2)
	if desired2.Singbox == nil || desired2.Singbox.Port != 34567 {
		t.Fatalf("desired after port change: %+v", desired2.Singbox)
	}
	if !strings.Contains(desired2.Singbox.ConfigJSON, `"listen_port": 34567`) {
		t.Fatalf("config not regenerated:\n%s", desired2.Singbox.ConfigJSON)
	}

	// start/stop/restart go through the commands queue with existing kinds
	for _, action := range []string{"start", "stop", "restart"} {
		r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/"+action, cookie, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", action, r.Status, r.Body)
		}
	}
	r = doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID+"/commands", cookie, nil)
	if !strings.Contains(string(r.Body), "restart_singbox") || !strings.Contains(string(r.Body), "start_singbox") ||
		!strings.Contains(string(r.Body), "stop_singbox") {
		t.Fatalf("commands not queued: %s", r.Body)
	}

	// version validation: malformed and unknown versions are refused
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/install", cookie,
		map[string]string{"version": "../evil"})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_version" {
		t.Fatalf("bad version: %d %s", r.Status, r.Body)
	}
}

// TestSingboxUninstallIsDesiredStateNotACommand covers the panel's uninstall
// action (design §9.2 实现修订 2026-09-16): the intent is a declared state, the
// reported half survives until the probe confirms, and the node stops being a
// batch-update target the moment the removal is declared.
func TestSingboxUninstallIsDesiredStateNotACommand(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	nodeID, secret := seedNode(t, api, "probe-un", "m-un-1", "203.0.113.14")

	// nothing installed (no row at all) → refused, nothing is queued
	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/uninstall", cookie, nil)
	if r.Status != http.StatusBadRequest || r.errCode(t) != "singbox_not_installed" {
		t.Fatalf("uninstall without sing-box: %d %s", r.Status, r.Body)
	}

	// a managed, agent-reported node: desired + reported halves both filled
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: nodeID, Version: "1.10.0", DesiredVersion: "1.10.0", ConfigHash: "h",
		Status: "running", Port: 23456, CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed managed node: %v", err)
	}
	if targets, err := api.Store.ListSingboxTargets(); err != nil || len(targets) != 1 {
		t.Fatalf("managed node is not a batch target: %+v err=%v", targets, err)
	}

	// fake agent connects so the declared removal must be pushed live
	hdr := http.Header{"X-Fobe-Node-ID": {nodeID}, "X-Fobe-Node-Secret": {secret}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[len("http"):]+"/ws/agent", hdr)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()
	readType := func(want string) protocol.Envelope {
		for {
			ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			var env protocol.Envelope
			if err := ws.ReadJSON(&env); err != nil {
				t.Fatalf("read (want %s): %v", want, err)
			}
			if env.Type == want {
				return env
			}
		}
	}
	_ = ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{MachineID: "m-un-1", Version: "dev"}))
	readType(protocol.TypeHelloAck)

	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/uninstall", cookie, nil)
	if r.Status != 200 {
		t.Fatalf("uninstall: %d %s", r.Status, r.Body)
	}
	var desired protocol.DesiredState
	if err := json.Unmarshal(readType(protocol.TypeDesired).Payload, &desired); err != nil {
		t.Fatalf("desired payload: %v", err)
	}
	if desired.Singbox == nil || !desired.Singbox.Uninstall || desired.Singbox.Version != "" {
		t.Fatalf("desired removal not declared: %+v", desired.Singbox)
	}
	if desired.Singbox.Port != 23456 {
		t.Fatalf("removal declaration dropped the port: %+v", desired.Singbox)
	}

	sb, err := api.Store.GetNodeSingbox(nodeID)
	if err != nil {
		t.Fatalf("get node singbox: %v", err)
	}
	if !sb.DesiredUninstall || sb.DesiredVersion != "" {
		t.Fatalf("desired half not cleared: %+v", sb)
	}
	// the reported half is the probe's business: it stays until the probe says
	// otherwise, so the panel never claims the binary is gone before it is
	if sb.Version != "1.10.0" || sb.CertPEM != testCertPEM || sb.Port != 23456 {
		t.Fatalf("reported half was cleared too early: %+v", sb)
	}
	// §9.5.4: a later batch update must not reinstall what was just removed
	if targets, err := api.Store.ListSingboxTargets(); err != nil || len(targets) != 0 {
		t.Fatalf("uninstalled node still a batch target: %+v err=%v", targets, err)
	}
	// the panel reads the flag from both endpoints
	m := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID+"/singbox", cookie, nil).JSONMap(t)
	if sm, _ := m["singbox"].(map[string]any); sm == nil || sm["desired_uninstall"] != true {
		t.Fatalf("GET singbox missing desired_uninstall: %s", m)
	}
	dm := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID, cookie, nil).JSONMap(t)
	if sm, _ := dm["singbox"].(map[string]any); sm == nil || sm["desired_uninstall"] != true {
		t.Fatalf("GET node missing desired_uninstall: %s", dm)
	}

	// while the removal is pending the port endpoint refuses: a write there
	// would cancel the removal the operator just asked for
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/singbox/port", cookie, map[string]any{"port": 30000})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "uninstall_pending" {
		t.Fatalf("port change while a removal is pending: %d %s", r.Status, r.Body)
	}

	// a second click while the removal is still pending re-arms it (the
	// declaration may have been missed) instead of failing
	if r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/uninstall", cookie, nil); r.Status != 200 {
		t.Fatalf("re-arm uninstall: %d %s", r.Status, r.Body)
	}
	readType(protocol.TypeDesired)

	// the probe confirms: nothing installed, nothing wrong
	_ = ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{
		Singbox: &protocol.SingboxState{Running: false, Version: "", Port: 23456},
	}))
	deadline := time.Now().Add(3 * time.Second)
	for {
		sb, err = api.Store.GetNodeSingbox(nodeID)
		if err == nil && sb.Status == "absent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("absent report not applied: %+v err=%v", sb, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sb.DesiredUninstall || sb.Version != "" || sb.CertPEM != "" || sb.CertSHA256 != "" {
		t.Fatalf("reported half not cleared on confirmation: %+v", sb)
	}
	// the port survives so a reinstall reuses it
	if sb.Port != 23456 {
		t.Fatalf("port lost on uninstall: %d", sb.Port)
	}

	// nothing is installed any more: a further uninstall click is honest about
	// there being nothing to remove
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/uninstall", cookie, nil)
	if r.Status != http.StatusBadRequest || r.errCode(t) != "singbox_not_installed" {
		t.Fatalf("uninstall after confirmation: %d %s", r.Status, r.Body)
	}
	// and it can be installed again, reusing the port the operator chose
	// (§9.2: 版本必须显式指定)
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/singbox/install", cookie,
		map[string]string{"version": "1.11.5"})
	if r.Status != 200 {
		t.Fatalf("reinstall: %d %s", r.Status, r.Body)
	}
	if sb, err = api.Store.GetNodeSingbox(nodeID); err != nil || sb.DesiredUninstall || sb.DesiredVersion != "1.11.5" || sb.Port != 23456 {
		t.Fatalf("state after reinstall: %+v err=%v", sb, err)
	}
	// the removal guard is gone: the port can be re-pointed again
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/singbox/port", cookie, map[string]any{"port": 30000})
	if r.Status != 200 {
		t.Fatalf("port change after reinstall: %d %s", r.Status, r.Body)
	}
}

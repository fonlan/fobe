// §9.3 实现修订 2026-09-17b: the editor model.
//
// The probe's own config.json is the source of truth. What the panel shows and
// what a subscription renders both come from the file the agent reported, and
// editing is a read-merge-write back onto that file. These tests pin the three
// things that model promises: hand-added inbounds appear without adoption,
// panel edits keep what they did not touch, and a stale edit is refused instead
// of silently building on an old base.
package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// liveConfigWithPanelInbound is one-sing.sh's file plus the inbound the panel
// installed (port 22039, which is also node_singbox.port) — the configuration a
// node is in after fobe has touched it once.
const liveConfigWithPanelInbound = `{
  "log": {"level": "info", "timestamp": true},
  "inbounds": [
    {
      "type": "anytls",
      "tag": "anytls-in",
      "listen": "::",
      "listen_port": 22039,
      "users": [{"name": "default", "password": "panel-generated-pw"}],
      "tls": {
        "enabled": true,
        "server_name": "www.bing.com",
        "certificate_path": "/etc/one-sing/cert/cert.crt",
        "key_path": "/etc/one-sing/cert/private.key"
      }
    },
    {
      "type": "anytls",
      "tag": "anytls-in-28711",
      "listen": "::",
      "listen_port": 28711,
      "users": [{"password": "AnyTlsScriptPw1"}],
      "padding_scheme": ["stop=6"],
      "sniff": true,
      "tls": {
        "enabled": true,
        "certificate_path": "/etc/one-sing/cert/cert.crt",
        "key_path": "/etc/one-sing/cert/private.key"
      }
    },
    {
      "type": "vless",
      "tag": "vless-in-16929",
      "listen": "::",
      "listen_port": 16929,
      "users": [{"uuid": "b2f0a2f4-1111-2222-3333-444455556666", "flow": "xtls-rprx-vision"}],
      "tls": {
        "enabled": true,
        "server_name": "www.microsoft.com",
        "reality": {
          "enabled": true,
          "handshake": {"server": "www.microsoft.com", "server_port": 443},
          "private_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
          "short_id": [""]
        }
      }
    }
  ],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}`

// seedLiveConfig stores a reported config.json with its hash, the way the hub's
// recordSingboxLocal does.
func seedLiveConfig(t *testing.T, api *Server, nodeID, cfg string) {
	t.Helper()
	snap := store.NodeSingboxLocal{
		LocalHash:    hashOf(cfg),
		ConfigPath:   "/etc/one-sing/config.json",
		LocalPresent: true,
		LocalVersion: "1.13.0-beta.7",
		LocalRunning: true,
		LocalUnit:    true,
		LocalUnitOK:  true,
		ConfigJSON:   cfg,
		// What the agent reports for every anytls inbound in the file: the PEM
		// behind certificate_path, keyed by port. Without it a hand-managed
		// anytls listener cannot be pinned and is skipped from subscriptions.
		AnytlsCerts: map[int]string{22039: testCertPEM, 28711: testCertPEM},
	}
	if err := api.Store.SetNodeSingboxLocal(nodeID, snap, api.Crypt); err != nil {
		t.Fatalf("seed live config: %v", err)
	}
}

func hashOf(s string) string {
	return singbox.ConfigHash([]byte(s))
}

// The subscription payload is the file: a `jq`-appended inbound is there with
// its own credential, without any adoption step.
func TestSubscriptionRendersLiveConfigFile(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	if err := api.Store.ReplaceNodeIPs(id, []store.IPRow{
		{IP: "203.0.113.9", Family: 4, Scope: "public", IsPrimary: true},
	}); err != nil {
		t.Fatalf("seed ips: %v", err)
	}
	setAnytlsPassword(t, srv, cookie)
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)
	// The panel's own bookkeeping: port + reported certificate.
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: "1.13.0-beta.7", DesiredVersion: "1.13.0-beta.7",
		Status: "running", Port: 22039, CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	subID, token := createSubscription(t, srv, cookie, "HK")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: id, Selected: true}})

	body := string(fetchSub(t, srv, token, "", "sing-box/1.11").Body)
	var doc struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("subscription is not JSON: %v\n%s", err, body)
	}
	byPort := map[int]map[string]any{}
	for _, o := range doc.Outbounds {
		if p, ok := o["server_port"].(float64); ok {
			byPort[int(p)] = o
		}
	}
	if len(byPort) != 3 {
		t.Fatalf("got %d outbounds, want 3 (the whole file): %v", len(byPort), byPort)
	}
	// The panel's own inbound: the file's credential and the reported
	// certificate for pinning, plus the node's plain name. The credential is
	// the file's — §9.3 实现修订 2026-09-17d: the probe is what actually serves
	// it, so a global password the file does not carry would hand clients a
	// secret that cannot authenticate. (On a node the panel installed, the
	// file *is* the global password, so nothing changes there.)
	panel := byPort[22039]
	if panel == nil || panel["type"] != "anytls" || panel["password"] != "panel-generated-pw" {
		t.Fatalf("panel inbound = %v", panel)
	}
	if panel["password"] == "shared-proxy-pw" {
		t.Fatal("the global anytls password replaced the credential the probe serves")
	}
	if panel["tag"] != "fobe-HK-Sharon" {
		t.Fatalf("panel inbound tag = %v, want the node name unsuffixed", panel["tag"])
	}
	// The hand-added anytls keeps the script's password and gets a suffixed name.
	added := byPort[28711]
	if added == nil || added["password"] != "AnyTlsScriptPw1" {
		t.Fatalf("hand-added anytls = %v", added)
	}
	if added["tag"] != "fobe-HK-Sharon · anytls:28711" {
		t.Fatalf("hand-added tag = %v", added["tag"])
	}
	// VLESS REALITY with the derived public key.
	vless := byPort[16929]
	if vless == nil || vless["uuid"] != "b2f0a2f4-1111-2222-3333-444455556666" {
		t.Fatalf("vless = %v", vless)
	}
	tls, _ := vless["tls"].(map[string]any)
	reality, _ := tls["reality"].(map[string]any)
	if reality == nil || reality["public_key"] == nil {
		t.Fatalf("vless reality = %v", tls)
	}
}

// The editor endpoint shows the file with credentials (this is the operator's
// own configuration, and he has to be able to edit it) plus its hash.
func TestGetNodeSingboxConfigListsEditableInbounds(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)

	r := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie, nil)
	if r.Status != 200 {
		t.Fatalf("GET config = %d %s", r.Status, r.Body)
	}
	var body struct {
		Reported bool `json:"reported"`
		Inbounds []struct {
			Type       string `json:"type"`
			Port       int    `json:"port"`
			Number     int    `json:"number"`
			Credential string `json:"credential"`
			HasCred    bool   `json:"has_cred"`
			Editable   bool   `json:"editable"`
			ServerName string `json:"server_name"`
			UUID       string `json:"uuid"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Reported || len(body.Inbounds) != 3 {
		t.Fatalf("body = %+v", body)
	}
	if body.Inbounds[0].Port != 22039 || body.Inbounds[0].Credential != "panel-generated-pw" {
		t.Fatalf("first inbound = %+v", body.Inbounds[0])
	}
	if body.Inbounds[2].UUID != "b2f0a2f4-1111-2222-3333-444455556666" || !body.Inbounds[2].Editable {
		t.Fatalf("vless view = %+v", body.Inbounds[2])
	}
}

// One edit: change a port, add an inbound, delete another. Everything the
// operator did not touch — including options fobe does not model (sniff,
// padding_scheme) — survives the merge, and the pushed document is the file.
func TestPutNodeSingboxConfigMergesEdits(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, DesiredVersion: "1.13.0-beta.7", Status: "running", Port: 22039,
		CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	body := map[string]any{
		"reported_hash": hashOf(liveConfigWithPanelInbound),
		"update": []map[string]any{
			{"number": 1, "type": "anytls", "port": 28712, "tag": "anytls-in-28712"},
		},
		"add": []map[string]any{
			{"type": "socks", "port": 1080, "credential": "sock-pw", "username": "sockuser", "new": true},
		},
		"delete": []int{2},
	}
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie, body)
	if r.Status != 200 {
		t.Fatalf("PUT config = %d %s", r.Status, r.Body)
	}

	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("pushed config: %v", err)
	}
	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("pushed config is not JSON: %v", err)
	}
	byPort := map[int]map[string]any{}
	for _, in := range doc.Inbounds {
		byPort[int(in["listen_port"].(float64))] = in
	}
	if len(byPort) != 3 {
		t.Fatalf("got %d inbounds, want 3: %v", len(byPort), byPort)
	}
	if byPort[22039] == nil {
		t.Fatalf("the untouched panel inbound disappeared: %v", byPort)
	}
	moved := byPort[28712]
	if moved == nil {
		t.Fatalf("the port edit did not land: %v", byPort)
	}
	// Options fobe does not model must survive an edit of that inbound.
	if _, ok := moved["sniff"]; !ok {
		t.Fatalf("hand-written option lost in the merge: %v", moved)
	}
	if _, ok := moved["padding_scheme"]; !ok {
		t.Fatalf("padding_scheme lost in the merge: %v", moved)
	}
	users, _ := moved["users"].([]any)
	if len(users) == 0 || users[0].(map[string]any)["password"] != "AnyTlsScriptPw1" {
		t.Fatalf("credential not inherited on a blank field: %v", moved["users"])
	}
	socks := byPort[1080]
	if socks == nil || socks["type"] != "socks" {
		t.Fatalf("added inbound missing: %v", byPort)
	}
	socksUsers, _ := socks["users"].([]any)
	if len(socksUsers) == 0 {
		t.Fatalf("added socks lost its users: %v", socks)
	}
	su := socksUsers[0].(map[string]any)
	if su["password"] != "sock-pw" || su["name"] != "sockuser" {
		t.Fatalf("added socks users = %v", su)
	}
	if byPort[16929] != nil {
		t.Fatalf("the deleted inbound is still there: %v", byPort[16929])
	}
}

// An edit built on an old report is refused: the file may have been changed by
// one-sing.sh in the meantime, and merging onto a stale base would silently
// drop whatever arrived since.
func TestPutNodeSingboxConfigRefusesStaleReport(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, DesiredVersion: "1.13.0-beta.7", Port: 22039, CertPEM: testCertPEM,
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": "stale-hash",
			"add":           []map[string]any{{"type": "socks", "port": 1081, "credential": "x", "new": true}},
		})
	if r.Status != http.StatusConflict || !bytes.Contains(r.Body, []byte("config_changed")) {
		t.Fatalf("stale edit = %d %s", r.Status, r.Body)
	}
	if _, err := api.Store.GetSetting("singbox_config:" + id); err == nil {
		t.Fatal("a refused edit must not push anything")
	}
}

// Two inbounds on one port would leave the second unbindable: the service
// flapping under Restart=always while the panel reports a version.
func TestPutNodeSingboxConfigRejectsDuplicatePort(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, DesiredVersion: "1.13.0-beta.7", Port: 22039, CertPEM: testCertPEM,
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": hashOf(liveConfigWithPanelInbound),
			"add":           []map[string]any{{"type": "socks", "port": 16929, "credential": "x", "new": true}},
		})
	if r.Status != http.StatusBadRequest || !bytes.Contains(r.Body, []byte("duplicate_port")) {
		t.Fatalf("duplicate port = %d %s", r.Status, r.Body)
	}
}

// A new inbound with no credential cannot be written: sing-box would reject the
// config and the probe would be left on a rolled-back file.
func TestPutNodeSingboxConfigRequiresCredentialForNewInbound(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, DesiredVersion: "1.13.0-beta.7", Port: 22039, CertPEM: testCertPEM,
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": hashOf(liveConfigWithPanelInbound),
			"add":           []map[string]any{{"type": "anytls", "port": 9999, "new": true}},
		})
	if r.Status != http.StatusBadRequest || !bytes.Contains(r.Body, []byte("credential_required")) {
		t.Fatalf("credential-less add = %d %s", r.Status, r.Body)
	}
}

// A node the panel installs from scratch still works: no file reported yet, so
// the managed pair renders (the editor model must not break the original flow).
func TestSubscriptionFallsBackToManagedPairWithoutReport(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "fresh", "m-fresh", "203.0.113.10")
	if err := api.Store.ReplaceNodeIPs(id, []store.IPRow{
		{IP: "203.0.113.10", Family: 4, Scope: "public", IsPrimary: true},
	}); err != nil {
		t.Fatalf("seed ips: %v", err)
	}
	setAnytlsPassword(t, srv, cookie)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: "1.13.0-beta.7", DesiredVersion: "1.13.0-beta.7",
		Status: "running", Port: 22039, CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}
	subID, token := createSubscription(t, srv, cookie, "fresh")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: id, Selected: true}})
	body := string(fetchSub(t, srv, token, "", "sing-box/1.11").Body)
	if !bytes.Contains([]byte(body), []byte("22039")) {
		t.Fatalf("managed node did not render: %s", body)
	}
}

// A node the panel has never installed has no inbound of its own. The panel
// does not ask which one to use: when it writes the file it takes the last
// anytls inbound (§9.3 实现修订 2026-09-17d). That port is what gives one
// subscription entry the node's plain name and its pinned certificate.
func TestPutConfigPicksTheNodesOwnPortFromTheFile(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)
	// No port on the node row: nothing was ever installed through the panel.
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{NodeID: id, Status: "absent"}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	// A plain edit — no "adopt" flag exists any more.
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": hashOf(liveConfigWithPanelInbound),
			"update": []map[string]any{{
				"number": 2, "type": "vless", "port": 16929, "tag": "vless-in-16929",
			}},
		})
	if r.Status != 200 {
		t.Fatalf("edit = %d %s", r.Status, r.Body)
	}
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if sb.Port != 28711 {
		t.Fatalf("node port = %d, want the file's last anytls listener 28711", sb.Port)
	}
	if !api.configEdited(id) {
		t.Fatal("the node is not marked as panel-edited: startup sync would overwrite the file")
	}
	// The edit touched the VLESS inbound only: both anytls credentials in the
	// file are exactly as the operator wrote them.
	cfg, _ := api.Store.GetSetting("singbox_config:" + id)
	for _, pw := range []string{"AnyTlsScriptPw1", "panel-generated-pw"} {
		if !bytes.Contains([]byte(cfg), []byte(pw)) {
			t.Fatalf("the merge rotated a credential it did not touch (%s):\n%s", pw, cfg)
		}
	}
}

// A node the panel never installed gets its port from the file it adopted, and
// startup reconciliation leaves it alone from then on.
func TestSyncSingboxConfigsSkipsEditedNodes(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLiveConfig(t, api, id, liveConfigWithPanelInbound)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, DesiredVersion: "1.13.0-beta.7", Status: "running", Port: 22039,
		CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}
	// The operator's own file, edited once through the panel.
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": hashOf(liveConfigWithPanelInbound),
			"add":           []map[string]any{{"type": "socks", "port": 1080, "credential": "sock-pw", "new": true}},
		})
	if r.Status != 200 {
		t.Fatalf("edit = %d %s", r.Status, r.Body)
	}
	before, _ := api.Store.GetSetting("singbox_config:" + id)

	// A template change must not rewrite a file the operator owns.
	if n := api.SyncSingboxConfigs(); n != 0 {
		t.Fatalf("SyncSingboxConfigs rewrote %d node(s), want 0 for an edited config", n)
	}
	after, _ := api.Store.GetSetting("singbox_config:" + id)
	if before != after {
		t.Fatal("the startup sync replaced an operator-edited config")
	}
}

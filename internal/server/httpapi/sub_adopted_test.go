// §9.3 实现修订 2026-09-17, subscription half: inbounds adopted from a probe's
// own config.json must reach clients with the credentials they already have,
// in both output formats.
package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/store"
)

func TestSubscriptionRendersAdoptedInbounds(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	if err := api.Store.ReplaceNodeIPs(id, []store.IPRow{
		{IP: "203.0.113.9", Family: 4, Scope: "public", IsPrimary: true},
	}); err != nil {
		t.Fatalf("seed ips: %v", err)
	}
	seedCachedVersion(t, api, "1.13.0-beta.7")
	seedLocalSnapshot(t, api, id, true)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: "1.13.0-beta.7", DesiredVersion: "1.13.0-beta.7",
		Status: "running", Port: 22039, CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+id+"/singbox/adopt", cookie,
		map[string]any{"inbounds": []map[string]any{
			{"type": "anytls", "port": 28711},
			{"type": "vless", "port": 16929},
		}})
	if r.Status != 200 {
		t.Fatalf("adopt = %d %s", r.Status, r.Body)
	}

	setAnytlsPassword(t, srv, cookie)
	subID, token := createSubscription(t, srv, cookie, "HK")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: id, Selected: true}})

	// --- sing-box JSON ---
	body := string(fetchSub(t, srv, token, "", "sing-box/1.11").Body)
	var doc struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("subscription is not JSON: %v\n%s", err, body)
	}
	// There are two anytls outbounds on purpose (the panel's own on 22039 and
	// the adopted one on 28711), so collect by (type, port) instead of by type.
	byPort := map[int]map[string]any{}
	for _, o := range doc.Outbounds {
		if p, ok := o["server_port"].(float64); ok {
			byPort[int(p)] = o
		}
	}
	panel := byPort[22039]
	if panel == nil || panel["type"] != "anytls" {
		t.Fatalf("the panel's own inbound is missing: %s", body)
	}
	if panel["password"] != "shared-proxy-pw" {
		t.Fatalf("the panel inbound used the wrong password: %v", panel["password"])
	}
	adopted, ok := byPort[28711]
	if !ok || adopted["type"] != "anytls" {
		t.Fatalf("the adopted anytls inbound must stay dialable: %s", body)
	}
	// §19.9: the adopted password is the script's, not the global one.
	if adopted["password"] != "AnyTlsScriptPw1" {
		t.Fatalf("adopted anytls password = %v, want the script's credential", adopted["password"])
	}
	vless := byPort[16929]
	if vless == nil || vless["uuid"] != "b2f0a2f4-1111-2222-3333-444455556666" {
		t.Fatalf("vless entry = %v: %s", vless, body)
	}
	tls, _ := vless["tls"].(map[string]any)
	reality, _ := tls["reality"].(map[string]any)
	if reality == nil || reality["public_key"] == nil || reality["public_key"] == "" {
		t.Fatalf("vless entry has no usable REALITY public key: %v", vless)
	}

	// --- clash YAML ---
	yaml := string(fetchSub(t, srv, token, "?format=clash", "clash-verge/1.5").Body)
	for _, want := range []string{
		"type: anytls",
		"type: vless",
		"reality-opts:",
		"port: 16929",
		"port: 22039",
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("clash output missing %q:\n%s", want, yaml)
		}
	}
	// The adopted credential must survive into the YAML too.
	if !strings.Contains(yaml, "AnyTlsScriptPw1") {
		t.Fatalf("clash output lost the adopted password:\n%s", yaml)
	}
}

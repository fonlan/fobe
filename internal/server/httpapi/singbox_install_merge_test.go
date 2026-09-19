// Regression cover for design §9.3 实现修订 2026-09-19.
//
// Install/update used to regenerate config.json from the template whole: a
// node the panel had installed and the operator had then extended in the
// editor (or one-sing.sh had) lost every non-template inbound — VLESS, socks,
// the route section — on the first press of 安装 / 更新. The file the probe
// reported is the base now; the only thing an install may add is the panel's
// own anytls inbound when the document does not already serve one.
package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// The user's scenario: a panel-installed node whose file carries the panel
// anytls plus a hand-added VLESS and socks listener (and a hand-written route
// section). Install a newer version on it and everything non-template must
// still be there — including the fields the operator hand-edited on the panel
// inbound itself.
const installMergeConfig = `{
  "log": {"level": "info", "timestamp": true},
  "dns": {"servers": [{"type": "local", "tag": "local-dns"}]},
  "inbounds": [
    {"type": "anytls", "tag": "anytls-in", "listen": "::", "listen_port": 22039,
     "users": [{"name": "default", "password": "panel-generated-pw"}],
     "padding_scheme": ["stop=6"],
     "tls": {"enabled": true, "server_name": "custom.example.com",
             "certificate_path": "/etc/one-sing/cert/cert.crt", "key_path": "/etc/one-sing/cert/private.key"}},
    {"type": "vless", "tag": "vless-in-16929", "listen": "::", "listen_port": 16929,
     "users": [{"name": "u", "uuid": "b2f0a2f4-1111-2222-3333-444455556666", "flow": "xtls-rprx-vision"}]},
    {"type": "socks", "tag": "socks-in-31080", "listen": "127.0.0.1", "listen_port": 31080,
     "users": [{"name": "lan", "password": "socks-pw"}]}
  ],
  "route": {"rules": [{"inbound": ["socks-in-31080"], "outbound": "direct"}]},
  "outbounds": [{"type": "direct", "tag": "direct"}]
}`

func TestInstallKeepsNonTemplateInbounds(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-install-merge", "203.0.113.51")
	seedLiveConfig(t, api, id, installMergeConfig)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: "1.13.0-beta.7", DesiredVersion: "1.13.0-beta.7",
		Status: "running", Port: 22039,
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	if r := installSingbox(t, srv.URL, cookie, id, "1.14.1"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	for _, want := range []string{
		`"tag": "vless-in-16929"`, `"listen_port": 16929`,
		`b2f0a2f4-1111-2222-3333-444455556666`, `"flow": "xtls-rprx-vision"`,
		`"tag": "socks-in-31080"`, `"listen_port": 31080`, `"password": "socks-pw"`,
		`"route"`, // the hand-written non-inbound section survives too
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("install lost %q from the operator's file:\n%s", want, cfg)
		}
	}
	// The panel inbound is left as the file has it: its credential was just read
	// back, and re-asserting cert paths or padding would undo the hand edits.
	if pw := singbox.InboundPasswordAtPort(cfg, 22039); pw != "panel-generated-pw" {
		t.Fatalf("install rotated the panel inbound's credential: %q", pw)
	}
	for _, want := range []string{`"server_name": "custom.example.com"`, `"stop=6"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("install overwrote a hand-edited panel-inbound field (%q):\n%s", want, cfg)
		}
	}
	if n := generationAudits(t, api, "panel-generated-pw"); n != 0 {
		t.Fatalf("install minted over a working credential (%d generation audits)", n)
	}
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if sb.DesiredVersion != "1.14.1" || sb.Port != 22039 {
		t.Fatalf("desired half = version %q port %d, want 1.14.1/22039", sb.DesiredVersion, sb.Port)
	}
	if sb.ConfigHash != hashOf(cfg) {
		t.Fatal("config_hash does not fingerprint the stored document")
	}
	// The file carries operator content: the startup template sync must keep
	// its hands off from here on.
	if edited, err := api.Store.GetSetting("singbox_edited:" + id); err != nil || edited != "1" {
		t.Fatalf("a merged file with operator content must be marked edited: %q %v", edited, err)
	}
}

// A panel-installed node that never touched its file reports exactly the
// template back. Install is still allowed to change the version — but the
// stored document stays the template's own bytes and the node keeps receiving
// template fixes from the startup sync.
func TestInstallOnATemplateIdenticalFileStaysSyncManaged(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	id, _ := seedNode(t, api, "plain", "m-install-plain", "203.0.113.52")
	template, err := singbox.BuildNodeConfig(22039, "shared-pw")
	if err != nil {
		t.Fatal(err)
	}
	seedLiveConfig(t, api, id, string(template))
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: "1.13.0-beta.7", DesiredVersion: "1.13.0-beta.7",
		Status: "running", Port: 22039,
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	if r := installSingbox(t, srv.URL, cookie, id, "1.14.1"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if cfg != string(template) {
		t.Fatalf("a template-identical file must store the template bytes:\n%s", cfg)
	}
	if _, err := api.Store.GetSetting("singbox_edited:" + id); err == nil {
		t.Fatal("a template-identical file must not be marked edited: template fixes would never reach it")
	}
	if n := api.SyncSingboxConfigs(); n != 0 {
		t.Fatalf("sync rewrote %d node(s) on a template-identical file, want 0", n)
	}
}

// A one-sing.sh node serving only VLESS: install adds the panel's own anytls
// inbound next to the script's listener (not instead of it), mints the
// credential for it, and the lifecycle table shows 添加中 until the probe
// reports.
func TestInstallInsertsThePanelInboundNextToTheScriptsInbounds(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-install-insert", "203.0.113.53")
	seedLiveConfig(t, api, id, `{
	  "inbounds": [
	    {"type": "vless", "tag": "vless-in-16929", "listen": "::", "listen_port": 16929,
	     "users": [{"uuid": "b2f0a2f4-1111-2222-3333-444455556666"}]}
	  ],
	  "outbounds": [{"type": "direct", "tag": "direct"}]
	}`)

	if r := installSingbox(t, srv.URL, cookie, id, "1.14.1"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if !strings.Contains(cfg, `"tag": "vless-in-16929"`) {
		t.Fatalf("install lost the script's VLESS listener:\n%s", cfg)
	}
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if !singbox.ValidPort(sb.Port) {
		t.Fatalf("panel inbound drew port %d", sb.Port)
	}
	if !strings.Contains(cfg, `"type": "anytls"`) || singbox.InboundPasswordAtPort(cfg, sb.Port) == "" {
		t.Fatalf("install did not add the panel's anytls inbound on %d:\n%s", sb.Port, cfg)
	}
	if n := generationAudits(t, api, singbox.InboundPasswordAtPort(cfg, sb.Port)); n != 1 {
		t.Fatalf("creating the panel inbound audited %d generations, want 1", n)
	}
	rows, err := api.Store.ListNodeSingboxInbounds(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Port != sb.Port || rows[0].Status != "pending" {
		t.Fatalf("inserted listener lifecycle rows = %+v, want one pending row on %d", rows, sb.Port)
	}
}

// The resolved port is occupied by a listener the panel does not own: refuse
// instead of building a config that cannot bind (the old full rewrite silently
// dropped that listener instead).
func TestInstallRefusesAPortAnotherInboundServes(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-install-conflict", "203.0.113.54")
	seedLiveConfig(t, api, id, `{
	  "inbounds": [
	    {"type": "vless", "tag": "vless-in-23456", "listen": "::", "listen_port": 23456,
	     "users": [{"uuid": "b2f0a2f4-1111-2222-3333-444455556666"}]}
	  ],
	  "outbounds": [{"type": "direct", "tag": "direct"}]
	}`)

	if r := installSingboxAtPort(t, srv.URL, cookie, id, 23456); r.Status != http.StatusBadRequest || r.errCode(t) != "duplicate_port" {
		t.Fatalf("install onto an occupied port = %d %s, want 400 duplicate_port", r.Status, r.Body)
	}
	if n := countGenerationAudits(t, api); n != 0 {
		t.Fatalf("a refused install minted a credential (%d generation audits)", n)
	}
	if _, err := api.Store.GetSetting("singbox_config:" + id); err == nil {
		t.Fatal("a config was written despite the port conflict")
	}
	if rows, err := api.Store.ListNodeSingboxInbounds(id); err != nil || len(rows) != 0 {
		t.Fatalf("a refused install left lifecycle rows: %+v %v", rows, err)
	}
}

// countGenerationAudits counts credential generations without a leak check —
// for refusals, where there is no credential value to guard.
func countGenerationAudits(t *testing.T, api *Server) int {
	t.Helper()
	entries, err := api.Store.ListAudit(200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == "anytls_password_generated" {
			n++
		}
	}
	return n
}

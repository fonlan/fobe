// Regression cover for design §9.2 实现修订 2026-09-18c.
//
// The panel's install/update path regenerates the *whole* config.json from the
// template, so the port it writes decides which listener the probe ends up
// serving. Taking that port from node_singbox.port (the management column)
// used to move a listener the operator had already moved in the editor back
// onto the old port — and, because the credential no longer sat there, to mint
// a fresh password over a working client. A probe that had never been installed
// by the panel has port 0 in that column and got a random one instead.
package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// The editor moves the listener, the probe applies and reports the new file,
// and then the operator presses 安装 / 更新: the listener must stay where it is.
func TestInstallAfterTheEditorMovedThePortKeepsIt(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-install-port-move", "203.0.113.41")
	seedLiveConfig(t, api, id, portMoveConfig)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: "1.13.0-beta.7", DesiredVersion: "1.13.0-beta.7",
		Status: "running", Port: 22039,
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	if r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": hashOf(portMoveConfig),
			"update": []map[string]any{
				{"number": 0, "type": "anytls", "tag": "anytls-in-22039", "port": 22040},
			},
		}); r.Status != http.StatusOK {
		t.Fatalf("port move = %d %s", r.Status, r.Body)
	}
	moved := strings.Replace(portMoveConfig, `"listen_port": 22039`, `"listen_port": 22040`, 1)
	// The probe applied the pushed document: it now serves 22040, while the
	// management column still names the port the operator moved away from.
	seedLiveConfig(t, api, id, moved)

	if r := installSingbox(t, srv.URL, cookie, id, "1.13.0-beta.7"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if !strings.Contains(cfg, `"listen_port": 22040`) {
		t.Fatalf("install lost the moved port:\n%s", cfg)
	}
	if strings.Contains(cfg, `"listen_port": 22039`) {
		t.Fatalf("install resurrected the retired port:\n%s", cfg)
	}
	if pw := singbox.InboundPasswordAtPort(cfg, 22040); pw != "panel-generated-pw" {
		t.Fatalf("install rotated the credential of a listener that kept its port: %q", pw)
	}
	if n := generationAudits(t, api, "panel-generated-pw"); n != 0 {
		t.Fatalf("install minted over a working credential (%d generation audits)", n)
	}
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if sb.Port != 22040 {
		t.Fatalf("management port = %d, want the one the probe serves (22040)", sb.Port)
	}
}

// A probe the panel never installed (one-sing.sh) reports its own anytls
// listener; the install must build on *that* port instead of drawing a random
// one, and must keep the script's password.
func TestInstallOnADiscoveredNodeKeepsTheReportsPort(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-install-discovered", "203.0.113.42")
	seedLocalSnapshot(t, api, id, true)

	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if sb.Port != 0 {
		t.Fatalf("a discovered node starts with no managed port, got %d", sb.Port)
	}

	if r := installSingbox(t, srv.URL, cookie, id, "1.13.0-beta.7"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if !strings.Contains(cfg, `"listen_port": 28711`) {
		t.Fatalf("install did not build on the listener the probe reports:\n%s", cfg)
	}
	if pw := singbox.InboundPasswordAtPort(cfg, 28711); pw != "AnyTlsScriptPw1" {
		t.Fatalf("install rotated the script listener's credential: %q", pw)
	}
	if n := generationAudits(t, api, "AnyTlsScriptPw1"); n != 0 {
		t.Fatalf("install minted over the script listener's credential (%d audits)", n)
	}
	sb, err = api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if sb.Port != 28711 {
		t.Fatalf("management port = %d, want 28711", sb.Port)
	}
}

// The genuinely-fresh case is unchanged: nothing on the probe and nothing in
// the management column still means a random high port.
func TestInstallOnAFreshNodeStillDrawsARandomPort(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	id, _ := seedNode(t, api, "fresh", "m-install-fresh", "203.0.113.43")

	if r := installSingbox(t, srv.URL, cookie, id, "1.13.0-beta.7"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if !singbox.ValidPort(sb.Port) {
		t.Fatalf("fresh install drew port %d", sb.Port)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if want := fmt.Sprintf(`"listen_port": %d`, sb.Port); !strings.Contains(cfg, want) {
		t.Fatalf("stored config does not serve the drawn port %d:\n%s", sb.Port, cfg)
	}
}

// The startup template sync regenerates the same document, so it must resolve
// the port the same way: a template fix may not move a listener the operator
// moved on the probe (outside the panel, so there is no editor marker).
func TestSyncSingboxConfigsKeepsTheReportsPort(t *testing.T) {
	_, api := newTestServer(t)
	nodeID, _ := seedNode(t, api, "sb", "m-sync-port", "198.51.100.12")

	const stale = `{"dns":{"servers":[{"tag":"local-dns","address":"local"}]},
	  "inbounds":[{"type":"anytls","listen_port":22039,"users":[{"name":"default","password":"shared-pw"}]}]}`
	if err := api.Store.SetSetting("singbox_config:"+nodeID, stale, false); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: nodeID, DesiredVersion: "1.15.0-alpha.4", Port: 22039,
		ConfigHash: singbox.ConfigHash([]byte(stale)), Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	// The probe reports a file whose anytls listener sits on another port: the
	// operator moved it there without going through the panel.
	seedLiveConfig(t, api, nodeID,
		`{"inbounds":[{"type":"anytls","listen_port":22040,"users":[{"name":"default","password":"shared-pw"}]}]}`)

	if n := api.SyncSingboxConfigs(); n != 1 {
		t.Fatalf("sync updated %d nodes, want 1", n)
	}
	stored, err := api.Store.GetSetting("singbox_config:" + nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := singbox.BuildNodeConfig(22040, "shared-pw")
	if err != nil {
		t.Fatal(err)
	}
	if stored != string(want) {
		t.Fatalf("sync rebuilt on the wrong port:\n got %s\nwant %s", stored, want)
	}
	sb, err := api.Store.GetNodeSingbox(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Port != 22040 {
		t.Fatalf("management port = %d, want the reported 22040", sb.Port)
	}
}

// §9.3 实现修订 2026-09-17d: local sing-box discovery.
//
// The scenario these tests pin down is the one that motivated the feature: a
// probe that one-sing.sh already manages. The agent reports its config.json and
// the panel serves clients out of that same file. There is no adoption step to
// take — nothing here writes to the probe, and a listener the script created
// keeps handing out the credential its clients already hold.
package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// oneSingLocalConfig is one-sing.sh's output as the agent would report it.
const oneSingLocalConfig = `{
  "log": {"level": "info", "timestamp": true},
  "inbounds": [
    {
      "type": "anytls",
      "tag": "anytls-in-28711",
      "listen": "::",
      "listen_port": 28711,
      "users": [{"password": "AnyTlsScriptPw1"}],
      "padding_scheme": ["stop=6"],
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

// seedLocalSnapshot stores what the agent's discovery frame would have stored,
// encrypted exactly like the hub does. The certificate bytes ride along the way
// the agent reports them: config.json only names a *path*, and a client can
// only pin bytes.
func seedLocalSnapshot(t *testing.T, api *Server, nodeID string, running bool) {
	t.Helper()
	snap := store.NodeSingboxLocal{
		LocalHash:    "cafe",
		ConfigPath:   "/etc/one-sing/config.json",
		LocalPresent: true,
		LocalVersion: "1.13.0-beta.7",
		LocalRunning: running,
		LocalUnit:    running,
		LocalUnitOK:  true,
		ConfigJSON:   oneSingLocalConfig,
		AnytlsCerts:  map[int]string{28711: testCertPEM},
	}
	if err := api.Store.SetNodeSingboxLocal(nodeID, snap, api.Crypt); err != nil {
		t.Fatalf("seed local snapshot: %v", err)
	}
}

func TestSingboxDiscoveryShowsLocalInstallWithoutManagedState(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLocalSnapshot(t, api, id, true)

	r := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+id+"/singbox", cookie, nil)
	if r.Status != 200 {
		t.Fatalf("GET singbox = %d %s", r.Status, r.Body)
	}
	var body struct {
		Singbox *struct {
			Version        string `json:"version"`
			DesiredVersion string `json:"desired_version"`
			Local          *struct {
				Present  bool   `json:"present"`
				Running  bool   `json:"running"`
				Version  string `json:"version"`
				Adopted  int    `json:"adopted"`
				Inbounds []struct {
					Type      string `json:"type"`
					Port      int    `json:"port"`
					CredSet   bool   `json:"cred_set"`
					Supported bool   `json:"supported"`
				} `json:"inbounds"`
			} `json:"local"`
		} `json:"singbox"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Body)
	}
	sb := body.Singbox
	if sb == nil || sb.Local == nil {
		t.Fatalf("no discovery in the payload: %s", r.Body)
	}
	// fobe manages nothing here — that is the whole point. The panel must show
	// the probe's own sing-box anyway.
	if sb.DesiredVersion != "" {
		t.Fatalf("desired_version = %q, want empty on a probe fobe does not manage", sb.DesiredVersion)
	}
	if !sb.Local.Present || !sb.Local.Running || sb.Local.Version != "1.13.0-beta.7" {
		t.Fatalf("discovery = %+v", sb.Local)
	}
	if sb.Local.Adopted != 0 {
		t.Fatalf("adopted = %d, want 0 (nothing is imported any more)", sb.Local.Adopted)
	}
	if len(sb.Local.Inbounds) != 2 {
		t.Fatalf("inbounds = %+v", sb.Local.Inbounds)
	}
	if !sb.Local.Inbounds[0].CredSet || !sb.Local.Inbounds[1].CredSet {
		t.Fatalf("credential presence lost: %+v", sb.Local.Inbounds)
	}
	// Secrets never travel to the panel.
	for _, secret := range []string{"AnyTlsScriptPw1", "b2f0a2f4-1111-2222-3333-444455556666"} {
		if bytes.Contains(r.Body, []byte(secret)) {
			t.Fatalf("discovery payload leaked %q", secret)
		}
	}
}

// proxyNodeAt finds the rendered entry for one port.
func proxyNodeAt(nodes []singbox.ProxyNode, port int) *singbox.ProxyNode {
	for i := range nodes {
		if nodes[i].Port == port {
			return &nodes[i]
		}
	}
	return nil
}

// The editor renders every inbound from the probe's own file, with that
// listener's credential, without electing a primary entry.
func TestLocalInboundsRenderFromTheFileAsEqualEntries(t *testing.T) {
	_, api := newTestServer(t)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedLocalSnapshot(t, api, id, true)

	// Nothing is managed: the file alone makes both inbounds dialable.
	nodes, ok := api.liveNodesFor(id, "HK-Sharon", "203.0.113.9")
	if !ok {
		t.Fatal("the reported config did not render")
	}
	if len(nodes) != 2 {
		t.Fatalf("rendered %d nodes from the file, want 2: %+v", len(nodes), nodes)
	}
	anytls := proxyNodeAt(nodes, 28711)
	if anytls == nil {
		t.Fatalf("anytls inbound vanished: %+v", nodes)
	}
	// Plain node name: 2026-09-17j removed the protocol:port suffix — the
	// renderers, not the node builder, own duplicate-name dedup.
	if anytls.Name != "HK-Sharon" {
		t.Fatalf("anytls name = %q, want the plain node name", anytls.Name)
	}
	if anytls.Password != "AnyTlsScriptPw1" {
		t.Fatalf("password = %q, want the file's (rotating it breaks every client holding the script's URI)", anytls.Password)
	}
	// Reading a node's file is not a moment that mints anything: the credential
	// belongs to that inbound and nowhere else (§10.1 实现修订 2026-09-17e).
	if _, err := api.Store.GetSetting("anytls_password"); err == nil {
		t.Fatal("rendering the file minted a global anytls password")
	}
	if anytls.CertPEM != testCertPEM {
		t.Fatalf("certificate = %q, want the bytes the probe reported for that port", anytls.CertPEM)
	}
	other := proxyNodeAt(nodes, 16929)
	if other == nil || other.Name != "HK-Sharon" {
		t.Fatalf("vless inbound must keep the plain name too: %+v", nodes)
	}
	if other.RealityPublicKey == "" {
		t.Fatalf("vless reality key not derived: %+v", other)
	}
}

// A node adopted before the editor model existed still carries its inbounds:
// `extra_inbounds` is read on every regeneration even though nothing writes it
// any more (§9.3 实现修订 2026-09-17d). Losing them would take the operator's
// services down on the next install or port change.
func TestLegacyAdoptedInboundsSurviveRegeneration(t *testing.T) {
	_, api := newTestServer(t)
	id, _ := seedNode(t, api, "legacy", "m-legacy", "203.0.113.10")
	extras := []singbox.ExtraInbound{{
		Type: singbox.ProtoVLESS, Tag: "vless-in-16929", ListenPort: 16929,
		Users: []singbox.ExtraUser{{UUID: "b2f0a2f4-1111-2222-3333-444455556666"}},
	}}
	if err := api.Store.SetNodeSingboxExtraInbounds(id, extras, api.Crypt); err != nil {
		t.Fatalf("seed extras: %v", err)
	}
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if err := api.applySingboxDesired(id, sb, "1.13.0-beta.7", 22039, "installing"); err != nil {
		t.Fatalf("apply desired: %v", err)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if !strings.Contains(cfg, `"listen_port": 16929`) {
		t.Fatalf("regeneration dropped the adopted inbound:\n%s", cfg)
	}
	if !strings.Contains(cfg, `"listen_port": 22039`) {
		t.Fatalf("regeneration dropped the panel's own inbound:\n%s", cfg)
	}
}

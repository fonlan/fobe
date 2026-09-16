// §9.3 实现修订 2026-09-17: local sing-box discovery and adoption.
//
// The scenario these tests pin down is the one that motivated the feature: a
// probe that one-sing.sh already manages. The agent reports its config.json,
// the panel must be able to see it *without* fobe managing anything yet, and
// clicking adopt must put those inbounds into fobe's desired state with their
// ports and credentials intact.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
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
// encrypted exactly like the hub does.
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
	// A cached version is what adoption will declare; the discovery endpoint
	// itself must not need one.
	seedCachedVersion(t, api, "1.13.0-beta.7")

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
					Adoptable bool   `json:"adoptable"`
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
		t.Fatalf("adopted = %d, want 0 before any adoption", sb.Local.Adopted)
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

func errorsIsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

func TestSingboxAdoptMergesLocalInboundsIntoDesiredState(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedCachedVersion(t, api, "1.13.0-beta.7")
	seedLocalSnapshot(t, api, id, true)

	// A node fobe already manages: the port exists, nothing adopted yet.
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

	// The desired config is what the agent will apply: it must contain the
	// panel's own inbound *and* the operator's two, with his credentials.
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("stored config is not JSON: %v", err)
	}
	byPort := map[int]map[string]any{}
	for _, in := range doc.Inbounds {
		byPort[int(in["listen_port"].(float64))] = in
	}
	if len(byPort) != 3 {
		t.Fatalf("generated config has %d inbounds, want 3: %v", len(byPort), byPort)
	}
	anytls := byPort[28711]
	if anytls == nil || anytls["type"] != "anytls" {
		t.Fatalf("adopted anytls inbound missing: %v", byPort)
	}
	users, _ := anytls["users"].([]any)
	if len(users) == 0 {
		t.Fatalf("adopted anytls lost its users: %v", anytls)
	}
	if u, _ := users[0].(map[string]any); u["password"] != "AnyTlsScriptPw1" {
		t.Fatalf("adopted anytls password = %v (must not be rotated)", users[0])
	}
	if _, ok := anytls["padding_scheme"]; !ok {
		t.Fatalf("unmodelled inbound options were dropped: %v", anytls)
	}
	vless := byPort[16929]
	if vless == nil {
		t.Fatalf("adopted vless inbound missing: %v", byPort)
	}
	tls, _ := vless["tls"].(map[string]any)
	reality, _ := tls["reality"].(map[string]any)
	if reality == nil || reality["public_key"] == "" {
		t.Fatalf("reality public key was not derived: %v", vless)
	}

	// Adoption must NOT rotate the global credential: the panel's inbound keeps
	// the password the rest of the fleet shares, and the script's password
	// travels with the adopted inbound instead (see the subscription test).
	override, err := api.Store.GetNodeSingboxPasswordOverride(id)
	if err != nil && !errorsIsNotFound(err) {
		t.Fatalf("read override: %v", err)
	}
	if override != "" {
		t.Fatalf("adoption wrote a password override (%q): the panel's inbound must stay on the global password", override)
	}
	panel := byPort[22039]
	panelUsers, _ := panel["users"].([]any)
	if len(panelUsers) == 0 || panelUsers[0].(map[string]any)["password"] == "AnyTlsScriptPw1" {
		t.Fatalf("the panel's inbound was rotated to the script password: %v", panel)
	}

	// And the desired version is declared, or the probe would never converge.
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if sb.DesiredVersion != "1.13.0-beta.7" {
		t.Fatalf("desired_version = %q", sb.DesiredVersion)
	}
	if sb.ConfigHash != singbox.ConfigHash([]byte(cfg)) {
		t.Fatalf("config hash does not describe the stored config")
	}
	if !sb.ExtrasPresent {
		t.Fatal("ExtrasPresent = false after adoption")
	}
}

// Re-adopting must add to the set, not replace it: the panel shows the
// inbounds it knows about, and clicking a second one cannot drop the first.
func TestSingboxAdoptIsAdditive(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedCachedVersion(t, api, "1.13.0-beta.7")
	seedLocalSnapshot(t, api, id, true)
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, DesiredVersion: "1.13.0-beta.7", Status: "running", Port: 22039,
		CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}

	adopt := func(picks []map[string]any) {
		t.Helper()
		r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+id+"/singbox/adopt", cookie,
			map[string]any{"inbounds": picks})
		if r.Status != 200 {
			t.Fatalf("adopt = %d %s", r.Status, r.Body)
		}
	}
	adopt([]map[string]any{{"type": "vless", "port": 16929}})
	adopt([]map[string]any{{"type": "anytls", "port": 28711}})

	extras, err := api.Store.GetNodeSingboxExtraInbounds(id, api.Crypt)
	if err != nil {
		t.Fatalf("read adopted: %v", err)
	}
	if len(extras) != 2 {
		t.Fatalf("adopted set = %d entries, want 2: %+v", len(extras), extras)
	}
}

// Adoption refuses what it cannot serve instead of writing a half state.
func TestSingboxAdoptRejectsWhatItCannotDo(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedCachedVersion(t, api, "1.13.0-beta.7")

	// 1. no discovery reported yet
	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+id+"/singbox/adopt", cookie,
		map[string]any{"inbounds": []map[string]any{{"type": "vless", "port": 16929}}})
	if r.Status != http.StatusBadRequest || !bytes.Contains(r.Body, []byte("no_local_config")) {
		t.Fatalf("adopt without a snapshot = %d %s", r.Status, r.Body)
	}

	seedLocalSnapshot(t, api, id, true)

	// 2. a port that is not in the file at all
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+id+"/singbox/adopt", cookie,
		map[string]any{"inbounds": []map[string]any{{"type": "socks", "port": 19999}}})
	if r.Status != http.StatusBadRequest || !bytes.Contains(r.Body, []byte("nothing_to_adopt")) {
		t.Fatalf("adopt of an unknown inbound = %d %s", r.Status, r.Body)
	}

	// 3. a pending removal is not silently cancelled by an adoption
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, DesiredUninstall: true, Port: 28711,
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+id+"/singbox/adopt", cookie,
		map[string]any{"inbounds": []map[string]any{{"type": "vless", "port": 16929}}})
	if r.Status != http.StatusBadRequest || !bytes.Contains(r.Body, []byte("uninstall_pending")) {
		t.Fatalf("adopt while uninstalling = %d %s", r.Status, r.Body)
	}
}

// A probe the panel never installed has no inbound port of its own. Adoption
// takes over the anytls inbound that is already there rather than forcing the
// operator to install a second one — same port, same service, panel-managed.
func TestSingboxAdoptPromotesLocalAnytlsPort(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, _ := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")
	seedCachedVersion(t, api, "1.13.0-beta.7")
	seedLocalSnapshot(t, api, id, true)

	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+id+"/singbox/adopt", cookie,
		map[string]any{"inbounds": []map[string]any{
			{"type": "anytls", "port": 28711},
			{"type": "vless", "port": 16929},
		}})
	if r.Status != 200 {
		t.Fatalf("adopt = %d %s", r.Status, r.Body)
	}
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get singbox: %v", err)
	}
	if sb.Port != 28711 {
		t.Fatalf("panel port = %d, want the adopted anytls port 28711", sb.Port)
	}
	if sb.DesiredVersion != "1.13.0-beta.7" {
		t.Fatalf("desired_version = %q", sb.DesiredVersion)
	}
	extras, err := api.Store.GetNodeSingboxExtraInbounds(id, api.Crypt)
	if err != nil {
		t.Fatalf("read adopted: %v", err)
	}
	// The anytls inbound is the panel's own now (regenerated from the template),
	// so only the VLESS one is stored as an adopted extra.
	if len(extras) != 1 || extras[0].Type != "vless" || extras[0].ListenPort != 16929 {
		t.Fatalf("adopted extras = %+v", extras)
	}
	cfg, err := api.Store.GetSetting("singbox_config:" + id)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if !bytes.Contains([]byte(cfg), []byte(`"listen_port": 28711`)) {
		t.Fatalf("the panel's inbound is not on the adopted port:\n%s", cfg)
	}
}

// seedCachedVersion writes a fake cached release into the DL directory, which
// is what adoption declares as the desired version.
func seedCachedVersion(t *testing.T, api *Server, version string) {
	t.Helper()
	if api.DLDir == "" {
		api.DLDir = t.TempDir()
	}
	dir := api.DLDir + "/singbox/" + version
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(dir+"/linux-amd64", []byte("fake"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

package singbox

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func base64URL(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
func base64Std(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

// oneSingConfig is the shape one-sing.sh actually writes (HK-Sharon,
// 2026-09-17): an anytls inbound with the script's padding scheme and a VLESS
// REALITY inbound. Keys are generated, the private key is a real X25519 one
// (32 bytes, base64url) so the public-key derivation is exercised for real.
const oneSingConfig = `{
  "log": {"level": "info", "timestamp": true},
  "ntp": {"enabled": true, "server": "time.apple.com"},
  "inbounds": [
    {
      "type": "anytls",
      "tag": "anytls-in-28711",
      "listen": "::",
      "listen_port": 28711,
      "users": [{"password": "AnyTlsScriptPw1"}],
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

func TestParseLocalInbounds(t *testing.T) {
	list, err := ParseLocalInbounds(oneSingConfig)
	if err != nil {
		t.Fatalf("ParseLocalInbounds: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d inbounds, want 2", len(list))
	}
	if list[0].Type != ProtoAnytls || list[0].Port != 28711 || list[0].Tag != "anytls-in-28711" {
		t.Fatalf("first inbound = %+v", list[0])
	}
	if list[1].Type != ProtoVLESS || list[1].Port != 16929 {
		t.Fatalf("second inbound = %+v", list[1])
	}

	summaries := Summaries(list)
	if !summaries[0].CredSet || !summaries[0].Adoptable {
		t.Fatalf("anytls summary = %+v", summaries[0])
	}
	if !summaries[1].CredSet {
		t.Fatalf("vless summary lost its uuid: %+v", summaries[1])
	}
	// The panel view must never carry the credential itself.
	raw, _ := json.Marshal(summaries)
	for _, secret := range []string{"AnyTlsScriptPw1", "b2f0a2f4-1111-2222-3333-444455556666"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("summary leaked %q: %s", secret, raw)
		}
	}
}

func TestParseLocalInboundsRejectsNonJSON(t *testing.T) {
	if _, err := ParseLocalInbounds("{not json"); err == nil {
		t.Fatal("want an error for a config that is not JSON")
	}
	// An empty config is "nothing to report", not a failure.
	if list, err := ParseLocalInbounds(""); err != nil || len(list) != 0 {
		t.Fatalf("empty config: %v %v", list, err)
	}
}

// Inbounds fobe cannot adopt stay in the inventory with adoptable=false, so
// the operator sees that they were noticed rather than silently dropped.
func TestUnsupportedInboundIsListedNotAdoptable(t *testing.T) {
	cfg := `{"inbounds":[{"type":"mixed","tag":"mixed-in","listen_port":1080}]}`
	list, err := ParseLocalInbounds(cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d inbounds, want 1", len(list))
	}
	s := list[0].Summary()
	s.Adoptable = false
	if Summaries(list)[0].Adoptable {
		t.Fatal("mixed must not be adoptable")
	}
	if s.Type != "mixed" || s.Port != 1080 {
		t.Fatalf("summary = %+v", s)
	}
}

// Adoption derives the REALITY public key client-side; without it the imported
// VLESS inbound cannot be dialled at all.
func TestRealityPublicKeyDerivation(t *testing.T) {
	// Private key = 0x0102...20; the X25519 public half is a fixed value, so
	// this doubles as a known-answer test for the derivation path.
	priv := make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i + 1)
	}
	encoded := base64URL(priv)
	pub, err := realityPublicKey(encoded)
	if err != nil {
		t.Fatalf("realityPublicKey: %v", err)
	}
	if len(pub) != 43 {
		t.Fatalf("public key %q has length %d, want 43 (32 bytes base64url)", pub, len(pub))
	}
	again, err := realityPublicKey(encoded)
	if err != nil || again != pub {
		t.Fatalf("derivation is not deterministic: %q vs %q (%v)", pub, again, err)
	}

	// A padded standard-base64 key must be accepted too (hand-edited configs).
	standard := base64Std(priv)
	if _, err := realityPublicKey(standard); err != nil {
		t.Fatalf("standard base64 key rejected: %v", err)
	}
	if _, err := realityPublicKey("too-short"); err == nil {
		t.Fatal("want an error for a key that is not 32 bytes")
	}
}

func TestExtraInboundFromKeepsCredentialAndDerivesReality(t *testing.T) {
	list, err := ParseLocalInbounds(oneSingConfig)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	anytls := ExtraInboundFrom(list[0])
	if anytls.ListenPort != 28711 {
		t.Fatalf("anytls extra = %+v", anytls)
	}
	// The anytls credential lives on the user, not on the inbound: sing-box
	// rejects a top-level `password` on an anytls inbound, and gate ① caught a
	// generated config doing exactly that on a real probe (2026-09-17).
	if len(anytls.Users) != 1 || anytls.Users[0].Password != "AnyTlsScriptPw1" {
		t.Fatalf("anytls users = %+v", anytls.Users)
	}
	if anytls.Password != "" {
		t.Fatalf("anytls extra carries a top-level password: %q", anytls.Password)
	}
	if anytls.TLS == nil || anytls.TLS.CertificatePath != CertDir+"/"+CertFile {
		t.Fatalf("anytls tls = %+v", anytls.TLS)
	}

	vless := ExtraInboundFrom(list[1])
	if vless.TLS == nil || vless.TLS.Reality == nil {
		t.Fatalf("vless extra lost reality: %+v", vless)
	}
	if vless.TLS.Reality.PublicKey == "" {
		t.Fatal("vless extra has no derived public key")
	}
	if len(vless.Users) != 1 || vless.Users[0].UUID != "b2f0a2f4-1111-2222-3333-444455556666" {
		t.Fatalf("vless users = %+v", vless.Users)
	}
}

// The whole point of adoption: the operator's inbounds survive config
// generation, alongside the anytls inbound the panel owns.
func TestBuildNodeConfigWithInboundsKeepsAdoptedInbounds(t *testing.T) {
	list, err := ParseLocalInbounds(oneSingConfig)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	extras := []ExtraInbound{ExtraInboundFrom(list[0]), ExtraInboundFrom(list[1])}

	raw, err := BuildNodeConfigWithInbounds(22039, "GlobalPanelPw", extras)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("generated config is not JSON: %v", err)
	}
	byPort := map[int]map[string]any{}
	for _, in := range doc.Inbounds {
		byPort[intField(in["listen_port"])] = in
	}
	if len(byPort) != 3 {
		t.Fatalf("got %d inbounds (%v), want 3: anytls panel + 2 adopted", len(byPort), byPort)
	}
	if byPort[22039]["type"] != ProtoAnytls {
		t.Fatalf("panel inbound missing: %v", byPort[22039])
	}
	if byPort[28711] == nil || byPort[16929] == nil {
		t.Fatalf("adopted inbounds missing: %v", byPort)
	}
	// The script's own anytls password must survive, or every client handed
	// the script's URI stops authenticating the moment fobe takes over.
	users, _ := byPort[28711]["users"].([]any)
	if len(users) == 0 {
		t.Fatalf("adopted anytls lost its users: %v", byPort[28711])
	}
	if u, _ := users[0].(map[string]any); u["password"] != "AnyTlsScriptPw1" {
		t.Fatalf("adopted anytls password = %v", users[0])
	}
	// A generated inbound must carry no key sing-box does not know: gate ① is
	// the only thing between a bad generator and a rollback on a live probe, and
	// it already fired once for exactly this field (2026-09-17).
	for port, in := range byPort {
		if port == 16929 || port == 28711 {
			if _, ok := in["password"]; ok {
				t.Fatalf("inbound %d carries a top-level password: %v", port, in)
			}
		}
	}
}

// A generated inbound always wins its port: a stale adopted copy must never
// shadow the one the panel owns, or "install" would look like a no-op.
func TestBuildNodeConfigWithInboundsPortCollision(t *testing.T) {
	extras := []ExtraInbound{{
		Type: ProtoAnytls, Tag: "stale", Listen: "::", ListenPort: 22039,
		Users: []ExtraUser{{Password: "stale-pw"}},
	}}
	raw, err := BuildNodeConfigWithInbounds(22039, "GlobalPanelPw", extras)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Inbounds) != 1 {
		t.Fatalf("got %d inbounds, want 1 (collision dropped)", len(doc.Inbounds))
	}
	if doc.Inbounds[0]["tag"] != "anytls-in" {
		t.Fatalf("panel inbound was shadowed: %v", doc.Inbounds[0])
	}
}

// Without extras the output must stay byte-identical to the plain builder —
// the config hash of every existing node depends on it.
func TestBuildNodeConfigWithInboundsNoExtrasIsUnchanged(t *testing.T) {
	base, err := BuildNodeConfig(22039, "pw")
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	withEmpty, err := BuildNodeConfigWithInbounds(22039, "pw", nil)
	if err != nil {
		t.Fatalf("with extras: %v", err)
	}
	if string(base) != string(withEmpty) {
		t.Fatalf("config changed with no extras:\n%s\n%s", base, withEmpty)
	}
}

// The generated config is applied on a live probe after a single `sing-box
// check`, so every key in it has to be one sing-box knows. Both bugs found on
// the real machine (2026-09-17) were exactly this: a `password` on a vless
// inbound and a `public_key` inside the server-side reality object.
func TestGeneratedInboundCarriesNoUnknownKeys(t *testing.T) {
	list, err := ParseLocalInbounds(oneSingConfig)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := BuildNodeConfigWithInbounds(22039, "pw", []ExtraInbound{
		ExtraInboundFrom(list[0]), ExtraInboundFrom(list[1]),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, in := range doc.Inbounds {
		typ, _ := in["type"].(string)
		for k := range in {
			if !allowedInboundKeys[k] {
				t.Fatalf("%s inbound carries an unknown key %q: %v", typ, k, in)
			}
		}
		// A per-user credential must never also appear at the top level.
		if typ != ProtoShadowsocks {
			if _, ok := in["password"]; ok {
				t.Fatalf("%s inbound carries a top-level password: %v", typ, in)
			}
		}
		tls, _ := in["tls"].(map[string]any)
		for k := range tls {
			if !allowedTLSKeys[k] {
				t.Fatalf("%s tls carries an unknown key %q", typ, k)
			}
		}
		if reality, ok := tls["reality"].(map[string]any); ok {
			for k := range reality {
				if !allowedRealityKeys[k] {
					t.Fatalf("%s reality carries an unknown key %q", typ, k)
				}
			}
			if _, ok := reality["public_key"]; ok {
				t.Fatalf("the client-side public key reached the server config: %v", reality)
			}
			if reality["private_key"] != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
				t.Fatalf("reality private key was lost: %v", reality)
			}
		}
	}
	// The public key still has to be available for the *subscription* side.
	vless := ExtraInboundFrom(list[1])
	if vless.TLS == nil || vless.TLS.Reality == nil || vless.TLS.Reality.PublicKey == "" {
		t.Fatal("adoption did not retain the reality public key for clients")
	}
}

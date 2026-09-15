package singbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildNodeConfig(t *testing.T) {
	raw, err := BuildNodeConfig(23456, "pw-secret")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var cfg struct {
		Log struct {
			Level string `json:"level"`
		} `json:"log"`
		DNS struct {
			Servers []map[string]any `json:"servers"`
		} `json:"dns"`
		Inbounds []struct {
			Type       string `json:"type"`
			Listen     string `json:"listen"`
			ListenPort int    `json:"listen_port"`
			Users      []struct {
				Password string `json:"password"`
			} `json:"users"`
			TLS struct {
				Enabled         bool   `json:"enabled"`
				ServerName      string `json:"server_name"`
				CertificatePath string `json:"certificate_path"`
				KeyPath         string `json:"key_path"`
			} `json:"tls"`
		} `json:"inbounds"`
		Outbounds []struct {
			Type string `json:"type"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, raw)
	}
	if cfg.Log.Level == "" || len(cfg.DNS.Servers) == 0 {
		t.Fatalf("log/dns sections missing: %s", raw)
	}
	if len(cfg.Inbounds) != 1 || cfg.Inbounds[0].Type != "anytls" || cfg.Inbounds[0].ListenPort != 23456 {
		t.Fatalf("inbound wrong: %+v", cfg.Inbounds)
	}
	in := cfg.Inbounds[0]
	if in.Listen != "::" || len(in.Users) != 1 || in.Users[0].Password != "pw-secret" {
		t.Fatalf("inbound users wrong: %+v", in)
	}
	if !in.TLS.Enabled || in.TLS.ServerName != ServerName ||
		in.TLS.CertificatePath != CertDir+"/cert.pem" || in.TLS.KeyPath != CertDir+"/key.pem" {
		t.Fatalf("inbound tls wrong: %+v", in.TLS)
	}
	if len(cfg.Outbounds) == 0 || cfg.Outbounds[0].Type != "direct" {
		t.Fatalf("outbound missing: %+v", cfg.Outbounds)
	}

	// identical input → identical bytes and stable hash (§9.1 dedupe)
	raw2, _ := BuildNodeConfig(23456, "pw-secret")
	if !bytesEqual(raw, raw2) {
		t.Fatal("config not byte-stable for identical input")
	}
	if ConfigHash(raw) != sha256Hex(raw) {
		t.Fatal("config hash mismatch")
	}

	if _, err := BuildNodeConfig(80, "pw"); err == nil {
		t.Fatal("privileged port accepted")
	}
	if _, err := BuildNodeConfig(70000, "pw"); err == nil {
		t.Fatal("out-of-range port accepted")
	}
}

func TestRandomPort(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		p, err := RandomPort()
		if err != nil {
			t.Fatalf("random port: %v", err)
		}
		if p < MinPort || p > MaxPort {
			t.Fatalf("port %d outside %d-%d", p, MinPort, MaxPort)
		}
		seen[p] = true
	}
	if len(seen) < 10 {
		t.Fatalf("random port not varied: %d distinct in 50 draws", len(seen))
	}
}

func TestRenderNodes(t *testing.T) {
	nodes := []ProxyNode{
		{ID: "abc", Name: "东京 01", Server: "203.0.113.5", Port: 20001, Password: "pw", CertPEM: "-----BEGIN CERTIFICATE-----\nA\n-----END CERTIFICATE-----\n"},
		{ID: "def", Name: "东京 01", Server: "203.0.113.6", Port: 20002, Password: "pw", CertPEM: "-----BEGIN CERTIFICATE-----\nB\n-----END CERTIFICATE-----\n"},
		// missing cert → skipped, pinning is mandatory (§9.3)
		{ID: "ghi", Name: "no-cert", Server: "203.0.113.7", Port: 20003, Password: "pw"},
	}

	j, err := RenderNodesJSON(nodes)
	if err != nil {
		t.Fatalf("render json: %v", err)
	}
	if strings.Contains(j, "no-cert") {
		t.Fatal("node without certificate must be skipped")
	}
	var outs []map[string]any
	// the items are bare objects; wrap in an array to unmarshal
	if err := json.Unmarshal([]byte("["+j+"]"), &outs); err != nil {
		t.Fatalf("json items invalid: %v\n%s", err, j)
	}
	if len(outs) != 2 {
		t.Fatalf("json items = %d, want 2", len(outs))
	}
	if outs[0]["tag"] != "fobe-abc" || outs[1]["tag"] == outs[0]["tag"] {
		t.Fatalf("tags must be unique per node: %s", j)
	}

	y := RenderNodesYAML(nodes)
	if strings.Contains(y, "no-cert") {
		t.Fatal("node without certificate must be skipped (yaml)")
	}
	// duplicate names get a numeric suffix (clash requires unique names)
	if !strings.Contains(y, `"东京 01 #2"`) {
		t.Fatalf("duplicate name not disambiguated:\n%s", y)
	}
	if !strings.Contains(y, "    port: 20002") {
		t.Fatalf("yaml port wrong:\n%s", y)
	}
}

func bytesEqual(a, b []byte) bool {
	return sha256.Sum256(a) == sha256.Sum256(b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// §19.9: per-node password override; empty override = global shared password.
func TestBuildNodeConfigWithOverride(t *testing.T) {
	const (
		global   = "global-pass"
		override = "node-pass"
	)

	if EffectiveAnytlsPassword(global, "") != global {
		t.Fatal("empty override must fall back to the global password")
	}
	if EffectiveAnytlsPassword(global, override) != override {
		t.Fatal("override must win over the global password")
	}

	raw, err := BuildNodeConfigWithOverride(23456, global, override)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var cfg struct {
		Inbounds []struct {
			Users []struct {
				Password string `json:"password"`
			} `json:"users"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(cfg.Inbounds) != 1 || len(cfg.Inbounds[0].Users) != 1 ||
		cfg.Inbounds[0].Users[0].Password != override {
		t.Fatalf("config must carry the override password: %s", raw)
	}

	// empty override is byte-identical to the plain global-password build
	fallback, err := BuildNodeConfigWithOverride(23456, global, "")
	if err != nil {
		t.Fatalf("build fallback: %v", err)
	}
	plain, _ := BuildNodeConfig(23456, global)
	if !bytesEqual(fallback, plain) {
		t.Fatal("empty override must reproduce the global-password config")
	}

	if _, err := BuildNodeConfigWithOverride(80, global, override); err == nil {
		t.Fatal("port validation must still apply")
	}
}

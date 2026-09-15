// Package singbox builds the server-side sing-box desired state (design §9):
// the per-node config.json pushed to agents, and the defaults shared by the
// subscription renderer (§10). The agent owns binaries, certificates and
// service units; the panel only ever declares "what I want".
package singbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
)

// Shared anytls constants (design §9.3/§9.4).
const (
	// ServerName is the TLS SNI every node presents; clients pin the agent's
	// self-signed certificate against it instead of using insecure=true.
	ServerName = "www.bing.com"
	// CertDir is where the agent keeps its self-signed keypair. The private
	// key never leaves the probe (§9.3); only the PEM is reported back.
	CertDir = "/etc/sing-box/cert"
)

// MinPort / MaxPort bound the random high inbound port (§9.3: 10000-60000).
const (
	MinPort = 10000
	MaxPort = 60000
)

// RandomPort returns a cryptographically random inbound port in [MinPort, MaxPort].
func RandomPort() (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(MaxPort-MinPort+1))
	if err != nil {
		return 0, fmt.Errorf("random port: %w", err)
	}
	return int(n.Int64()) + MinPort, nil
}

// ValidPort reports whether the panel may apply this inbound port.
func ValidPort(p int) bool { return p >= MinPort && p <= MaxPort }

// --- node config.json (design §9.1 / §9.4) ---

type logSection struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

type dnsServer struct {
	Tag     string `json:"tag"`
	Address string `json:"address"`
	Detour  string `json:"detour,omitempty"`
}

type dnsSection struct {
	Servers []dnsServer `json:"servers"`
}

type anytlsUser struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

type inboundTLS struct {
	Enabled         bool   `json:"enabled"`
	ServerName      string `json:"server_name"`
	CertificatePath string `json:"certificate_path"`
	KeyPath         string `json:"key_path"`
}

type anytlsInbound struct {
	Type       string       `json:"type"`
	Tag        string       `json:"tag"`
	Listen     string       `json:"listen"`
	ListenPort int          `json:"listen_port"`
	Users      []anytlsUser `json:"users"`
	TLS        inboundTLS   `json:"tls"`
}

type directOutbound struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`
}

type nodeConfig struct {
	Log       logSection       `json:"log"`
	DNS       dnsSection       `json:"dns"`
	Inbounds  []anytlsInbound  `json:"inbounds"`
	Outbounds []directOutbound `json:"outbounds"`
}

// BuildNodeConfig renders the complete sing-box config.json for one probe:
// anytls inbound (shared global password, §10.1) + log + dns + direct egress.
// The output is byte-stable for identical inputs so config_hash dedupes
// unchanged pushes (§9.1).
func BuildNodeConfig(port int, password string) ([]byte, error) {
	if !ValidPort(port) {
		return nil, fmt.Errorf("port %d outside %d-%d", port, MinPort, MaxPort)
	}
	cfg := nodeConfig{
		Log: logSection{Level: "warn", Timestamp: true},
		DNS: dnsSection{Servers: []dnsServer{
			{Tag: "local-dns", Address: "local", Detour: "direct"},
		}},
		Inbounds: []anytlsInbound{{
			Type:       "anytls",
			Tag:        "anytls-in",
			Listen:     "::",
			ListenPort: port,
			Users:      []anytlsUser{{Name: "default", Password: password}},
			TLS: inboundTLS{
				Enabled:         true,
				ServerName:      ServerName,
				CertificatePath: CertDir + "/cert.pem",
				KeyPath:         CertDir + "/key.pem",
			},
		}},
		Outbounds: []directOutbound{{Type: "direct", Tag: "direct"}},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal sing-box config: %w", err)
	}
	return raw, nil
}

// EffectiveAnytlsPassword picks the per-node password override when set,
// else the global shared password (§19.9: empty override = 全局 anytls_password).
func EffectiveAnytlsPassword(globalPassword, override string) string {
	if override != "" {
		return override
	}
	return globalPassword
}

// BuildNodeConfigWithOverride is BuildNodeConfig with a per-node password
// override (§19.9). The override travels to the agent inside ConfigJSON —
// SingboxDesired carries no password field, so nothing changes on the wire.
// v1 keeps this data-model/generation only: no UI or API sets it yet.
func BuildNodeConfigWithOverride(port int, globalPassword, override string) ([]byte, error) {
	return BuildNodeConfig(port, EffectiveAnytlsPassword(globalPassword, override))
}

// ConfigHash is the fingerprint stored in node_singbox.config_hash (§9.1:
// panel change → hash change → desired push → agent applies).
func ConfigHash(config []byte) string {
	sum := sha256.Sum256(config)
	return hex.EncodeToString(sum[:])
}

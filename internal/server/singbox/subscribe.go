package singbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ProxyNode is one outbound as seen by the subscription renderer (design §10).
//
// The anytls case is the original shape: server = primary IP (or the relay's),
// port = the node's inbound port, password = the node's anytls credential,
// CertPEM = agent-reported self-signed certificate for pinning.
//
// §9.3 实现修订 2026-09-17 folded the adopted inbounds into the same struct
// rather than adding a second type: an adopted VLESS/SS/Socks inbound is just
// another way to dial the same host, so the renderers switch on Protocol and
// both formats keep one code path per node.
type ProxyNode struct {
	ID       string
	Name     string
	Server   string
	Port     int
	Password string
	CertPEM  string

	// Protocol is "" (treated as anytls) for the inbound fobe owns, and the
	// inbound's own type for an adopted one.
	Protocol string
	// NameSuffix disambiguates an adopted inbound's client-facing name: several
	// inbounds live on one host, and two proxies named "HK-Sharon" would make a
	// client's proxy group ambiguous.
	NameSuffix string
	// Method is the shadowsocks cipher (adopted inbounds only).
	Method string
	// UUID + Flow are the VLESS credential and its xtls flow.
	UUID string
	Flow string
	// Username is the SOCKS5 credential half; Password carries the other.
	Username string
	// ServerName is the TLS SNI: the shared www.bing.com for the anytls inbound,
	// the operator's own choice for an adopted one.
	ServerName string
	// RealityPublicKey / RealityShortID are the adopted VLESS REALITY half.
	// Without the public key such an inbound cannot be dialled at all, which is
	// why adoption derives it from the server-side private key.
	RealityPublicKey string
	RealityShortID   string
}

// effectiveProtocol collapses "empty means anytls" for the renderers.
func (n ProxyNode) effectiveProtocol() string {
	if n.Protocol == "" {
		return ProtoAnytls
	}
	return n.Protocol
}

// renderName is the client-facing name: the node's, plus the adopted inbound's
// type and port so an SS2022 inbound is distinguishable from the anytls one on
// the same host.
func (n ProxyNode) renderName() string {
	base := nodeName(n)
	if n.NameSuffix == "" {
		return base
	}
	return base + " · " + n.NameSuffix
}

// tlsServerName is the SNI a client must present: the shared value for the
// anytls inbound fobe generates, the operator's own for an adopted one.
func (n ProxyNode) tlsServerName() string {
	if n.ServerName != "" {
		return n.ServerName
	}
	return ServerName
}

// --- sing-box JSON rendering ---

type sbNodeTLS struct {
	Enabled     bool           `json:"enabled"`
	ServerName  string         `json:"server_name,omitempty"`
	Insecure    bool           `json:"insecure,omitempty"`
	Certificate string         `json:"certificate,omitempty"` // inline PEM → pinning, not insecure (§9.3)
	Reality     *sbNodeReality `json:"reality,omitempty"`
}

// sbNodeReality is the client half of a REALITY handshake.
type sbNodeReality struct {
	Enabled   bool   `json:"enabled"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id,omitempty"`
}

type sbNodeOutbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	Password   string `json:"password,omitempty"`
	Method     string `json:"method,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Flow       string `json:"flow,omitempty"`
	Username   string `json:"username,omitempty"`
	// Pointer on purpose: omitempty does nothing for a struct, and an empty
	// "tls": {} is not a section sing-box accepts on a shadowsocks inbound.
	TLS *sbNodeTLS `json:"tls,omitempty"`
}

// nodeName is the human-readable half of both renderers' identifiers: the node
// name, or the node id when the probe never got one.
func nodeName(n ProxyNode) string {
	if n.Name == "" {
		return n.ID
	}
	return n.Name
}

// nodeTag builds a sing-box outbound tag: "fobe-<name>", with "-2", "-3"
// appended for repeated names (same disambiguation rule as the Clash renderer,
// different separator because a JSON string has no comment character to dodge).
//
// Deriving the tag from the *name* rather than the random node id is what makes
// a sing-box template able to write selector/urltest groups and route.final at
// all — with `fobe-<id>` there was no way to know the tags in advance
// (§10 实现修订 2026-09-16). The cost: existing clients that pinned tags in
// their own config re-fetch the subscription, which is what a subscription is
// for.
func nodeTag(name string, seen map[string]int) string {
	seen[name]++
	if c := seen[name]; c > 1 {
		return fmt.Sprintf("fobe-%s-%d", name, c)
	}
	return "fobe-" + name
}

// RenderNodesJSON renders {{nodes}} for sing-box templates: a comma-joined list
// of outbound objects (no enclosing brackets — the template owns the surrounding
// JSON structure). An anytls node pins the probe certificate and never sets
// insecure=true; adopted protocols carry their own credential fields.
func RenderNodesJSON(nodes []ProxyNode) (string, error) {
	items := make([]string, 0, len(nodes))
	seen := map[string]int{}
	for _, n := range nodes {
		if n.Server == "" || n.Port <= 0 {
			continue
		}
		out := sbNodeOutbound{
			Type:       n.effectiveProtocol(),
			Tag:        nodeTag(n.renderName(), seen),
			Server:     n.Server,
			ServerPort: n.Port,
		}
		switch n.effectiveProtocol() {
		case ProtoAnytls:
			// Pinning needs the certificate; without it there is no secure way
			// to dial an anytls inbound, so it is skipped rather than rendered
			// insecure (§9.3).
			if n.CertPEM == "" {
				continue
			}
			out.Password = n.Password
			out.TLS = &sbNodeTLS{
				Enabled:     true,
				ServerName:  n.tlsServerName(),
				Certificate: strings.TrimSpace(n.CertPEM),
			}
		case ProtoShadowsocks:
			out.Method = n.Method
			out.Password = n.Password
		case ProtoVLESS:
			out.UUID = n.UUID
			out.Flow = n.Flow
			if n.RealityPublicKey == "" {
				continue // no public key = the handshake cannot succeed
			}
			out.TLS = &sbNodeTLS{
				Enabled:    true,
				ServerName: n.tlsServerName(),
				Reality: &sbNodeReality{
					Enabled:   true,
					PublicKey: n.RealityPublicKey,
					ShortID:   n.RealityShortID,
				},
			}
		case ProtoSocks, "socks5":
			out.Type = ProtoSocks
			out.Username = n.Username
			out.Password = n.Password
		default:
			continue // an inbound type this renderer does not speak
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return "", fmt.Errorf("render sing-box node: %w", err)
		}
		items = append(items, string(raw))
	}
	return strings.Join(items, ",\n"), nil
}

// --- clash (mihomo) YAML rendering ---

// RenderNodesYAML renders {{nodes}} for clash templates: list items only
// (the template owns the "proxies:" key). For anytls the certificate goes into
// ca-str (inline PEM) and skip-cert-verify stays false so the pin holds.
func RenderNodesYAML(nodes []ProxyNode) string {
	var b strings.Builder
	seen := map[string]int{}
	for _, n := range nodes {
		if n.Server == "" || n.Port <= 0 {
			continue
		}
		// Decide renderability *before* emitting anything: a half-written list
		// item is not valid YAML, and "skip this node" must not leave one
		// behind (the cert-pinning rule is the same as the JSON renderer's).
		switch n.effectiveProtocol() {
		case ProtoAnytls:
			if n.CertPEM == "" {
				continue
			}
		case ProtoVLESS:
			if n.RealityPublicKey == "" {
				continue
			}
		case ProtoShadowsocks, ProtoSocks, "socks5":
		default:
			continue
		}
		name := n.renderName()
		seen[name]++
		if c := seen[name]; c > 1 {
			name = fmt.Sprintf("%s #%d", name, c)
		}
		b.WriteString("  - name: " + yamlQuote(name) + "\n")
		b.WriteString("    server: " + yamlQuote(n.Server) + "\n")
		b.WriteString("    port: " + strconv.Itoa(n.Port) + "\n")
		switch n.effectiveProtocol() {
		case ProtoAnytls:
			b.WriteString("    type: anytls\n")
			b.WriteString("    password: " + yamlQuote(n.Password) + "\n")
			b.WriteString("    sni: " + n.tlsServerName() + "\n")
			b.WriteString("    skip-cert-verify: false\n")
			b.WriteString("    ca-str: |\n")
			for _, line := range strings.Split(strings.TrimRight(n.CertPEM, "\n"), "\n") {
				b.WriteString("      " + strings.TrimSpace(line) + "\n")
			}
		case ProtoShadowsocks:
			b.WriteString("    type: ss\n")
			b.WriteString("    cipher: " + yamlQuote(n.Method) + "\n")
			b.WriteString("    password: " + yamlQuote(n.Password) + "\n")
		case ProtoVLESS:
			b.WriteString("    type: vless\n")
			b.WriteString("    uuid: " + yamlQuote(n.UUID) + "\n")
			if n.Flow != "" {
				b.WriteString("    flow: " + yamlQuote(n.Flow) + "\n")
			}
			b.WriteString("    tls: true\n")
			b.WriteString("    servername: " + n.tlsServerName() + "\n")
			b.WriteString("    client-fingerprint: chrome\n")
			b.WriteString("    reality-opts:\n")
			b.WriteString("      public-key: " + yamlQuote(n.RealityPublicKey) + "\n")
			if n.RealityShortID != "" {
				b.WriteString("      short-id: " + yamlQuote(n.RealityShortID) + "\n")
			}
		case ProtoSocks, "socks5":
			b.WriteString("    type: socks5\n")
			if n.Username != "" {
				b.WriteString("    username: " + yamlQuote(n.Username) + "\n")
			}
			if n.Password != "" {
				b.WriteString("    password: " + yamlQuote(n.Password) + "\n")
			}
		default:
			continue
		}
	}
	return b.String()
}

// yamlQuote renders a safe YAML double-quoted scalar. Go's %q escaping is a
// superset of YAML's double-quoted style (YAML understands \uXXXX too), so a
// strconv.Quote string is always a valid YAML scalar.
func yamlQuote(s string) string {
	return strconv.Quote(s)
}

// --- built-in default templates (§10: complete, usable configs) ---
//
// The renderer substitutes {{nodes}} and nothing else: routing rules are part of
// the template (§10 实现修订 2026-09-16b), so the defaults carry theirs statically.

const DefaultTemplateSingbox = `{
  "log": {
    "level": "warn",
    "timestamp": true
  },
  "outbounds": [
{{nodes}}
  ]
}
`

const DefaultTemplateClash = `# fobe subscription (clash / mihomo)
mode: rule
log-level: warning
allow-lan: false
proxies:
{{nodes}}
proxy-groups:
  - name: PROXY
    type: select
    include-all: true
rules:
  - MATCH,PROXY
`

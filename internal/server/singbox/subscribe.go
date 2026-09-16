package singbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ProxyNode is one probe as seen by the subscription renderer (design §10):
// server = primary IP, port = node_singbox.port, password = global anytls
// password, CertPEM = agent-reported self-signed certificate for pinning.
type ProxyNode struct {
	ID       string
	Name     string
	Server   string
	Port     int
	Password string
	CertPEM  string
}

// --- sing-box JSON rendering ---

type sbNodeTLS struct {
	Enabled     bool   `json:"enabled"`
	ServerName  string `json:"server_name"`
	Insecure    bool   `json:"insecure"`
	Certificate string `json:"certificate,omitempty"` // inline PEM → pinning, not insecure (§9.3)
}

type sbNodeOutbound struct {
	Type       string    `json:"type"`
	Tag        string    `json:"tag"`
	Server     string    `json:"server"`
	ServerPort int       `json:"server_port"`
	Password   string    `json:"password"`
	TLS        sbNodeTLS `json:"tls"`
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

// RenderNodesJSON renders {{nodes}} for sing-box templates: a comma-joined
// list of anytls outbound objects (no enclosing brackets — the template owns
// the surrounding JSON structure). Each node pins the probe certificate and
// never sets insecure=true. Tags are `fobe-<node name>`.
func RenderNodesJSON(nodes []ProxyNode) (string, error) {
	items := make([]string, 0, len(nodes))
	seen := map[string]int{}
	for _, n := range nodes {
		if n.Server == "" || n.Port <= 0 || n.CertPEM == "" {
			continue // cannot render a securely pinned outbound without these
		}
		raw, err := json.MarshalIndent(sbNodeOutbound{
			Type:       "anytls",
			Tag:        nodeTag(nodeName(n), seen),
			Server:     n.Server,
			ServerPort: n.Port,
			Password:   n.Password,
			TLS: sbNodeTLS{
				Enabled:     true,
				ServerName:  ServerName,
				Insecure:    false,
				Certificate: strings.TrimSpace(n.CertPEM),
			},
		}, "", "  ")
		if err != nil {
			return "", fmt.Errorf("render sing-box node: %w", err)
		}
		items = append(items, string(raw))
	}
	return strings.Join(items, ",\n"), nil
}

// --- clash (mihomo) YAML rendering ---

// RenderNodesYAML renders {{nodes}} for clash templates: list items only
// (the template owns the "proxies:" key). The certificate goes into ca-str
// (inline PEM); skip-cert-verify stays false so the pin holds.
func RenderNodesYAML(nodes []ProxyNode) string {
	var b strings.Builder
	seen := map[string]int{}
	for _, n := range nodes {
		if n.Server == "" || n.Port <= 0 || n.CertPEM == "" {
			continue
		}
		name := nodeName(n)
		seen[name]++
		if c := seen[name]; c > 1 {
			name = fmt.Sprintf("%s #%d", name, c)
		}
		b.WriteString("  - name: " + yamlQuote(name) + "\n")
		b.WriteString("    type: anytls\n")
		b.WriteString("    server: " + yamlQuote(n.Server) + "\n")
		b.WriteString("    port: " + strconv.Itoa(n.Port) + "\n")
		b.WriteString("    password: " + yamlQuote(n.Password) + "\n")
		b.WriteString("    sni: " + ServerName + "\n")
		b.WriteString("    skip-cert-verify: false\n")
		b.WriteString("    ca-str: |\n")
		for _, line := range strings.Split(strings.TrimRight(n.CertPEM, "\n"), "\n") {
			b.WriteString("      " + strings.TrimSpace(line) + "\n")
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
// The renderer substitutes {{nodes}} only; {{rules}} is preserved verbatim
// for template authors. The defaults therefore keep their rules static.

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

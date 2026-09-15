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

// RenderNodesJSON renders {{nodes}} for sing-box templates: a comma-joined
// list of anytls outbound objects (no enclosing brackets — the template owns
// the surrounding JSON structure). Each node pins the probe certificate and
// never sets insecure=true.
func RenderNodesJSON(nodes []ProxyNode) (string, error) {
	items := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Server == "" || n.Port <= 0 || n.CertPEM == "" {
			continue // cannot render a securely pinned outbound without these
		}
		raw, err := json.MarshalIndent(sbNodeOutbound{
			Type:       "anytls",
			Tag:        "fobe-" + n.ID,
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
		name := n.Name
		if name == "" {
			name = n.ID
		}
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

// Local sing-box discovery (design §9.3 实现修订 2026-09-17): a probe that was
// set up by one-sing.sh before fobe arrived has a running sing-box, a
// config.json full of inbounds and a service unit — and, until this file
// existed, fobe could see none of it. The agent ships the config bytes
// verbatim; everything that *understands* sing-box lives here, because the
// server is where §9.1/§9.4 already model this format. A new inbound type is
// therefore a server-side change, not an agent release on every probe.
//
// Two shapes come out of the parser and they are deliberately different:
//
//   - LocalInbound is the *sanitized* inventory. It is what the panel renders
//     ("anytls · 28711 · 有凭据"), and it is also the source of the real config
//     bytes once adoption asks for them.
//   - LocalInboundSummary is what the HTTP layer is allowed to return. It
//     carries no credential material at all — not the password, not the UUID,
//     not the private key — only "there is one".
package singbox

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Inbound protocols fobe can adopt and render (§9.3/§10). The first four are
// exactly what one-sing.sh emits; anything else stays visible in the inventory
// but is not imported (the panel says so instead of silently dropping it).
const (
	ProtoAnytls      = "anytls"
	ProtoShadowsocks = "shadowsocks"
	ProtoVLESS       = "vless"
	ProtoSocks       = "socks"
)

// localConfigDoc is the slice of a sing-box config this package reads. Unknown
// sections are ignored on purpose: the operator's config may carry anything
// (route, experimental, extra outbounds) and fobe has no business rewriting it.
type localConfigDoc struct {
	Inbounds []json.RawMessage `json:"inbounds"`
}

type localTLS struct {
	Enabled         bool        `json:"enabled"`
	ServerName      string      `json:"server_name"`
	CertificatePath string      `json:"certificate_path"`
	KeyPath         string      `json:"key_path"`
	Reality         *localReali `json:"reality"`
}

type localReali struct {
	Enabled    bool     `json:"enabled"`
	PrivateKey string   `json:"private_key"`
	ShortID    []string `json:"short_id"`
	Handshake  *struct {
		Server     string `json:"server"`
		ServerPort int    `json:"server_port"`
	} `json:"handshake"`
}

// LocalInbound is one adopted inbound. The exported fields are what the panel
// shows and what the agent's config gets; Credentials is the sensitive half and
// is stored encrypted (§4.4), never returned by an API.
//
// Secrets is `json:"-"` on purpose: the stored blob is built from InboundJSON
// (the original bytes) rather than from a re-marshal of this struct, so a
// credential can never leak through a struct that is only meant for display.
type LocalInbound struct {
	Type    string         `json:"type"`
	Tag     string         `json:"tag"`
	Port    int            `json:"port"`
	Label   string         `json:"label"`
	Inbound map[string]any `json:"-"`
}

// LocalInboundSummary is the panel-facing view: no secrets, just the shape and
// whether a credential came with it.
type LocalInboundSummary struct {
	Type      string `json:"type"`
	Tag       string `json:"tag"`
	Port      int    `json:"port"`
	Label     string `json:"label"`
	CredSet   bool   `json:"cred_set"`
	Adoptable bool   `json:"adoptable"`
}

// Summary renders the panel-facing view.
func (i LocalInbound) Summary() LocalInboundSummary {
	return LocalInboundSummary{
		Type:      i.Type,
		Tag:       i.Tag,
		Port:      i.Port,
		Label:     i.Label,
		CredSet:   inboundCredential(i.Inbound) != "",
		Adoptable: i.Port > 0,
	}
}

// Summaries maps a whole inventory for the API. `adoptable` marks the subset
// adoption accepts; the rest is surfaced as "found but not imported" so the
// operator sees the whole file instead of wondering where an inbound went.
func Summaries(list []LocalInbound) []LocalInboundSummary {
	out := make([]LocalInboundSummary, 0, len(list))
	for _, i := range list {
		s := i.Summary()
		s.Adoptable = adoptableProtocol(i.Type) && i.Port > 0
		out = append(out, s)
	}
	return out
}

// ParseLocalInbounds extracts the inbounds of a config.json, in file order.
//
// Errors are deliberately non-fatal per inbound: a config carrying one
// malformed entry still yields the others, because "the panel shows 2 of your
// 3 inbounds" beats "the panel shows nothing and logs a parse error". The
// returned error is only for a config that is not JSON at all.
func ParseLocalInbounds(configJSON string) ([]LocalInbound, error) {
	if strings.TrimSpace(configJSON) == "" {
		return nil, nil
	}
	var doc localConfigDoc
	if err := json.Unmarshal([]byte(configJSON), &doc); err != nil {
		return nil, fmt.Errorf("parse sing-box config: %w", err)
	}
	var out []LocalInbound
	for _, raw := range doc.Inbounds {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		typ, _ := m["type"].(string)
		tag, _ := m["tag"].(string)
		port := intField(m["listen_port"])
		ib := LocalInbound{Type: typ, Tag: tag, Port: port, Label: tag, Inbound: m}
		if ib.Label == "" {
			ib.Label = fmt.Sprintf("%s:%d", typ, port)
		}
		if !adoptableProtocol(typ) || port <= 0 {
			// Keep it in the inventory (the panel shows it as not importable)
			// instead of dropping it: an operator who added a `mixed` inbound
			// must be able to see that fobe noticed and chose not to take it.
			out = append(out, ib)
			continue
		}
		normalizeInbound(&ib)
		out = append(out, ib)
	}
	return out, nil
}

// adoptableProtocol is the allow-list: the four protocols one-sing.sh writes
// plus their aliases. Anything else (a `tun`, a `mixed`, a future type) stays
// in the "found but not imported" list.
func adoptableProtocol(typ string) bool {
	switch typ {
	case ProtoAnytls, ProtoShadowsocks, ProtoSocks, "socks5", ProtoVLESS:
		return true
	default:
		return false
	}
}

// normalizeInbound fills what adoption needs before the inbound can be served
// again: the listen address one-sing.sh always writes, this project's
// certificate paths, and the public key for a REALITY inbound (clients need
// the public half; one-sing.sh keeps only the private one on the server).
func normalizeInbound(ib *LocalInbound) {
	if _, ok := ib.Inbound["listen"]; !ok {
		ib.Inbound["listen"] = "::"
	}
	tls, _ := ib.Inbound["tls"].(map[string]any)
	if tls != nil {
		// The generated config declares /etc/one-sing/cert/... — the paths
		// one-sing.sh itself uses. Re-pointing an adopted inbound at those
		// names is what keeps a relocated (unprivileged) probe consistent:
		// the agent remaps the prefix on write.
		if _, ok := tls["certificate_path"]; ok {
			tls["certificate_path"] = CertDir + "/" + CertFile
		}
		if _, ok := tls["key_path"]; ok {
			tls["key_path"] = CertDir + "/" + KeyFile
		}
	}
	// VLESS REALITY: turn the stored private key into the public key clients
	// pin, so an adopted inbound is usable without the operator running
	// one-sing.sh's openssl dance again.
	if ib.Type == ProtoVLESS {
		if reality, _ := tls["reality"].(map[string]any); reality != nil {
			if priv, _ := reality["private_key"].(string); priv != "" {
				if pub, err := realityPublicKey(priv); err == nil {
					reality["public_key"] = pub
				}
			}
		}
	}
}

// inboundCredential returns a non-empty marker for "this inbound carries a
// credential". It never returns the credential itself — the panel only needs
// to know whether the operator will have to supply one.
func inboundCredential(m map[string]any) string {
	if pw, _ := m["password"].(string); pw != "" {
		return pw
	}
	if users, ok := m["users"].([]any); ok && len(users) > 0 {
		if u, ok := users[0].(map[string]any); ok {
			if pw, _ := u["password"].(string); pw != "" {
				return pw
			}
			if id, _ := u["uuid"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

// InboundPassword returns the first password inside an inbound (top level or
// first user) — the value an adopted anytls inbound must keep serving.
func InboundPassword(m map[string]any) string {
	if pw, _ := m["password"].(string); pw != "" {
		return pw
	}
	if users, ok := m["users"].([]any); ok && len(users) > 0 {
		if u, ok := users[0].(map[string]any); ok {
			pw, _ := u["password"].(string)
			return pw
		}
	}
	return ""
}

// InboundUser returns the first user object of an inbound, if any.
func InboundUser(m map[string]any) map[string]any {
	users, ok := m["users"].([]any)
	if !ok || len(users) == 0 {
		return nil
	}
	u, _ := users[0].(map[string]any)
	return u
}

// InboundTLS returns the tls section of an inbound, if any.
func InboundTLS(m map[string]any) map[string]any {
	tls, _ := m["tls"].(map[string]any)
	return tls
}

// realityPublicKey derives the X25519 public key from sing-box's base64url
// (unpadded) private key.
//
// one-sing.sh shells out to openssl with a hand-written PKCS#8 DER header for
// this; Go's standard library has X25519 since 1.20, so the same value falls
// out of crypto/ecdh with no subprocess and no header literal to get wrong.
// Clients need the public half to dial a REALITY inbound at all, so an adopted
// VLESS inbound is unusable without this.
func realityPublicKey(privateB64 string) (string, error) {
	priv, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(privateB64, "="))
	if err != nil {
		// one-sing.sh writes base64url, but a hand-edited config may carry
		// standard base64 with padding: accept it rather than give up.
		priv, err = base64.StdEncoding.DecodeString(privateB64)
		if err != nil {
			return "", fmt.Errorf("decode reality private key: %w", err)
		}
	}
	if len(priv) != 32 {
		return "", fmt.Errorf("reality private key is %d bytes, want 32", len(priv))
	}
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("reality private key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), nil
}

// InboundRealityPublicKey returns the REALITY public key of a VLESS inbound,
// deriving it from the private key when the config does not carry one.
func InboundRealityPublicKey(m map[string]any) string {
	tls := InboundTLS(m)
	if tls == nil {
		return ""
	}
	reality, _ := tls["reality"].(map[string]any)
	if reality == nil {
		return ""
	}
	if pub, _ := reality["public_key"].(string); pub != "" {
		return pub
	}
	priv, _ := reality["private_key"].(string)
	if priv == "" {
		return ""
	}
	pub, err := realityPublicKey(priv)
	if err != nil {
		return ""
	}
	return pub
}

// realityShortID returns the first non-empty short_id, the value a client must
// present for REALITY to accept the handshake.
func realityShortID(m map[string]any) string {
	tls := InboundTLS(m)
	if tls == nil {
		return ""
	}
	reality, _ := tls["reality"].(map[string]any)
	if reality == nil {
		return ""
	}
	ids, _ := reality["short_id"].([]any)
	for _, v := range ids {
		if s, _ := v.(string); s != "" {
			return s
		}
	}
	return ""
}

// intField reads a JSON number that survived a round trip through any.
func intField(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

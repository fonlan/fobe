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
	// CertDir is where the agent keeps its self-signed keypair, and
	// CertFile/KeyFile name the two files inside it. The private key never
	// leaves the probe (§9.3); only the PEM is reported back.
	//
	// 实现修订 2026-09-15: the layout follows one-sing.sh (/etc/one-sing/cert),
	// so a probe already managed by that script can be taken over as-is. These
	// three values must stay in sync with internal/agent/service
	// (SingboxWorkDir / SingboxCertFile / SingboxKeyFile) — the agent writes the
	// files at the paths this config declares.
	CertDir  = "/etc/one-sing/cert"
	CertFile = "cert.crt"
	KeyFile  = "private.key"
)

// AnytlsPaddingScheme is the inbound padding scheme, taken verbatim from
// one-sing.sh (§9.3 实现修订: 「服务管理与 anytls 配置参考 one-sing.sh」). It is
// the traffic-shaping half of anytls: shorter first records and a stop marker
// after the sixth, instead of sing-box's own defaults.
var AnytlsPaddingScheme = []string{
	"stop=6",
	"0=30-30",
	"1=80-120",
	"2=350-550,c",
	"3=900-1400",
	"4=250-600",
	"5=250-600",
}

// MinPort / MaxPort bound the random high inbound port (§9.3: 10000-60000).
const (
	MinPort = 10000
	MaxPort = 60000
)

// Global anytls password shape (§10.1 实现修订 2026-09-16): the credential is
// machine-generated on first use and never typed by the operator, so it only
// has to be (a) high-entropy and (b) safe to paste anywhere. 16 chars of
// [A-Za-z0-9] is ~95 bits and survives every URI/JSON/YAML/shell context
// without percent-encoding — which matters because the same string travels as
// a JSON field in node configs, a URI auth component in third-party clients
// and a Clash YAML scalar in subscriptions.
const (
	AnytlsPasswordLen     = 16
	anytlsPasswordCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// GenerateAnytlsPassword returns a fresh AnytlsPasswordLen-char alnum password.
//
// Bytes at or above the largest multiple of the charset size (248 = 4*62) are
// rejected instead of folded with `%` — a plain modulo would bias the first 8
// characters of the alphabet, and a credential generator is exactly the place
// where "good enough" randomness is not.
func GenerateAnytlsPassword() (string, error) {
	const (
		charsetLen = len(anytlsPasswordCharset)
		// The largest multiple of the charset size that fits in a byte
		// (248 = 4*62); bytes at or above it are rejected.
		limit = 256 - 256%charsetLen
	)
	out := make([]byte, 0, AnytlsPasswordLen)
	buf := make([]byte, AnytlsPasswordLen*2)
	for len(out) < AnytlsPasswordLen {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("random anytls password: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, anytlsPasswordCharset[int(b)%charsetLen])
			if len(out) == AnytlsPasswordLen {
				break
			}
		}
	}
	return string(out), nil
}

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

// GenerateUUID returns a random RFC 4122 version-4 UUID: the credential a VLESS
// inbound needs (§10.1 实现修订 2026-09-17e).
//
// It exists for the same reason GenerateAnytlsPassword does — the panel is the
// one creating the inbound, so the panel can invent the secret. Asking the
// operator to produce a UUID by hand is busywork, and a hand-typed one is
// usually a reused one.
func GenerateUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// --- node config.json (design §9.1 / §9.4) ---

type logSection struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

// dnsServer is one entry of the DNS server list. sing-box moved to a type-based
// format in 1.12.0 and *removed* the address-based one in 1.14.0 — so the
// legacy `{"tag":…,"address":"local"}` this package used to emit made every
// install of a current release die at gate ① with "legacy DNS server formats …
// removed in sing-box 1.14.0" (the size cap of the artifact was the bug before
// it, §9.2 实现修订 2026-09-16). `local` is the only server fobe needs: the
// inbound serves anytls and egress is `direct`, so resolution is the system
// resolver's job. The type-based form works from 1.12 on — the floor this
// config targets. `detour` is a dialer option for *remote* servers and has
// nothing to do here.
type dnsServer struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`
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
	// PaddingScheme is the anytls traffic-shaping scheme (one-sing.sh's list,
	// §9.3 实现修订).
	PaddingScheme []string   `json:"padding_scheme"`
	TLS           inboundTLS `json:"tls"`
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
			{Type: "local", Tag: "local-dns"},
		}},
		Inbounds: []anytlsInbound{{
			Type:          "anytls",
			Tag:           "anytls-in",
			Listen:        "::",
			ListenPort:    port,
			Users:         []anytlsUser{{Name: "default", Password: password}},
			PaddingScheme: AnytlsPaddingScheme,
			TLS: inboundTLS{
				Enabled:         true,
				ServerName:      ServerName,
				CertificatePath: CertDir + "/" + CertFile,
				KeyPath:         CertDir + "/" + KeyFile,
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

// --- adopted inbounds (§9.3 实现修订 2026-09-17) ---

// ExtraInbound is one inbound lifted from a probe's own config.json and kept
// as part of fobe's desired state.
//
// It is legacy since §9.3 实现修订 2026-09-17d: nothing writes it any more (the
// probe's file is the truth and the subscription renders from it), but a node
// adopted by an earlier build still has its inbounds stored here, and a config
// regenerated by an install or a port change has to keep carrying them —
// dropping them would take the operator's VLESS/SS/Socks services down.
type ExtraInbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Listen     string `json:"listen,omitempty"`
	ListenPort int    `json:"listen_port"`
	Method     string `json:"method,omitempty"`
	// Password is the *top-level* credential, i.e. classic shadowsocks. Every
	// other protocol carries it per user: a top-level `password` on a vless or
	// anytls inbound is rejected by sing-box outright.
	Password string      `json:"password,omitempty"`
	Users    []ExtraUser `json:"users,omitempty"`
	TLS      *ExtraTLS   `json:"tls,omitempty"`
	// Raw keeps the parts of the original inbound this struct does not model
	// (multiplex, sniff, a padding_scheme, a detour…). It is never persisted —
	// the stored form is the typed fields, which are the ones the panel and the
	// subscription renderer understand — but a generated config puts them back,
	// so adoption does not quietly drop inbound options the operator set.
	Raw map[string]any `json:"-"`
}

// ExtraUser is one inbound user, modelled for the protocols fobe renders:
// anytls and socks5 use Password, vless uses UUID (+ Flow).
type ExtraUser struct {
	Name     string `json:"name,omitempty"`
	Password string `json:"password,omitempty"`
	UUID     string `json:"uuid,omitempty"`
	Flow     string `json:"flow,omitempty"`
}

// ExtraTLS mirrors the tls section closely enough to round-trip what
// one-sing.sh writes plus the REALITY public key adoption derives.
type ExtraTLS struct {
	Enabled         bool          `json:"enabled"`
	ServerName      string        `json:"server_name,omitempty"`
	CertificatePath string        `json:"certificate_path,omitempty"`
	KeyPath         string        `json:"key_path,omitempty"`
	Reality         *ExtraReality `json:"reality,omitempty"`
}

// ExtraReality is the REALITY half of a VLESS inbound. PublicKey is filled by
// adoption (derived from PrivateKey, see realityPublicKey) so a client can dial
// the inbound without the operator running one-sing.sh's key derivation again.
//
// It must NOT reach the probe's config: sing-box's server-side reality object
// has no `public_key` (it is the client half), and a config carrying it fails
// gate ① outright — verified on a live probe (2026-09-17), which is why the
// config builder rebuilds inbounds from a whitelist instead of marshalling
// this struct.
type ExtraReality struct {
	Enabled    bool                `json:"enabled"`
	PrivateKey string              `json:"private_key,omitempty"`
	PublicKey  string              `json:"public_key,omitempty"`
	ShortID    []string            `json:"short_id,omitempty"`
	Handshake  *ExtraRealityHandsh `json:"handshake,omitempty"`
}

// ExtraRealityHandsh is the REALITY handshake target (the site whose TLS the
// inbound borrows).
type ExtraRealityHandsh struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
}

// ExtraInboundFrom converts a parsed local inbound into the storable form.
// It keeps the credential (the inbound has to keep working) and the derived
// REALITY public key.
func ExtraInboundFrom(ib LocalInbound) ExtraInbound {
	extra := ExtraInbound{
		Type:       ib.Type,
		Tag:        ib.Tag,
		Listen:     "::",
		ListenPort: ib.Port,
	}
	if listen, _ := ib.Inbound["listen"].(string); listen != "" {
		extra.Listen = listen
	}
	if method, _ := ib.Inbound["method"].(string); method != "" {
		extra.Method = method
	}
	// The TOP-LEVEL password only (classic shadowsocks). A per-user password
	// belongs in Users below: sing-box rejects `password` on a vless/anytls
	// inbound outright ("inbounds[1].password: unknown field"), and gate ①
	// caught exactly that on a real probe. Reading the user's password here
	// instead of the inbound's is what put it there.
	if pw, _ := ib.Inbound["password"].(string); pw != "" {
		extra.Password = pw
	}
	if users, ok := ib.Inbound["users"].([]any); ok {
		for _, raw := range users {
			u, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			eu := ExtraUser{}
			eu.Name, _ = u["name"].(string)
			eu.Password, _ = u["password"].(string)
			eu.UUID, _ = u["uuid"].(string)
			eu.Flow, _ = u["flow"].(string)
			extra.Users = append(extra.Users, eu)
		}
	}
	if tls := InboundTLS(ib.Inbound); tls != nil {
		et := &ExtraTLS{}
		et.Enabled, _ = tls["enabled"].(bool)
		et.ServerName, _ = tls["server_name"].(string)
		et.CertificatePath, _ = tls["certificate_path"].(string)
		et.KeyPath, _ = tls["key_path"].(string)
		if reality, _ := tls["reality"].(map[string]any); reality != nil {
			er := &ExtraReality{}
			er.Enabled, _ = reality["enabled"].(bool)
			er.PrivateKey, _ = reality["private_key"].(string)
			er.PublicKey = InboundRealityPublicKey(ib.Inbound)
			if ids, ok := reality["short_id"].([]any); ok {
				for _, raw := range ids {
					if s, ok := raw.(string); ok {
						er.ShortID = append(er.ShortID, s)
					}
				}
			}
			if hs, ok := reality["handshake"].(map[string]any); ok {
				er.Handshake = &ExtraRealityHandsh{}
				er.Handshake.Server, _ = hs["server"].(string)
				er.Handshake.ServerPort = intField(hs["server_port"])
			}
			et.Reality = er
		}
		extra.TLS = et
	}
	// Keep the unmodelled keys (minus the ones this struct now owns, so the
	// typed values always win when the config is rebuilt).
	//
	// `password` is deliberately dropped for every protocol except shadowsocks:
	// the struct holds it as a top-level field there, and for anytls/vless/socks
	// the credential belongs to the user. Re-merging it "because it was in the
	// original file" is what produced `inbounds[1].password: unknown field` on a
	// live probe (2026-09-17) — gate ① caught it, but only after fobe had
	// stopped the operator's service to try.
	raw := make(map[string]any, len(ib.Inbound))
	for k, v := range ib.Inbound {
		switch k {
		case "type", "tag", "listen", "listen_port", "method", "password", "users", "tls":
			continue
		}
		raw[k] = v
	}
	if ib.Type == ProtoShadowsocks {
		if pw, ok := ib.Inbound["password"]; ok {
			raw["password"] = pw
		}
	}
	if len(raw) > 0 {
		extra.Raw = raw
	}
	return extra
}

// BuildNodeConfigWithInbounds is BuildNodeConfig plus the adopted inbounds.
//
// The generated anytls inbound always wins a port collision: it is the one
// the panel owns (its port, its certificate, its subscription entry), and
// letting a stale adopted copy shadow it would make "install this node" look
// like it did nothing. `extras` is passed already decoded so callers cannot
// forget to parse it and silently drop the operator's inbounds.
func BuildNodeConfigWithInbounds(port int, password string, extras []ExtraInbound) ([]byte, error) {
	base, err := BuildNodeConfig(port, password)
	if err != nil {
		return nil, err
	}
	if len(extras) == 0 {
		return base, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("re-read generated config: %w", err)
	}
	inbounds, _ := doc["inbounds"].([]any)
	used := map[int]bool{port: true}
	for _, e := range extras {
		if e.ListenPort <= 0 || used[e.ListenPort] {
			continue
		}
		if e.Type == ProtoAnytls && port == e.ListenPort {
			continue
		}
		used[e.ListenPort] = true
		m, err := e.configInbound()
		if err != nil {
			return nil, err
		}
		inbounds = append(inbounds, m)
	}
	doc["inbounds"] = inbounds
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal sing-box config: %w", err)
	}
	return out, nil
}

// ConfigHash is the fingerprint stored in node_singbox.config_hash (§9.1:
// panel change → hash change → desired push → agent applies).
func ConfigHash(config []byte) string {
	sum := sha256.Sum256(config)
	return hex.EncodeToString(sum[:])
}

// allowedInboundKeys is the whitelist of inbound keys fobe will put back into a
// probe's config. It is a whitelist rather than a blacklist for one reason: the
// config is generated on the server and applied by the agent after a single
// `sing-box check`, and a key sing-box does not know is a *hard* failure —
// `json: unknown field` — which on a live node means "stop the operator's
// service, fail the gate, roll back". That already happened twice while this
// feature was being built (`password` on a vless inbound, `public_key` inside
// reality), both caught on a real probe rather than in review.
//
// So: only keys sing-box documents are passed through, everything else is
// dropped with the rest of the adopted inbound still intact. An option fobe
// does not model yet is silently lost; a config sing-box refuses is a service
// outage on someone's VPS. The trade is deliberate.
var allowedInboundKeys = map[string]bool{
	"type": true, "tag": true, "listen": true, "listen_port": true,
	"method": true, "password": true, "users": true, "tls": true,
	// the antyls traffic-shaping list the panel's own template also writes
	"padding_scheme": true,
	// options one-sing.sh and hand-written configs commonly carry
	"multiplex": true, "sniff": true, "sniff_override_destination": true,
	"domain_strategy": true, "udp_timeout": true, "detour": true,
	"tcp_fast_open": true, "tcp_multi_path": true, "udp_fragment": true,
	"network": true, "set_system_proxy": true,
}

// allowedTLSKeys is the same idea one level down (the tls section).
var allowedTLSKeys = map[string]bool{
	"enabled": true, "server_name": true, "certificate_path": true,
	"key_path": true, "reality": true, "alpn": true, "min_version": true,
	"max_version": true, "cipher_suites": true, "acme": true,
}

// allowedRealityKeys is the server-side REALITY object. `public_key` is
// pointedly absent: it belongs to the client.
var allowedRealityKeys = map[string]bool{
	"enabled": true, "private_key": true, "short_id": true, "handshake": true,
	"max_time_difference": true,
}

// configInbound renders one adopted inbound as a sing-box config object.
//
// It is built from the typed fields plus the whitelisted pass-throughs, never
// by marshalling ExtraInbound wholesale: this struct also carries the REALITY
// *public* key and the adoption bookkeeping, and those are not config.
func (e ExtraInbound) configInbound() (map[string]any, error) {
	m := map[string]any{
		"type":        e.Type,
		"tag":         e.Tag,
		"listen":      e.Listen,
		"listen_port": e.ListenPort,
	}
	if e.Listen == "" {
		m["listen"] = "::"
	}
	if e.Method != "" {
		m["method"] = e.Method
	}
	if e.Password != "" {
		m["password"] = e.Password
	}
	if len(e.Users) > 0 {
		users := make([]map[string]any, 0, len(e.Users))
		for _, u := range e.Users {
			one := map[string]any{}
			if u.Name != "" {
				one["name"] = u.Name
			}
			if u.Password != "" {
				one["password"] = u.Password
			}
			if u.UUID != "" {
				one["uuid"] = u.UUID
			}
			if u.Flow != "" {
				one["flow"] = u.Flow
			}
			users = append(users, one)
		}
		m["users"] = users
	}
	if e.TLS != nil {
		tls := map[string]any{"enabled": e.TLS.Enabled}
		if e.TLS.ServerName != "" {
			tls["server_name"] = e.TLS.ServerName
		}
		if e.TLS.CertificatePath != "" {
			tls["certificate_path"] = e.TLS.CertificatePath
		}
		if e.TLS.KeyPath != "" {
			tls["key_path"] = e.TLS.KeyPath
		}
		if r := e.TLS.Reality; r != nil {
			reality := map[string]any{"enabled": true}
			if r.PrivateKey != "" {
				reality["private_key"] = r.PrivateKey
			}
			if len(r.ShortID) > 0 {
				reality["short_id"] = r.ShortID
			}
			if r.Handshake != nil {
				reality["handshake"] = map[string]any{
					"server":      r.Handshake.Server,
					"server_port": r.Handshake.ServerPort,
				}
			}
			tls["reality"] = reality
		}
		m["tls"] = tls
	}
	for k, v := range e.Raw {
		if !allowedInboundKeys[k] {
			continue
		}
		if _, taken := m[k]; taken {
			continue
		}
		m[k] = sanitizeConfigValue(k, v)
	}
	if tls, ok := m["tls"].(map[string]any); ok {
		for k := range tls {
			if !allowedTLSKeys[k] {
				delete(tls, k)
			}
		}
		if reality, ok := tls["reality"].(map[string]any); ok {
			for k := range reality {
				if !allowedRealityKeys[k] {
					delete(reality, k)
				}
			}
		}
	}
	return m, nil
}

// sanitizeConfigValue drops unknown keys from a nested pass-through section
// (tls / reality / a user object) so a hand-written config cannot smuggle a
// field into the generated one.
func sanitizeConfigValue(key string, v any) any {
	switch key {
	case "tls":
		if section, ok := v.(map[string]any); ok {
			for k := range section {
				if !allowedTLSKeys[k] {
					delete(section, k)
				}
			}
			if reality, ok := section["reality"].(map[string]any); ok {
				for k := range reality {
					if !allowedRealityKeys[k] {
						delete(reality, k)
					}
				}
			}
			return section
		}
	case "users":
		if users, ok := v.([]any); ok {
			for _, raw := range users {
				if u, ok := raw.(map[string]any); ok {
					for k := range u {
						switch k {
						case "name", "password", "uuid", "flow":
						default:
							delete(u, k)
						}
					}
				}
			}
			return users
		}
	}
	return v
}

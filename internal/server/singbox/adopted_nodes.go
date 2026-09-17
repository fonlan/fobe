package singbox

// Adopted inbounds as subscription nodes (§9.3 实现修订 2026-09-17).
//
// This is the last mile of adoption: the config side (BuildNodeConfigWithInbounds)
// keeps the operator's inbounds *running*, and this file is what makes them
// usable from a client — the same inbound, dialled directly, with the
// credential it already has.
//
// One rule shapes everything here: an adopted inbound belongs to the probe
// that runs it, so it always dials that probe's own address. It is deliberately
// *not* relayed through §10.2's DNAT entries, which are derived from rules
// targeting the node's anytls port — a client aiming at a relay's src port
// would reach the wrong inbound.
//
// Since §9.3 实现修订 2026-09-17d this file has two entry points: ProxyNodesFor
// renders the extras of a node adopted before the editor model existed, and
// LiveProxyNodes renders whatever the probe's own file declares today.

// ProxyNodesFor renders every adopted inbound of one host as subscription
// nodes. `server` is the host a client should dial (the target's primary IP)
// and `certPEM` is the node's own certificate, used to pin an adopted anytls
// inbound.
//
// Inbounds this renderer cannot express are skipped, never guessed at: an
// imported inbound that a client would fail to authenticate against is worse
// than one that is absent from the list (the panel still shows it, so the
// operator knows why).
func ProxyNodesFor(extras []ExtraInbound, nodeID, name, server, certPEM string) []ProxyNode {
	out := make([]ProxyNode, 0, len(extras))
	for _, e := range extras {
		if e.ListenPort <= 0 || server == "" {
			continue
		}
		n := ProxyNode{
			ID:       nodeID,
			Name:     name,
			Server:   server,
			Port:     e.ListenPort,
			Protocol: e.Type,
		}
		switch e.Type {
		case ProtoShadowsocks:
			n.Method = e.Method
			n.Password = e.Password
		case ProtoAnytls:
			n.Password = firstUserPassword(e, e.Password)
			// Only overwrite when the caller has a certificate for this
			// inbound. A report-side caller passes "" for everything except the
			// panel's own port, and clobbering the others with "" is what made
			// a hand-added anytls inbound vanish from the subscription while it
			// was plainly in the file.
			if certPEM != "" {
				n.CertPEM = certPEM
			}
			if e.TLS != nil && e.TLS.ServerName != "" {
				n.ServerName = e.TLS.ServerName
			}
		case ProtoVLESS:
			if len(e.Users) == 0 {
				continue
			}
			n.UUID = e.Users[0].UUID
			n.Flow = e.Users[0].Flow
			if e.TLS != nil && e.TLS.ServerName != "" {
				n.ServerName = e.TLS.ServerName
			}
			n.RealityPublicKey = extraRealityPublicKey(e)
			n.RealityShortID = extraRealityShortID(e)
		case ProtoSocks, "socks5":
			if len(e.Users) > 0 {
				n.Username = e.Users[0].Name
				n.Password = e.Users[0].Password
			}
			if n.Username == "" && n.Password == "" {
				// A socks inbound without users is unauthenticated: a client
				// entry with no credential is still correct, so this is not a
				// skip.
				break
			}
		default:
			continue
		}
		out = append(out, n)
	}
	return out
}

// firstUserPassword reads the credential the way the inbound stores it: on the
// user for every modern protocol, at the top level only for classic
// shadowsocks. Getting this backwards is how a generated config ended up with
// an invalid top-level password on a vless inbound (2026-09-17).
func firstUserPassword(e ExtraInbound, fallback string) string {
	if len(e.Users) > 0 && e.Users[0].Password != "" {
		return e.Users[0].Password
	}
	return fallback
}

// extraRealityPublicKey prefers the stored public key and falls back to
// deriving it from the private key: a snapshot adopted by an older build (or a
// hand-written row) may not carry one, and a VLESS REALITY node without a
// public key cannot be dialled at all.
func extraRealityPublicKey(e ExtraInbound) string {
	if e.TLS == nil || e.TLS.Reality == nil {
		return ""
	}
	if e.TLS.Reality.PublicKey != "" {
		return e.TLS.Reality.PublicKey
	}
	pub, err := realityPublicKey(e.TLS.Reality.PrivateKey)
	if err != nil {
		return ""
	}
	return pub
}

// extraRealityShortID returns the first non-empty short id.
func extraRealityShortID(e ExtraInbound) string {
	if e.TLS == nil || e.TLS.Reality == nil {
		return ""
	}
	for _, id := range e.TLS.Reality.ShortID {
		if id != "" {
			return id
		}
	}
	return ""
}

// LiveProxyNodes renders everything a probe's own config.json declares
// (design §9.3 实现修订 2026-09-17b, the editor model).
//
// This is the difference between "the panel owns the file" and "the file is the
// truth": a `jq`-appended inbound shows up in the subscription without ever
// having been adopted, because the payload is rendered from the file the probe
// reported rather than from a server-side desired state.
//
// Every entry carries what the file says, credential included (§9.3 实现修订
// 2026-09-17d): the file is what the probe is serving right now, so a listener
// one-sing.sh set up keeps handing out the password its clients already have.
// The panel's global anytls password only reaches a listener the *panel* wrote
// (install / edit), and from then on the file carries it like any other value.
//
// Every listener renders under the node's plain name (2026-09-17j removed the
// type:port suffix): several inbounds on a node are peers, none is a hidden
// primary entry with a special name or subscription meaning. Same names would
// collide in a client's proxy group, so the renderers dedupe with "#2"/"-2".
func LiveProxyNodes(configJSON, nodeID, name, server string, certs map[int]string) []ProxyNode {
	inbounds, err := ParseLocalInbounds(configJSON)
	if err != nil {
		return nil
	}
	nodes := make([]ProxyNode, 0, len(inbounds))
	for i, ib := range inbounds {
		if ib.Port <= 0 {
			continue
		}
		e := ExtraInboundFrom(ib)
		built := ProxyNodesFor([]ExtraInbound{e}, nodeID, name, server, certs[ib.Port])
		for _, n := range built {
			if n.Protocol == ProtoAnytls && n.CertPEM == "" {
				// The file names a certificate *path*; a client needs the bytes
				// to pin. No bytes → skip: rendering insecure=true is the one
				// thing this project does not do (§9.3). The panel still lists
				// it, so the operator can see why it is missing.
				continue
			}
			nodes = append(nodes, n)
		}
		_ = i
	}
	return nodes
}

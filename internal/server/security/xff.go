package security

import (
	"net"
	"net/http"
	"strings"
)

// TrustChain decides the real client IP behind (possibly stacked) reverse
// proxies (design §3/§4.3). Only upstreams inside TrustedProxies may set
// X-Forwarded-For; anything else falls back to the socket address so a
// client cannot forge the header.
type TrustChain struct {
	trusted []*net.IPNet
}

// NewTrustChain parses a comma-separated CIDR list
// (default: 127.0.0.1/32,::1/128,172.16.0.0/12).
func NewTrustChain(spec string) (*TrustChain, error) {
	if strings.TrimSpace(spec) == "" {
		spec = "127.0.0.1/32,::1/128,172.16.0.0/12"
	}
	tc := &TrustChain{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			if strings.Contains(part, ":") {
				part += "/128"
			} else {
				part += "/32"
			}
		}
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			return nil, err
		}
		tc.trusted = append(tc.trusted, ipnet)
	}
	return tc, nil
}

func (tc *TrustChain) isTrusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range tc.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// RealIP resolves the client IP for a request (design §3/§4.3): the socket
// source when the peer is not a trusted proxy, otherwise the first entry of
// X-Forwarded-For that is not itself a trusted proxy, walking RIGHT to LEFT.
//
// Right-to-left is the only direction that works with the documented nginx
// config: `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for` appends
// $remote_addr *after* whatever the client sent, so the trustworthy end is the
// right one. Reading the leftmost entry (what this used to do, 2026-09-20
// 实现修订) is correct only when the proxy *replaces* the header — with the
// documented config it handed every client control over its own apparent
// address, which defeated the login blacklist twice over: spoof a private
// address and §4.3's anti-lockout rule exempts you from counting entirely,
// spoof a public one and you get an unlimited supply of identities to guess
// passwords from.
//
// An entry that does not parse ends the walk: continuing left would mean
// trusting a value that no hop can be held accountable for, so the socket
// address wins instead (fail-closed).
func (tc *TrustChain) RealIP(r *http.Request) string {
	socketIP := socketIP(r)
	if !tc.isTrusted(socketIP) {
		return socketIP.String()
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return socketIP.String()
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := parseForwarded(parts[i])
		if ip == nil {
			break // unparseable hop: nothing to its left is attributable
		}
		if tc.isTrusted(ip) {
			continue // our own proxy chain: keep walking left
		}
		return ip.String()
	}
	return socketIP.String()
}

// parseForwarded accepts the address forms a proxy may write into XFF: a bare
// IP, or ip:port (some proxies append the port).
func parseForwarded(s string) net.IP {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return net.ParseIP(host)
	}
	return nil
}

// NeverBlacklist implements the anti-lockout rules (design §4.3):
// loopback/private/link-local (incl. Docker nets) and the trusted proxy
// ranges themselves are never added to the blacklist.
func (tc *TrustChain) NeverBlacklist(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return true // unparseable: refuse to blacklist junk
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return true
	}
	return tc.isTrusted(ip)
}

func socketIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return net.ParseIP(r.RemoteAddr)
	}
	return net.ParseIP(host)
}

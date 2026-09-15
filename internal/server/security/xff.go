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

// RealIP resolves the client IP for a request:
// socket source when the peer is not a trusted proxy, otherwise the
// leftmost entry of X-Forwarded-For (design §3: leftmost = original client).
func (tc *TrustChain) RealIP(r *http.Request) string {
	socketIP := socketIP(r)
	if !tc.isTrusted(socketIP) {
		return socketIP.String()
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return socketIP.String()
	}
	leftmost := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
	if ip := net.ParseIP(leftmost); ip != nil {
		return ip.String()
	}
	return socketIP.String()
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

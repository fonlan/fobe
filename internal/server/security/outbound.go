package security

import (
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ErrBlockedDestination reports an outbound endpoint that must never be dialed
// from this process.
var ErrBlockedDestination = errors.New("destination is not reachable on the public internet")

// CheckOutboundHost rejects the destinations that turn an operator-supplied URL
// (an AI provider base_url, a notification webhook) into a probe of the host's
// own network (design §12.5 实现修订 2026-09-20).
//
// Blocked: link-local (169.254.0.0/16 — the cloud metadata service — and
// fe80::/10), the unspecified address, and multicast. Allowed on purpose:
// loopback and RFC1918. A gateway on 127.0.0.1 (a local Ollama) or on the LAN is
// a normal deployment for a self-hosted panel, and the operator is the only one
// who can set these endpoints; blocking them would break working setups while
// the metadata service has no legitimate role here at all. The accepted residue
// is that a session holding the settings endpoint can still use it as a blind
// port scanner — which is why the upstream body is no longer echoed back
// (aiprotocol.UpstreamError).
//
// A hostname is resolved so that a name pointing at a blocked address is caught
// too. Resolution is inherently a snapshot; GuardClient re-checks every redirect
// hop for the same reason.
func CheckOutboundHost(host string) error {
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrBlockedDestination)
	}
	if ip := net.ParseIP(host); ip != nil {
		return checkBlockedIP(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// Unresolvable now: let the request itself fail rather than reject a
		// name that may resolve once DNS is available.
		return nil
	}
	for _, ip := range ips {
		if err := checkBlockedIP(ip); err != nil {
			return err
		}
	}
	return nil
}

func checkBlockedIP(ip net.IP) error {
	if ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("%w: link-local, unspecified or multicast address %s", ErrBlockedDestination, ip)
	}
	return nil
}

// GuardClient returns a copy of c whose redirects are re-validated hop by hop.
// Go's default policy follows up to ten redirects to ANY host, so a provider or
// webhook that answers 302 could otherwise reach an address the endpoint check
// had no chance to see.
func GuardClient(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	cp := *c
	prev := cp.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if err := CheckOutboundHost(req.URL.Hostname()); err != nil {
			return err
		}
		if prev != nil {
			return prev(req, via)
		}
		return nil
	}
	return &cp
}

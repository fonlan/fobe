package security

import (
	"net/http"
	"testing"
)

// The login blacklist (design §4.3) is only as good as RealIP: if a client can
// choose its own apparent address, the fail counter is attacker-controlled.

func testChain(t *testing.T) *TrustChain {
	t.Helper()
	tc, err := NewTrustChain("127.0.0.1/32,::1/128,172.16.0.0/12")
	if err != nil {
		t.Fatalf("NewTrustChain: %v", err)
	}
	return tc
}

func xffReq(remote, xff string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

// TestRealIPIgnoresXFFFromUntrustedPeer: a direct client cannot forge at all.
func TestRealIPIgnoresXFFFromUntrustedPeer(t *testing.T) {
	tc := testChain(t)
	for _, xff := range []string{"", "10.0.0.1", "1.2.3.4, 10.0.0.1"} {
		if got := tc.RealIP(xffReq("203.0.113.9:5555", xff)); got != "203.0.113.9" {
			t.Fatalf("xff=%q: got %q, want the socket address", xff, got)
		}
	}
}

// TestRealIPWalksRightToLeft is the regression test for the 2026-09-20 fix:
// behind nginx's $proxy_add_x_forwarded_for the last entry is $remote_addr, so
// a client-supplied prefix must never win.
func TestRealIPWalksRightToLeft(t *testing.T) {
	tc := testChain(t)
	cases := []struct {
		name, remote, xff, want string
	}{
		{"spoofed private prefix", "127.0.0.1:5000", "10.0.0.1, 203.0.113.9", "203.0.113.9"},
		{"spoofed public prefix", "127.0.0.1:5000", "1.2.3.4, 203.0.113.9", "203.0.113.9"},
		{"single spoofed entry is alone", "127.0.0.1:5000", "1.2.3.4", "1.2.3.4"}, // XFF=$remote_addr style
		{"stacked trusted proxies", "127.0.0.1:5000", "1.2.3.4, 172.16.0.7, 203.0.113.9", "203.0.113.9"},
		{"no header falls back to socket", "127.0.0.1:5000", "", "127.0.0.1"},
		{"whole chain trusted falls back", "127.0.0.1:5000", "172.16.0.7", "127.0.0.1"},
		{"unparseable hop fails closed", "127.0.0.1:5000", "1.2.3.4, nope", "127.0.0.1"},
		{"ip:port entries parse", "127.0.0.1:5000", "1.2.3.4, 203.0.113.9:443", "203.0.113.9"},
		{"ipv6 client", "127.0.0.1:5000", "10.0.0.1, 2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tc.RealIP(xffReq(c.remote, c.xff)); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestSpoofedPrivateXFFIsStillBlacklistable pins the security property that the
// combination of RealIP + NeverBlacklist has to preserve: after the fix, a
// client behind a proxy can no longer present itself as an exempt address.
func TestSpoofedPrivateXFFIsStillBlacklistable(t *testing.T) {
	tc := testChain(t)
	ip := tc.RealIP(xffReq("127.0.0.1:5000", "10.0.0.1, 203.0.113.9"))
	if tc.NeverBlacklist(ip) {
		t.Fatalf("RealIP=%q is treated as protected, the fail counter would be skipped", ip)
	}
}

func TestNeverBlacklist(t *testing.T) {
	tc := testChain(t)
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.1.2.3", true},
		{"192.168.1.5", true},
		{"172.16.5.5", true},
		{"169.254.1.1", true},
		{"203.0.113.9", false},
		{"8.8.8.8", false},
		{"not-an-ip", true}, // refuse to blacklist junk
	}
	for _, c := range cases {
		if got := tc.NeverBlacklist(c.ip); got != c.want {
			t.Errorf("NeverBlacklist(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}

package security

import (
	"errors"
	"testing"
)

// CheckOutboundHost is the guard in front of every operator-supplied URL (an AI
// provider base_url, a notification webhook; design §12.5). It blocks the
// classes that turn a settings form into a probe of the host's own network and
// leaves loopback/RFC1918 usable — a local Ollama or a LAN gateway is a normal
// deployment for a self-hosted panel.
//
// Every input is an IP literal on purpose: those are classified without DNS, so
// the test stays hermetic.
func TestCheckOutboundHostAddressClasses(t *testing.T) {
	if err := CheckOutboundHost(""); !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("empty host: err = %v, want ErrBlockedDestination", err)
	}

	blocked := []string{
		"169.254.169.254", // link-local: the cloud metadata service
		"fe80::1",         // link-local
		"0.0.0.0",         // unspecified
		"::",              // unspecified
		"224.0.0.251",     // link-local multicast (mDNS)
		"239.1.2.3",       // multicast
		"ff05::1",         // multicast
	}
	for _, host := range blocked {
		if err := CheckOutboundHost(host); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("CheckOutboundHost(%q) = %v, want ErrBlockedDestination", host, err)
		}
	}

	allowed := []string{
		"127.0.0.1", // loopback: a local model gateway
		"::1",
		"10.1.2.3", // RFC1918: the LAN
		"192.168.1.5",
		"172.16.5.5",
		"8.8.8.8",
	}
	for _, host := range allowed {
		if err := CheckOutboundHost(host); err != nil {
			t.Errorf("CheckOutboundHost(%q) = %v, want nil", host, err)
		}
	}
}

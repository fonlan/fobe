//go:build !linux

package agent

import (
	"net"
	"time"
)

// hasRawSocket probes raw-ICMP capability portably where the syscall route
// is unavailable: opening an ip4:icmp packet conn needs the same privilege.
func hasRawSocket() bool {
	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// icmpAvailable is the Caps.ICMP bit. Off-linux there is no echo transport
// yet: the latency loop marks such samples as loss (design §5.1).
func icmpAvailable() bool { return false }

// icmpEchoMs stays unimplemented off-linux: the agent targets x86 Linux and
// OpenWrt (design §5.1). The latency loop marks such samples as loss.
func icmpEchoMs(string, time.Duration) (float64, error) {
	return 0, errICMPUnimplemented
}

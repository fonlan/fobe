//go:build linux

package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Raw-socket ICMP echo (design §13): the agent runs as root, so a plain
// AF_INET/SOCK_RAW/IPPROTO_ICMP socket works everywhere including OpenWrt
// busybox (no ping binary needed). One probe in flight at a time — the
// latency loop is serial, the mutex only guards stray concurrent callers.
var (
	icmpMu     sync.Mutex
	icmpSeqNum atomic.Uint32
)

var errICMPTimeout = errors.New("icmp echo timeout")

// hasRawSocket probes whether ICMP echo is possible (root / CAP_NET_RAW).
func hasRawSocket() bool {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
}

func nextICMPSeq() uint16 {
	return uint16(icmpSeqNum.Add(1) & 0xffff)
}

// icmpEchoMs sends one ICMP echo to host and waits for the matching reply,
// returning the RTT in milliseconds. timeout bounds the whole exchange (3s).
func icmpEchoMs(host string, timeout time.Duration) (float64, error) {
	icmpMu.Lock()
	defer icmpMu.Unlock()

	dst, err := resolveIPv4(host)
	if err != nil {
		return 0, err
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err != nil {
		return 0, fmt.Errorf("raw socket: %w", err)
	}
	defer func() { _ = syscall.Close(fd) }()

	// bound every recvfrom; SO_RCVTIMEO wakes the read instead of blocking forever
	tv := syscall.NsecToTimeval(int64(timeout))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return 0, fmt.Errorf("set rcvtimeo: %w", err)
	}

	id := uint16(os.Getpid() & 0xffff)
	seq := nextICMPSeq()
	start := time.Now()
	pkt := buildICMPEchoRequest(id, seq, start.UnixNano())

	if err := syscall.Sendto(fd, pkt, 0, dst); err != nil {
		return 0, fmt.Errorf("sendto: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
				return 0, errICMPTimeout
			}
			return 0, fmt.Errorf("recvfrom: %w", err)
		}
		rep, ok := findEchoReply(buf[:n])
		// match id+seq and the echoed timestamp: stale replies from an
		// earlier probe of the same pid must not count as success
		if !ok || rep.ID != id || rep.Seq != seq {
			continue
		}
		if len(rep.Payload) != icmpPayloadLen ||
			binary.BigEndian.Uint64(rep.Payload) != uint64(start.UnixNano()) {
			continue
		}
		return float64(time.Since(start).Microseconds()) / 1000.0, nil
	}
}

// resolveIPv4 picks the first IPv4 address for host (ICMP v4 echo only).
func resolveIPv4(host string) (*syscall.SockaddrInet4, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		addrs, err := net.LookupIP(host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		for _, a := range addrs {
			if v4 := a.To4(); v4 != nil {
				ip = v4
				break
			}
		}
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, fmt.Errorf("no IPv4 address for %q", host)
	}
	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], v4)
	return sa, nil
}

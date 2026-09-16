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

// ICMP echo transports (design §13), probed in this order:
//
//  1. SOCK_RAW/IPPROTO_ICMP — root or CAP_NET_RAW. The historical path; works
//     everywhere including OpenWrt busybox (no ping binary needed). The socket
//     receives the host's *whole* ICMP stream, hence the id+seq+payload match.
//  2. SOCK_DGRAM/IPPROTO_ICMP — the kernel "ping socket": an unprivileged
//     user may open one when ping_group_range covers its groups. The kernel
//     assigns the echo identifier (getsockname's "port"), drops replies that
//     do not carry it, and delivers datagrams without the IP header.
//
// Neither available → ICMP targets are marked loss and caps.icmp reports
// false (§5.1: degrade to TCP-only, never guess). One probe in flight at a
// time — the latency loop is serial, the mutex only guards stray concurrent
// callers.
var (
	icmpMu     sync.Mutex
	icmpSeqNum atomic.Uint32
)

var (
	errICMPTimeout     = errors.New("icmp echo timeout")
	errICMPNoIdent     = errors.New("ping socket: kernel assigned no echo ident")
	icmpTransportFuncs = []struct {
		kind  icmpTransportMode
		open  func() (*icmpConn, error)
		probe func() bool
	}{
		{icmpRawSocket, openRawICMP, hasRawSocket},
		{icmpPingSocket, openPingSocket, hasPingSocket},
	}
)

type icmpTransportMode int

const (
	icmpNone icmpTransportMode = iota
	icmpRawSocket
	icmpPingSocket
)

// icmpMode picks the best echo transport available right now. Not cached:
// the probe is one socket() call and capabilities can change under the
// process (a unit restarted with different AmbientCapabilities).
func icmpMode() icmpTransportMode {
	for _, t := range icmpTransportFuncs {
		if t.probe() {
			return t.kind
		}
	}
	return icmpNone
}

// icmpAvailable is the Caps.ICMP bit (§5.1): "this process can send echoes".
func icmpAvailable() bool { return icmpMode() != icmpNone }

// hasRawSocket probes raw ICMP (root / CAP_NET_RAW).
func hasRawSocket() bool {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
}

// hasPingSocket probes the unprivileged ping socket (ping_group_range).
func hasPingSocket() bool {
	conn, err := openPingSocket()
	if err != nil {
		return false
	}
	_ = syscall.Close(conn.fd)
	return true
}

// icmpConn bundles what one echo exchange needs from a transport: the fd and
// the echo identifier the kernel will accept (pid-derived for raw; the
// assigned "port" for a ping socket).
type icmpConn struct {
	fd int
	id uint16
}

// openRawICMP: any identifier goes on a raw socket; pid & 0xffff keeps ours
// distinct from other pingers on the host.
func openRawICMP() (*icmpConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err != nil {
		return nil, fmt.Errorf("raw socket: %w", err)
	}
	return &icmpConn{fd: fd, id: uint16(os.Getpid() & 0xffff)}, nil
}

// openPingSocket binds a SOCK_DGRAM ICMP socket so the kernel assigns a
// stable echo identifier before the first send; replies not carrying it are
// filtered kernel-side.
func openPingSocket() (*icmpConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		return nil, fmt.Errorf("ping socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{}); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("bind ping socket: %w", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("ping socket ident: %w", err)
	}
	v4, ok := sa.(*syscall.SockaddrInet4)
	if !ok || v4.Port <= 0 {
		_ = syscall.Close(fd)
		return nil, errICMPNoIdent
	}
	return &icmpConn{fd: fd, id: uint16(v4.Port)}, nil
}

func nextICMPSeq() uint16 {
	return uint16(icmpSeqNum.Add(1) & 0xffff)
}

// icmpEchoMs sends one ICMP echo to host and waits for the matching reply,
// returning the RTT in milliseconds. timeout bounds the whole exchange (3s).
func icmpEchoMs(host string, timeout time.Duration) (float64, error) {
	icmpMu.Lock()
	defer icmpMu.Unlock()

	mode := icmpMode()
	for _, t := range icmpTransportFuncs {
		if t.kind != mode {
			continue
		}
		return icmpEchoVia(t.open, host, timeout)
	}
	return 0, errICMPUnimplemented
}

// icmpEchoVia resolves host, opens the transport and runs one bounded echo
// exchange. Shared by both transports: the packet layout is identical, only
// the identifier source and the kernel-side reply filtering differ.
func icmpEchoVia(open func() (*icmpConn, error), host string, timeout time.Duration) (float64, error) {
	dst, err := resolveIPv4(host)
	if err != nil {
		return 0, err
	}
	conn, err := open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = syscall.Close(conn.fd) }()

	// bound every recvfrom; SO_RCVTIMEO wakes the read instead of blocking forever
	tv := syscall.NsecToTimeval(int64(timeout))
	if err := syscall.SetsockoptTimeval(conn.fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return 0, fmt.Errorf("set rcvtimeo: %w", err)
	}

	seq := nextICMPSeq()
	start := time.Now()
	pkt := buildICMPEchoRequest(conn.id, seq, start.UnixNano())

	if err := syscall.Sendto(conn.fd, pkt, 0, dst); err != nil {
		return 0, fmt.Errorf("sendto: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := syscall.Recvfrom(conn.fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
				return 0, errICMPTimeout
			}
			return 0, fmt.Errorf("recvfrom: %w", err)
		}
		rep, ok := findEchoReply(buf[:n])
		// match id+seq and the echoed timestamp: stale replies from an
		// earlier probe of the same pid must not count as success
		if !ok || rep.ID != conn.id || rep.Seq != seq {
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

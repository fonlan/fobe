//go:build linux

package agent

import "testing"

func TestICMPModePrefersRawThenPingSocket(t *testing.T) {
	original := icmpTransportFuncs
	t.Cleanup(func() { icmpTransportFuncs = original })

	transport := func(kind icmpTransportMode, available bool) struct {
		kind  icmpTransportMode
		open  func() (*icmpConn, error)
		probe func() bool
	} {
		return struct {
			kind  icmpTransportMode
			open  func() (*icmpConn, error)
			probe func() bool
		}{kind: kind, probe: func() bool { return available }}
	}

	icmpTransportFuncs = []struct {
		kind  icmpTransportMode
		open  func() (*icmpConn, error)
		probe func() bool
	}{transport(icmpRawSocket, false), transport(icmpPingSocket, true)}
	if got := icmpMode(); got != icmpPingSocket {
		t.Fatalf("icmpMode() = %v, want ping socket when raw is unavailable", got)
	}

	icmpTransportFuncs = []struct {
		kind  icmpTransportMode
		open  func() (*icmpConn, error)
		probe func() bool
	}{transport(icmpRawSocket, true), transport(icmpPingSocket, true)}
	if got := icmpMode(); got != icmpRawSocket {
		t.Fatalf("icmpMode() = %v, want raw socket preferred over ping socket", got)
	}

	icmpTransportFuncs = []struct {
		kind  icmpTransportMode
		open  func() (*icmpConn, error)
		probe func() bool
	}{transport(icmpRawSocket, false), transport(icmpPingSocket, false)}
	if got := icmpMode(); got != icmpNone || icmpAvailable() {
		t.Fatalf("unavailable transports yielded mode=%v available=%v", got, icmpAvailable())
	}
}

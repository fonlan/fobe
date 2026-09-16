//go:build !linux

package collect

import "github.com/fonlan/fobe/internal/protocol"

// Non-linux stubs so the repo builds on dev machines (macOS). The agent
// binary itself is only ever cross-compiled for linux/amd64 (design §5.1).

type netCounters struct {
	rx, tx uint64
}

func (c *Collector) readNet(string, *protocol.Metrics) (name string, rx, tx uint64, ok bool) {
	return "", 0, 0, false
}

func readDisks() []protocol.Disk { return []protocol.Disk{} }

func NetworkInterfaces() []protocol.NetworkInterface { return []protocol.NetworkInterface{} }

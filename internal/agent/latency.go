package agent

import (
	"context"
	"net"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
)

// hasRawSocket lives in the per-platform ICMP files: it probes whether raw
// ICMP is possible (root / CAP_NET_RAW). Missing capability → ICMP disabled,
// TCP-only latency (design §5.1).

// latencyLoop implements design §13: probe each target every 5s locally and
// report a 60s batch (12 samples). TCP handshake RTT plus raw-socket ICMP
// echo on linux (agent is root; missing CAP_NET_RAW degrades to TCP-only).
func (s *agentSession) latencyLoop() {
	const (
		localEvery   = 5 * time.Second
		batchEvery   = 60 * time.Second
		probeTimeout = 3 * time.Second
	)

	local := time.NewTicker(localEvery)
	defer local.Stop()
	batch := time.NewTicker(batchEvery)
	defer batch.Stop()

	// ring of the last 60s of samples
	samples := make([]protocol.LatencySample, 0, 16)
	flush := func() {
		if len(samples) == 0 {
			return
		}
		s.sendEnvelope(protocol.NewEnvelope(protocol.TypeLatency, "", protocol.LatencyBatch{Samples: samples}))
		samples = samples[:0]
	}

	for {
		select {
		case <-s.done:
			return
		case <-local.C:
			for _, t := range s.targets {
				sm := protocol.LatencySample{
					TargetID: t.ID,
					TS:       protocol.Now(),
					ICMPMs:   -1,
					TCPMs:    -1,
				}
				if t.Kind == "tcp" && t.Port > 0 {
					if ms, err := tcpHandshakeMs(t.Host, t.Port, probeTimeout); err == nil {
						sm.TCPMs = ms
					} else {
						sm.Loss = 1
					}
				} else if t.Kind == "icmp" && hasRawSocket() {
					if ms, err := icmpEchoMs(t.Host, probeTimeout); err == nil {
						sm.ICMPMs = ms
					} else {
						sm.Loss = 1
					}
				} else {
					sm.Loss = 1
				}
				samples = append(samples, sm)
			}
		case <-batch.C:
			flush()
		}
	}
}

func tcpHandshakeMs(host string, port int, timeout time.Duration) (float64, error) {
	d := net.Dialer{Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, itoa(port)))
	if err != nil {
		return 0, err
	}
	elapsed := float64(time.Since(start).Microseconds()) / 1000.0
	_ = conn.Close()
	return elapsed, nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

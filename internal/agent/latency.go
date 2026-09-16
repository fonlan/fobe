package agent

import (
	"context"
	"net"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
)

// hasRawSocket/hasPingSocket live in the per-platform ICMP files: they probe
// whether raw ICMP is possible (root / CAP_NET_RAW) or the kernel ping socket
// is open to us (ping_group_range). Neither available → ICMP disabled,
// TCP-only latency (design §5.1).

const (
	defaultLatencyInterval = 5 * time.Second
	latencyBatchEvery      = 60 * time.Second
	latencyProbeTimeout    = 3 * time.Second
)

// latencyLoop implements design §13: probe each target at the panel-selected
// cadence and report a 60s batch. TCP handshake RTT plus ICMP echo on linux
// (raw socket as root/CAP_NET_RAW, kernel ping socket under ping_group_range,
// otherwise TCP-only).
func (s *agentSession) latencyLoop() {
	local := time.NewTicker(s.latencyEvery())
	defer local.Stop()
	batch := time.NewTicker(latencyBatchEvery)
	defer batch.Stop()

	// The batch is bounded by the selected interval and enabled targets.
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
			for _, t := range s.latencyTargets() {
				sm := protocol.LatencySample{
					TargetID: t.ID,
					TS:       protocol.Now(),
					ICMPMs:   -1,
					TCPMs:    -1,
				}
				if t.Kind == "tcp" && t.Port > 0 {
					if ms, err := tcpHandshakeMs(t.Host, t.Port, latencyProbeTimeout); err == nil {
						sm.TCPMs = ms
					} else {
						sm.Loss = 1
					}
				} else if t.Kind == "icmp" && icmpAvailable() {
					if ms, err := icmpEchoMs(t.Host, latencyProbeTimeout); err == nil {
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
		case <-s.latencyCadence:
			local.Stop()
			local = time.NewTicker(s.latencyEvery())
		}
	}
}

func (s *agentSession) latencyEvery() time.Duration {
	seconds := s.latencyInterval.Load()
	if seconds <= 0 {
		return defaultLatencyInterval
	}
	return time.Duration(seconds) * time.Second
}

func (s *agentSession) latencyTargets() []protocol.TargetSpec {
	s.latencyMu.RLock()
	defer s.latencyMu.RUnlock()
	return append([]protocol.TargetSpec{}, s.targets...)
}

func (s *agentSession) setLatencyTargets(targets []protocol.TargetSpec) {
	s.latencyMu.Lock()
	s.targets = append([]protocol.TargetSpec{}, targets...)
	s.latencyMu.Unlock()
}

func (s *agentSession) setLatencyInterval(seconds int) {
	if seconds < 1 || seconds > 3600 {
		seconds = int(defaultLatencyInterval / time.Second)
	}
	s.latencyInterval.Store(int64(seconds))
	select {
	case s.latencyCadence <- struct{}{}:
	default:
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

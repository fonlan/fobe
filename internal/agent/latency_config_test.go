package agent

import (
	"log/slog"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
)

func TestLatencyIntervalDefaultsAndUpdates(t *testing.T) {
	s := &agentSession{latencyCadence: make(chan struct{}, 1)}

	if got := s.latencyEvery(); got != defaultLatencyInterval {
		t.Fatalf("default interval = %s, want %s", got, defaultLatencyInterval)
	}

	s.setLatencyInterval(17)
	if got := s.latencyEvery(); got != 17*time.Second {
		t.Fatalf("configured interval = %s, want 17s", got)
	}
	select {
	case <-s.latencyCadence:
	default:
		t.Fatal("interval update did not wake the latency loop")
	}

	s.setLatencyInterval(0)
	if got := s.latencyEvery(); got != defaultLatencyInterval {
		t.Fatalf("zero interval = %s, want default %s", got, defaultLatencyInterval)
	}
}

// The latency_config frame now carries the per-node target list too (design
// §13 实现修订 2026-09-17k). nil keeps the handshake list, a populated list
// replaces it, and an empty list must really stop the probes.
func TestLatencyConfigFrameUpdatesTargets(t *testing.T) {
	s := &agentSession{
		log:            slog.Default(),
		latencyCadence: make(chan struct{}, 1),
	}
	s.setLatencyTargets([]protocol.TargetSpec{{ID: 1, Kind: "icmp", Host: "203.0.113.1"}})

	// Global cadence broadcast: no targets — keep probing as told.
	s.handleServerFrame(protocol.NewEnvelope(protocol.TypeLatencyCfg, "",
		protocol.LatencyConfig{IntervalSec: 17}))
	if got := s.latencyTargets(); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("broadcast wiped targets: %+v", got)
	}
	if got := s.latencyEvery(); got != 17*time.Second {
		t.Fatalf("interval = %s, want 17s", got)
	}

	// Per-node push with a new list replaces it.
	s.handleServerFrame(protocol.NewEnvelope(protocol.TypeLatencyCfg, "",
		protocol.LatencyConfig{IntervalSec: 17, Targets: []protocol.TargetSpec{
			{ID: 2, Kind: "tcp", Host: "203.0.113.2", Port: 443},
			{ID: 3, Kind: "icmp", Host: "203.0.113.3"},
		}}))
	if got := s.latencyTargets(); len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("targets not replaced: %+v", got)
	}

	// Empty list clears: the node must be able to opt out of measurement.
	s.handleServerFrame(protocol.NewEnvelope(protocol.TypeLatencyCfg, "",
		protocol.LatencyConfig{IntervalSec: 17, Targets: []protocol.TargetSpec{}}))
	if got := s.latencyTargets(); len(got) != 0 {
		t.Fatalf("empty list did not clear targets: %+v", got)
	}
}

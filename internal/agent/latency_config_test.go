package agent

import (
	"testing"
	"time"
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

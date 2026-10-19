package agent

import (
	"testing"
	"time"
)

// The backoff is a tail *after* the link returns, so its cap has to stay well
// inside the server's 90s heartbeat window: a multi-minute sleep turns a short
// path blip into a false "offline" edge (2026-10-19).
func TestBackoffCapStaysInsideHeartbeatWindow(t *testing.T) {
	worst := backoffMax + backoffMax/2 // Run's +50% jitter
	if worst >= 90*time.Second {
		t.Fatalf("max reconnect delay %s must stay under the 90s heartbeat window", worst)
	}
}

// A session that outlived stableSession proves the link works; the next drop
// must start over instead of inheriting growth from earlier short sessions.
func TestNextBackoffResetsAfterStableSession(t *testing.T) {
	if got := nextBackoff(8*time.Second, stableSession+time.Second); got != backoffStart {
		t.Fatalf("stable session backoff = %s, want %s", got, backoffStart)
	}
}

func TestNextBackoffDoublesAndCaps(t *testing.T) {
	cases := []struct {
		backoff time.Duration
		want    time.Duration
	}{
		{1 * time.Second, 2 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{8 * time.Second, backoffMax},
		{backoffMax, backoffMax},
	}
	for _, c := range cases {
		if got := nextBackoff(c.backoff, time.Second); got != c.want {
			t.Fatalf("nextBackoff(%s, short session) = %s, want %s", c.backoff, got, c.want)
		}
	}
}

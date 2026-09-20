package httpapi

import (
	"testing"
	"time"
)

// The gate is what keeps an unauthenticated flood from turning into N×64 MiB of
// argon2id working set, so both halves (rate and in-flight) are pinned.

func TestLoginGateCapsInflight(t *testing.T) {
	now := time.Unix(0, 0)
	g := &loginGate{now: func() time.Time { return now }}

	for i := 0; i < loginGateMaxInflight; i++ {
		if !g.tryAcquire() {
			t.Fatalf("slot %d refused, want %d concurrent verifications", i+1, loginGateMaxInflight)
		}
	}
	if g.tryAcquire() {
		t.Fatalf("a %dth concurrent verification was allowed", loginGateMaxInflight+1)
	}
	g.release()
	if !g.tryAcquire() {
		t.Fatal("a released slot must be reusable")
	}
}

func TestLoginGateRefillsOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	g := &loginGate{now: func() time.Time { return now }}

	// Drain the bucket while releasing each slot, so what fails next is the
	// rate and not the in-flight cap.
	for i := 0; i < loginGateBurst; i++ {
		if !g.tryAcquire() {
			t.Fatalf("burst attempt %d refused", i+1)
		}
		g.release()
	}
	if g.tryAcquire() {
		t.Fatal("the bucket must be empty after a full burst")
	}

	now = now.Add(time.Duration(float64(time.Second)/loginGatePerSecond) + time.Millisecond)
	if !g.tryAcquire() {
		t.Fatal("the bucket must refill with elapsed time")
	}
}

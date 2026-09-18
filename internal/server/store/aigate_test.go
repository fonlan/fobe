package store

import (
	"path/filepath"
	"testing"
)

// Tests for the §12.3 circuit breaker (CheckAICommandGate).
//
// The gate shipped with ZERO coverage — `failure_paused` / `rate_limited` were
// unreachable by any test — while §12.6 builds the autonomous loop on top of it.
// Rows are inserted directly because CreateCommandWithMeta always stamps
// created_at = now() and status = 'pending', and the whole point here is
// controlling the timestamps and outcomes the gate reads.

func newGateTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateNode(&Node{ID: "n1", Name: "gate", MachineID: "m1", TZ: "UTC"}, "hash"); err != nil {
		t.Fatal(err)
	}
	return st
}

// insertCommand writes one command row with an explicit outcome and clock.
func insertCommand(t *testing.T, st *Store, id, actor, status string, createdAt int64, finishedAt int64) {
	t.Helper()
	var finished any
	if finishedAt > 0 {
		finished = finishedAt
	}
	if _, err := st.db.Exec(
		`INSERT INTO commands (id, node_id, kind, payload, status, created_at, ttl_seconds, actor, finished_at)
		 VALUES (?, 'n1', 'run_shell', '{}', ?, ?, 600, ?, ?)`,
		id, status, createdAt, actor, finished,
	); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAICommandGateRateLimitedByMinuteWindow(t *testing.T) {
	st := newGateTestStore(t)
	at := now()

	// Ten AI commands inside the window, plus panel-actor noise that must not
	// count: the gate is about what the MODEL can cause, not what the operator
	// clicked.
	for i := 0; i < 10; i++ {
		insertCommand(t, st, "ai-"+string(rune('a'+i)), "ai", "ok", at-10, at-10)
	}
	for i := 0; i < 5; i++ {
		insertCommand(t, st, "panel-"+string(rune('a'+i)), "panel", "ok", at-10, at-10)
	}

	allowed, reason, err := st.CheckAICommandGate("n1", 10, at)
	if err != nil {
		t.Fatal(err)
	}
	if allowed || reason != "rate_limited" {
		t.Fatalf("gate = %v/%q, want blocked rate_limited", allowed, reason)
	}
	// A wider window budget (the §12.6 widening to 30/min) lets the same load
	// through — that is the whole reason the limit is a parameter.
	allowed, reason, err = st.CheckAICommandGate("n1", 30, at)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatalf("gate blocked under a 30/min budget: %q", reason)
	}
}

func TestCheckAICommandGateIgnoresCommandsOlderThanTheWindow(t *testing.T) {
	st := newGateTestStore(t)
	at := now()
	// 61 seconds old: outside the one-minute window, so a busy turn an hour ago
	// must not keep the gate shut now.
	for i := 0; i < 20; i++ {
		insertCommand(t, st, "old-"+string(rune('a'+i)), "ai", "ok", at-61, at-61)
	}
	allowed, reason, err := st.CheckAICommandGate("n1", 10, at)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatalf("stale commands still counted: %q", reason)
	}
}

func TestCheckAICommandGatePausesAfterConsecutiveFailures(t *testing.T) {
	st := newGateTestStore(t)
	at := now()
	// Three failed AI commands, most recent last. The pause is the §12.3
	// "连续失败自动暂停" control, and it is what stops an autonomous loop from
	// hammering a probe with a command that cannot succeed.
	insertCommand(t, st, "f1", "ai", "failed", at-30, at-30)
	insertCommand(t, st, "f2", "ai", "failed", at-20, at-20)
	insertCommand(t, st, "f3", "ai", "failed", at-5, at-5)

	allowed, reason, err := st.CheckAICommandGate("n1", 30, at)
	if err != nil {
		t.Fatal(err)
	}
	if allowed || reason != "failure_paused" {
		t.Fatalf("gate = %v/%q, want blocked failure_paused", allowed, reason)
	}

	var failures, pausedUntil int64
	if err := st.db.QueryRow(
		`SELECT consecutive_failures, paused_until FROM ai_node_state WHERE node_id = 'n1'`,
	).Scan(&failures, &pausedUntil); err != nil {
		t.Fatal(err)
	}
	if failures != 3 {
		t.Fatalf("consecutive_failures = %d, want 3", failures)
	}
	if pausedUntil != at+300 {
		t.Fatalf("paused_until = %d, want %d (a fixed 5-minute pause)", pausedUntil, at+300)
	}
}

func TestCheckAICommandGatePauseExpiresAndResets(t *testing.T) {
	st := newGateTestStore(t)
	at := now()
	insertCommand(t, st, "f1", "ai", "failed", at-30, at-30)
	insertCommand(t, st, "f2", "ai", "failed", at-20, at-20)
	insertCommand(t, st, "f3", "ai", "failed", at-5, at-5)

	// First call arms the pause...
	if allowed, _, err := st.CheckAICommandGate("n1", 30, at); err != nil || allowed {
		t.Fatalf("gate = %v (err %v), want blocked to arm the pause", allowed, err)
	}
	// ...and a call past BOTH the 5-minute pause and the 10-minute failure
	// window allows again and clears the state. This is the property that was
	// missing: re-deriving the streak from rows that never age out froze the
	// node's AI execution forever, and no success could ever clear it because
	// the gate itself blocks the execution that would produce one.
	after := at + 700
	allowed, reason, err := st.CheckAICommandGate("n1", 30, after)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatalf("gate still blocked after the window passed: %q", reason)
	}
	var failures, pausedUntil int64
	if err := st.db.QueryRow(
		`SELECT consecutive_failures, paused_until FROM ai_node_state WHERE node_id = 'n1'`,
	).Scan(&failures, &pausedUntil); err != nil {
		t.Fatal(err)
	}
	if pausedUntil != 0 || failures != 0 {
		t.Fatalf("state not reset after expiry: failures=%d paused_until=%d", failures, pausedUntil)
	}
	// A fresh failure inside the window still pauses again: the fix must bound
	// the window, not disable the control.
	insertCommand(t, st, "f4", "ai", "failed", after-5, after-5)
	insertCommand(t, st, "f5", "ai", "failed", after-4, after-4)
	insertCommand(t, st, "f6", "ai", "failed", after-3, after-3)
	if allowed, _, err := st.CheckAICommandGate("n1", 30, after); err != nil || allowed {
		t.Fatalf("fresh failure cluster did not pause: allowed=%v err=%v", allowed, err)
	}
}

func TestCheckAICommandGateSuccessBreaksTheFailureStreak(t *testing.T) {
	st := newGateTestStore(t)
	at := now()
	// Oldest two failed, the newest succeeded: the gate looks at the LEADING
	// run of failures, so a working probe that failed once must not be paused.
	insertCommand(t, st, "f1", "ai", "failed", at-30, at-30)
	insertCommand(t, st, "f2", "ai", "failed", at-20, at-20)
	insertCommand(t, st, "ok1", "ai", "ok", at-5, at-5)

	allowed, reason, err := st.CheckAICommandGate("n1", 30, at)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatalf("a success did not break the streak: %q", reason)
	}
}

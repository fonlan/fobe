package store

import "testing"

// §4.3 实现修订 2026-09-20: the failure streak must not outlive the block it
// produced, otherwise a single typo re-blocks an IP forever.

func TestBlacklistStreakResetsAfterBlockExpires(t *testing.T) {
	st := newTestStore(t)
	const ip = "203.0.113.9"

	for i := 0; i < 3; i++ {
		if _, err := st.RecordLoginFail(ip, "login_failed", 3, 1800); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if e, _ := st.IsBlacklisted(ip); e == nil {
		t.Fatal("three failures must produce an active block")
	}

	// The block serves its time.
	if _, err := st.db.Exec("UPDATE ip_blacklist SET expires_at = ? WHERE ip = ?", now()-10, ip); err != nil {
		t.Fatal(err)
	}
	if e, _ := st.IsBlacklisted(ip); e != nil {
		t.Fatal("an expired block must not report as active")
	}

	count, err := st.RecordLoginFail(ip, "login_failed", 3, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count=%d, want 1: an expired block starts a new streak", count)
	}
	if e, _ := st.IsBlacklisted(ip); e != nil {
		t.Fatal("one failure after expiry must not re-block the IP immediately")
	}
}

func TestClearLoginFailsForgetsAnExpiredBlock(t *testing.T) {
	st := newTestStore(t)
	const ip = "203.0.113.10"

	for i := 0; i < 3; i++ {
		if _, err := st.RecordLoginFail(ip, "login_failed", 3, 1800); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec("UPDATE ip_blacklist SET expires_at = ? WHERE ip = ?", now()-10, ip); err != nil {
		t.Fatal(err)
	}
	st.ClearLoginFails(ip) // the operator logged in successfully

	count, _ := st.RecordLoginFail(ip, "login_failed", 3, 1800)
	if count != 1 {
		t.Fatalf("count=%d, want 1: a successful login must forget the old streak", count)
	}
}

func TestRevokeOtherSessionsKeepsCurrent(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := st.CreateSession(id, "ua", "1.2.3.4", now()-60); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.RevokeOtherSessions("a")
	if err != nil || n != 2 {
		t.Fatalf("revoked=%d err=%v, want 2", n, err)
	}
	if s, _ := st.GetSession("a"); s.Revoked {
		t.Fatal("the caller's own session must survive a password change")
	}
	for _, id := range []string{"b", "c"} {
		if s, _ := st.GetSession(id); !s.Revoked {
			t.Fatalf("session %s must be revoked", id)
		}
	}
}

func TestPruneExpiredSessions(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession("old", "ua", "", now()-100); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("new", "ua", "", now()-1); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneExpiredSessions(now() - 50)
	if err != nil || n != 1 {
		t.Fatalf("pruned=%d err=%v, want 1", n, err)
	}
	if _, err := st.GetSession("old"); err == nil {
		t.Fatal("the expired session row must be gone")
	}
	if _, err := st.GetSession("new"); err != nil {
		t.Fatalf("the fresh session must survive: %v", err)
	}
}

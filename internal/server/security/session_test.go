package security

import "testing"

// §4.1 实现修订 2026-09-20: the cookie's MaxAge was the only TTL; the server now
// enforces the same absolute lifetime.

func TestSessionExpired(t *testing.T) {
	created := int64(1700000000)
	ttl := int64(SessionTTL.Seconds())

	if SessionExpired(created, created) {
		t.Fatal("a session created now is valid")
	}
	if SessionExpired(created, created+ttl-1) {
		t.Fatal("a session must be valid one second before the boundary")
	}
	if !SessionExpired(created, created+ttl) {
		t.Fatal("a session expires exactly at TTL")
	}
	if !SessionExpired(created, created+ttl+1) {
		t.Fatal("a session stays expired past TTL")
	}
}

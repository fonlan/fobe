package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fonlan/fobe/internal/server/store"
)

// Login failures are the §15 events the panel raises about itself (§15 实现修订
// 2026-09-20): the alert carries node_id="" because its subject is a source
// address, not a probe. These tests pin the two kinds and the global merge
// window that stops an unauthenticated endpoint from flooding the chat.

func TestLoginFailureAlertForProtectedSource(t *testing.T) {
	srv, api := newTestServer(t)
	client := &http.Client{}
	// httptest serves on 127.0.0.1, which §4.3 treats as protected: the failure
	// is alerted but never counted, and the payload says so.
	for i := 0; i < 4; i++ {
		resp, err := client.Post(srv.URL+"/api/login", "application/json",
			bytes.NewReader([]byte(`{"password":"nope"}`)))
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("protected failure: got %d, want 401", resp.StatusCode)
		}
	}
	alerts := mustAlerts(t, api, 10)
	if len(alerts) != 1 {
		t.Fatalf("protected failures: got %d alerts, want 1 (merged within the hour): %+v", len(alerts), alerts)
	}
	if alerts[0].Kind != alertLoginFailed || alerts[0].NodeID != "" {
		t.Fatalf("alert = %+v, want kind %s and no node_id", alerts[0], alertLoginFailed)
	}
	p := loginAlertPayload(t, alerts[0].Payload)
	if p["ip"] != "127.0.0.1" || p["protected"] != true {
		t.Fatalf("payload = %v, want ip 127.0.0.1 and protected true", p)
	}
	if _, counted := p["count"]; counted {
		t.Fatalf("a protected failure must not be counted: %v", p)
	}
}

func TestLoginBlacklistAlertForRemoteSource(t *testing.T) {
	srv, api := newTestServer(t)
	login := func(ip, password string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/login",
			bytes.NewReader([]byte(`{"password":"`+password+`"}`)))
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("login as %s: %v", ip, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	const ip = "203.0.113.90"
	if code := login(ip, "nope"); code != http.StatusUnauthorized {
		t.Fatalf("first remote failure: got %d, want 401", code)
	}
	// The merge window is global, not per-source (this endpoint is
	// unauthenticated): a rotating attacker must not be able to buy more
	// messages by changing address.
	if code := login("203.0.113.91", "nope"); code != http.StatusUnauthorized {
		t.Fatalf("rotated source failure: got %d, want 401", code)
	}
	alerts := mustAlerts(t, api, 10)
	if len(alerts) != 1 || alerts[0].Kind != alertLoginFailed {
		t.Fatalf("below threshold: %+v, want one %s alert", alerts, alertLoginFailed)
	}
	if p := loginAlertPayload(t, alerts[0].Payload); p["count"] != float64(1) || p["max_fails"] != float64(maxLoginFails) {
		t.Fatalf("payload = %v, want count 1 / max_fails %d", p, maxLoginFails)
	}

	// Two more failures cross the threshold. The crossing failure is the
	// blacklist event; a different kind means the first login_failed alert
	// cannot merge it away.
	if code := login(ip, "nope"); code != http.StatusUnauthorized {
		t.Fatalf("second failure: got %d, want 401", code)
	}
	if code := login(ip, "nope"); code != http.StatusUnauthorized {
		t.Fatalf("third failure: got %d, want 401", code)
	}
	alerts = mustAlerts(t, api, 10)
	if len(alerts) != 2 {
		t.Fatalf("after blacklist: got %d alerts, want 2: %+v", len(alerts), alerts)
	}
	blocked := alerts[0] // ListAlerts returns newest first
	if blocked.Kind != alertLoginBlacklisted {
		t.Fatalf("newest alert = %s, want %s", blocked.Kind, alertLoginBlacklisted)
	}
	if p := loginAlertPayload(t, blocked.Payload); p["ip"] != ip ||
		p["count"] != float64(maxLoginFails) || p["block_seconds"] != float64(blockSeconds) {
		t.Fatalf("blacklist payload = %v", p)
	}

	// A blocked address is refused before verification: no alert, no flood.
	if code := login(ip, "test-password-123"); code != http.StatusForbidden {
		t.Fatalf("blocked login: got %d, want 403", code)
	}
	if alerts := mustAlerts(t, api, 10); len(alerts) != 2 {
		t.Fatalf("blocked re-attempt must not add an alert: %+v", alerts)
	}
}

func loginAlertPayload(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("alert payload %q: %v", raw, err)
	}
	return m
}

func mustAlerts(t *testing.T, api *Server, limit int) []store.Alert {
	t.Helper()
	alerts, err := api.Store.ListAlerts(limit)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	return alerts
}

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/security"
)

// Session hardening added 2026-09-20: the cookie's MaxAge was the only TTL, the
// session id itself travelled in the sessions API, and must_change_password was
// enforced by the SPA redirect alone. Each of those is pinned here.

func TestExpiredSessionIsRefusedAndRevoked(t *testing.T) {
	srv, api := newTestServer(t)

	stale := "stale-" + strings.Repeat("x", 20)
	created := nowUnix() - int64(security.SessionTTL.Seconds()) - 60
	if err := api.Store.CreateSession(stale, "ua", "203.0.113.9", created); err != nil {
		t.Fatal(err)
	}
	cookie := security.SessionCookieName + "=" + stale

	resp, _ := doAuthed(t, http.MethodGet, srv.URL+"/api/me", cookie, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session: status %d, want 401", resp.StatusCode)
	}
	sess, err := api.Store.GetSession(stale)
	if err != nil {
		t.Fatalf("session row should still exist: %v", err)
	}
	if !sess.Revoked {
		t.Fatal("an expired session must be marked revoked, not merely refused")
	}
}

func TestMustChangePasswordIsEnforcedServerSide(t *testing.T) {
	srv, api := newTestServer(t)
	user, err := api.Store.GetUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.UpdatePassword(user.ID, user.PasswordHash, true); err != nil {
		t.Fatal(err)
	}

	cookie := loginSession(t, srv)

	// The change-password flow itself must stay reachable.
	if resp, _ := doAuthed(t, http.MethodGet, srv.URL+"/api/me", cookie, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me: status %d, want 200 while a change is pending", resp.StatusCode)
	}

	resp, body := doAuthed(t, http.MethodGet, srv.URL+"/api/nodes", cookie, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("/api/nodes: status %d, want 409 while a change is pending", resp.StatusCode)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err != nil || errBody.Error.Code != "password_change_required" {
		t.Fatalf("body %s: want error code password_change_required", body)
	}
}

func TestSessionListNeverEchoesTheCredential(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginSession(t, srv)
	sid := strings.TrimPrefix(cookie, security.SessionCookieName+"=")

	resp, body := doAuthed(t, http.MethodGet, srv.URL+"/api/sessions?all=1", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), sid) {
		t.Fatal("the session list echoed the raw session id (which is the cookie value)")
	}
	if !strings.Contains(string(body), "id_short") {
		t.Fatalf("body %s: want the id_short label", body)
	}
}

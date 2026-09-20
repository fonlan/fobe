package security

import (
	"net/http"
	"time"
)

// SessionCookieName is the HttpOnly session cookie (design §4.1).
const SessionCookieName = "fobe_session"

// SessionTTL is the absolute lifetime of a panel session (design §4.1). It
// drives both the cookie's MaxAge and the server-side check in requireSession:
// the cookie alone is enforced by the browser, so a stolen id stayed valid
// forever (implementation revision 2026-09-20).
const SessionTTL = 30 * 24 * time.Hour

// SessionExpired reports whether a session created at createdAt has outlived
// SessionTTL at time now (both unix seconds).
func SessionExpired(createdAt, now int64) bool {
	return createdAt+int64(SessionTTL.Seconds()) <= now
}

// SetSessionCookie writes the session cookie. secure is decided by the caller
// (httpapi.cookieSecure): it ORs the request's own TLS evidence with the
// configured server.public_url scheme, because a proxy that forgets
// X-Forwarded-Proto used to silently strip Secure from an HTTPS deployment
// (§4.1 实现修订 2026-09-20).
func SetSessionCookie(w http.ResponseWriter, r *http.Request, id string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

// ClearSessionCookie expires the session cookie. secure must match the flag the
// cookie was set with, or some browsers keep the original.
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// SessionIDFromRequest extracts the cookie value, "" when absent.
func SessionIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

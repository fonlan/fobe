package security

import (
	"net/http"
	"time"
)

// SessionCookieName is the HttpOnly session cookie (design §4.1).
const SessionCookieName = "fobe_session"

const sessionTTL = 30 * 24 * time.Hour

// SetSessionCookie writes the session cookie. Secure is set when the request
// arrived over https (X-Forwarded-Proto) so plain-http local dev keeps working.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
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

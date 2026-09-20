package httpapi

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

const (
	maxLoginFails       = 3    // design §4.3: blacklist after 3 consecutive failures
	blockSeconds  int64 = 1800 // 30 min per offense; panel/CLI can unblock earlier
)

type loginReq struct {
	Password string `json:"password"`
}

type meResp struct {
	MustChangePassword bool   `json:"must_change_password"`
	Version            string `json:"version"`
}

// cookieSecure decides the Secure flag for the session cookie. It ORs the
// request's own TLS evidence with the configured server.public_url scheme: the
// first term keeps plain-http local dev working, and the second stops a proxy
// that forgets X-Forwarded-Proto from silently downgrading an HTTPS deployment
// to a cleartext cookie (§4.1 实现修订 2026-09-20).
func (s *Server) cookieSecure(r *http.Request) bool {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	v, err := s.Store.GetSetting("server.public_url")
	if err != nil || v == "" {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https"
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.Trust.RealIP(r)
	// 回环/私有/可信代理网段永不加黑(§4.3 防自锁硬规则):不查封禁、
	// 不记失败计数,避免本机操作或代理故障把自己锁在门外
	protected := s.Trust.NeverBlacklist(ip)

	if !protected {
		// blacklist first: a blocked IP never reaches password verification
		if entry, err := s.Store.IsBlacklisted(ip); err == nil && entry != nil {
			s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "login_blocked", SourceIP: ip})
			writeErr(w, http.StatusForbidden, "ip_blacklisted")
			return
		}
	}

	// Identity-independent brakes (§4.3 实现修订 2026-09-20). The blacklist
	// above is per-IP, so it cannot bound a distributed or header-rotating
	// attacker, while every verification below allocates 64 MiB. Refuse before
	// reading the body or touching the database: a rejected request must cost
	// the server nothing.
	if !s.loginGate.tryAcquire() {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "too_many_requests")
		return
	}
	defer s.loginGate.release()

	var req loginReq
	if err := decodeJSON(r, &req); err != nil || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}

	user, err := s.Store.GetUser()
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusServiceUnavailable, "not_initialized")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	if !security.VerifyPassword(req.Password, user.PasswordHash) {
		if protected {
			s.Store.InsertAudit(&store.AuditEntry{
				Actor: "panel", Action: "login_failed", SourceIP: ip, Command: "protected_ip_not_counted",
			})
			writeErr(w, http.StatusUnauthorized, "bad_credentials")
			return
		}
		count, err := s.Store.RecordLoginFail(ip, "login_failed", maxLoginFails, blockSeconds)
		if err == nil {
			detail := ""
			if count >= maxLoginFails {
				detail = "blacklisted"
			}
			s.Store.InsertAudit(&store.AuditEntry{
				Actor: "panel", Action: "login_failed", SourceIP: ip, Command: detail,
			})
		}
		code := "bad_credentials"
		if count >= maxLoginFails {
			code = "now_blacklisted"
		}
		writeErr(w, http.StatusUnauthorized, code)
		return
	}

	// success: clear the failure counter so the next run starts from zero
	s.Store.ClearLoginFails(ip)

	sid, err := security.RandomToken(32)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.CreateSession(sid, r.UserAgent(), ip, nowUnix()); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	security.SetSessionCookie(w, r, sid, s.cookieSecure(r))
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "login_ok", SourceIP: ip})
	writeJSON(w, http.StatusOK, meResp{MustChangePassword: user.MustChange, Version: s.Version})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sid := security.SessionIDFromRequest(r); sid != "" {
		_ = s.Store.RevokeSession(sid)
	}
	security.ClearSessionCookie(w, s.cookieSecure(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.Store.GetUser()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, meResp{MustChangePassword: user.MustChange, Version: s.Version})
}

type changePasswordReq struct {
	Old string `json:"old"`
	New string `json:"new"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordReq
	if err := decodeJSON(r, &req); err != nil || req.New == "" {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if len(req.New) < 8 {
		writeErr(w, http.StatusBadRequest, "password_too_short")
		return
	}
	user, err := s.Store.GetUser()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !security.VerifyPassword(req.Old, user.PasswordHash) {
		writeErr(w, http.StatusUnauthorized, "bad_credentials")
		return
	}
	hash, err := security.HashPassword(req.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.UpdatePassword(user.ID, hash, false); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Changing the password must evict whoever else holds a session — including
	// any socket they already upgraded, which would otherwise survive (§4.1
	// 实现修订 2026-09-20). The current browser stays signed in.
	keep := security.SessionIDFromRequest(r)
	if _, err := s.Store.RevokeOtherSessions(keep); err == nil {
		s.wsReg.closeExcept(keep)
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "password_changed", SourceIP: s.Trust.RealIP(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleHealth is the only unauthenticated status endpoint. It deliberately
// says nothing about whether a user exists: "initialized: false" told any
// scanner that this was a fresh panel whose one-time password is sitting in the
// startup log (§4.1 实现修订 2026-09-20).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.Version,
		"web":     s.WebDir != "",
	})
}

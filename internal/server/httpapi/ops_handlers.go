package httpapi

import (
	"net/http"
	"strconv"

	"github.com/fonlan/fobe/internal/server/store"
)

// audit writes a panel-actor audit entry.
func (s *Server) audit(action, detail, ip string) {
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: action, Command: detail, SourceIP: ip})
}

// --- blacklist surface (design §4.3: panel list + one-click unblock) ---

func (s *Server) handleListBlacklist(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.ListBlacklist()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	type entryView struct {
		IP        string `json:"ip"`
		Reason    string `json:"reason"`
		FailCount int    `json:"fail_count"`
		CreatedAt int64  `json:"created_at"`
		ExpiresAt int64  `json:"expires_at"`
	}
	out := make([]entryView, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryView{
			IP: e.IP, Reason: e.Reason, FailCount: e.FailCount,
			CreatedAt: e.CreatedAt, ExpiresAt: e.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (s *Server) handleUnblockIP(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if err := s.Store.UnblockIP(ip); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.audit("unblock", ip, s.Trust.RealIP(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- sessions (design §4.1: view + revoke all) ---

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("all") != "1"
	sessions, err := s.Store.ListSessions(activeOnly)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	type sessionView struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
		LastSeen  int64  `json:"last_seen"`
		UA        string `json:"ua"`
		IP        string `json:"ip"`
		Revoked   bool   `json:"revoked"`
	}
	out := make([]sessionView, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionView{
			ID: sess.ID, CreatedAt: sess.CreatedAt, LastSeen: sess.LastSeen,
			UA: sess.UA, IP: sess.IP, Revoked: sess.Revoked,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.RevokeAllSessions(); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.audit("revoke_all_sessions", "", s.Trust.RealIP(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- audit + alerts ---

// handleListAudit serves one numbered page of the audit trail, newest first.
// page/limit rather than a keyset cursor: the panel offers page numbers and a
// "jump to page" box, which needs OFFSET plus the total — see the cost written
// down in design §16.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	// Same leniency as limit: a missing or junk page is page 1, not an error.
	page := 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	total, err := s.Store.CountAudit()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	pages := (total + limit - 1) / limit
	if pages < 1 {
		pages = 1
	}
	// Clamp rather than serve an empty table: the operator may still hold a
	// page number from before the trail grew, or have typed one by hand. The
	// response echoes the page actually served so the control can snap to it.
	if page > pages {
		page = pages
	}
	entries, err := s.Store.ListAuditPage(limit, (page-1)*limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
		"page":    page,
		"limit":   limit,
	})
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	alerts, err := s.Store.ListAlerts(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}

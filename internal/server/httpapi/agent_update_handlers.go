package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fonlan/fobe/internal/server/agentupdate"
	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

// Agent self-update surface (design §5.5). The decisions themselves live in
// internal/server/agentupdate; these handlers only expose them to the panel:
// the cluster status (why is it on/off), the per-node operator retry, and the
// reinstall command for a probe whose binary predates §5.5.

// handleAgentUpdateStatus answers GET /api/agent/update.
func (s *Server) handleAgentUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.AgentUpdate == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": agentupdate.Status{Enabled: false, Reason: "not_wired"},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": s.AgentUpdate.Status()})
}

// handleAgentUpdateRetry answers POST /api/nodes/{id}/agent/retry: forget the
// attempt counters (both here and on the probe, via the state file's target
// change) and nudge an online agent so it does not wait for its next handshake.
func (s *Server) handleAgentUpdateRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if s.AgentUpdate == nil {
		writeErr(w, http.StatusServiceUnavailable, "not_wired")
		return
	}
	if err := s.AgentUpdate.Retry(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: id, Action: "agent_update_retry", SourceIP: s.Trust.RealIP(r),
	})
	// Recompute the plan and hand it to the agent right away. Offline nodes get
	// it from hello_ack on reconnect (§7) — nothing is replayed.
	pushed := s.Hub != nil && s.Hub.PushDesired(id)
	s.publishEvent("node_updated", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pushed": pushed})
}

// handleAgentReinstallCommand answers POST /api/nodes/{id}/agent/reinstall-command.
//
// Two situations need the same command, so they share one path (§4.2 revision
// 2026-09-17):
//
//   - a probe whose agent binary predates §5.5 never follows on its own (the new
//     fields are unknown to it and Caps reports no self_update);
//   - a probe whose config.json was lost, overwritten or moved to another machine
//     cannot authenticate any more, and the server holds nothing but the old
//     secret's hash — so there is no way to hand the credentials back out.
//
// The token minted here is therefore **bound to this node**: register accepts it
// for this node's machine_id without the old secret and issues fresh credentials
// (action node_reissued). The node id, its forwards, billing and settings survive,
// which a "delete node + add node" would not.
func (s *Server) handleAgentReinstallCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.Store.GetNode(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	baseURL, err := s.publicBaseURL(r)
	if errors.Is(err, errInvalidPublicURL) {
		writeErr(w, http.StatusBadRequest, "invalid_public_url")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	token, err := security.RandomToken(24)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.CreateRegToken(tokenHash(token), n.Name, "reinstall "+id, id, 1800); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: id, Action: "agent_reinstall_token", Command: n.Name,
		SourceIP: s.Trust.RealIP(r),
	})
	cmd := fmt.Sprintf("curl -fsSL %s | sh -s -- --token %s --server %s",
		shellQuote(baseURL+"/install.sh"), shellQuote(token), shellQuote(baseURL))
	writeJSON(w, http.StatusOK, map[string]any{
		"install_command": cmd,
		"ttl":             1800,
		// The token is node-bound: running this command always keeps the node
		// (and reissues its credentials if the probe no longer has them).
		"keeps_node": strings.TrimSpace(n.Name) != "",
		"reissues":   true,
	})
}

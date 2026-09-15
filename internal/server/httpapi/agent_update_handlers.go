package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fobe-panel/fobe/internal/server/agentupdate"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
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
// A probe whose agent binary predates §5.5 will never follow on its own: the
// new fields are unknown to it (encoding/json drops them) and its Caps never
// reports self_update. The honest fix is one manual reinstall. Reusing the
// add-node flow keeps this free of a second installation path: a fresh
// single-use registration token plus the same install command, which rebinds
// to the same node because the machine-id and the node credentials survive
// (§4.2).
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
	if err := s.Store.CreateRegToken(tokenHash(token), n.Name, "reinstall "+id, 1800); err != nil {
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
		// What the credential-keeping rebind depends on, spelled out for the UI.
		"keeps_node": strings.TrimSpace(n.Name) != "",
	})
}

package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- registration tokens (design §4.2: single-use, TTL 30min) ---

type createRegTokenReq struct {
	// Name is required: it becomes the node's name once the probe registers.
	Name string `json:"name"`
	Note string `json:"note"`
}

func (s *Server) handleCreateRegToken(w http.ResponseWriter, r *http.Request) {
	var req createRegTokenReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name_required")
		return
	}

	// Resolve the public origin before consuming/generating anything. An
	// invalid configured URL must not leave behind an unusable registration token.
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
	hash := tokenHash(token)
	// "" = generic add-node token; bound tokens are minted per node by
	// handleAgentReinstallCommand (§4.2 revision).
	if err := s.Store.CreateRegToken(hash, name, strings.TrimSpace(req.Note), "", 1800); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "reg_token_created", Command: name, SourceIP: s.Trust.RealIP(r)})

	// install command shown once in the add-node dialog
	cmd := fmt.Sprintf("curl -fsSL %s | sh -s -- --token %s --server %s",
		shellQuote(baseURL+"/install.sh"), shellQuote(token), shellQuote(baseURL))
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "install_command": cmd, "ttl": 1800})
}

func (s *Server) handleListRegTokens(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("all") != "1"
	tokens, err := s.Store.ListRegTokens(activeOnly)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	type tokenView struct {
		ID        int64   `json:"id"`
		Name      string  `json:"name"`
		Note      string  `json:"note"`
		NodeID    string  `json:"node_id,omitempty"` // set = reissue/reinstall token
		CreatedAt int64   `json:"created_at"`
		ExpiresAt int64   `json:"expires_at"`
		UsedAt    *int64  `json:"used_at,omitempty"`
		UsedBy    *string `json:"used_by,omitempty"`
	}
	out := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		v := tokenView{ID: t.ID, Name: t.Name, Note: t.Note, NodeID: t.NodeID, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt}
		if t.UsedAt.Valid {
			v.UsedAt = &t.UsedAt.Int64
		}
		if t.UsedBy.Valid {
			v.UsedBy = &t.UsedBy.String
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// tokenHash: SHA-256 is fine for 192-bit random tokens (no need for a slow KDF).
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// --- agent registration (design §4.2) ---

type agentRegisterReq struct {
	Token     string `json:"token"`
	MachineID string `json:"machine_id"`
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Kernel    string `json:"kernel"`
	Version   string `json:"version"`
	TZ        string `json:"tz"`
	CPUCores  int    `json:"cpu_cores"`
	// os-release distro (§16 基本信息); empty from agents older than the field.
	DistroID      string `json:"distro_id"`
	DistroVersion string `json:"distro_version"`
	// present when reinstalling an already-configured probe (§4.2 reuse path)
	NodeID     string `json:"node_id,omitempty"`
	NodeSecret string `json:"node_secret,omitempty"`
}

type agentRegisterResp struct {
	NodeID     string `json:"node_id"`
	NodeSecret string `json:"node_secret"`
	Reused     bool   `json:"reused"`
}

// handleAgentRegister exchanges a single-use reg token for node credentials.
//
// machine_id dedupe has these outcomes (design §4.2):
//   - node-bound token whose node is this machine → reuse the node and issue a
//     fresh secret **without** asking for the old one. This is the credential
//     recovery path: a probe whose config.json was lost/overwritten can only be
//     saved here, because the server keeps nothing but the old secret's hash.
//   - node-bound token + unknown machine_id → same, and the node adopts the new
//     machine id (the whole state directory was wiped, or the node moved host).
//   - same machine + the node's own valid old credentials → reuse the node
//     (the ordinary reinstall case, kept for tokens issued before this revision).
//   - same machine without either → refuse as a suspected duplicate install.
func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req agentRegisterReq
	if err := decodeJSON(r, &req); err != nil || req.Token == "" || req.MachineID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}

	tokenName, note, boundNode, err := s.Store.ConsumeRegToken(tokenHash(req.Token))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "invalid_token")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	existing, err := s.Store.GetNodeByMachineID(req.MachineID)
	switch {
	case err == nil:
		// A node-bound token is scoped to exactly one node. Here the machine is
		// already registered, so the token is only usable when it was minted for
		// *that* node: it cannot be replayed onto another host's node.
		if boundNode != "" && boundNode != existing.ID {
			s.Store.InsertAudit(&store.AuditEntry{
				Actor: "agent", NodeID: existing.ID, Action: "register_machine_mismatch",
				Command: "token for " + boundNode, SourceIP: s.Trust.RealIP(r),
			})
			writeErr(w, http.StatusConflict, "machine_mismatch")
			return
		}
		if boundNode == existing.ID || (req.NodeID == existing.ID && req.NodeSecret != "") {
			// Bound token: no old secret needed. Legacy path: verify it.
			if boundNode == "" {
				oldHash, err := s.Store.GetNodeSecretHash(existing.ID)
				if err != nil || !security.VerifyPassword(req.NodeSecret, oldHash) {
					s.Store.InsertAudit(&store.AuditEntry{
						Actor: "agent", NodeID: existing.ID, Action: "register_duplicate",
						SourceIP: s.Trust.RealIP(r),
					})
					writeErr(w, http.StatusConflict, "duplicate_machine")
					return
				}
			}
			secret, hash, err := newNodeSecret()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			if _, err := s.Store.Exec(`UPDATE nodes SET node_secret_hash = ? WHERE id = ?`, hash, existing.ID); err != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			action := "node_rebound"
			if boundNode != "" {
				action = "node_reissued"
			}
			s.Store.InsertAudit(&store.AuditEntry{
				Actor: "agent", NodeID: existing.ID, Action: action,
				Command: note, SourceIP: s.Trust.RealIP(r),
			})
			writeJSON(w, http.StatusOK, agentRegisterResp{NodeID: existing.ID, NodeSecret: secret, Reused: true})
			return
		}
		s.Store.InsertAudit(&store.AuditEntry{
			Actor: "agent", NodeID: existing.ID, Action: "register_duplicate",
			SourceIP: s.Trust.RealIP(r),
		})
		writeErr(w, http.StatusConflict, "duplicate_machine")
		return
	case errors.Is(err, store.ErrNotFound):
		// A node-bound token authorises exactly one thing: re-establishing the
		// node it was minted for. An unknown machine_id means the probe lost its
		// whole state directory (machine-id included) or the node moved to new
		// hardware — both are precisely what a reissue token is for, and the
		// panel never exposes machine_id, so refusing here would leave no way
		// back. Adopt the new identity instead (audited).
		if boundNode != "" {
			bound, berr := s.Store.GetNode(boundNode)
			if errors.Is(berr, store.ErrNotFound) {
				s.Store.InsertAudit(&store.AuditEntry{
					Actor: "agent", NodeID: boundNode, Action: "register_node_missing",
					SourceIP: s.Trust.RealIP(r),
				})
				writeErr(w, http.StatusNotFound, "node_not_found")
				return
			}
			if berr != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			secret, hash, err := newNodeSecret()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			if _, err := s.Store.Exec(
				`UPDATE nodes SET node_secret_hash = ?, machine_id = ? WHERE id = ?`,
				hash, req.MachineID, bound.ID,
			); err != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			s.Store.InsertAudit(&store.AuditEntry{
				Actor: "agent", NodeID: bound.ID, Action: "node_reissued",
				Command: note + " machine=" + req.MachineID, SourceIP: s.Trust.RealIP(r),
			})
			writeJSON(w, http.StatusOK, agentRegisterResp{NodeID: bound.ID, NodeSecret: secret, Reused: true})
			return
		}
		// fresh node, continue below
	default:
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	id, err := randomID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	secret, hash, err := newNodeSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	n := &store.Node{
		// token 名称必填(面板强制),注册时成为节点名;老 token 无名称时
		// 回退 hostname / node-xxxxxx
		ID: id, Name: defaultNodeName(tokenName, req.Hostname, id), MachineID: req.MachineID,
		Note: note, AgentVersion: req.Version, OS: req.OS, Arch: req.Arch,
		Kernel: req.Kernel, DistroID: req.DistroID, DistroVersion: req.DistroVersion,
		Hostname: req.Hostname, CPUCores: req.CPUCores,
		TZ: tzOrDefault(req.TZ),
	}
	if err := s.Store.CreateNode(n, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "agent", NodeID: id, Action: "node_registered", Command: note, SourceIP: s.Trust.RealIP(r),
	})
	s.publishEvent("node_created", id)
	writeJSON(w, http.StatusOK, agentRegisterResp{NodeID: id, NodeSecret: secret})
}

func newNodeSecret() (plain, hash string, err error) {
	plain, err = security.RandomToken(32)
	if err != nil {
		return "", "", err
	}
	hash, err = security.HashPassword(plain)
	return plain, hash, err
}

// --- /ws/agent authentication ---

// handleAgentWS validates node credentials from headers, then hands the
// socket to the hub.
func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	nodeID := r.Header.Get("X-Fobe-Node-ID")
	secret := r.Header.Get("X-Fobe-Node-Secret")
	if nodeID == "" || secret == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	hash, err := s.Store.GetNodeSecretHash(nodeID)
	if err != nil || !security.VerifyPassword(secret, hash) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// §5.5 bypass handshake: a freshly downloaded binary proves it can talk to
	// the server *before* anything is committed. It must not take the normal
	// path — that would replace the live connection (kicking a healthy agent
	// offline) and write a version into the panel that is not serving.
	if r.Header.Get("X-Fobe-Selfcheck") != "" {
		s.Hub.HandleAgentSelfCheck(w, r, nodeID)
		return
	}
	s.Hub.HandleAgentWS(w, r, nodeID)
}

// --- latency targets (design §13) ---

type latencyTargetReq struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// normalizeLatencyTarget trims and validates a target definition in place,
// returning the error code to answer with ("" = valid). Create and update share
// it so the two cannot drift; icmp targets drop their port because the field is
// meaningless for them (import already normalized it, create used to store
// whatever the form sent — the list then showed a port that was never dialed).
func normalizeLatencyTarget(req *latencyTargetReq) string {
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" {
		return "bad_request"
	}
	if req.Kind != "icmp" && req.Kind != "tcp" {
		return "bad_kind"
	}
	if req.Kind == "tcp" {
		if req.Port <= 0 || req.Port > 65535 {
			return "bad_port"
		}
	} else {
		req.Port = 0
	}
	return ""
}

func (s *Server) handleListLatencyTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.Store.ListLatencyTargets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": targets})
}

func (s *Server) handleCreateLatencyTarget(w http.ResponseWriter, r *http.Request) {
	var req latencyTargetReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if code := normalizeLatencyTarget(&req); code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	id, err := s.Store.CreateLatencyTarget(req.Name, req.Kind, req.Host, req.Port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleUpdateLatencyTarget rewrites a global target (§13). Two consequences
// beyond the row itself:
//   - the definition (kind/host/port) is what a probe dials, so every node that
//     selects this target gets the new list right away — waiting for the next
//     hello_ack means a long-lived connection keeps probing the old endpoint
//     (same rule as the delete path, 实现修订 2026-09-17k);
//   - samples recorded against the old endpoint are dropped, else the detail
//     chart would draw two different hosts on one line (store does it in the
//     same transaction).
func (s *Server) handleUpdateLatencyTarget(w http.ResponseWriter, r *http.Request) {
	tid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	var req latencyTargetReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if code := normalizeLatencyTarget(&req); code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	affected, _ := s.Store.NodeIDsForLatencyTarget(tid)
	endpointChanged, err := s.Store.UpdateLatencyTarget(tid, req.Name, req.Kind, req.Host, req.Port)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, nodeID := range affected {
		s.Hub.PushLatencyTargets(nodeID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"endpoint_changed": endpointChanged,
		"samples_purged":   endpointChanged,
	})
}

func (s *Server) handleDeleteLatencyTarget(w http.ResponseWriter, r *http.Request) {
	tid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	// Capture the probing nodes before the FK cascade removes the link rows:
	// an online probe must be told to stop, or it keeps sampling a target the
	// panel no longer knows (§13 实现修订 2026-09-17k).
	affected, _ := s.Store.NodeIDsForLatencyTarget(tid)
	if err := s.Store.DeleteLatencyTarget(tid); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, nodeID := range affected {
		s.Hub.PushLatencyTargets(nodeID)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- small helpers ---

func randomID() (string, error) { return security.RandomToken(8) }

// defaultNodeName prefers the first non-empty candidate.
func defaultNodeName(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return "node"
}

func tzOrDefault(tz string) string {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "UTC"
	}
	return tz
}

// schemeHost derives the externally visible origin. nginx must forward Host;
// X-Forwarded-Proto decides http vs https (README: 接入层四件事).
func (s *Server) schemeHost(r *http.Request) (scheme, host string) {
	host = r.Host
	scheme = "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
		scheme = "https"
	}
	return scheme, host
}

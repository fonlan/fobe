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
	if err := s.Store.CreateRegToken(hash, name, strings.TrimSpace(req.Note), 1800); err != nil {
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
		CreatedAt int64   `json:"created_at"`
		ExpiresAt int64   `json:"expires_at"`
		UsedAt    *int64  `json:"used_at,omitempty"`
		UsedBy    *string `json:"used_by,omitempty"`
	}
	out := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		v := tokenView{ID: t.ID, Name: t.Name, Note: t.Note, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt}
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
// machine_id dedupe: same machine + valid old credentials → reuse the node;
// same machine without credentials → refuse as a suspected duplicate install.
func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req agentRegisterReq
	if err := decodeJSON(r, &req); err != nil || req.Token == "" || req.MachineID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}

	tokenName, note, err := s.Store.ConsumeRegToken(tokenHash(req.Token))
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
		// machine already registered: only the holder of the old secret may rebind
		if req.NodeID == existing.ID && req.NodeSecret != "" {
			oldHash, err := s.Store.GetNodeSecretHash(existing.ID)
			if err == nil && security.VerifyPassword(req.NodeSecret, oldHash) {
				secret, hash, err := newNodeSecret()
				if err != nil {
					writeErr(w, http.StatusInternalServerError, "internal")
					return
				}
				if _, err := s.Store.Exec(`UPDATE nodes SET node_secret_hash = ? WHERE id = ?`, hash, existing.ID); err != nil {
					writeErr(w, http.StatusInternalServerError, "internal")
					return
				}
				s.Store.InsertAudit(&store.AuditEntry{
					Actor: "agent", NodeID: existing.ID, Action: "node_rebound",
					Command: note, SourceIP: s.Trust.RealIP(r),
				})
				writeJSON(w, http.StatusOK, agentRegisterResp{NodeID: existing.ID, NodeSecret: secret, Reused: true})
				return
			}
		}
		s.Store.InsertAudit(&store.AuditEntry{
			Actor: "agent", NodeID: existing.ID, Action: "register_duplicate",
			SourceIP: s.Trust.RealIP(r),
		})
		writeErr(w, http.StatusConflict, "duplicate_machine")
		return
	case errors.Is(err, store.ErrNotFound):
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
	if err := decodeJSON(r, &req); err != nil || req.Host == "" {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Kind != "icmp" && req.Kind != "tcp" {
		writeErr(w, http.StatusBadRequest, "bad_kind")
		return
	}
	if req.Kind == "tcp" && (req.Port <= 0 || req.Port > 65535) {
		writeErr(w, http.StatusBadRequest, "bad_port")
		return
	}
	id, err := s.Store.CreateLatencyTarget(req.Name, req.Kind, req.Host, req.Port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleDeleteLatencyTarget(w http.ResponseWriter, r *http.Request) {
	tid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if err := s.Store.DeleteLatencyTarget(tid); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
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

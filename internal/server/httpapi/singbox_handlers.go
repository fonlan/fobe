package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/singbox"
	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// --- sing-box lifecycle management (design §9) ---

// singboxVersionRE guards the desired version: it is stored, embedded in the
// download path on the agent, and echoed back in UIs — never free-form text.
var singboxVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// SingboxVersion is one entry of the DL release manifest (§9.2: the panel
// shows selectable versions from the server-side release list, never latest).
type SingboxVersion struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256,omitempty"`
	// Size is the cached binary size in bytes.
	Size int64 `json:"size"`
	// DownloadedAt is when the version was cached (unix seconds).
	DownloadedAt int64 `json:"downloaded_at"`
	// Refs counts nodes whose desired_version is this version.
	Refs int `json:"refs"`
}

// singboxDL builds the release-cache client used by panel reads. The upstream
// bases are overridable (s.SingboxAPIBase / s.SingboxDownloadBase) so a mirror
// or an air-gapped test server can be pointed at; Refs annotates each version
// with the nodes that still point at it.
func (s *Server) singboxDL() *singboxdl.Client {
	refs := map[string]int{}
	if s.Store != nil {
		if m, err := s.Store.SingboxDesiredVersionRefs(); err == nil {
			refs = m
		}
	}
	return singboxdl.New(singboxdl.Config{
		DLDir:        s.DLDir,
		APIBase:      s.SingboxAPIBase,
		DownloadBase: s.SingboxDownloadBase,
		Log:          s.Log,
		Refs:         func(v string) int { return refs[v] },
	})
}

// singboxVersions lists <DLDir>/singbox/<version>/ entries, newest first by
// semver. An unset DLDir yields an empty list (the panel then cannot offer
// installs).
func (s *Server) singboxVersions() []SingboxVersion {
	out := []SingboxVersion{}
	if s.DLDir == "" {
		return out
	}
	cached, err := s.singboxDL().ScanCache()
	if err != nil {
		return out
	}
	for _, cv := range cached {
		out = append(out, SingboxVersion{
			Version:      cv.Version,
			SHA256:       cv.SHA256,
			Size:         cv.Size,
			DownloadedAt: cv.DownloadedAt,
			Refs:         cv.Refs,
		})
	}
	return out
}

func (s *Server) handleSingboxVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"versions": s.singboxVersions()})
}

func (s *Server) handleGetNodeSingbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	sb, err := s.Store.GetNodeSingbox(id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"singbox": nil})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// cert_pem omitted: potentially large and not needed by the panel (§9.3).
	writeJSON(w, http.StatusOK, map[string]any{"singbox": map[string]any{
		"node_id": sb.NodeID, "version": sb.Version, "desired_version": sb.DesiredVersion,
		"config_hash": sb.ConfigHash, "status": sb.Status, "last_error": sb.LastError,
		"rollback_version": sb.RollbackVersion, "cert_sha256": sb.CertSHA256,
		"cert_not_after": sb.CertNotAfter, "port": sb.Port, "updated_at": sb.UpdatedAt,
	}})
}

type singboxInstallReq struct {
	Version string `json:"version"`
	Port    int    `json:"port,omitempty"`
}

// handleSingboxInstall records the desired sing-box version, (re)generates
// the node config and pushes the desired frame (§9.1/§9.2). Offline nodes
// pick the state up from hello_ack on reconnect — declarative desired state
// means no command replay is needed.
func (s *Server) handleSingboxInstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req singboxInstallReq
	if err := decodeJSON(r, &req); err != nil || !singboxVersionRE.MatchString(req.Version) {
		writeErr(w, http.StatusBadRequest, "bad_version")
		return
	}
	// explicit version from the DL manifest only (§9.2: 不追 latest)
	if known := s.singboxVersions(); len(known) > 0 {
		found := false
		for _, v := range known {
			if v.Version == req.Version {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, http.StatusBadRequest, "version_unavailable")
			return
		}
	}

	port := req.Port
	sb, err := s.Store.GetNodeSingbox(id)
	if errors.Is(err, store.ErrNotFound) {
		sb = &store.NodeSingbox{NodeID: id, Status: "absent"}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if port == 0 {
		port = sb.Port
	}
	if !singbox.ValidPort(port) {
		p, err := singbox.RandomPort()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		port = p
	}
	if err := s.applySingboxDesired(id, sb, req.Version, port, "installing"); err != nil {
		singboxWriteErr(w, err)
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: id, Action: "singbox_install", Command: req.Version, SourceIP: s.Trust.RealIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "desired_version": req.Version, "port": port})
}

// handleSingboxAction dispatches start/stop/restart through the commands
// queue (§7: offline-queued, TTL-limited) using the existing command kinds.
func (s *Server) handleSingboxAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	kind := map[string]string{
		"start":   "start_singbox",
		"stop":    "stop_singbox",
		"restart": "restart_singbox",
	}[r.PathValue("action")]
	if kind == "" {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if sb, err := s.Store.GetNodeSingbox(id); err == nil && sb.DesiredVersion == "" {
		writeErr(w, http.StatusBadRequest, "singbox_not_installed")
		return
	}
	cmdID, err := s.enqueueCommand(commandEnqueue{
		NodeID: id, Kind: kind, Payload: "{}",
		Audit: store.AuditEntry{Actor: "panel", Risk: "normal", SourceIP: s.Trust.RealIP(r)},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": cmdID})
}

type singboxPortReq struct {
	Port int `json:"port"`
}

// handleSingboxPort changes the inbound port: regenerate config, update the
// desired state and push (§9.3: 默认随机高位端口,面板可改).
func (s *Server) handleSingboxPort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req singboxPortReq
	if err := decodeJSON(r, &req); err != nil || !singbox.ValidPort(req.Port) {
		writeErr(w, http.StatusBadRequest, "bad_port")
		return
	}
	sb, err := s.Store.GetNodeSingbox(id)
	if errors.Is(err, store.ErrNotFound) {
		sb = &store.NodeSingbox{NodeID: id, Status: "absent"}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.applySingboxDesired(id, sb, sb.DesiredVersion, req.Port, sb.Status); err != nil {
		singboxWriteErr(w, err)
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: id, Action: "singbox_port", Command: strconv.Itoa(req.Port), SourceIP: s.Trust.RealIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "port": req.Port})
}

// applySingboxDesired is the single write path for node sing-box desired
// state: build config from the global anytls password, persist it under
// `singbox_config:<node_id>` (the same key hub.buildDesiredState reads), keep
// agent-reported fields, and push the desired frame when the node is online.
func (s *Server) applySingboxDesired(nodeID string, sb *store.NodeSingbox, version string, port int, status string) error {
	password, ok := s.GetDecryptedSetting("anytls_password")
	if !ok || password == "" {
		return errAnytlsPasswordUnset
	}
	config, err := singbox.BuildNodeConfig(port, password)
	if err != nil {
		return err
	}
	if err := s.Store.SetSetting("singbox_config:"+nodeID, string(config), false); err != nil {
		return err
	}

	// preserve what the agent reported (version/cert/rollback) — only the
	// desired half and the config fingerprint change here
	sb.DesiredVersion = version
	sb.Port = port
	sb.ConfigHash = singbox.ConfigHash(config)
	if status != "" {
		sb.Status = status
	}
	if err := s.Store.UpsertNodeSingbox(sb); err != nil {
		return err
	}

	s.pushDesired(nodeID)
	s.publishEvent("node_updated", nodeID)
	return nil
}

// pushDesired sends the current desired state to an online agent. Offline
// nodes receive it inside hello_ack after reconnect (§7) — nothing to replay.
// It reports whether the frame reached a connected agent (false covers both
// "offline" and "the agent vanished between the online check and the send").
func (s *Server) pushDesired(nodeID string) bool {
	sb, err := s.Store.GetNodeSingbox(nodeID)
	if err != nil || sb.DesiredVersion == "" {
		return false
	}
	desired := protocol.DesiredState{Singbox: &protocol.SingboxDesired{
		Version: sb.DesiredVersion,
		Port:    sb.Port,
	}}
	if cfg, err := s.Store.GetSetting("singbox_config:" + nodeID); err == nil {
		desired.Singbox.ConfigJSON = cfg
	}
	return s.Hub.Send(nodeID, protocol.NewEnvelope(protocol.TypeDesired, "", desired))
}

var errAnytlsPasswordUnset = errors.New("anytls_password not configured")

func singboxWriteErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errAnytlsPasswordUnset) {
		writeErr(w, http.StatusBadRequest, "anytls_password_unset")
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal")
}

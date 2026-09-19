package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/singboxdl"
	"github.com/fonlan/fobe/internal/server/store"
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

// SingboxDL returns the process-wide release-cache client (built on first use).
// The upstream bases are overridable (s.SingboxAPIBase / s.SingboxDownloadBase)
// so a mirror or an air-gapped test server can be pointed at; Refs annotates
// each version with the nodes that still point at it.
//
// There is exactly one client per server process, and that is what keeps a
// download single-flight: singboxdl serializes Install on a per-client mutex,
// so the startup auto-download, a manual retry and an "update sing-box" job
// that needs the same version all wait on each other instead of fetching the
// same tarball again (design §9.5.3). Building a client per request would
// silently restore the duplicate download this exists to prevent.
func (s *Server) SingboxDL() *singboxdl.Client {
	s.sbDLOnce.Do(func() {
		s.sbDL = singboxdl.New(singboxdl.Config{
			DLDir:        s.DLDir,
			APIBase:      s.SingboxAPIBase,
			DownloadBase: s.SingboxDownloadBase,
			Log:          s.Log,
			// Refs is resolved per call rather than captured once: the client
			// outlives every node edit, so a snapshot would show stale
			// reference counts (ScanCache asks once per cached version).
			Refs:     func(v string) int { return s.singboxRefs()[v] },
			Progress: s.onSingboxProgress,
		})
	})
	return s.sbDL
}

// singboxRefs counts nodes per desired_version. A store failure degrades to an
// empty map (the badge shows 0) instead of failing the read.
func (s *Server) singboxRefs() map[string]int {
	if s.Store == nil {
		return nil
	}
	m, err := s.Store.SingboxDesiredVersionRefs()
	if err != nil {
		return nil
	}
	return m
}

// singboxVersions lists <DLDir>/singbox/<version>/ entries, newest first by
// semver. An unset DLDir yields an empty list (the panel then cannot offer
// installs).
func (s *Server) singboxVersions() []SingboxVersion {
	out := []SingboxVersion{}
	if s.DLDir == "" {
		return out
	}
	cached, err := s.SingboxDL().ScanCache()
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
		"desired_uninstall": sb.DesiredUninstall,
		"config_hash":       sb.ConfigHash, "status": sb.Status, "last_error": sb.LastError,
		"cert_sha256":    sb.CertSHA256,
		"cert_not_after": sb.CertNotAfter, "port": sb.Port, "updated_at": sb.UpdatedAt,
		// §9.3 实现修订 2026-09-17: what sing-box this probe already runs.
		// nil = the probe never reported (old agent, or nothing to report is
		// still reported — see the agent's detectLocal).
		"local": s.singboxDiscovery(id),
	}})
}

// handleSingboxUninstall removes sing-box from a probe (design §9.2 实现修订
// 2026-09-16).
//
// It writes *desired state* rather than queueing a one-shot command: the
// operator may click while the probe is offline, and a queued command expires
// after 10 minutes while hello_ack keeps re-delivering the declaration. It
// deliberately does not delete the node_singbox row — the agent's next report
// blanks the reported half (version, certificate, status → absent), which is
// what drops the node out of subscriptions and lets an operator reinstall with
// the same port.
func (s *Server) handleSingboxUninstall(w http.ResponseWriter, r *http.Request) {
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
		writeErr(w, http.StatusBadRequest, "singbox_not_installed")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Nothing on the probe and no removal pending: there is nothing to remove.
	// A second click while a removal is pending is allowed on purpose — it
	// re-pushes the declaration to a probe that may have missed it.
	if sb.Version == "" && sb.CertPEM == "" && !sb.DesiredUninstall {
		writeErr(w, http.StatusBadRequest, "singbox_not_installed")
		return
	}
	sb.DesiredVersion = ""
	sb.DesiredUninstall = true
	// The config is derived from the desired version: keep the bytes (a failed
	// removal followed by a reinstall reuses them) but drop the fingerprint so
	// a stale hash can never describe an uninstall.
	sb.ConfigHash = ""
	if err := s.Store.UpsertNodeSingbox(sb); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: id, Action: "singbox_uninstall", Command: sb.Version, SourceIP: s.Trust.RealIP(r),
	})
	s.Hub.PushDesired(id) // offline probes get it from hello_ack on reconnect
	s.publishEvent("node_updated", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

	sb, err := s.Store.GetNodeSingbox(id)
	if errors.Is(err, store.ErrNotFound) {
		sb = &store.NodeSingbox{NodeID: id, Status: "absent"}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// §9.2 实现修订 2026-09-18c: the port is the one the probe reports it is
	// already serving; only a node that has never reported a config draws a
	// random one. Resolving node_singbox.port first is what used to drag a
	// listener the operator had moved back onto the old port.
	port := s.inboundPortFor(id, sb, req.Port)
	if port == 0 {
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

// inboundPortFor decides which port the panel's own anytls inbound is (re)built
// on — the port the template writes, and the port the node's credential is read
// back from.
//
// The order answers "what is this probe serving right now" (§9.2 实现修订
// 2026-09-18c):
//
//  1. an explicitly requested port — the HTTP API may name one;
//  2. the single anytls listener the probe's own config.json declares. The file
//     is the truth (§9.3 实现修订 2026-09-17b): after a port move in the editor
//     the management column still names the port the operator moved *away*
//     from, and preferring it here is what dragged the listener back — and,
//     because the credential no longer sat at that old port either, minted a
//     fresh password over a working client;
//  3. node_singbox.port, for a probe that never reported a file (an old agent)
//     or whose file carries several anytls listeners (nothing to pick between);
//  4. 0 — nothing to build on, so the caller draws a random port. That is the
//     genuinely-fresh-install case.
//
// The report is read through the process Cryptor; a snapshot that exists but
// cannot be decrypted is the wrong-master-key incident, not "no report", and the
// caller's credential lookup fails closed over it (§10.1).
func (s *Server) inboundPortFor(nodeID string, sb *store.NodeSingbox, requested int) int {
	if singbox.ValidPort(requested) {
		return requested
	}
	if local, err := s.Store.GetNodeSingboxLocal(nodeID, s.Crypt); err == nil {
		if p := store.SoleAnytlsPortInDoc(local.ConfigJSON); singbox.ValidPort(p) {
			return p
		}
	}
	if sb != nil && singbox.ValidPort(sb.Port) {
		return sb.Port
	}
	return 0
}

// applySingboxDesired is the single write path for node sing-box desired
// state: build the config, persist it under `singbox_config:<node_id>` (the
// same key hub.buildDesiredState reads), keep agent-reported fields, and push
// the desired frame when the node is online.
//
// Creating the node's inbound is where its credential is decided (§10.1 实现
// 修订 2026-09-17e): an existing password (the panel's own document, the
// probe's report, a hand-set override) is reused — an install over a listener
// one-sing.sh already serves keeps that listener's password, so no client is
// cut off — and only a node with no inbound at all gets a fresh 16-char alnum
// one. Changing a port runs through here too, and must not rotate.
//
// §9.3 实现修订 2026-09-19: the config itself is merged onto the file the
// probe last reported (buildInstallConfig) instead of regenerated from the
// template — an install or a version update must not erase the inbounds the
// operator added.
func (s *Server) applySingboxDesired(nodeID string, sb *store.NodeSingbox, version string, port int, status string) (err error) {
	// A port another listener in the probe's own file serves is refused before
	// anything else: the merge would catch it too, but only after the
	// credential step below may have minted a password — an audit line for a
	// credential an install then refused to persist.
	if base, ok := s.liveConfig(nodeID); ok {
		if err := singbox.CheckPanelPortFree([]byte(base), port); err != nil {
			return err
		}
	}
	// The credential is looked up at the port the listener is being built on,
	// falling back to the port the node was managed on before: a port change
	// names a port that has no credential yet, and the one to keep sits in the
	// document under the old port — asking only for the new one would mint a
	// second password for the same listener (§10.1 实现修订 2026-09-17e).
	password, err := s.ensureNodeProxyPassword(nodeID, port, sb.Port)
	if err != nil {
		return err
	}
	config, inserted, operatorContent, err := s.buildInstallConfig(nodeID, port, password)
	if err != nil {
		return err
	}
	// A listener the panel just created gets its 添加中 row before the push,
	// the same way an editor add does: an online agent may apply and report
	// within this call, and the report's reconcile would then be overwritten
	// by a row written afterwards.
	if inserted {
		if err := s.Store.SetNodeSingboxInboundPending(nodeID, store.NodeSingboxInbound{
			Port: port, Type: singbox.ProtoAnytls, Tag: "anytls-in",
		}); err != nil {
			return err
		}
		// A write that never leaves the server must not leave the panel claiming
		// a listener was added (§9.3 实现修订 2026-09-18, the editor's same rule).
		defer func() {
			if err != nil {
				s.Store.DeleteNodeSingboxInbound(nodeID, port)
			}
		}()
	}
	if err := s.Store.SetSetting("singbox_config:"+nodeID, string(config), false); err != nil {
		return err
	}
	// The document carries the operator's own content: from here on the startup
	// template sync must treat this file as theirs. Written only once the
	// config is stored, so a failed install leaves no marker without a
	// document to protect.
	if operatorContent {
		s.markConfigEdited(nodeID)
	}

	// preserve what the agent reported (version/cert/rollback) — only the
	// desired half and the config fingerprint change here
	sb.DesiredVersion = version
	sb.Port = port
	sb.ConfigHash = singbox.ConfigHash(config)
	// Installing is the operator re-managing the node: it cancels a pending
	// removal (§9.2 实现修订 2026-09-16). The agent may still be converging to
	// the uninstall it was told a moment ago; the install declaration that
	// follows is authoritative, and the two cannot both be in flight — the
	// manager keeps one desired state.
	sb.DesiredUninstall = false
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

// buildInstallConfig produces the config an install/update pushes (§9.3 实现
// 修订 2026-09-19): the file the probe last reported is the base when there is
// one — everything it declares survives — and the template plus the legacy
// adopted inbounds otherwise (an agent too old to report a file, or one the
// panel cannot parse: install is the recovery path for a broken config too).
//
// inserted reports whether the panel's own inbound had to be appended, and
// operatorContent whether the result carries the operator's own bytes — the
// condition the `singbox_edited` marker (and with it the startup template
// sync) hangs on. An unparseable base and a port another inbound serves are
// the two refusals/fallbacks; both keep the probe's service out of the decision.
func (s *Server) buildInstallConfig(nodeID string, port int, password string) (config []byte, inserted, operatorContent bool, err error) {
	extras := s.loadExtraInbounds(nodeID)
	base, ok := s.liveConfig(nodeID)
	if !ok {
		config, err = singbox.BuildNodeConfigWithInbounds(port, password, extras)
		return config, false, false, err
	}
	merged, fileInserted, merr := singbox.MergePanelInboundIntoDoc([]byte(base), port, password)
	if errors.Is(merr, singbox.ErrInboundPortTaken) {
		return nil, false, false, merr
	}
	if merr != nil {
		s.Log.Warn("singbox install: unreadable local config, falling back to template", "node", nodeID, "err", merr)
		config, err = singbox.BuildNodeConfigWithInbounds(port, password, extras)
		return config, false, false, err
	}
	if !templateEquivalent(merged, port, password, extras) {
		return merged, fileInserted, true, nil
	}
	// The file says exactly what the template says: store the generator's own
	// bytes so the stored config stays byte-identical to what the startup sync
	// derives — a re-serialization of the same content would differ in key
	// order, churn the hash and teach sync nothing.
	config, err = singbox.BuildNodeConfigWithInbounds(port, password, extras)
	return config, fileInserted, false, err
}

// templateEquivalent reports whether the merged document says exactly what the
// generator would produce for the same port, credential and adopted inbounds.
// Both sides go through a decode/encode round-trip so key order and indentation
// cannot decide the answer — the probe's file is compared by content, not by
// formatting. False is the safe side: it marks the file as the operator's and
// keeps the startup template sync away from it.
func templateEquivalent(doc []byte, port int, password string, extras []singbox.ExtraInbound) bool {
	fallback, err := singbox.BuildNodeConfigWithInbounds(port, password, extras)
	if err != nil {
		return false
	}
	return canonicalJSON(doc) == canonicalJSON(fallback)
}

// canonicalJSON renders a JSON document as content: map keys sorted, array
// order kept, formatting gone. Unparseable input never compares equal to a
// parseable one.
func canonicalJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "\x00" + string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "\x00" + string(raw)
	}
	return string(out)
}

// SyncSingboxConfigs regenerates the stored config of every managed node whose
// bytes no longer match what the current template produces, and pushes it.
//
// A node's config is written once per change and then kept in
// `singbox_config:<node_id>`; nothing re-derives it. That is fine while the
// generator is stable, but when generation itself changes — as it did when the
// address-based DNS section had to go (§9.4 实现修订 2026-09-16) — every
// existing node keeps receiving the stale bytes for good: the agent compares
// hashes, finds disk == desired, and has no reason to rewrite the file, so the
// operator's only way out is "click install again". A template bug must not
// need that. Startup-only and idempotent: nodes already on the current template
// are skipped, so an ordinary restart rewrites nothing.
//
// Since §10.1 实现修订 2026-09-16 it is also one of the places the global anytls
// password can come into existence — but only once a node is actually managed,
// so a fresh install never mints a credential it does not use.
func (s *Server) SyncSingboxConfigs() int {
	targets, err := s.Store.ListSingboxTargets()
	if err != nil {
		s.Log.Warn("singbox config sync: list targets", "err", err)
		return 0
	}
	if len(targets) == 0 {
		return 0 // nothing is managed — do not mint a credential nobody asked for
	}
	updated := 0
	for _, t := range targets {
		// §9.3 实现修订 2026-09-17b: a node whose config the panel has edited
		// (or which was taken over from one-sing.sh) owns its own file. Rebuilding
		// it from the current template would erase every inbound the operator
		// added — the exact behaviour the editor model removes. The template fix
		// this pass exists for still reaches every node the panel installed from
		// scratch and never edited.
		if s.configEdited(t.NodeID) {
			continue
		}
		sb, err := s.Store.GetNodeSingbox(t.NodeID)
		if err != nil {
			continue
		}
		// §10.1 实现修订 2026-09-17e: the credential comes from the node's own
		// config, and a node whose config carries none is *skipped* rather than
		// given a fresh password — this pass exists to pick up a template change,
		// and minting here would rewrite a working config with a credential no
		// client has.
		// §9.2 实现修订 2026-09-18c: the same port resolution an install uses, so
		// a template fix never drags a listener back onto the port the panel
		// remembers instead of the one the probe reports it serves.
		port := s.inboundPortFor(t.NodeID, sb, 0)
		if !singbox.ValidPort(port) {
			s.Log.Warn("singbox config sync: no usable port", "node", t.NodeID, "port", sb.Port)
			continue
		}
		password := s.nodeProxyPassword(t.NodeID, port, sb.Port)
		if password == "" {
			s.Log.Warn("singbox config sync: no credential to re-derive, skipped", "node", t.NodeID)
			continue
		}
		config, err := singbox.BuildNodeConfigWithInbounds(port, password, s.loadExtraInbounds(t.NodeID))
		if err != nil {
			// A port the template refuses is not this pass's business; the next
			// operator change fixes it.
			s.Log.Warn("singbox config sync: build", "node", t.NodeID, "port", port, "err", err)
			continue
		}
		if stored, err := s.Store.GetSetting("singbox_config:" + t.NodeID); err == nil && stored == string(config) {
			continue
		}
		if err := s.Store.SetSetting("singbox_config:"+t.NodeID, string(config), false); err != nil {
			s.Log.Warn("singbox config sync: persist", "node", t.NodeID, "err", err)
			continue
		}
		old := sb.ConfigHash
		sb.ConfigHash = singbox.ConfigHash(config)
		// The document now serves `port`, so the management column follows it:
		// leaving the old value behind would make every later reader (the
		// firewall hint, an install on a probe that has not reported yet) name a
		// port this document no longer declares.
		sb.Port = port
		if err := s.Store.UpsertNodeSingbox(sb); err != nil {
			s.Log.Warn("singbox config sync: upsert", "node", t.NodeID, "err", err)
			continue
		}
		s.Store.InsertAudit(&store.AuditEntry{
			Actor: "system", NodeID: t.NodeID, Action: "singbox_config_sync",
			Command: shortHash(old) + " -> " + shortHash(sb.ConfigHash),
		})
		s.pushDesired(t.NodeID)
		updated++
	}
	if updated > 0 {
		s.Log.Info("singbox config sync: regenerated stale node configs", "nodes", updated)
	}
	return updated
}

// shortHash keeps audit lines readable; the full fingerprint lives in
// node_singbox.config_hash and in the setting itself.
func shortHash(h string) string {
	if h == "" {
		return "-"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// pushDesired sends the current desired state to an online agent. Offline
// nodes receive it inside hello_ack after reconnect (§7) — nothing to replay.
// It reports whether the frame reached a connected agent (false covers both
// "offline" and "the agent vanished between the online check and the send").
func (s *Server) pushDesired(nodeID string) bool {
	sb, err := s.Store.GetNodeSingbox(nodeID)
	if err != nil {
		return false
	}
	cfg, cfgErr := s.Store.GetSetting("singbox_config:" + nodeID)
	// §9.3 实现修订 2026-09-17b: a config edit is a desired state of its own.
	// A node adopted from one-sing.sh has no version the panel ever declared
	// (fobe did not install its binary), yet the edit still has to reach the
	// probe — requiring a desired version here is what silently dropped it.
	if sb.DesiredVersion == "" && (cfgErr != nil || cfg == "") {
		return false
	}
	desired := protocol.DesiredState{Singbox: &protocol.SingboxDesired{
		Version: sb.DesiredVersion,
		Port:    sb.Port,
	}}
	if cfgErr == nil {
		desired.Singbox.ConfigJSON = cfg
	}
	return s.Hub.Send(nodeID, protocol.NewEnvelope(protocol.TypeDesired, "", desired))
}

// singboxWriteErr maps a desired-state write failure onto an API error code.
// A port the probe's own file already serves with another listener is an
// operator-visible refusal — the old full rewrite silently dropped that
// listener instead (§9.3 实现修订 2026-09-19). Every remaining cause is a
// storage or generator failure, so there is exactly one answer: internal. The
// old `anytls_password_unset` 400 died with operator-supplied passwords
// (§10.1 实现修订 2026-09-16).
func singboxWriteErr(w http.ResponseWriter, err error) {
	if errors.Is(err, singbox.ErrInboundPortTaken) {
		writeErr(w, http.StatusBadRequest, "duplicate_port")
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal")
}

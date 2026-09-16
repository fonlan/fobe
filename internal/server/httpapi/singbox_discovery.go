// Local sing-box discovery and adoption (design §9.3 实现修订 2026-09-17).
//
// The feature exists because a probe can arrive at fobe with sing-box already
// installed and serving — one-sing.sh's layout is fobe's layout on purpose, so
// the binary, the unit and config.json are all in the places fobe already
// looks at. What was missing is the *reading* half: fobe is declarative, so
// the first convergence after an operator installs a node rewrites config.json
// whole, and every inbound the script had added disappears.
//
// Two halves:
//
//   - discovery: the agent reports its config.json (§7 State.SingboxLocal), the
//     server parses it, and the panel shows what is there without fobe
//     touching anything.
//   - adoption: the operator clicks, and the inbounds become part of the
//     desired config — same ports, same credentials, same certificate. From
//     then on fobe owns the file, which is what "managing" means.
package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// nodeDiscovery is the discovery half of GET /api/nodes/{id}/singbox.
//
// `inbounds` deliberately carries no credential material (see
// singbox.LocalInboundSummary): the panel needs to show what is on the probe
// and whether adopting it keeps working credentials, never the secrets
// themselves — those stay encrypted in the database and only ever leave it
// inside a generated config pushed to the probe.
type nodeDiscovery struct {
	// Present / Running / Unit describe the probe's own sing-box.
	Present bool   `json:"present"`
	Running bool   `json:"running"`
	Unit    bool   `json:"unit_active"`
	UnitOK  bool   `json:"unit_known"`
	Version string `json:"version,omitempty"`
	// ConfigPath is where the agent looked (the effective layout — an
	// unprivileged probe reports its relocated directory).
	ConfigPath string `json:"config_path,omitempty"`
	// Hash fingerprints the snapshot the panel is looking at: it is what lets
	// the UI tell "this is the file I adopted" from "the operator edited it
	// again since".
	Hash string `json:"hash,omitempty"`
	// Error is the agent's or the parser's complaint about the local file.
	Error string `json:"error,omitempty"`
	// Inbounds is the inventory, adoptable ones and not (a `mixed` inbound is
	// shown with adoptable=false rather than hidden).
	Inbounds []singbox.LocalInboundSummary `json:"inbounds"`
	// Adopted is how many inbounds of this node's desired config came from
	// adoption. Zero on a node fobe installed from scratch.
	Adopted int `json:"adopted"`
}

// singboxDiscovery assembles the discovery view for a node. It never fails the
// request: a store error degrades to a nil discovery, because the sing-box
// card's primary job (install/start/stop) must not be blocked by an optional
// informational section.
func (s *Server) singboxDiscovery(nodeID string) *nodeDiscovery {
	local, err := s.Store.GetNodeSingboxLocal(nodeID, s.Crypt)
	if err != nil {
		// ErrNotFound is the ordinary case: this probe never reported one.
		return nil
	}
	out := &nodeDiscovery{
		Present:    local.LocalPresent,
		Running:    local.LocalRunning,
		Unit:       local.LocalUnit,
		UnitOK:     local.LocalUnitOK,
		Version:    local.LocalVersion,
		ConfigPath: local.ConfigPath,
		Hash:       local.LocalHash,
		Error:      local.Error,
		Inbounds:   []singbox.LocalInboundSummary{},
	}
	inbounds, perr := singbox.ParseLocalInbounds(local.ConfigJSON)
	if perr != nil {
		// Keep the raw error visible: "fobe found a config.json it cannot
		// parse" is exactly what the operator needs to be told, and the file
		// stays adopted-able by hand (the snapshot is stored either way).
		if out.Error == "" {
			out.Error = perr.Error()
		} else {
			out.Error += "; " + perr.Error()
		}
		return out
	}
	out.Inbounds = singbox.Summaries(inbounds)
	// The layout path lives in the report, not in the parse result: show it
	// even when the parse failed.
	if extras, err := s.Store.GetNodeSingboxExtraInbounds(nodeID, s.Crypt); err == nil {
		out.Adopted = len(extras)
	}
	return out
}

// adoptRequest is the operator's selection. Both fields are required per
// entry: the (type, port) pair is the identity of an inbound in the file, and
// matching on it (rather than on the tag) means a hand-edited tag cannot make
// the panel adopt a different inbound than the one that was clicked.
type adoptRequest struct {
	Inbounds []adoptPick `json:"inbounds"`
}

type adoptPick struct {
	Type string `json:"type"`
	Port int    `json:"port"`
}

// handleSingboxAdopt turns inbounds the probe already runs into part of fobe's
// desired state (§9.3 实现修订 2026-09-17).
//
// It is a desired-state write, not a command: the merged config is persisted
// under `singbox_config:<node_id>` and pushed like any other change, so an
// offline probe converges from hello_ack. The credentials come from the stored
// snapshot, which is why the snapshot is kept even after adoption — a node
// whose agent has not reported since the adopt click still gets the right
// bytes.
func (s *Server) handleSingboxAdopt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req adoptRequest
	if err := decodeJSON(r, &req); err != nil || len(req.Inbounds) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}

	sb, err := s.Store.GetNodeSingbox(id)
	if errors.Is(err, store.ErrNotFound) {
		sb = &store.NodeSingbox{NodeID: id, Status: "absent"}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if sb.DesiredUninstall {
		writeErr(w, http.StatusBadRequest, "uninstall_pending")
		return
	}
	local, err := s.Store.GetNodeSingboxLocal(id, s.Crypt)
	if err != nil || strings.TrimSpace(local.ConfigJSON) == "" {
		// Nothing to adopt from: either the probe never reported, or it has no
		// config file. Both are "ask the probe first", not an internal failure.
		writeErr(w, http.StatusBadRequest, "no_local_config")
		return
	}
	// A node the panel has not installed yet has no inbound port, and the config
	// the panel owns *is* an anytls inbound on that port. Rather than making the
	// operator install a second anytls service just to get a port, adoption
	// takes over the one that is already there: the operator's anytls inbound
	// becomes the panel's, which is exactly the "same inbounds, no rewrite"
	// promise. With no anytls in the file there is nothing to promote, so a
	// random high port is assigned and the adopted inbounds ride alongside it.
	parsed, perr := singbox.ParseLocalInbounds(local.ConfigJSON)
	if perr != nil {
		writeErr(w, http.StatusBadRequest, "bad_local_config")
		return
	}
	if !singbox.ValidPort(sb.Port) {
		for _, ib := range parsed {
			if ib.Type == singbox.ProtoAnytls && singbox.ValidPort(ib.Port) {
				sb.Port = ib.Port
				break
			}
		}
	}
	if !singbox.ValidPort(sb.Port) {
		port, err := singbox.RandomPort()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		sb.Port = port
	}
	if perr != nil {
		writeErr(w, http.StatusBadRequest, "bad_local_config")
		return
	}

	want := map[string]bool{}
	for _, p := range req.Inbounds {
		want[adoptKey(p.Type, p.Port)] = true
	}
	// The panel's inbound is regenerated from the template on every push, so it
	// keeps the *global* password. An adopted anytls inbound on that same port
	// would be a second, password-different listener on one port — impossible
	// to serve. Its presence in the desired config is the panel's own inbound;
	// what the panel must carry over instead is the port (done above), so the
	// client's URI still points at a live anytls service.
	skipAnytlsPort := sb.Port

	// Start from what is already adopted so a second adopt adds to the set
	// instead of replacing it — the panel offers the inbounds it knows about,
	// and clicking one must not silently drop another.
	extras, err := s.Store.GetNodeSingboxExtraInbounds(id, s.Crypt)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	byPort := map[int]int{} // port -> index in extras
	for i, e := range extras {
		byPort[e.ListenPort] = i
	}
	var added []singbox.LocalInbound
	for _, ib := range parsed {
		if !want[adoptKey(ib.Type, ib.Port)] {
			continue
		}
		if ib.Port <= 0 {
			continue
		}
		if ib.Port == skipAnytlsPort && ib.Type == singbox.ProtoAnytls {
			// Taken over as the panel's own inbound (see above) rather than
			// imported twice: the port is in the desired config, and every push
			// regenerates that listener with the global password.
			continue
		}
		extra := singbox.ExtraInboundFrom(ib)
		if i, ok := byPort[ib.Port]; ok {
			extras[i] = extra
		} else {
			byPort[ib.Port] = len(extras)
			extras = append(extras, extra)
		}
		added = append(added, ib)
	}
	if len(added) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing_to_adopt")
		return
	}

	// The version to declare: whatever the server already has cached, preferring
	// the build the probe is literally running (that is the no-re-download
	// path). No cached version at all means the operator has to fetch one first
	// (§9.2: versions are explicit), and adoption stops here rather than
	// declaring a version the agent cannot download.
	version := sb.DesiredVersion
	if version == "" {
		version, err = s.adoptVersion(local.LocalVersion)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "no_cached_version")
			return
		}
	}

	password, err := s.ensureAnytlsPassword()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The panel's own anytls inbound keeps the *global* credential — it is the
	// inbound the panel owns, and re-installing the node later regenerates it
	// the same way. The operator's anytls inbound, if it is on its own port,
	// travels as an adopted inbound with its own password (see the extras
	// branch of the subscription renderer): that is what makes adoption not a
	// forced rotation for the clients one-sing.sh already handed a URI to.
	config, err := singbox.BuildNodeConfigWithInbounds(sb.Port, password, extras)
	if err != nil {
		s.Log.Error("singbox adopt: build config", "node", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.SetSetting("singbox_config:"+id, string(config), false); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.SetNodeSingboxExtraInbounds(id, extras, s.Crypt); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	sb.DesiredVersion = version
	sb.ConfigHash = singbox.ConfigHash(config)
	sb.DesiredUninstall = false
	if sb.Status == "" {
		sb.Status = "installing"
	}
	if err := s.Store.UpsertNodeSingbox(sb); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	// Audit the shape, never the credentials: the action is the fact worth
	// keeping ("this node took over 2 local inbounds"), and §4.4's rule is that
	// sensitive values never reach the audit log.
	ports := make([]string, 0, len(added))
	for _, ib := range added {
		ports = append(ports, strconv.Itoa(ib.Port))
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: id, Action: "singbox_adopt",
		Command: strings.Join(ports, ","), SourceIP: s.Trust.RealIP(r),
	})
	s.pushDesired(id)
	s.publishEvent("node_updated", id)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "adopted": len(extras), "desired_version": version,
	})
}

// adoptVersion picks the version to declare for a freshly adopted node: the
// probe's own build when the server has it cached, else the newest cached one.
func (s *Server) adoptVersion(localVersion string) (string, error) {
	versions := s.singboxVersions()
	if len(versions) == 0 {
		return "", errors.New("no cached sing-box version")
	}
	norm := normalizeVersionString(localVersion)
	if norm != "" {
		for _, v := range versions {
			if normalizeVersionString(v.Version) == norm {
				return v.Version, nil
			}
		}
	}
	return versions[0].Version, nil
}

// normalizeVersionString strips the decorations a version may carry on either
// side of the wire ("v1.13.0-beta.7" vs "1.13.0-beta.7"), so comparing the
// probe's report with the server's cache is not a string-equality accident.
func normalizeVersionString(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// adoptKey is the identity of an adoptable inbound: its type and its port.
func adoptKey(typ string, port int) string {
	return typ + ":" + strconv.Itoa(port)
}

// loadExtraInbounds is the read used by every path that regenerates a node's
// config. A failure degrades to "no adopted inbounds" — a config that rebuilds
// without them is a bug the panel shows (the adopted list is still stored), but
// refusing to generate a config at all would take the node down.
func (s *Server) loadExtraInbounds(nodeID string) []singbox.ExtraInbound {
	extras, err := s.Store.GetNodeSingboxExtraInbounds(nodeID, s.Crypt)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Log.Warn("singbox: read adopted inbounds", "node", nodeID, "err", err)
		}
		return nil
	}
	return extras
}

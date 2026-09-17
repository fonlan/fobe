// Local sing-box discovery (§9.3 实现修订 2026-09-17, revised 2026-09-17d).
//
// The feature exists because a probe can arrive at fobe with sing-box already
// installed and serving — one-sing.sh's layout is fobe's layout on purpose, so
// the binary, the unit and config.json are all in the places fobe already
// looks at. What was missing is the *reading* half: fobe is declarative, so the
// first convergence after an operator installs a node rewrites config.json
// whole, and every inbound the script had added disappears.
//
// What the panel shows is therefore the probe's own file: the agent reports its
// config.json (§7 State.SingboxLocal), this file parses it into a summary the
// API may return, and the subscription renderer (§9.3 实现修订 2026-09-17b)
// serves clients straight out of it. Nothing here is "adopted": there is no
// takeover step left to take, no desired state to write and no credential to
// rotate. Each listener in the report is an equal subscription entry.
//
// `extra_inbounds` still exists for nodes adopted before the editor model
// existed: their stored config has to keep carrying those inbounds when an
// install or a port change regenerates it.
package httpapi

import (
	"errors"

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
	// Inbounds is the inventory, supported protocols and the rest (a `mixed`
	// inbound is shown with supported=false rather than hidden).
	Inbounds []singbox.LocalInboundSummary `json:"inbounds"`
	// Adopted is how many inbounds of this node's stored config came from a
	// takeover performed before the editor model (§9.3 实现修订 2026-09-17d).
	// Always zero on a node that was installed or edited by this panel.
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

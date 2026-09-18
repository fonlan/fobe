// The config.json editor (design §9.3 实现修订 2026-09-17b).
//
// The earlier revision let fobe *adopt* the inbounds a probe already ran and
// then treated its own generated config as the source of truth. That made the
// panel and one-sing.sh fight over one file: the script's `jq` append was
// erased by the next convergence, and an operator who wanted both had to stop
// using the script.
//
// This file implements the other model, the one an operator actually wants:
// **the file on the probe is the truth**. fobe reads it (the agent reports it
// on every change), the panel shows exactly what is there, and editing is a
// read-merge-write back onto that same file plus a `systemctl restart`. A
// hand-added inbound is left alone because fobe never rewrites what it did not
// touch; a panel-added inbound survives the script's next `jq` run because the
// script only appends.
//
// What that costs, plainly:
//
//   - the subscription reflects the *last reported* file. The window is one
//     agent cadence (the agent pushes immediately when it sees a change, so it
//     is usually seconds, and the panel has a "refresh from probe" button).
//   - two writers on one file is last-write-wins. If the script edits while a
//     panel edit is in flight, the script's write is the one that survives.
//     The panel says so instead of pretending otherwise.
//   - nothing is "adopted": management is a per-inbound decision, and an
//     inbound fobe does not model is still shown, still listed, just not
//     editable.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- live config as the source of truth ---

// SettingConfigEdited marks a node whose config.json the panel has edited (or
// whose node was installed by the panel). It is the "fobe wrote here" flag,
// and it exists so startup reconciliation knows whose config it owns:
// rewriting an operator's file because the *template* changed is the exact
// behaviour this revision removes.
func SettingConfigEdited(nodeID string) string { return "singbox_edited:" + nodeID }

// configEdited reports whether fobe has ever written this node's config.
func (s *Server) configEdited(nodeID string) bool {
	v, err := s.Store.GetSetting(SettingConfigEdited(nodeID))
	return err == nil && v == "1"
}

func (s *Server) markConfigEdited(nodeID string) {
	if err := s.Store.SetSetting(SettingConfigEdited(nodeID), "1", false); err != nil {
		s.Log.Warn("mark config edited", "node", nodeID, "err", err)
	}
}

// liveConfig reads the config.json the probe last reported.
func (s *Server) liveConfig(nodeID string) (string, bool) {
	local, err := s.Store.GetNodeSingboxLocal(nodeID, s.Crypt)
	if err != nil || strings.TrimSpace(local.ConfigJSON) == "" {
		return "", false
	}
	return local.ConfigJSON, true
}

// liveInbounds is the parsed inbound inventory of that file, in file order.
func (s *Server) liveInbounds(nodeID string) ([]singbox.LocalInbound, bool) {
	raw, ok := s.liveConfig(nodeID)
	if !ok {
		return nil, false
	}
	inbounds, err := singbox.ParseLocalInbounds(raw)
	if err != nil {
		return nil, false
	}
	return inbounds, true
}

// liveProxyNodes turns the reported file into subscription entries, plus
// whether it produced anything at all.
//
// It is the single place "what is on this probe" becomes "what a client may
// dial", so the renderer and the node-picker predicate cannot drift apart
// (they did once: `subRenderable` answered from node_singbox while the renderer
// answered from the config).
// liveProxyNodes is the checked variant used by the editor endpoints: it also
// reports whether the node has a reported file at all.
func (s *Server) liveProxyNodes(nodeID, name, server string) ([]singbox.ProxyNode, bool, bool) {
	nodes, ok := s.liveNodesFor(nodeID, name, server)
	return nodes, len(nodes) > 0, ok
}

// liveNodesFor renders a node's reported config.json into subscription
// entries.
//
// The credentials come from the file, not from the panel's global anytls
// password (§9.3 实现修订 2026-09-17d): the file is what the probe serves, so a
// listener one-sing.sh created has to keep handing out the password its clients
// already hold. That also means this read path never mints a credential — the
// global password is only demanded by the paths that *write* a listener.
func (s *Server) liveNodesFor(nodeID, name, server string) ([]singbox.ProxyNode, bool) {
	local, err := s.Store.GetNodeSingboxLocal(nodeID, s.Crypt)
	if err != nil || strings.TrimSpace(local.ConfigJSON) == "" {
		return nil, false
	}
	// Certificates are keyed by listener. The file the agent reported is the
	// authority, so every anytls inbound must carry its own reported PEM; using
	// node_singbox.cert_pem as a hidden primary-entry fallback would make one
	// listener special again.
	certs := map[int]string{}
	for p, pem := range local.AnytlsCerts {
		certs[p] = pem
	}
	nodes := singbox.LiveProxyNodes(local.ConfigJSON, nodeID, name, server, certs)
	return nodes, true
}

// hasLiveConfig reports whether the reported file yields at least one
// subscription entry. The address is irrelevant here — this answers "is there
// anything to render at all", which is what the node list and the editor badge
// need; the renderer itself is called with the node's real address.
func (s *Server) hasLiveConfig(nodeID string) bool {
	nodes, ok := s.liveNodesFor(nodeID, "", "probe")
	return ok && len(nodes) > 0
}

// relayableInbound mirrors the renderer's relay leg (§10.2): the relayed
// outbound terminates on the target's anytls inbound, so only an anytls entry
// with a reported certificate can carry it (an empty protocol means anytls —
// the inbound fobe generated itself).
func relayableInbound(live []singbox.ProxyNode) *singbox.ProxyNode {
	for i := range live {
		if (live[i].Protocol == "" || live[i].Protocol == singbox.ProtoAnytls) && live[i].CertPEM != "" {
			return &live[i]
		}
	}
	return nil
}

// liveAnytlsPorts lists the anytls inbound ports of the reported config — the
// ports a §10.2 relay leg can land on. Listeners are equal peers (§9.3 实现
// 修订 2026-09-17d), so there is no hidden primary port to fall back to; the
// managed port only answers for a node whose file has not been reported yet.
func (s *Server) liveAnytlsPorts(nodeID string, sb *store.NodeSingbox) []int {
	ports := map[int]bool{}
	if inbounds, ok := s.liveInbounds(nodeID); ok {
		for _, ib := range inbounds {
			if ib.Type == singbox.ProtoAnytls && ib.Port > 0 {
				ports[ib.Port] = true
			}
		}
	}
	if len(ports) == 0 && sb != nil && sb.Port > 0 {
		ports[sb.Port] = true
	}
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// --- editing ---

// inboundView is one editable inbound as the panel sees it. `credential` is
// filled only on the editor endpoints (the operator has to be able to see and
// change the password of an inbound that has no UI-managed counterpart); the
// read-only node card keeps using the credential-free summary.
type inboundView struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Port       int    `json:"port"`
	Label      string `json:"label"`
	Editable   bool   `json:"editable"`
	Credential string `json:"credential,omitempty"`
	HasCred    bool   `json:"has_cred"`
	ServerName string `json:"server_name,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Flow       string `json:"flow,omitempty"`
	Username   string `json:"username,omitempty"`
	Method     string `json:"method,omitempty"`
	// Status is pending until the agent reports that this exact listener accepts
	// a local TCP connection, then running. A listener the panel removed reads
	// deleting until a report no longer lists the port. A desired config is not
	// proof that sing-box bound every inbound it contains.
	Status string `json:"status"`
	// Number is the position in the file's inbound array. Every edit carries
	// the number it was rendered from, so an edit can be rejected instead of
	// applied to a different inbound than the operator clicked.
	Number int `json:"number"`
}

func inboundViewOf(ib singbox.LocalInbound, n int) inboundView {
	v := inboundView{
		Type: ib.Type, Tag: ib.Tag, Port: ib.Port, Label: ib.Label, Number: n,
		Editable: singbox.SupportedProtocol(ib.Type) && ib.Port > 0,
	}
	if u := singbox.InboundUser(ib.Inbound); u != nil {
		v.UUID, _ = u["uuid"].(string)
		v.Flow, _ = u["flow"].(string)
		v.Username, _ = u["name"].(string)
		if pw, _ := u["password"].(string); pw != "" {
			v.Credential = pw
		}
	}
	if pw, _ := ib.Inbound["password"].(string); pw != "" {
		v.Credential = pw
	}
	if m, _ := ib.Inbound["method"].(string); m != "" {
		v.Method = m
	}
	if tls := singbox.InboundTLS(ib.Inbound); tls != nil {
		v.ServerName, _ = tls["server_name"].(string)
	}
	v.HasCred = v.Credential != ""
	return v
}

// handleGetNodeSingboxConfig returns the editable inbound list of the reported
// config.json (GET /api/nodes/{id}/singbox/config).
func (s *Server) handleGetNodeSingboxConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	inbounds, ok := s.liveInbounds(id)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"inbounds": []inboundView{}, "reported": false})
		return
	}
	states, err := s.Store.ListNodeSingboxInbounds(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	byPort := map[int]store.NodeSingboxInbound{}
	for _, state := range states {
		byPort[state.Port] = state
	}
	views := make([]inboundView, 0, len(inbounds)+len(states))
	livePorts := map[int]bool{}
	for i, ib := range inbounds {
		view := inboundViewOf(ib, i)
		// pending/running/deleting all live in the lifecycle row; the default
		// covers a listener the panel has no record of (a script added it
		// between reports).
		view.Status = "pending"
		if state, ok := byPort[ib.Port]; ok {
			view.Status = state.Status
		}
		views = append(views, view)
		livePorts[ib.Port] = true
	}
	// The endpoint normally mirrors the last report. Keep a just-added listener
	// visible before that report arrives, otherwise the user sees no row during
	// the interval its pending state matters. A deleting row is the mirror
	// image — its port is still in the reported file by definition — so it
	// never renders from here.
	for _, state := range states {
		if livePorts[state.Port] || state.Status == "deleting" {
			continue
		}
		views = append(views, inboundView{
			Type: state.Type, Tag: state.Tag, Port: state.Port,
			Label:  fmt.Sprintf("%s:%d", state.Type, state.Port),
			Status: state.Status, Number: -1,
		})
	}
	local, _ := s.Store.GetNodeSingboxLocal(id, s.Crypt)
	writeJSON(w, http.StatusOK, map[string]any{
		"inbounds":   views,
		"reported":   true,
		"renderable": s.hasLiveConfig(id),
		// The hash of the file these rows were rendered from: a write carries it
		// back, and a mismatch means the probe's file changed since. Refusing the
		// write beats merging onto a stale base (which would silently drop
		// whatever one-sing.sh added in the meantime).
		"hash":        local.LocalHash,
		"config_path": local.ConfigPath,
		"edited":      s.configEdited(id),
	})
}

// configEditRequest is one edit against the report the panel rendered from.
//
// `ReportedHash` is the whole concurrency story: the operator edits what he
// saw, and if the probe's file changed since (one-sing.sh added something, or
// another tab edited), the write is refused with `config_changed` instead of
// silently building on a stale base. Merge-then-push, never blind replace.
type configEditRequest struct {
	ReportedHash string        `json:"reported_hash"`
	Add          []inboundEdit `json:"add,omitempty"`
	Update       []inboundEdit `json:"update,omitempty"`
	Delete       []int         `json:"delete,omitempty"`
}

// deletedInbound is one listener the panel removed, with enough identity from
// the file it was rendered from to keep its lifecycle row presentable while it
// reads 删除中.
type deletedInbound struct {
	port int
	typ  string
	tag  string
}

type inboundEdit struct {
	Number int    `json:"number"`
	Type   string `json:"type"`
	Tag    string `json:"tag"`
	Port   int    `json:"port"`
	// Credential is the protocol's secret: anytls/socks password, vless uuid.
	// Empty means "keep what the file has" — a panel that echoes back a masked
	// field must not blank the credential.
	Credential string `json:"credential,omitempty"`
	Username   string `json:"username,omitempty"`
	Method     string `json:"method,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	Flow       string `json:"flow,omitempty"`
	// New marks an inbound the panel creates: there is nothing in the file to
	// inherit, so a missing credential is an error rather than "keep".
	New bool `json:"new,omitempty"`
}

// handlePutNodeSingboxConfig applies the edits and pushes the merged file
// (PUT /api/nodes/{id}/singbox/config).
func (s *Server) handlePutNodeSingboxConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req configEditRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	local, err := s.Store.GetNodeSingboxLocal(id, s.Crypt)
	if err != nil || strings.TrimSpace(local.ConfigJSON) == "" {
		writeErr(w, http.StatusBadRequest, "no_local_config")
		return
	}
	if req.ReportedHash != "" && req.ReportedHash != local.LocalHash {
		writeErr(w, http.StatusConflict, "config_changed")
		return
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(local.ConfigJSON), &doc); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_local_config")
		return
	}
	inbounds, _ := doc["inbounds"].([]any)

	// Delete first (highest index first so the remaining numbers stay valid),
	// then update by number, then append. Order matters: the numbers the panel
	// sent describe the file it rendered, not the file being built.
	drop := map[int]bool{}
	deleted := []deletedInbound{}
	for _, n := range req.Delete {
		if n < 0 || n >= len(inbounds) {
			writeErr(w, http.StatusBadRequest, "bad_index")
			return
		}
		drop[n] = true
		if inbound, ok := inbounds[n].(map[string]any); ok {
			tag, _ := inbound["tag"].(string)
			deleted = append(deleted, deletedInbound{
				port: intFieldOf(inbound["listen_port"]),
				typ:  stringFieldOf(inbound["type"]),
				tag:  tag,
			})
		}
	}
	if len(drop) > 0 {
		kept := make([]any, 0, len(inbounds)-len(drop))
		for i, ib := range inbounds {
			if !drop[i] {
				kept = append(kept, ib)
			}
		}
		inbounds = kept
	}
	// A port change on an existing listener is a move, and the two ports it
	// touches are the panel's business until the probe has applied the document
	// (§9.3 实现修订 2026-09-18): the new one does not exist on the probe yet
	// (an add), and the old one is about to stop being served (a delete). Both
	// halves are collected here, from the file as this request rendered it.
	var moves []portMove
	var kept []int
	for _, up := range req.Update {
		if up.Number < 0 || up.Number >= len(inbounds) {
			writeErr(w, http.StatusBadRequest, "bad_index")
			return
		}
		prev, _ := inbounds[up.Number].(map[string]any)
		oldPort := intFieldOf(prev["listen_port"])
		next, err := applyInboundEdit(inbounds[up.Number], up)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errCodeOf(err))
			return
		}
		inbounds[up.Number] = next
		switch {
		case oldPort <= 0:
			// The file declared no port on this row (a listener fobe does not
			// model): there is no listener to move or to keep.
		case up.Port == oldPort:
			kept = append(kept, oldPort)
		default:
			moves = append(moves, portMove{
				from: oldPort, to: up.Port,
				fromType: stringFieldOf(prev["type"]), fromTag: stringFieldOf(prev["tag"]),
				toType: stringFieldOf(next["type"]), toTag: stringFieldOf(next["tag"]),
			})
		}
	}
	for _, add := range req.Add {
		next, err := applyInboundEdit(nil, add)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errCodeOf(err))
			return
		}
		inbounds = append(inbounds, next)
	}
	if len(inbounds) == 0 && len(req.Delete) > 0 {
		writeErr(w, http.StatusBadRequest, "no_inbounds")
		return
	}
	if err := checkPortsUnique(inbounds); err != nil {
		writeErr(w, http.StatusBadRequest, "duplicate_port")
		return
	}
	doc["inbounds"] = inbounds
	merged, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	// The panel's own record of what this edit does, written before the desired
	// state goes out: an online agent can apply and report immediately, and a
	// mark written afterwards could overwrite a genuine acknowledgement. Each
	// entry carries its own undo, because a write that fails halfway — or a push
	// that never leaves the server — must not leave the panel claiming a
	// listener was added, moved or retired (§9.3 实现修订 2026-09-17f/2026-09-18).
	//
	// `rows` is that record as it stands *before* the edit, which is what the
	// undo restores.
	rows, err := s.Store.ListNodeSingboxInbounds(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	rowsByPort := map[int]store.NodeSingboxInbound{}
	for _, row := range rows {
		rowsByPort[row.Port] = row
	}
	var intents []configIntent
	for _, add := range req.Add {
		port := add.Port
		intents = append(intents, configIntent{
			apply: func() error {
				return s.Store.SetNodeSingboxInboundPending(id, store.NodeSingboxInbound{
					Port: port, Type: add.Type, Tag: add.Tag,
				})
			},
			undo: func() error { return s.Store.DeleteNodeSingboxInbound(id, port) },
		})
	}
	// The listener's lifecycle row is not dropped on a delete: it flips to
	// `deleting` and stays until an agent report no longer lists the port.
	// Deleting it at edit time made the row — still present in the last reported
	// file — come back from the GET as plain `pending`, i.e. a running listener
	// showed 添加中 for the whole apply window (§9.3 实现修订 2026-09-17).
	for _, del := range deleted {
		if del.port <= 0 {
			continue
		}
		before, hadRow := rowsByPort[del.port]
		intents = append(intents, configIntent{
			apply: func() error {
				return s.Store.SetNodeSingboxInboundDeleting(id, store.NodeSingboxInbound{
					Port: del.port, Type: del.typ, Tag: del.tag,
				})
			},
			undo: func() error {
				if !hadRow {
					return s.Store.DeleteNodeSingboxInbound(id, del.port)
				}
				return s.Store.RestoreNodeSingboxInbound(id, before)
			},
		})
	}
	// A port move is recorded as both halves at once: the target row (pending,
	// the probe has never seen that port) and the source row (deleting, the
	// probe still serves it). Without them the GET keeps rendering the port the
	// operator moved away from — the file on the probe has not changed yet, and
	// the agent's own report only arrives after its apply window — so the saved
	// port is not on the page and nothing reads pending for the UI to poll.
	for _, move := range moves {
		before, hadRow := rowsByPort[move.from]
		intents = append(intents, configIntent{
			apply: func() error {
				return s.Store.SetNodeSingboxInboundPending(id, store.NodeSingboxInbound{
					Port: move.to, Type: move.toType, Tag: move.toTag,
				})
			},
			undo: func() error { return s.Store.DeleteNodeSingboxInbound(id, move.to) },
		})
		intents = append(intents, configIntent{
			apply: func() error {
				return s.Store.SetNodeSingboxInboundDeleting(id, store.NodeSingboxInbound{
					Port: move.from, Type: move.fromType, Tag: move.fromTag,
				})
			},
			undo: func() error {
				// The undo of the retirement is the state the report implies:
				// this port is the one still on the probe.
				if hadRow {
					return s.Store.RestoreNodeSingboxInbound(id, before)
				}
				return s.Store.DeleteNodeSingboxInbound(id, move.from)
			},
		})
	}
	// An edit that keeps a port an earlier edit retired cancels that removal:
	// the document the panel is about to push serves it again. The row must go
	// back to what the last report says, not stay 删除中 — the probe will not
	// report anything new (its file never changed), so nothing else would ever
	// clear it.
	for _, port := range kept {
		before, ok := rowsByPort[port]
		if !ok || before.Status != "deleting" {
			continue
		}
		restored := before
		restored.Status = reportedInboundStatus(local, port)
		intents = append(intents, configIntent{
			apply: func() error { return s.Store.RestoreNodeSingboxInbound(id, restored) },
			undo:  func() error { return s.Store.SetNodeSingboxInboundDeleting(id, before) },
		})
	}
	applied := make([]configIntent, 0, len(intents))
	for _, in := range intents {
		if err := in.apply(); err != nil {
			undoIntents(applied)
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		applied = append(applied, in)
	}
	if err := s.pushConfigDocument(id, string(merged)); err != nil {
		undoIntents(applied)
		singboxWriteErr(w, err)
		return
	}
	// The document is the panel's own declaration, so it is authoritative over
	// the rows the panel itself invented: a 添加中 row for a port this document
	// no longer declares is a stale intent (a move edited again, an add taken
	// back) and would otherwise stay parked forever. Rows mirrored from the
	// probe's reports are untouched.
	if err := s.Store.PruneNodeSingboxInboundOptimism(id, docPorts(inbounds)); err != nil {
		s.Log.Warn("prune singbox inbound intents", "node", id, "err", err)
	}
	s.markConfigEdited(id)
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: id, Action: "singbox_config_edit",
		Command:  fmt.Sprintf("add:%d update:%d delete:%d", len(req.Add), len(req.Update), len(req.Delete)),
		SourceIP: s.Trust.RealIP(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inbounds": len(inbounds)})
}

// pushConfigDocument is the write path of the editor model: it stores the
// merged document as the *pushed* config and sends it as desired state with
// the node's existing version (or the node's installed version when none was
// ever declared).
//
// The desired frame carries ConfigJSON, which in this model means "write these
// bytes and restart" — the agent applies it only when the bytes differ, so a
// push that merged to the same content is a no-op.
func (s *Server) pushConfigDocument(nodeID, doc string) error {
	sb, err := s.Store.GetNodeSingbox(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		sb = &store.NodeSingbox{NodeID: nodeID, Status: "absent"}
	} else if err != nil {
		return err
	}
	if err := s.Store.SetSetting("singbox_config:"+nodeID, doc, false); err != nil {
		return err
	}
	sb.ConfigHash = singbox.ConfigHash([]byte(doc))
	if sb.Status == "" || sb.Status == "absent" {
		sb.Status = "installing"
	}
	if err := s.Store.UpsertNodeSingbox(sb); err != nil {
		return err
	}
	s.pushDesired(nodeID)
	s.publishEvent("node_updated", nodeID)
	return nil
}

// portMove is a listener whose port an edit changed: the probe keeps serving
// `from` until it applies the document, which serves `to` instead.
type portMove struct {
	from, to int
	fromType string
	fromTag  string
	toType   string
	toTag    string
}

// reportedInboundStatus is the status a listener's lifecycle row has when the
// last report is the only authority: running once the probe confirmed the port
// takes a local TCP connection, pending otherwise — including when the probe's
// agent predates the check (an unproven listener is not a running one).
func reportedInboundStatus(local store.NodeSingboxLocal, port int) string {
	if !local.InboundChecksKnown {
		return "pending"
	}
	for _, p := range local.EffectiveInboundPorts {
		if p == port {
			return "running"
		}
	}
	return "pending"
}

// docPorts lists the ports a document declares, for the optimism prune.
func docPorts(inbounds []any) []int {
	ports := make([]int, 0, len(inbounds))
	for _, raw := range inbounds {
		m, _ := raw.(map[string]any)
		if port := intFieldOf(m["listen_port"]); port > 0 {
			ports = append(ports, port)
		}
	}
	return ports
}

// configIntent is one piece of the panel's own record of an edit, with the way
// back: an edit that fails halfway, or a push that never leaves the server,
// must not leave the panel claiming a listener was added, moved or retired.
type configIntent struct {
	apply func() error
	undo  func() error
}

// undoIntents takes back the panel's intent marks in reverse order. Failures are
// swallowed on purpose: the rows belong to the probe's reports, and the next
// one owns them again.
func undoIntents(applied []configIntent) {
	for i := len(applied) - 1; i >= 0; i-- {
		_ = applied[i].undo()
	}
}

// errCode is a config-edit failure carrying the API error code.
type errCode string

func (e errCode) Error() string   { return string(e) }
func errCodeOf(err error) string  { return err.Error() }
func badConfig(code string) error { return errCode(code) }

// applyInboundEdit turns one edit into the inbound object that goes into the
// file. `existing` is nil for an append.
//
// The merge rule is the whole point of the editor model: fields the operator
// did not touch keep whatever the file had, including options fobe does not
// model (sniff, multiplex, padding_scheme…), because losing a hand-written
// option is indistinguishable from breaking the operator's service.
func applyInboundEdit(existing any, edit inboundEdit) (map[string]any, error) {
	if !singbox.SupportedProtocol(edit.Type) {
		return nil, badConfig("unsupported_type")
	}
	if edit.Port <= 0 || edit.Port > 65535 {
		return nil, badConfig("bad_port")
	}
	base, _ := existing.(map[string]any)
	if base == nil {
		base = map[string]any{}
	}
	m := map[string]any{}
	for k, v := range base {
		m[k] = v
	}
	m["type"] = edit.Type
	m["listen_port"] = edit.Port
	if _, ok := m["listen"]; !ok {
		m["listen"] = "::"
	}
	if edit.Tag != "" {
		m["tag"] = edit.Tag
	} else if _, ok := m["tag"]; !ok {
		m["tag"] = fmt.Sprintf("%s-in-%d", edit.Type, edit.Port)
	}
	if edit.Method != "" {
		m["method"] = edit.Method
	}

	// The first user of the file, if any: edits merge onto it so a credential
	// the operator did not touch is never blanked.
	user := map[string]any{}
	if u, ok := m["users"].([]any); ok && len(u) > 0 {
		if prev, ok := u[0].(map[string]any); ok {
			for k, v := range prev {
				user[k] = v
			}
		}
	}
	// credential resolves one field: the operator's value, else the file's, else
	// (only for a new inbound) a freshly minted one.
	credential := func(fromEdit string, existing any) (string, error) {
		if cred := pickCredential(fromEdit, existing); cred != "" {
			return cred, nil
		}
		if !edit.New {
			return "", nil
		}
		return mintCredential(edit.Type)
	}

	switch edit.Type {
	case singbox.ProtoVLESS:
		cred, err := credential(edit.Credential, user["uuid"])
		if err != nil {
			return nil, err
		}
		if cred != "" {
			user["uuid"] = cred
		}
		if _, ok := user["uuid"]; !ok {
			return nil, badConfig("credential_required")
		}
		if edit.Flow != "" {
			user["flow"] = edit.Flow
		}
		m["users"] = []any{user}
		delete(m, "password")
	case singbox.ProtoSocks, "socks5":
		m["type"] = singbox.ProtoSocks
		if edit.Username != "" {
			user["name"] = edit.Username
		}
		cred, err := credential(edit.Credential, user["password"])
		if err != nil {
			return nil, err
		}
		if cred != "" {
			user["password"] = cred
		}
		if len(user) > 0 {
			m["users"] = []any{user}
		}
		delete(m, "password")
	case singbox.ProtoShadowsocks:
		if _, ok := m["method"]; !ok {
			return nil, badConfig("method_required")
		}
		cred, err := credential(edit.Credential, m["password"])
		if err != nil {
			return nil, err
		}
		if cred != "" {
			m["password"] = cred
		}
		if _, ok := m["password"]; !ok {
			return nil, badConfig("credential_required")
		}
	case singbox.ProtoAnytls:
		cred, err := credential(edit.Credential, user["password"])
		if err != nil {
			return nil, err
		}
		if cred != "" {
			user["password"] = cred
		}
		if _, ok := user["password"]; !ok {
			return nil, badConfig("credential_required")
		}
		m["users"] = []any{user}
		delete(m, "password")
		// The certificate the inbound serves: keep the file's, else the layout
		// default (the agent generates the pair if it is missing).
		tls, _ := m["tls"].(map[string]any)
		if tls == nil {
			tls = map[string]any{}
		}
		if _, ok := tls["certificate_path"]; !ok {
			tls["certificate_path"] = singbox.CertDir + "/" + singbox.CertFile
			tls["key_path"] = singbox.CertDir + "/" + singbox.KeyFile
		}
		tls["enabled"] = true
		m["tls"] = tls
	}
	if edit.ServerName != "" {
		if tls, ok := m["tls"].(map[string]any); ok {
			tls["server_name"] = edit.ServerName
		}
	}
	return m, nil
}

// mintCredential invents the credential of an inbound the panel is *creating*:
// a 16-char alnum password for the password protocols, a v4 UUID for VLESS
// (§10.1 实现修订 2026-09-17e). The server owns the inbound anyway — asking the
// operator to type a secret is how credentials get reused, and a UUID is not a
// value anybody should have to make up.
//
// It is deliberately only reachable for `New`: on an existing inbound a blank
// field means "keep what the file has", and minting there would rotate a
// working credential.
func mintCredential(typ string) (string, error) {
	if typ == singbox.ProtoVLESS {
		return singbox.GenerateUUID()
	}
	return singbox.GenerateAnytlsPassword()
}

// pickCredential is "the operator's value, else the file's, else nothing".
// Never erases: a panel that echoes back a masked field must not blank a
// credential.
func pickCredential(fromEdit string, existing any) string {
	if fromEdit != "" {
		return fromEdit
	}
	if s, ok := existing.(string); ok {
		return s
	}
	return ""
}

// checkPortsUnique refuses two inbounds on one port: sing-box would fail to
// bind the second, the service would flap under Restart=always, and the panel
// would report a version while nothing served the port.
func checkPortsUnique(inbounds []any) error {
	seen := map[int]bool{}
	for _, raw := range inbounds {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		port := intFieldOf(m["listen_port"])
		if port == 0 {
			continue
		}
		if seen[port] {
			return fmt.Errorf("port %d appears twice", port)
		}
		seen[port] = true
	}
	return nil
}

func intFieldOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func stringFieldOf(v any) string {
	s, _ := v.(string)
	return s
}

// handleNodeSingboxRefresh asks the probe to re-read its config now
// (POST /api/nodes/{id}/singbox/refresh).
//
// The agent scans on its own cadence and pushes when it sees a change; a queued
// command is the "I edited the file and I am looking at the page" path, and it
// answers with a fresh state frame within a round trip.
func (s *Server) handleNodeSingboxRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	cmdID, err := s.enqueueCommand(commandEnqueue{
		NodeID: id, Kind: protocol.CmdKindSingboxScan, Payload: "{}",
		Audit: store.AuditEntry{Actor: "panel", Risk: "readonly", SourceIP: s.Trust.RealIP(r)},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "command_id": cmdID})
}

// sortInboundsByPort keeps the generated document stable across edits, which
// matters because the file is compared by hash on the probe.
func sortInboundsByPort(inbounds []any) {
	sort.SliceStable(inbounds, func(i, j int) bool {
		a, _ := inbounds[i].(map[string]any)
		b, _ := inbounds[j].(map[string]any)
		return intFieldOf(a["listen_port"]) < intFieldOf(b["listen_port"])
	})
}

// nodeRenderable answers "would this node appear in a subscription" for the
// node list and picker: the reported file when there is one, the managed pair
// otherwise. It replaced a direct call to singbox's predicate so the list and
// the renderer cannot disagree (a node shown as renderable but producing
// nothing is the bug that predicate was introduced to prevent, §10).
func (s *Server) nodeRenderable(node *store.Node, sb *store.NodeSingbox) bool {
	if node == nil || node.PrimaryIP == "" {
		return false
	}
	if s.hasLiveConfig(node.ID) {
		return true
	}
	return subRenderable(node, sb)
}

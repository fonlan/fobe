// §10.2 subscription entries: relay detection, candidate assembly and the
// auto-enrolment reconciler. Kept out of sub_handlers.go because this is the
// half that reasons about nftables topology; the handlers only marshal it.
package httpapi

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// Settings keys of §10.2 (settings_handlers.go owns their validation).
const (
	// SettingRelayNameFormat is the auto-name template for relayed entries.
	SettingRelayNameFormat = "sub.relay_name_format"
	// SettingRelayAutoInclude turns auto-enrolment off, leaving detected relays
	// as unchecked candidates the operator has to pick by hand.
	SettingRelayAutoInclude = "sub.relay_auto_include"
)

// DefaultRelayNameFormat is the built-in relay auto-name. It carries the relay
// name *and* the port on purpose: several rules may point at the same target,
// and several relays may point at the same target, and neither of those may
// silently produce two identically named nodes.
const DefaultRelayNameFormat = "{name} · {relay}:{port}"

// relayNamePlaceholders are the only substitutions a format may use. Kept next
// to the renderer so a new placeholder is one edit, not a hunt.
var relayNamePlaceholders = []string{"{name}", "{relay}", "{host}", "{port}", "{proto}", "{iface}"}

// entryKey mirrors store's unexported identity rendering. The panel needs the
// same key to correlate candidates with bound rows.
func entryKey(e store.SubscriptionEntry) string {
	return fmt.Sprintf("%s|%s|%s|%d|%s", e.NodeID, e.RelayNodeID, e.Proto, e.SrcPort, e.Iface)
}

// entryBaseName is the naming fallback chain for a node: subscription name →
// panel name → node id (design §10.2).
func entryBaseName(n *store.Node) string {
	if n.SubName != "" {
		return n.SubName
	}
	if n.Name != "" {
		return n.Name
	}
	return n.ID
}

// directEntryName names one inbound of a node (§9.3/§10.2 实现修订 2026-09-18).
//
// The port is appended only when the node serves more than one inbound, i.e.
// only when it carries information: 2026-09-17j removed the unconditional
// `协议:端口` suffix because it made `nodes.sub_name` look like it did not
// apply, and a single-inbound node — most of them — must keep rendering the
// plain name. With two inbounds the alternative is worse than a suffix: both
// outbounds would carry the same name and only the renderer's `-2`/`#2` fallback
// would tell them apart, which says nothing about which inbound is which.
//
// `port` <= 0 is the legacy node-level entry ("every inbound of this node"),
// which keeps the plain name exactly as it did before the split.
func directEntryName(base string, ports []int, port int) string {
	if port <= 0 || len(ports) < 2 {
		return base
	}
	return base + ":" + strconv.Itoa(port)
}

// relayAutoName expands the configured template. strings.NewReplacer does a
// single pass, so a node literally named "{port}" cannot re-trigger expansion.
func relayAutoName(format string, target, relay *store.Node, e store.SubscriptionEntry) string {
	if strings.TrimSpace(format) == "" {
		format = DefaultRelayNameFormat
	}
	name := strings.NewReplacer(
		"{name}", entryBaseName(target),
		"{relay}", entryBaseName(relay),
		"{host}", relay.PrimaryIP,
		"{port}", strconv.Itoa(e.SrcPort),
		"{proto}", e.Proto,
		"{iface}", e.Iface,
	).Replace(format)
	return strings.TrimSpace(name)
}

// validRelayNameFormat accepts the operator's template: it must produce a
// distinguishable name (hence the mandatory {name}) and may only use known
// placeholders — a typo like {relayname} would otherwise ship to every client
// verbatim.
func validRelayNameFormat(format string) bool {
	if format == "" {
		return true // reset to the built-in default
	}
	if len([]rune(format)) > 128 || strings.ContainsFunc(format, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return false
	}
	if !strings.Contains(format, "{name}") {
		return false
	}
	probe := format
	for _, ph := range relayNamePlaceholders {
		probe = strings.ReplaceAll(probe, ph, "")
	}
	// Any brace left over is an unknown placeholder (or a stray one).
	return !strings.ContainsAny(probe, "{}")
}

// relayNameFormat reads the configured template, falling back to the default.
func (s *Server) relayNameFormat() string {
	if v, err := s.Store.GetSetting(SettingRelayNameFormat); err == nil && validRelayNameFormat(v) && v != "" {
		return v
	}
	return DefaultRelayNameFormat
}

// relayAutoInclude reports whether detected relays are enrolled automatically.
// Unset means on: the whole point of the topology is that B becomes reachable
// through A, so the entry appearing is the expected outcome, not a surprise.
func (s *Server) relayAutoInclude() bool {
	v, err := s.Store.GetSetting(SettingRelayAutoInclude)
	if err != nil || strings.TrimSpace(v) == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// relayCandidate is one detected "target via relay" ingress: the relay node's
// rule that lands on the target's anytls inbound.
type relayCandidate struct {
	RelayNodeID string
	Proto       string
	SrcPort     int
	Iface       string
	Comment     string
}

func (c relayCandidate) entry(nodeID string) store.SubscriptionEntry {
	return store.SubscriptionEntry{
		NodeID: nodeID, RelayNodeID: c.RelayNodeID,
		Proto: c.Proto, SrcPort: c.SrcPort, Iface: c.Iface,
	}
}

// relayCandidates derives the §10.2 relay entries of one target node from the
// fleet's forwards snapshot: a tcp rule on some *other* node whose destination
// is one of this node's IPv4 addresses and one of its anytls inbound ports.
// The landable ports come from the reported file — the editor model keeps the
// listeners equal, so there is no single primary port to match against — and
// fall back to the managed port for a node whose file has not arrived yet.
// The ruleset is the source of truth (§21.1), so this is recomputed on every
// read instead of being stored as a second opinion.
func (s *Server) relayCandidates(target *store.Node, sb *store.NodeSingbox) []relayCandidate {
	ports := s.liveAnytlsPorts(target.ID, sb)
	if len(ports) == 0 {
		return nil
	}
	ips, err := s.Store.ListNodeIPs(target.ID)
	if err != nil {
		return nil
	}
	v4 := map[string]bool{}
	for _, ip := range ips {
		if ip.Family == 4 {
			v4[ip.IP] = true
		}
	}
	if len(v4) == 0 {
		return nil
	}
	out := []relayCandidate{}
	// Two rules can share one tuple (§21: the handle is what tells them apart —
	// an extra `ip saddr` match, a rule added twice). The *entry* they produce is
	// the same either way, so the candidate list dedupes by identity: two
	// identical candidates reached the picker as two rows keyed alike, editing
	// one row's name silently wrote the other's too, and the save then failed
	// with alias_conflict for two names the operator believed were different.
	index := map[string]int{}
	for _, port := range ports {
		forwards, err := s.Store.ListForwardsToDstPort(port)
		if err != nil {
			continue
		}
		for _, f := range forwards {
			// A rule on the target itself is a self-loop (or a port collision), not
			// a relay: it would produce an entry identical to the direct one.
			if f.NodeID == target.ID || !v4[f.Forward.DstIP] {
				continue
			}
			c := relayCandidate{
				RelayNodeID: f.NodeID, Proto: f.Forward.Proto, SrcPort: f.Forward.SrcPort,
				Iface: f.Forward.Iface, Comment: f.Forward.Comment,
			}
			key := entryKey(c.entry(target.ID))
			if i, ok := index[key]; ok {
				// The comment is the only part that can differ, and it is the one
				// thing the 来源 column has to say: keep the first non-empty one.
				if out[i].Comment == "" {
					out[i].Comment = c.Comment
				}
				continue
			}
			index[key] = len(out)
			out = append(out, c)
		}
	}
	return out
}

// directEntryShadowed reports whether the node's own inbound is unreachable
// because one of its own DNAT rules steals the port in prerouting (§21.9).
// Reported, never enforced: the port is the probe's fact, not the panel's call.
// `port` is the entry's dial port; 0 (the legacy node-level row) asks about
// every ingress the node has.
func (s *Server) directEntryShadowed(node *store.Node, sb *store.NodeSingbox, port int) bool {
	ports := []int{port}
	if port <= 0 {
		ports = s.liveAnytlsPorts(node.ID, sb)
	}
	if len(ports) == 0 {
		return false
	}
	rules, err := s.Store.ListNodeForwards(node.ID)
	if err != nil {
		return false
	}
	stolen := map[int]bool{}
	for _, r := range rules {
		if r.Proto == "tcp" {
			stolen[r.SrcPort] = true
		}
	}
	for _, p := range ports {
		if stolen[p] {
			return true
		}
	}
	return false
}

// subEntryView is the picker's row: what the entry is, whether it is bound, how
// it will be named, and — when it cannot render right now — why not.
type subEntryView struct {
	NodeID      string `json:"node_id"`
	NodeName    string `json:"node_name"`
	RelayNodeID string `json:"relay_node_id,omitempty"`
	RelayName   string `json:"relay_name,omitempty"`
	Proto       string `json:"proto,omitempty"`
	// SrcPort is the port the client dials: the node's own inbound port for a
	// direct entry (§9.3/§10.2 实现修订 2026-09-18), the relay's forward source
	// port for a relayed one. 0 = the legacy node-level direct row.
	SrcPort int    `json:"src_port,omitempty"`
	Iface   string `json:"iface,omitempty"`
	// Protocols lists the wire protocols this entry renders as ("anytls",
	// "vless", …), read from the probe's reported file — the running config is
	// the authority (§9.3), never the panel's managed state. A relayed entry is
	// always anytls (a relay leg can only terminate on an anytls inbound).
	// Empty means unknown right now: nothing reported yet, or the entry's
	// inbound is gone from the file.
	Protocols []string `json:"protocols,omitempty"`
	// AutoName is what the renderer will use when Alias is empty.
	AutoName string `json:"auto_name"`
	Alias    string `json:"alias"`
	Selected bool   `json:"selected"`
	// Available=false means the entry is listed but would render nothing.
	Available bool `json:"available"`
	// Reason is the error code behind Available=false; Warning is a non-fatal
	// code (currently only `shadowed`).
	Reason     string `json:"reason,omitempty"`
	Warning    string `json:"warning,omitempty"`
	Discovered bool   `json:"discovered"` // derived from a live forward rule
	Source     string `json:"source,omitempty"`
}

// targetReadiness is what per-entry availability needs to know about the
// landing node, computed once per node instead of once per row. The reported
// file decides first (the editor model), the managed port/certificate pair
// answers only for a node whose file has not been reported yet. `relayable`
// is the stricter half: a relay leg terminates on the target's anytls
// inbound, so a node that renders only non-anytls inbounds can host direct
// entries but no relay leg.
type targetReadiness struct {
	live       []singbox.ProxyNode
	renderable bool
	relayable  bool
}

func (s *Server) targetReadinessOf(target *store.Node, sb *store.NodeSingbox) targetReadiness {
	live, _ := s.liveNodesFor(target.ID, "", "probe")
	legacyPair := sb != nil && sb.Port > 0 && sb.CertPEM != ""
	return targetReadiness{
		live: live,
		renderable: target.PrimaryIP != "" &&
			(len(live) > 0 || legacyPair || (sb != nil && sb.ExtrasPresent)),
		relayable: relayableInbound(live) != nil || legacyPair,
	}
}

// subscriptionEntryViews assembles the entries the panel may offer: the §10
// "only nodes that render" rule, applied per entry. A direct entry is offered
// for renderable nodes; a relayed entry only when its *landing* node renders
// (the client's TLS session ends there) and the relay has an address to dial.
// Entries already bound are always included, even when they cannot render right
// now: a bound row whose rule or node disappeared must stay on screen, or the
// panel would silently change what the clients fetch and leave no way to unbind
// it.
func (s *Server) subscriptionEntryViews(sub *store.Subscription) ([]subEntryView, error) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	bound, err := s.Store.SubscriptionEntries(sub.ID)
	if err != nil {
		return nil, err
	}
	boundByKey := map[string]store.SubscriptionEntry{}
	for _, e := range bound {
		boundByKey[entryKey(e)] = e
	}
	byID := map[string]*store.Node{}
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	format := s.relayNameFormat()
	views := make([]subEntryView, 0, len(nodes))
	seen := map[string]bool{}

	view := func(target, relay *store.Node, e store.SubscriptionEntry, sb *store.NodeSingbox, rd targetReadiness, discovered bool) subEntryView {
		key := entryKey(e)
		seen[key] = true
		direct := e.RelayNodeID == ""
		// The ports this node's own inbounds are reachable on. Computed from the
		// same list the renderer uses, so the name the picker promises (with or
		// without the port) is the name the client will get.
		ports := nodeIngressPorts(rd.live, sb)
		v := subEntryView{
			NodeID: target.ID, NodeName: target.Name,
			RelayNodeID: e.RelayNodeID, Proto: e.Proto, SrcPort: e.SrcPort, Iface: e.Iface,
			Discovered: discovered, Available: true,
		}
		if relay != nil {
			v.RelayName = relay.Name
		}
		// The protocol column follows the same source as availability: the
		// reported file when there is one, the managed anytls pair before the
		// first report. A stale row (inbound_gone) names no protocol at all —
		// that is the honest answer while the running config disagrees.
		if direct {
			v.Protocols = entryProtocols(rd.live, e.SrcPort, sb)
		} else if rd.relayable {
			// A relay leg terminates on the target's anytls inbound, so anytls
			// is the only protocol it can render as.
			v.Protocols = []string{singbox.ProtoAnytls}
		}
		switch {
		case direct:
			v.AutoName = directEntryName(entryBaseName(target), ports, e.SrcPort)
		case relay != nil:
			v.AutoName = relayAutoName(format, target, relay, e)
		default:
			// The relay node is gone; the naming chain falls back to its id so
			// the row still says which ingress it used to be.
			v.AutoName = relayAutoName(format, target, &store.Node{ID: e.RelayNodeID}, e)
		}
		if b, ok := boundByKey[key]; ok {
			v.Selected, v.Alias = b.Enabled, b.Alias
		}
		// Availability is per leg: the target must be renderable — from its
		// reported file, or from the managed port/certificate pair before the
		// first report arrives (§10) — and a relayed entry additionally needs
		// an anytls inbound to terminate on plus the relay's address to dial.
		switch {
		case !rd.renderable:
			v.Available, v.Reason = false, "not_ready"
		case direct:
			// A direct entry names one inbound, so it is only renderable while
			// that listener is still in the probe's file. A bound row whose
			// inbound disappeared stays listed (unbindable, and visibly stale)
			// instead of being dropped behind the operator's back.
			if e.SrcPort > 0 && !containsPort(ports, e.SrcPort) {
				v.Available, v.Reason = false, "inbound_gone"
			} else if s.directEntryShadowed(target, sb, e.SrcPort) {
				v.Warning = "shadowed"
			}
		case relay == nil, relay.PrimaryIP == "":
			v.Available, v.Reason = false, "relay_not_ready"
		case !discovered:
			v.Available, v.Reason = false, "relay_gone"
		case !rd.relayable:
			v.Available, v.Reason = false, "not_ready"
		}
		return v
	}

	// Only nodes that would actually render are *offered* (§10: "节点多选只列
	// 能出现在输出里的节点"), and per ingress since 2026-09-18: one row per
	// inbound port, so two anytls listeners on one probe are two entries. A node
	// already bound stays listed even when it is unrenderable right now — the
	// operator must be able to unbind it without the panel hiding the binding
	// behind their back.
	for i := range nodes {
		target := &nodes[i]
		sb, _ := s.Store.GetNodeSingbox(target.ID)
		rd := s.targetReadinessOf(target, sb)
		for _, port := range directCandidatePorts(rd.live, sb) {
			e := store.SubscriptionEntry{NodeID: target.ID, SrcPort: port}
			if seen[entryKey(e)] {
				continue // one row per entry identity: never list an entry twice
			}
			if _, bound := boundByKey[entryKey(e)]; bound || rd.renderable {
				views = append(views, view(target, nil, e, sb, rd, false))
			}
		}
		if !rd.renderable {
			// The landing node of a relay entry is the node the client's TLS
			// session ends on, so it has to be a working anytls node. The relay
			// itself does not: it only has to route packets (and have an
			// address to dial), which is why it needs no sing-box at all.
			continue
		}
		for _, c := range s.relayCandidates(target, sb) {
			relay := byID[c.RelayNodeID]
			if relay == nil || relay.PrimaryIP == "" {
				continue // nothing to dial: not a usable ingress
			}
			e := c.entry(target.ID)
			if seen[entryKey(e)] {
				continue // relayCandidates dedupes already; this is the backstop
			}
			v := view(target, relay, e, sb, rd, true)
			v.Source = c.Comment
			views = append(views, v)
		}
	}
	// Bound rows that are not candidates any more (rule deleted, node went
	// unrenderable): keep them visible and unbindable.
	for _, e := range bound {
		if seen[entryKey(e)] {
			continue
		}
		target := byID[e.NodeID]
		if target == nil {
			continue
		}
		sb, _ := s.Store.GetNodeSingbox(target.ID)
		views = append(views, view(target, byID[e.RelayNodeID], e, sb, s.targetReadinessOf(target, sb), false))
	}
	return views, nil
}

// entryProtocols names the protocols a direct entry renders as, in the
// reported file's order. `port` > 0 narrows to one inbound; 0 (the legacy
// node-level row) means every inbound of the node, deduplicated. The fallback
// mirrors nodeIngressPorts: before the first report the managed pair stands in
// for the file, and that pair is always the anytls inbound fobe generated.
// Once a file exists it is the truth, so a port it no longer declares yields
// no protocol — exactly the rows the picker marks inbound_gone.
func entryProtocols(live []singbox.ProxyNode, port int, sb *store.NodeSingbox) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range live {
		if port > 0 && n.Port != port {
			continue
		}
		p := n.Protocol
		if p == "" {
			p = singbox.ProtoAnytls // the fobe-generated inbound leaves it empty
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 && len(live) == 0 && sb != nil && sb.Port > 0 && sb.CertPEM != "" {
		return []string{singbox.ProtoAnytls}
	}
	return out
}

// directCandidatePorts is what the picker offers for one node: one row per
// dialable ingress. A node with nothing dialable yields the legacy node-level
// row (port 0) so it stays bindable — that is also the identity of every direct
// row written before 2026-09-18, and one the reconciler has not been able to
// split yet (the probe has not reported its inbounds).
func directCandidatePorts(live []singbox.ProxyNode, sb *store.NodeSingbox) []int {
	if ports := nodeIngressPorts(live, sb); len(ports) > 0 {
		return ports
	}
	return []int{0}
}

// containsPort is a linear scan: a node has a handful of inbounds, and the
// alternative (a map per row) costs more than it saves.
func containsPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// relayKey is a relayed entry's identity minus the landing node — what ties it
// to one forward rule: (relay node, proto, source port, interface).
func relayKey(relayNodeID, proto string, srcPort int, iface string) string {
	return fmt.Sprintf("%s|%s|%d|%s", relayNodeID, proto, srcPort, iface)
}

// pruneStaleEntries deletes §10.2 rows that can never render again
// (2026-09-19 实现修订). Two shapes qualify:
//
//   - a **direct row whose port the probe's reported file no longer
//     declares** — bound or tombstone. The binding names a port and the
//     running config is the truth (§9.3), so once the file drops that port the
//     row renders nothing, clients never saw it, and all it does is park a
//     permanent 暂不可渲染 line in the entry list. A direct tombstone has no
//     auto-enrolment to hold back (nothing writes direct rows on its own), so
//     it goes with the bound row; if the port returns, the picker offers it as
//     a fresh unchecked candidate.
//   - a **relay row whose leg is gone** — bound or tombstone. A relay leg needs
//     both halves, and it is *derived* from them: a tcp forward rule on the
//     relay whose destination is one of the landing node's anytls listeners.
//     Lose either half and the candidate stops existing, so the row can never
//     render again (the ingress either forwards to a closed port or terminates
//     on a listener that is not there). Tombstones go too: keeping one would
//     only matter if the entry could come back by itself, and when the topology
//     *does* come back the reconciler enrols it again — which is the designed
//     default the operator opted out of, not a surprise (§10.2).
//
// Three guards keep this from eating live bindings:
//
//   - only when a reported file actually renders something (rd.live non-empty).
//     A node whose file is missing or unparseable reads `not_ready`, never
//     `inbound_gone`, and its rows are left exactly as they are — the panel
//     cannot tell "mid-install" from "the listener is gone" without a file;
//   - for direct rows, only when the port is absent from `nodeIngressPorts`,
//     the same list the renderer and the picker's availability check use, so
//     "what is shown" and "what is pruned" cannot drift apart;
//   - for the rule half of a relay row, only when the relay's forwards snapshot
//     is *trustworthy* (`Supported`): an agent that cannot read nftables, or
//     one older than §21, reports an empty rule list that means "unknown", not
//     "deleted" — deleting on that would drop every relay leg of the node.
//
// Plus the in-flight gate: a direct row is never pruned while a lifecycle row
// for its port is `pending` or `deleting` — that is the panel's own change
// still in flight (a port it added or moved before the probe applied the
// document), where the port's absence is a window, not a verdict. Deleting on
// that signal would destroy the binding on every port move (§9.3/§10.2 实现
// 修订 2026-09-18b rewrote those rows to follow the move on purpose). A relay
// row needs no such gate: the panel moving or retiring a listener leaves the
// old port in the file until the probe applies, so the leg stays derivable for
// the whole window.
func (s *Server) pruneStaleEntries(sub *store.Subscription) int {
	entries, err := s.Store.SubscriptionEntries(sub.ID)
	if err != nil {
		return 0
	}
	// One read per node, not per row: every entry of a node asks the same
	// questions about the same reported file.
	type nodeView struct {
		renderable bool
		landable   bool
		ports      []int
		live       bool
		inFlight   map[int]bool
		// derived is the set of relay identities this landing node still
		// produces — the same list the reconciler enrols from and the picker
		// calls `discovered`.
		derived map[string]bool
	}
	views := map[string]nodeView{}
	nodeState := func(nodeID string) (nodeView, bool) {
		if v, ok := views[nodeID]; ok {
			return v, true
		}
		target, err := s.Store.GetNode(nodeID)
		if err != nil {
			return nodeView{}, false
		}
		sb, err := s.Store.GetNodeSingbox(nodeID)
		if err != nil {
			sb = nil
		}
		rd := s.targetReadinessOf(target, sb)
		v := nodeView{
			renderable: rd.renderable,
			// landable is the file's own answer, from the same helper the
			// renderer's relay leg uses — not rd.relayable, which folds in the
			// managed port/certificate pair. With a file on the probe that pair
			// is not the truth (§9.3).
			landable: relayableInbound(rd.live) != nil,
			ports:    nodeIngressPorts(rd.live, sb),
			live:     len(rd.live) > 0,
			inFlight: map[int]bool{},
			derived:  map[string]bool{},
		}
		for _, c := range s.relayCandidates(target, sb) {
			v.derived[relayKey(c.RelayNodeID, c.Proto, c.SrcPort, c.Iface)] = true
		}
		if states, err := s.Store.ListNodeSingboxInbounds(nodeID); err == nil {
			for _, st := range states {
				if st.Status == "pending" || st.Status == "deleting" {
					v.inFlight[st.Port] = true
				}
			}
		}
		views[nodeID] = v
		return v, true
	}

	gone := []store.SubscriptionEntry{}
	reasons := map[string]string{}
	for _, e := range entries {
		if e.RelayNodeID == "" {
			// The legacy node-level row (port 0) has no port to be gone, and the
			// split owns it.
			if e.SrcPort <= 0 {
				continue
			}
			v, ok := nodeState(e.NodeID)
			if !ok || !v.renderable || !v.live {
				continue
			}
			if containsPort(v.ports, e.SrcPort) || v.inFlight[e.SrcPort] {
				continue
			}
			gone = append(gone, e)
			reasons[entryKey(e)] = fmt.Sprintf("direct %d (inbound gone)", e.SrcPort)
			continue
		}
		// Relay rows (tombstones included): the leg is gone when either half
		// is — no pinnable anytls listener left to terminate on, or no rule
		// that still lands on one. Both are "not derivable", which is the same
		// predicate the picker shows as relay_gone and the reconciler enrols
		// from, so the row cannot come back by itself. A tombstone whose leg is
		// still derivable is left alone: there it is the opt-out, and deleting
		// it would hand the entry straight back to auto-enrolment.
		v, ok := nodeState(e.NodeID)
		if !ok || !v.renderable || !v.live {
			continue // no file on the landing node: unknown, not gone
		}
		if !v.landable {
			gone = append(gone, e)
			reasons[entryKey(e)] = fmt.Sprintf("relay %s:%d (landing node has no anytls inbound)",
				e.RelayNodeID, e.SrcPort)
			continue
		}
		if v.derived[relayKey(e.RelayNodeID, e.Proto, e.SrcPort, e.Iface)] {
			continue
		}
		st, err := s.Store.GetNodeForwardStatus(e.RelayNodeID)
		if err != nil || !st.Supported {
			// The rule list is "unknown", not "empty": an agent that cannot read
			// nftables (or predates §21) must not cost the operator a leg.
			continue
		}
		gone = append(gone, e)
		reasons[entryKey(e)] = fmt.Sprintf("relay %s:%d (forward rule gone)",
			e.RelayNodeID, e.SrcPort)
	}
	if len(gone) == 0 {
		return 0
	}
	deleted, err := s.Store.DeleteSubscriptionEntries(sub.ID, gone)
	if err != nil {
		s.Log.Warn("prune stale subscription entries", "subscription", sub.ID, "err", err)
		return 0
	}
	if deleted == 0 {
		return 0
	}
	for _, e := range gone {
		// Audited because it is client-visible intent: the operator's binding
		// (alias included) is gone, and knowing when it went is the only way to
		// tell "the probe stopped serving it" from "the panel lost it".
		s.Store.InsertAudit(&store.AuditEntry{
			Actor: "server", NodeID: e.NodeID, Action: "subscription_entry_pruned",
			Command: reasons[entryKey(e)],
		})
	}
	s.publishEvent("subscriptions_changed", sub.ID)
	return deleted
}

// splitNodeLevelDirectEntries rewrites this subscription's legacy node-level
// direct rows (src_port=0, "every inbound of this node") into one row per
// inbound port (§9.3/§10.2 实现修订 2026-09-18). Without it an upgraded panel
// would list the old node-level row *and* the new per-inbound candidates, and
// checking one of the latter would render the node twice.
//
// It runs from the same triggers as the relay half and for the same reason
// (startup, a probe's report, the moment the operator opens the picker): the
// split needs the node's inbounds, and those only exist once the probe has
// reported a file or the panel has a managed port/certificate pair. A node in
// neither state keeps its row — that row renders exactly what it did before,
// and the next trigger tries again.
func (s *Server) splitNodeLevelDirectEntries(sub *store.Subscription) int {
	entries, err := s.Store.SubscriptionEntries(sub.ID)
	if err != nil {
		return 0
	}
	legacy := map[string]store.SubscriptionEntry{}
	for _, e := range entries {
		// Only one row per node can match (proto/port/iface empty is the whole
		// identity), so a map is not losing anything.
		if e.RelayNodeID == "" && e.SrcPort == 0 {
			legacy[e.NodeID] = e
		}
	}
	split := 0
	for nodeID, row := range legacy {
		target, err := s.Store.GetNode(nodeID)
		if err != nil {
			continue // node deleted: the row went with it (FK cascade)
		}
		sb, err := s.Store.GetNodeSingbox(nodeID)
		if err != nil {
			sb = nil // never managed: the reported file decides on its own
		}
		live, _ := s.liveNodesFor(nodeID, "", target.PrimaryIP)
		ports := nodeIngressPorts(live, sb)
		if len(ports) == 0 {
			continue // nothing dialable yet: the row is still the best answer
		}
		ok, err := s.Store.SplitSubscriptionDirectEntry(sub.ID, nodeID, row.Alias, row.Enabled, ports)
		if err != nil {
			s.Log.Warn("split node-level subscription entry",
				"subscription", sub.ID, "node", nodeID, "err", err)
			continue
		}
		if !ok {
			continue // a concurrent trigger got there first
		}
		split++
		// Audited because it is client-visible: the node's outbounds are now
		// named per inbound (and a multi-inbound node gains the port suffix).
		s.Store.InsertAudit(&store.AuditEntry{
			Actor: "server", NodeID: nodeID, Action: "subscription_entry_split",
			Command: fmt.Sprintf("direct -> %s", portsList(ports)),
		})
		s.publishEvent("subscriptions_changed", sub.ID)
	}
	return split
}

// portsList renders ports for an audit line ("8443,8444").
func portsList(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, strconv.Itoa(p))
	}
	return strings.Join(out, ",")
}

// ReconcileSubscriptionEntries keeps §10.2 rows in step with the fleet: it
// prunes rows that can never render again (a direct row whose port left the
// probe's file, an enabled relay row whose landing node lost its anytls
// inbounds), splits legacy node-level rows, and auto-enrols relay entries.
// Idempotent and cheap (a handful of indexed reads); called at startup,
// whenever a probe reports new forwards, and before the panel reads the
// candidate list. Returns how many entries were created (pruning and splitting
// are migrations of rows the operator already owns, not enrolments).
func (s *Server) ReconcileSubscriptionEntries() int {
	subs, err := s.Store.ListSubscriptions()
	if err != nil {
		s.Log.Warn("list subscriptions for relay reconcile", "err", err)
		return 0
	}
	added := 0
	for i := range subs {
		added += s.reconcileSubscriptionEntries(&subs[i])
	}
	return added
}

// reconcileSubscriptionEntries enrols the relay candidates of the nodes this
// subscription already binds directly. That precondition is the whole safety
// argument: a new subscription or a node nobody picked never gains entries by
// itself, so auto-enrolment can only extend a decision the operator made.
func (s *Server) reconcileSubscriptionEntries(sub *store.Subscription) int {
	// Not behind sub.relay_auto_include, and not part of the returned count:
	// that switch is about relays, while these two are about rows the operator
	// already bound — a dead row would otherwise sit in the picker forever, and
	// half-migrated legacy rows would make it offer the same node twice.
	s.pruneStaleEntries(sub)
	s.splitNodeLevelDirectEntries(sub)
	// The switch is checked here rather than in the callers: every path that
	// could enrol an entry (startup, a forwards report, opening the picker) goes
	// through this function, and one of them forgetting the guard is exactly the
	// bug the switch exists to prevent.
	if !s.relayAutoInclude() {
		return 0
	}
	bound, err := s.Store.SubscriptionEntries(sub.ID)
	if err != nil {
		return 0
	}
	targets := map[string]bool{}
	for _, e := range bound {
		if e.RelayNodeID == "" && e.Enabled {
			targets[e.NodeID] = true
		}
	}
	if len(targets) == 0 {
		return 0
	}
	added := 0
	for id := range targets {
		target, err := s.Store.GetNode(id)
		if err != nil {
			continue
		}
		sb, err := s.Store.GetNodeSingbox(id)
		if err != nil {
			sb = nil // never managed: the reported file decides on its own
		}
		if !s.targetReadinessOf(target, sb).relayable {
			continue // not renderable yet: enrol when it is, not before
		}
		for _, c := range s.relayCandidates(target, sb) {
			relay, err := s.Store.GetNode(c.RelayNodeID)
			if err != nil || relay.PrimaryIP == "" {
				continue
			}
			entry := c.entry(target.ID)
			entry.SubscriptionID = sub.ID
			created, err := s.Store.InsertSubscriptionEntryIfAbsent(entry)
			if err != nil {
				s.Log.Warn("auto-enrol relay entry", "subscription", sub.ID, "node", target.ID, "err", err)
				continue
			}
			if !created {
				continue // already bound, or an explicit tombstone: leave it
			}
			added++
			s.Store.InsertAudit(&store.AuditEntry{
				Actor: "server", NodeID: c.RelayNodeID, Action: "subscription_entry_auto_added",
				Command: fmt.Sprintf("%s/%d%s -> %s via %s", c.Proto, c.SrcPort, ifaceSuffix(c.Iface), target.Name, relay.Name),
			})
			s.publishEvent("subscriptions_changed", sub.ID)
		}
	}
	return added
}

// ifaceSuffix renders the rule's interface restriction for the audit line.
func ifaceSuffix(iface string) string {
	if iface == "" {
		return ""
	}
	return "@" + iface
}

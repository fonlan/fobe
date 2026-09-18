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
			out = append(out, relayCandidate{
				RelayNodeID: f.NodeID, Proto: f.Forward.Proto, SrcPort: f.Forward.SrcPort,
				Iface: f.Forward.Iface, Comment: f.Forward.Comment,
			})
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
			v := view(target, relay, c.entry(target.ID), sb, rd, true)
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

// ReconcileSubscriptionEntries auto-enrols §10.2 relay entries across every
// subscription. Idempotent and cheap (a handful of indexed reads); called at
// startup, whenever a probe reports new forwards, and before the panel reads
// the candidate list. Returns how many entries were created.
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
	// that switch is about relays, while this is a migration of rows the
	// operator already bound — leaving half of them node-level would make the
	// picker offer the same node twice.
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

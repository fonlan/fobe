package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/quota"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- frame handlers ---

// onHello marks the node online and refreshes its static facts (§7).
func (h *Hub) onHello(c *Conn, hello *protocol.Hello) {
	now := protocol.Now()
	if err := h.store.TouchNode(c.nodeID, hello.Version, now); err != nil {
		h.log.Warn("touch node", "node", c.nodeID, "err", err)
	}
	if err := h.store.UpdateNodeInfo(c.nodeID, hello.OS, hello.Arch, hello.Kernel,
		hello.DistroID, hello.DistroVersion,
		hello.Hostname, agentTZ(hello.TZ), hello.CPUCores); err != nil {
		h.log.Warn("update node info", "node", c.nodeID, "err", err)
	}
	caps, _ := json.Marshal(hello.Caps)
	_ = h.store.SetNodeCaps(c.nodeID, caps)
	// §5.5: the target was decided while building hello_ack, i.e. against the
	// version this node reported *last* time. Now that this hello is in, ask
	// again — Target() closes a converged plan, and without this a probe that
	// came back on its own (manual reinstall, self-update, downgrade) would
	// keep the previous binary's verdict ("planned" / "unsupported" + last
	// error) on the panel until its next reconnect.
	if h.agentUp != nil {
		h.agentUp.Reconcile(c.nodeID)
	}
	if len(hello.IPs) > 0 {
		h.recordIPs(c.nodeID, hello.IPs)
	}
	h.replaceInterfaces(c.nodeID, hello.Interfaces)
	_ = h.store.RecoverAlert("node_offline", c.nodeID)
	h.log.Debug("node hello", "node", c.nodeID, "version", hello.Version)
}

func agentTZ(tz string) string {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "UTC"
	}
	return tz
}

func (h *Hub) onMetrics(nodeID string, ts int64, m *protocol.Metrics) {
	if ts == 0 {
		ts = protocol.Now()
	}
	diskUsed, diskTotal := pickMainDisk(m.Disks)
	sample := &store.MetricsSample{
		TS: ts, CPU: m.CPU,
		MemUsed: int64(m.MemUsed), MemTotal: int64(m.MemTotal),
		DiskUsed: diskUsed, DiskTotal: diskTotal,
		NetRxRate: m.NetRxRate, NetTxRate: m.NetTxRate,
		Load1: m.Load1, Uptime: int64(m.Uptime),
	}
	if err := h.store.InsertMetricsSample(nodeID, sample); err != nil {
		h.log.Warn("insert metrics", "node", nodeID, "err", err)
	}
}

// pickMainDisk reports the largest-volume mount (the panel shows the main
// partition; multiple mounts arrive from statfs, design §8.1).
func pickMainDisk(disks []protocol.Disk) (used, total int64) {
	var best uint64
	for _, d := range disks {
		if d.Total > best {
			best = d.Total
			used, total = int64(d.Used), int64(d.Total)
		}
	}
	return
}

// onTraffic applies the counter delta with reset detection (design §8.2).
func (h *Hub) onTraffic(nodeID string, ts int64, t *protocol.Traffic) {
	if ts == 0 {
		ts = protocol.Now()
	}
	net, err := h.store.GetNodeNetwork(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		// §8.2 accounting does not depend on node configuration: a probe whose
		// interface was never picked in the panel still accumulates. Returning
		// early here (the previous behaviour) left traffic_counters and
		// traffic_daily permanently empty — the daily-traffic chart and the
		// card's "today" numbers had no data at all.
		net = &store.NodeNetwork{NodeID: nodeID, Iface: t.Iface, Mode: quota.ModeBoth, CycleType: "none", TZ: "UTC"}
		if n, err := h.store.GetNode(nodeID); err == nil && n.TZ != "" {
			net.TZ = n.TZ // daily buckets follow the probe's clock (§8.2.3)
		}
	} else if err != nil {
		h.log.Warn("get node network", "node", nodeID, "err", err)
		return
	}
	if net.Iface == "" {
		net.Iface = t.Iface // automatic mode follows the agent-detected default
	}
	if t.Iface != "" && t.Iface != net.Iface {
		return // report for an interface the panel doesn't track
	}
	h.accumulate(nodeID, net, ts, "rx", int64(t.Rx))
	h.accumulate(nodeID, net, ts, "tx", int64(t.Tx))
}

func (h *Hub) accumulate(nodeID string, net *store.NodeNetwork, ts int64, direction string, raw int64) {
	prev, err := h.store.GetTrafficCounter(nodeID, net.Iface, direction)
	if err != nil {
		// first sight of this counter: baseline only, no delta
		_ = h.store.UpsertTrafficCounter(&store.TrafficCounter{
			NodeID: nodeID, Iface: net.Iface, Direction: direction,
			LastRaw: raw, LastTS: ts,
			PeriodStart: quota.PeriodStart(net.CycleType, net.NextResetAt, net.TZ, time.Unix(ts, 0)),
		})
		return
	}

	delta := raw - prev.LastRaw
	if delta < 0 || ts < prev.LastTS {
		// wrap / reboot / interface reset: re-baseline, never count negative (§8.2.2)
		h.warnCounterReset(nodeID, net.Iface, direction, delta)
		delta = 0
	}

	counter := &store.TrafficCounter{
		NodeID: nodeID, Iface: net.Iface, Direction: direction,
		LastRaw: raw, LastTS: ts,
		PeriodStart: quota.PeriodStart(net.CycleType, net.NextResetAt, net.TZ, time.Unix(ts, 0)),
		PeriodUsed:  prev.PeriodUsed + delta,
	}
	// period rolled since the last report: restart the cache
	if counter.PeriodStart != prev.PeriodStart {
		counter.PeriodUsed = delta
	}
	if err := h.store.UpsertTrafficCounter(counter); err != nil {
		h.log.Warn("upsert traffic counter", "node", nodeID, "err", err)
	}

	if delta > 0 {
		date := quota.LocalDate(ts, net.TZ)
		if direction == "rx" {
			err = h.store.AddTrafficDaily(nodeID, date, delta, 0)
		} else {
			err = h.store.AddTrafficDaily(nodeID, date, 0, delta)
		}
		if err != nil {
			h.log.Warn("add traffic daily", "node", nodeID, "err", err)
		}
	}
}

func (h *Hub) warnCounterReset(nodeID, iface, direction string, delta int64) {
	payload, _ := json.Marshal(map[string]any{
		"iface": iface, "direction": direction, "delta": delta,
	})
	id, created, err := h.store.CreateAlert("counter_reset", nodeID, string(payload), 3600)
	if err != nil {
		h.log.Warn("create counter_reset alert", "err", err)
		return
	}
	if created {
		h.log.Info("traffic counter reset", "node", nodeID, "iface", iface, "direction", direction)
		_ = id
	}
}

func (h *Hub) onLatency(nodeID string, b *protocol.LatencyBatch) {
	if len(b.Samples) == 0 {
		return
	}
	rows := make([]store.LatencySampleRow, 0, len(b.Samples))
	for _, s := range b.Samples {
		rows = append(rows, store.LatencySampleRow{
			TargetID: s.TargetID, TS: s.TS, ICMPMs: s.ICMPMs, TCPMs: s.TCPMs, Loss: s.Loss,
		})
	}
	if err := h.store.InsertLatencySamples(nodeID, rows); err != nil {
		h.log.Warn("insert latency samples", "node", nodeID, "err", err)
	}
}

func (h *Hub) onState(nodeID string, st *protocol.State) {
	if len(st.IPs) > 0 {
		h.recordIPs(nodeID, st.IPs)
	}
	h.replaceInterfaces(nodeID, st.Interfaces)
	if st.Singbox != nil {
		h.recordSingboxState(nodeID, st.Singbox)
	}
	// nil (not empty) means the agent predates the field: keep what is stored
	// rather than wiping a snapshot for a probe that simply cannot report one.
	if st.SingboxLocal != nil {
		h.recordSingboxLocal(nodeID, st.SingboxLocal)
	}
	// nil (not empty) means the agent predates §21: keep the last known
	// inventory instead of wiping rows for rules that are still on the probe.
	if st.Forwards != nil {
		h.RecordForwardState(nodeID, st.Forwards)
	}
}

// recordIPs stores one full address report and keeps nodes.primary_ip and
// nodes.country_code in step with it (design §14). hello and state both funnel
// through here on purpose: they used to carry copies of this logic, and the
// hello copy resolved no country at all — so a probe whose primary_ip was first
// written by its hello kept an empty flag forever, because the re-pin guard
// skips exactly that state (the stored address is already the reported one).
func (h *Hub) recordIPs(nodeID string, ips []protocol.IPInfo) {
	rows := make([]store.IPRow, 0, len(ips))
	primary := ""
	for _, ip := range ips {
		rows = append(rows, store.IPRow{
			IP: ip.IP, Family: ip.Family, Scope: ip.Scope, IsPrimary: ip.IsPrimary,
		})
		if ip.IsPrimary && primary == "" {
			primary = ip.IP
		}
	}
	if err := h.store.ReplaceNodeIPs(nodeID, rows); err != nil {
		h.log.Warn("replace node ips", "node", nodeID, "err", err)
		return
	}
	if primary == "" {
		// The agent marks a suggestion; without one (or from a build that did
		// not), fall back to the first public IPv4 like the store's heuristic.
		for _, ip := range ips {
			if ip.Scope == "public" && ip.Family == 4 {
				primary = ip.IP
				break
			}
		}
	}
	n, err := h.store.GetNode(nodeID)
	if err != nil {
		return
	}
	// Re-pin only when the stored address is gone (or was never set). A §14
	// manual pick that is still reported must survive: unconditional re-pinning
	// would fight the store's manual-pick sync and flip the list column back to
	// the agent's suggestion on every report.
	if primary != "" && (n.PrimaryIP == "" || !ipInRows(ips, n.PrimaryIP)) {
		code, _ := h.resolveCountry(n, ips, primary) // a miss keeps the stored code
		if err := h.store.SetNodePrimaryIP(nodeID, primary, code); err != nil {
			h.log.Warn("set node primary ip", "node", nodeID, "err", err)
		}
		n.PrimaryIP, n.CountryCode = primary, code
	}
	h.applyCountry(n, ips, n.PrimaryIP)
}

// RefreshCountry re-derives nodes.country_code from the addresses the node
// currently reports (§14). The panel calls it after an operator action that
// changes what the flag must follow — a manual primary pick, or clearing the pin
// back to auto — so the flag lands at once instead of at the next state report
// (up to 5 minutes).
func (h *Hub) RefreshCountry(nodeID string) {
	if h.geo == nil {
		return
	}
	n, err := h.store.GetNode(nodeID)
	if err != nil {
		return
	}
	rows, err := h.store.ListNodeIPs(nodeID)
	if err != nil {
		return
	}
	ips := make([]protocol.IPInfo, 0, len(rows))
	for _, r := range rows {
		ips = append(ips, protocol.IPInfo{IP: r.IP, Family: r.Family, Scope: r.Scope, IsPrimary: r.IsPrimary})
	}
	h.applyCountry(n, ips, n.PrimaryIP)
}

// resolveCountry maps a node's addresses to a §14 country: the primary address
// first, then the node's other public addresses (IPv4 before IPv6). The fallback
// is what keeps a LAN primary pick harmless — a private address can never carry
// a country, so judging the flag by the primary address alone left every NAT'd
// probe blank. A manually pinned country short-circuits the lookup entirely;
// ok=false means "nothing resolved, keep the stored code" so a missing or
// unhelpful database never wipes a flag.
func (h *Hub) resolveCountry(n *store.Node, ips []protocol.IPInfo, primary string) (string, bool) {
	if h.geo == nil || n.CountryManual {
		return n.CountryCode, false
	}
	if primary != "" {
		if code, ok := h.geo.Country(primary); ok {
			return code, true
		}
	}
	for _, family := range []int{4, 6} {
		for _, ip := range ips {
			if ip.Scope != "public" || ip.Family != family || ip.IP == primary {
				continue
			}
			if code, ok := h.geo.Country(ip.IP); ok {
				return code, true
			}
		}
	}
	return n.CountryCode, false
}

// applyCountry writes the resolved country when it differs from the stored one.
// It runs on every report rather than only when the primary address changes:
// flags have to heal by themselves for rows whose primary_ip was stored without
// a lookup (a hello before this revision, a manual primary pick).
func (h *Hub) applyCountry(n *store.Node, ips []protocol.IPInfo, primary string) {
	if h.geo == nil || n.CountryManual {
		return
	}
	code, ok := h.resolveCountry(n, ips, primary)
	if !ok || code == n.CountryCode {
		return
	}
	if err := h.store.SetNodeCountry(n.ID, code, false); err != nil {
		h.log.Warn("set node country", "node", n.ID, "err", err)
	}
}

// RecordForwardState stores the probe's nftables port-forward snapshot (§21).
// It is exported because the panel's own §21 command answers carry the same
// payload: persisting them here keeps one code path for "what the panel sees".
// The probe's ruleset is the source of truth, so this is a wholesale replace: a
// rule missing from the report is one an external tool (nfpf.sh) removed.
func (h *Hub) RecordForwardState(nodeID string, f *protocol.ForwardsState) {
	rows := make([]store.NodeForward, 0, len(f.Rules))
	for _, r := range f.Rules {
		rows = append(rows, store.NodeForward{
			Handle: r.Handle, Proto: r.Proto, SrcPort: r.SrcPort, Iface: r.Iface,
			DstIP: r.DstIP, DstPort: r.DstPort, Comment: r.Comment, ExtraMatch: r.ExtraMatch,
		})
	}
	status := store.NodeForwardStatus{
		Supported:   f.Supported,
		Initialized: f.Initialized,
		Code:        f.Code,
		Message:     f.Message,
		ReportedAt:  protocol.Now(),
	}
	if err := h.store.ReplaceNodeForwards(nodeID, status, rows); err != nil {
		h.log.Warn("replace node forwards", "node", nodeID, "err", err)
		return
	}
	// §10.2: the ruleset changed, so the set of relay entries derived from it
	// may have too. Run the reconciler out of band — the agent's frame handler
	// must not block on subscription bookkeeping.
	if h.onForwards != nil {
		go h.onForwards()
	}
}

func (h *Hub) replaceInterfaces(nodeID string, interfaces []protocol.NetworkInterface) {
	if interfaces == nil {
		return // old agents omit this field; retain their last known inventory
	}
	rows := make([]store.NodeInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		rows = append(rows, store.NodeInterface{Name: iface.Name, IsDefault: iface.Default})
	}
	if err := h.store.ReplaceNodeInterfaces(nodeID, rows); err != nil {
		h.log.Warn("replace node interfaces", "node", nodeID, "err", err)
	}
}

func ipInRows(ips []protocol.IPInfo, want string) bool {
	for _, ip := range ips {
		if ip.IP == want {
			return true
		}
	}
	return false
}

func (h *Hub) recordSingboxState(nodeID string, s *protocol.SingboxState) {
	cur, err := h.store.GetNodeSingbox(nodeID)
	if err != nil {
		cur = &store.NodeSingbox{NodeID: nodeID}
	}
	prevStatus, prevErr := cur.Status, cur.LastError
	if s.CertPEM != "" || s.CertSHA256 != "" {
		sum := sha256.Sum256([]byte(s.CertPEM))
		if s.CertSHA256 == "" && s.CertPEM != "" {
			s.CertSHA256 = hex.EncodeToString(sum[:])
		}
		cur.CertPEM = s.CertPEM
		cur.CertSHA256 = s.CertSHA256
		cur.CertNotAfter = s.CertNotAfter
	}
	cur.Version = s.Version
	if s.Port > 0 {
		// A port of 0 is "no inbound right now", not a correction: keeping the
		// stored value is what lets an uninstall/reinstall round trip reuse the
		// operator's port (and its firewall rule) instead of drawing a new
		// random one (§9.2 实现修订 2026-09-16).
		cur.Port = s.Port
	}
	cur.LastError = s.LastError
	switch {
	case s.Running:
		cur.Status = "running"
	case s.LastError != "":
		cur.Status = "degraded"
	case s.Version == "" && cur.DesiredUninstall:
		// The probe says "no sing-box here and nothing went wrong" — the
		// confirmation of a panel-requested uninstall (§9.2 实现修订
		// 2026-09-16). Gated on the pending removal on purpose: the probe also
		// pushes a periodic `state` frame carrying its *last* snapshot, and
		// without the gate an install that takes minutes (a ~90 MiB download)
		// would be reported as absent by the stale frame of the uninstall
		// before it. Clearing the reported half is what drops the node out of
		// every subscription (§10: a node is listed only while it has a port
		// and a pinned certificate).
		cur.Status = "absent"
		cur.DesiredUninstall = false
		cur.ConfigHash = ""
		cur.CertPEM, cur.CertSHA256, cur.CertNotAfter = "", "", 0
	}
	if err := h.store.UpsertNodeSingbox(cur); err != nil {
		h.log.Warn("upsert singbox state", "node", nodeID, "err", err)
	}
	// the agent owns the hint value: mirror it verbatim ('' = resolved/cleared)
	if err := h.store.SetNodeSingboxFirewallHint(nodeID, s.FirewallHint); err != nil {
		h.log.Warn("store firewall hint", "node", nodeID, "err", err)
	}
	h.alertSingboxState(nodeID, s, prevStatus, prevErr, cur)
}

// recordSingboxLocal stores what sing-box the probe runs outside fobe's
// desired state (§9.3 实现修订 2026-09-17).
//
// It is deliberately inert: no alert, no status change, no desired-state
// change. Discovery explains a node; it must never act on one — the operator
// decides whether to adopt what fobe found. The only side effect is the stored
// snapshot, and even that is written solely when something moved, so a probe
// that reports the same file every minute costs one read.
//
// A malformed payload is kept rather than dropped: the panel's whole job here
// is to show the operator what is on his machine, and "fobe received a
// config.json it cannot parse" is information, not noise.
func (h *Hub) recordSingboxLocal(nodeID string, s *protocol.SingboxLocal) {
	snap := store.NodeSingboxLocal{
		LocalHash:    s.ConfigSHA256,
		ConfigPath:   s.ConfigPath,
		LocalVersion: s.Version,
		LocalRunning: s.Running,
		LocalUnit:    s.UnitActive,
		LocalUnitOK:  s.UnitKnown,
		LocalPresent: s.Present,
		ConfigJSON:   s.ConfigJSON,
		Error:        s.Error,
	}
	// Skip the write when the whole snapshot is unchanged. Comparing the file
	// hash alone would freeze every other field: the report gains fields over
	// time (config_path, unit state), and a probe whose config nobody touches
	// would keep a snapshot missing them forever — the panel would show a
	// discovery with holes and no way to refresh it short of editing the file.
	if cur, err := h.store.GetNodeSingboxLocal(nodeID, h.crypt); err == nil && cur == snap {
		return
	}
	if err := h.store.SetNodeSingboxLocal(nodeID, snap, h.crypt); err != nil {
		h.log.Warn("store local sing-box snapshot", "node", nodeID, "err", err)
	}
}

// alertSingboxState raises/recovers the §15 sing-box alerts off the status
// transition: degraded (or a new error while already degraded) opens a
// singbox_down alert (deduped 1h), running recovers it, and a detected
// rollback opens singbox_rollback (deduped 1h).
func (h *Hub) alertSingboxState(nodeID string, s *protocol.SingboxState, prevStatus, prevErr string, cur *store.NodeSingbox) {
	if cur.Status == "degraded" && (prevStatus != "degraded" || (s.LastError != "" && s.LastError != prevErr)) {
		payload, _ := json.Marshal(map[string]any{"error": s.LastError, "port": s.Port})
		if _, _, err := h.store.CreateAlert("singbox_down", nodeID, string(payload), 3600); err != nil {
			h.log.Warn("create singbox_down alert", "node", nodeID, "err", err)
		}
	}
	if cur.Status == "running" && prevStatus != "running" {
		if err := h.store.RecoverAlert("singbox_down", nodeID); err != nil {
			h.log.Warn("recover singbox_down alert", "node", nodeID, "err", err)
		}
	}
	if s.RollbackHappened || looksLikeRollback(s, cur) {
		payload, _ := json.Marshal(map[string]any{
			"error":           s.LastError,
			"version":         s.Version,
			"desired_version": cur.DesiredVersion,
			"port":            s.Port,
		})
		if _, _, err := h.store.CreateAlert("singbox_rollback", nodeID, string(payload), 3600); err != nil {
			h.log.Warn("create singbox_rollback alert", "node", nodeID, "err", err)
		}
	}
}

// looksLikeRollback covers agents that predate the explicit rollback flag:
// the reported version fell behind the panel's desired version while the
// error text admits a rollback happened (§9.2 rollback → §15 告警).
func looksLikeRollback(s *protocol.SingboxState, cur *store.NodeSingbox) bool {
	desired := normalizeVersion(cur.DesiredVersion)
	if desired == "" || normalizeVersion(s.Version) == desired {
		return false
	}
	err := strings.ToLower(s.LastError)
	return strings.Contains(err, "rollback") || strings.Contains(err, "rolled back")
}

// normalizeVersion strips the conventional v/V prefix (agent §9 semantics).
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "V")
}

func (h *Hub) onCmdResult(nodeID string, r *protocol.CmdResult) {
	status := "ok"
	if r.ExitCode != 0 || r.Error != "" {
		status = "failed"
	}
	result, _ := json.Marshal(r)
	if err := h.store.FinishCommand(r.ID, status, string(result)); err != nil {
		h.log.Warn("finish command", "node", nodeID, "id", r.ID, "err", err)
	}
	_ = h.store.InsertAudit(&store.AuditEntry{
		Actor: "agent", NodeID: nodeID, Action: "cmd_result:" + status, Command: truncate(r.Stdout+"\n"+r.Stderr, 2000),
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// buildDesiredState assembles the current desired state for a node (§7).
func (h *Hub) buildDesiredState(nodeID string) protocol.DesiredState {
	desired := protocol.DesiredState{}
	if net, err := h.store.GetNodeNetwork(nodeID); err == nil {
		iface := net.Iface
		desired.TrafficIface = &iface
	}
	if sb, err := h.store.GetNodeSingbox(nodeID); err == nil {
		switch {
		case sb.DesiredUninstall:
			// §9.2 实现修订 2026-09-16: removal is a declared state, so an
			// offline probe converges on reconnect even though the operator's
			// click may be hours old (a queued command would have expired).
			// The port rides along purely so the probe can hand it back in its
			// absent report (see recordSingboxState).
			desired.Singbox = &protocol.SingboxDesired{Uninstall: true, Port: sb.Port}
		case sb.DesiredVersion != "":
			desired.Singbox = &protocol.SingboxDesired{
				Version: sb.DesiredVersion,
				Port:    sb.Port,
			}
			if cfg, err := h.store.GetSetting("singbox_config:" + nodeID); err == nil {
				desired.Singbox.ConfigJSON = cfg
			}
		}
	}
	// §5.5: the agent build this server wants, plus the earliest moment the
	// probe may start. Filled here so hello_ack and an operator's retry push
	// (a plain `desired` frame) carry exactly the same values.
	if h.agentUp != nil {
		h.agentUp.Desired(nodeID, &desired)
	}
	return desired
}

// --- terminal relay (design §11) ---

func (h *Hub) relayTerminal(env protocol.Envelope) {
	var sid string
	if env.Type == protocol.TypeTerminalOutput {
		var p protocol.TerminalOutput
		if json.Unmarshal(env.Payload, &p) == nil {
			sid = p.SessionID
		}
	} else {
		var p protocol.TerminalClosed
		if json.Unmarshal(env.Payload, &p) == nil {
			sid = p.SessionID
		}
	}
	if sid == "" {
		return
	}
	h.termMu.Lock()
	ch, ok := h.termSubs[sid]
	h.termMu.Unlock()
	if ok {
		select {
		case ch <- env:
		default: // slow browser: drop rather than block the agent pump
		}
	}
}

// SubscribeTerminal registers a browser sink for an agent terminal session.
func (h *Hub) SubscribeTerminal(sessionID string) (<-chan protocol.Envelope, func()) {
	ch := make(chan protocol.Envelope, 256)
	h.termMu.Lock()
	h.termSubs[sessionID] = ch
	h.termMu.Unlock()
	return ch, func() {
		h.termMu.Lock()
		delete(h.termSubs, sessionID)
		h.termMu.Unlock()
	}
}

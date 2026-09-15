package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/quota"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// --- frame handlers ---

// onHello marks the node online and refreshes its static facts (§7).
func (h *Hub) onHello(c *Conn, hello *protocol.Hello) {
	now := protocol.Now()
	if err := h.store.TouchNode(c.nodeID, hello.Version, now); err != nil {
		h.log.Warn("touch node", "node", c.nodeID, "err", err)
	}
	if err := h.store.UpdateNodeInfo(c.nodeID, hello.OS, hello.Arch, hello.Kernel,
		hello.Hostname, hello.TZ, hello.CPUCores); err != nil {
		h.log.Warn("update node info", "node", c.nodeID, "err", err)
	}
	caps, _ := json.Marshal(hello.Caps)
	_ = h.store.SetNodeCaps(c.nodeID, caps)
	if len(hello.IPs) > 0 {
		rows := make([]store.IPRow, 0, len(hello.IPs))
		primary := ""
		for _, ip := range hello.IPs {
			rows = append(rows, store.IPRow{IP: ip.IP, Family: ip.Family, Scope: ip.Scope})
			if ip.IsPrimary && primary == "" {
				primary = ip.IP
			}
		}
		_ = h.store.ReplaceNodeIPs(c.nodeID, rows)
		if primary != "" {
			if n, err := h.store.GetNode(c.nodeID); err == nil {
				_ = h.store.SetNodePrimaryIP(c.nodeID, primary, n.CountryCode)
			}
		}
	}
	_ = h.store.RecoverAlert("node_offline", c.nodeID)
	h.log.Debug("node hello", "node", c.nodeID, "version", hello.Version)
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
	if err != nil {
		return // node has no selected interface yet: keep raw counters only
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
			PeriodStart: quota.PeriodStart(net.AnchorAt, net.CycleDays, net.TZ, time.Unix(ts, 0)),
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
		PeriodStart: quota.PeriodStart(net.AnchorAt, net.CycleDays, net.TZ, time.Unix(ts, 0)),
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
		rows := make([]store.IPRow, 0, len(st.IPs))
		primary := ""
		for _, ip := range st.IPs {
			rows = append(rows, store.IPRow{IP: ip.IP, Family: ip.Family, Scope: ip.Scope})
			if ip.IsPrimary && primary == "" {
				primary = ip.IP
			}
		}
		if err := h.store.ReplaceNodeIPs(nodeID, rows); err != nil {
			h.log.Warn("replace node ips", "node", nodeID, "err", err)
		}
		// primary pinned server-side only when the agent didn't mark one
		if primary != "" {
			if n, err := h.store.GetNode(nodeID); err == nil && (n.PrimaryIP == "" || !ipInRows(st.IPs, n.PrimaryIP)) {
				_ = h.store.SetNodePrimaryIP(nodeID, primary, h.countryFor(primary, n.CountryCode))
			}
		} else if n, err := h.store.GetNode(nodeID); err == nil {
			for _, ip := range st.IPs {
				if ip.Scope == "public" && ip.Family == 4 {
					_ = h.store.SetNodePrimaryIP(nodeID, ip.IP, h.countryFor(ip.IP, n.CountryCode))
					break
				}
			}
		}
	}
	if st.Singbox != nil {
		h.recordSingboxState(nodeID, st.Singbox)
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

// countryFor resolves the country of the node's primary IP (design §14):
// local MMDB first, online fallback when configured. A nil resolver or a
// lookup miss returns prev so an existing country_code survives.
func (h *Hub) countryFor(ip, prev string) string {
	if h.geo == nil || ip == "" {
		return prev
	}
	if code, ok := h.geo.Country(ip); ok {
		return code
	}
	return prev
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
	cur.Port = s.Port
	cur.LastError = s.LastError
	switch {
	case s.Running:
		cur.Status = "running"
	case s.LastError != "":
		cur.Status = "degraded"
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
	if sb, err := h.store.GetNodeSingbox(nodeID); err == nil && sb.DesiredVersion != "" {
		desired.Singbox = &protocol.SingboxDesired{
			Version: sb.DesiredVersion,
			Port:    sb.Port,
		}
		if cfg, err := h.store.GetSetting("singbox_config:" + nodeID); err == nil {
			desired.Singbox.ConfigJSON = cfg
		}
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

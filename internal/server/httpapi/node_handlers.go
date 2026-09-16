package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/quota"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// nodeView is the list/card shape the frontend renders (design §16).
type nodeView struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	Online       bool    `json:"online"`
	Note         string  `json:"note"`
	OS           string  `json:"os"`
	Arch         string  `json:"arch"`
	Hostname     string  `json:"hostname"`
	CPUCores     int     `json:"cpu_cores"`
	PrimaryIP    string  `json:"primary_ip"`
	CountryCode  string  `json:"country_code"`
	AgentVersion string  `json:"agent_version"`
	TZ           string  `json:"tz"`
	LastSeen     *int64  `json:"last_seen,omitempty"`
	CPU          float64 `json:"cpu"`
	MemUsed      int64   `json:"mem_used"`
	MemTotal     int64   `json:"mem_total"`
	DiskUsed     int64   `json:"disk_used"`
	DiskTotal    int64   `json:"disk_total"`
	NetRxRate    float64 `json:"net_rx_rate"`
	NetTxRate    float64 `json:"net_tx_rate"`
	// traffic per selected mode
	Iface             string  `json:"iface"`
	Mode              string  `json:"mode"`
	QuotaBytes        *int64  `json:"quota_bytes,omitempty"`
	PeriodUsed        float64 `json:"period_used"`
	PeriodStart       *int64  `json:"period_start,omitempty"`
	PeriodPct         float64 `json:"period_pct"` // -1 = no quota
	TodayRx           int64   `json:"today_rx"`
	TodayTx           int64   `json:"today_tx"`
	BillingConfigured bool    `json:"billing_configured"`
	NextDueAt         *int64  `json:"next_due_at,omitempty"`

	// Agent self-update state (design §5.5): what the panel needs to answer
	// "why did this probe not follow the server?".
	AgentTargetVersion   string `json:"agent_target_version"`
	AgentUpdateState     string `json:"agent_update_state"`
	AgentUpdateAttempts  int    `json:"agent_update_attempts"`
	AgentUpdateError     string `json:"agent_update_error,omitempty"`
	AgentUpdatePlannedAt *int64 `json:"agent_update_planned_at,omitempty"`
	AgentUpdateDoneAt    *int64 `json:"agent_update_done_at,omitempty"`
	// AgentSelfUpdate is the capability bit an agent built before §5.5 never
	// sends; false is how the panel recognises "needs a manual reinstall".
	AgentSelfUpdate bool `json:"agent_self_update"`
	// AgentCapsSeen tells "no self-update support" apart from "never said
	// hello": a freshly registered node has no caps yet, and accusing it of
	// needing a reinstall would be wrong.
	AgentCapsSeen bool `json:"agent_caps_seen"`
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]nodeView, 0, len(nodes))
	for i := range nodes {
		out = append(out, s.buildNodeView(&nodes[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

func (s *Server) buildNodeView(n *store.Node) nodeView {
	v := nodeView{
		ID: n.ID, Name: n.Name, Status: n.Status, Online: n.Status == "online",
		Note: n.Note, OS: n.OS, Arch: n.Arch, Hostname: n.Hostname,
		CPUCores: n.CPUCores, PrimaryIP: n.PrimaryIP, CountryCode: n.CountryCode,
		AgentVersion: n.AgentVersion,
		TZ:           n.TZ,
		PeriodPct:    -1,

		AgentTargetVersion:  n.AgentTargetVersion,
		AgentUpdateState:    n.AgentUpdateState,
		AgentUpdateAttempts: n.AgentUpdateAttempts,
		AgentUpdateError:    n.AgentUpdateError,
	}
	if n.AgentUpdatePlannedAt > 0 {
		v.AgentUpdatePlannedAt = &n.AgentUpdatePlannedAt
	}
	if n.AgentUpdateDoneAt > 0 {
		v.AgentUpdateDoneAt = &n.AgentUpdateDoneAt
	}
	// §5.5: the capability bit, not a version guess, decides whether this probe
	// can follow on its own.
	var caps protocol.Caps
	if len(n.Caps) > 0 {
		_ = json.Unmarshal(n.Caps, &caps)
	}
	v.AgentSelfUpdate = caps.SelfUpdate
	v.AgentCapsSeen = capsSeen(n.Caps)
	if n.LastSeen.Valid {
		v.LastSeen = &n.LastSeen.Int64
	}

	if m, err := s.Store.LatestMetrics(n.ID); err == nil {
		v.CPU = m.CPU
		v.MemUsed, v.MemTotal = m.MemUsed, m.MemTotal
		v.DiskUsed, v.DiskTotal = m.DiskUsed, m.DiskTotal
		v.NetRxRate, v.NetTxRate = m.NetRxRate, m.NetTxRate
	}

	// Today's bucket is computed outside the network-config branch: §8.2
	// accounting runs from the probe's first traffic report, so the card's
	// 今日下行/上行 must not stay 0 until someone fills in 网卡/配额.
	tz := n.TZ
	if net, err := s.Store.GetNodeNetwork(n.ID); err == nil {
		v.Iface = net.Iface
		v.Mode = net.Mode
		v.QuotaBytes = net.QuotaBytes
		start := quota.PeriodStart(net.CycleType, net.NextResetAt, net.TZ, time.Now())
		v.PeriodStart = &start
		sinceDate := quota.LocalDate(start, net.TZ)
		if rx, tx, err := s.Store.SumTrafficSince(n.ID, sinceDate); err == nil {
			used, pct := quota.Usage(net.Mode, rx, tx, deref(net.QuotaBytes))
			v.PeriodUsed = used
			v.PeriodPct = pct
		}
		if net.TZ != "" {
			tz = net.TZ
		}
	}
	if tz == "" {
		tz = "UTC"
	}
	today := quota.LocalDate(nowUnix(), tz)
	if rows, err := s.Store.ListTrafficDaily(n.ID, today); err == nil && len(rows) > 0 {
		v.TodayRx, v.TodayTx = rows[0].RxBytes, rows[0].TxBytes
	}

	if b, err := s.Store.GetNodeBilling(n.ID); err == nil {
		v.BillingConfigured = b.CycleType != "" && b.CycleType != "none" && b.CycleDays != nil && *b.CycleDays > 0
		v.NextDueAt = b.NextDueAt
	}
	return v
}

// capsSeen reports whether the agent ever completed a handshake: nodes are
// created with "{}" and only a hello replaces it (§5.5 uses this to avoid
// labelling a brand-new node as "needs a manual reinstall").
func capsSeen(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "{}"
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.Store.GetNode(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	view := s.buildNodeView(n)

	ips, _ := s.Store.ListNodeIPs(id)
	ipViews := make([]map[string]any, 0, len(ips))
	for _, ip := range ips {
		ipViews = append(ipViews, map[string]any{
			"ip": ip.IP, "family": ip.Family, "scope": ip.Scope, "is_primary": ip.IsPrimary,
		})
	}

	resp := map[string]any{
		"node":       view,
		"ips":        ipViews,
		"online_now": s.Hub.IsOnline(id),
	}
	if sb, err := s.Store.GetNodeSingbox(id); err == nil {
		resp["singbox"] = map[string]any{
			"version": sb.Version, "desired_version": sb.DesiredVersion,
			"status": sb.Status, "last_error": sb.LastError, "port": sb.Port,
			"cert_sha256": sb.CertSHA256, "cert_not_after": sb.CertNotAfter,
		}
	}
	if net, err := s.Store.GetNodeNetwork(id); err == nil {
		resp["network"] = net
		resp["traffic_cycle"] = trafficCycleView(net, time.Now())
	}
	if interfaces, err := s.Store.ListNodeInterfaces(id); err == nil {
		resp["interfaces"] = interfaces
	}
	if b, err := s.Store.GetNodeBilling(id); err == nil {
		resp["billing"] = b
	}
	targets, _ := s.Store.TargetsForNode(id)
	resp["latency_targets"] = targets
	writeJSON(w, http.StatusOK, resp)
}

type updateNodeReq struct {
	Name             *string          `json:"name,omitempty"`
	Note             *string          `json:"note,omitempty"`
	Network          *networkReq      `json:"network,omitempty"`
	TrafficCycle     *trafficCycleReq `json:"traffic_cycle,omitempty"`
	Billing          *billingReq      `json:"billing,omitempty"`
	LatencyTargetIDs *[]int64         `json:"latency_target_ids,omitempty"`
}

type networkReq struct {
	Iface      string `json:"iface"`
	Mode       string `json:"mode"`
	QuotaBytes *int64 `json:"quota_bytes"`
}

type trafficCycleReq struct {
	CycleType   string `json:"cycle_type"`
	NextResetAt *int64 `json:"next_reset_at"`
}

type trafficCycleResp struct {
	CycleType   string `json:"cycle_type"`
	NextResetAt *int64 `json:"next_reset_at"`
}

func trafficCycleView(net *store.NodeNetwork, now time.Time) trafficCycleResp {
	return trafficCycleResp{
		CycleType:   net.CycleType,
		NextResetAt: quota.NextReset(net.CycleType, net.NextResetAt, net.TZ, now),
	}
}

type billingReq struct {
	CycleType string `json:"cycle_type"`
	CycleDays *int64 `json:"cycle_days"`
	NextDueAt *int64 `json:"next_due_at"`
	Note      string `json:"note"`
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	var req updateNodeReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Name != nil {
		_ = s.Store.RenameNode(id, *req.Name)
	}
	if req.Note != nil {
		_ = s.Store.SetNodeNote(id, *req.Note)
	}
	networkChanged := req.Network != nil || req.TrafficCycle != nil
	if req.Network != nil || req.TrafficCycle != nil {
		net, err := s.Store.GetNodeNetwork(id)
		if errors.Is(err, store.ErrNotFound) {
			net = &store.NodeNetwork{NodeID: id, Mode: quota.ModeBoth, CycleType: "none"}
		} else if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		if req.Network != nil {
			net.Iface = strings.TrimSpace(req.Network.Iface)
			mode := req.Network.Mode
			switch mode {
			case quota.ModeIn, quota.ModeOut, quota.ModeBoth, quota.ModeMax:
			default:
				mode = quota.ModeBoth
			}
			net.Mode = mode
			net.QuotaBytes = req.Network.QuotaBytes
			if net.Iface != "" {
				interfaces, err := s.Store.ListNodeInterfaces(id)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, "internal")
					return
				}
				if len(interfaces) > 0 && !containsInterface(interfaces, net.Iface) {
					writeErr(w, http.StatusBadRequest, "interface_not_reported")
					return
				}
			}
		}
		if req.TrafficCycle != nil {
			switch req.TrafficCycle.CycleType {
			case "none":
				net.CycleType = "none"
				net.NextResetAt = nil
			case "month", "year":
				if req.TrafficCycle.NextResetAt == nil || *req.TrafficCycle.NextResetAt <= 0 {
					writeErr(w, http.StatusBadRequest, "next_reset_required")
					return
				}
				net.CycleType = req.TrafficCycle.CycleType
				net.NextResetAt = req.TrafficCycle.NextResetAt
			default:
				writeErr(w, http.StatusBadRequest, "invalid_cycle_type")
				return
			}
		}
		if err := s.Store.UpsertNodeNetwork(net); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if req.Billing != nil {
		_ = s.Store.UpsertNodeBilling(&store.NodeBilling{
			NodeID: id, CycleType: req.Billing.CycleType,
			CycleDays: req.Billing.CycleDays, NextDueAt: req.Billing.NextDueAt, Note: req.Billing.Note,
		})
	}
	if req.LatencyTargetIDs != nil {
		if err := s.Store.SetNodeLatencyTargets(id, *req.LatencyTargetIDs); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if networkChanged {
		s.Hub.PushDesired(id)
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", NodeID: id, Action: "node_updated", SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("node_updated", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteNode(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", NodeID: id, Action: "node_deleted", SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("node_deleted", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- metric / traffic / latency queries ---

func (s *Server) handleNodeMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from := parseInt64Default(r.URL.Query().Get("from"), nowUnix()-7*86400)
	samples, err := s.Store.ListMetrics(id, from)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": samples})
}

func (s *Server) handleNodeTraffic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	days := parseInt64Default(r.URL.Query().Get("days"), 31)
	fromDate := time.Unix(nowUnix()-days*86400, 0).UTC().Format("2006-01-02")
	daily, err := s.Store.ListTrafficDaily(id, fromDate)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var rx, tx int64
	if net, err := s.Store.GetNodeNetwork(id); err == nil {
		start := quota.PeriodStart(net.CycleType, net.NextResetAt, net.TZ, time.Now())
		rx, tx, _ = s.Store.SumTrafficSince(id, quota.LocalDate(start, net.TZ))
	} else {
		// No cycle configured yet: the window the chart shows is the only
		// "period" that exists, and reporting 0 B there was pure noise.
		rx, tx, _ = s.Store.SumTrafficSince(id, fromDate)
	}
	writeJSON(w, http.StatusOK, map[string]any{"daily": daily, "period_rx": rx, "period_tx": tx})
}

func containsInterface(interfaces []store.NodeInterface, want string) bool {
	for _, iface := range interfaces {
		if iface.Name == want {
			return true
		}
	}
	return false
}

// handleNodeLatency serves the node's latency history. `target=all` (the
// detail-page chart) returns every target enabled on the node, averaged into
// `bucket`-second buckets (design §13: 7d views are downsampled server-side,
// else 5s points × N targets flood the payload). `target=<id>` keeps
// returning raw points for a single target.
func (s *Server) handleNodeLatency(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from := parseInt64Default(r.URL.Query().Get("from"), nowUnix()-7*86400)
	var (
		samples []store.LatencySampleRow
		err     error
	)
	switch target := r.URL.Query().Get("target"); target {
	case "", "all":
		bucket := parseInt64Default(r.URL.Query().Get("bucket"), 1800)
		if bucket <= 0 {
			bucket = 1800
		}
		samples, err = s.Store.ListLatencyBucketed(id, from, bucket)
	default:
		targetID := parseInt64Default(target, 0)
		if targetID == 0 {
			writeErr(w, http.StatusBadRequest, "target_required")
			return
		}
		samples, err = s.Store.ListLatency(id, targetID, from)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]map[string]any, 0, len(samples))
	for _, sm := range samples {
		out = append(out, map[string]any{
			"target_id": sm.TargetID, "ts": sm.TS,
			"icmp_ms": sm.ICMPMs, "tcp_ms": sm.TCPMs, "loss": sm.Loss,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": out})
}

// --- commands ---

func (s *Server) handleListCommands(w http.ResponseWriter, r *http.Request) {
	cmds, err := s.Store.ListCommands(r.PathValue("id"), 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	type commandView struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		Payload    string `json:"payload"`
		Status     string `json:"status"`
		CreatedAt  int64  `json:"created_at"`
		SentAt     *int64 `json:"sent_at,omitempty"`
		FinishedAt *int64 `json:"finished_at,omitempty"`
		Result     string `json:"result"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	out := make([]commandView, 0, len(cmds))
	for _, c := range cmds {
		v := commandView{
			ID: c.ID, Kind: c.Kind, Payload: c.Payload, Status: c.Status,
			CreatedAt: c.CreatedAt, Result: c.Result, TTLSeconds: c.TTLSeconds,
		}
		if c.SentAt.Valid {
			v.SentAt = &c.SentAt.Int64
		}
		if c.FinishedAt.Valid {
			v.FinishedAt = &c.FinishedAt.Int64
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": out})
}

type enqueueReq struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Risky   bool            `json:"risky"`
	Reason  string          `json:"reason"`
}

type commandEnqueue struct {
	NodeID  string
	Kind    string
	Payload string
	TTL     int64
	Audit   store.AuditEntry
}

func (s *Server) enqueueCommand(req commandEnqueue) (string, error) {
	if req.TTL <= 0 {
		req.TTL = 600
	}
	if req.Audit.Actor == "" {
		req.Audit.Actor = "panel"
	}
	cmdID, err := security.RandomToken(8)
	if err != nil {
		return "", err
	}
	if err := s.Store.CreateCommandWithMeta(cmdID, req.NodeID, req.Kind, req.Payload, req.TTL, store.CommandMeta{
		Actor:       req.Audit.Actor,
		AISessionID: req.Audit.AISessionID,
		Reason:      req.Audit.Reason,
		Risk:        req.Audit.Risk,
	}); err != nil {
		return "", err
	}
	req.Audit.NodeID = req.NodeID
	req.Audit.Action = "cmd:" + req.Kind
	req.Audit.Command = req.Payload
	if err := s.Store.InsertAudit(&req.Audit); err != nil {
		return "", err
	}
	s.Hub.NotifyCommand(req.NodeID)
	return cmdID, nil
}

// handleEnqueueCommand queues a command for the node (panel actions now;
// the AI path reuses this with actor=ai, design §12).
func (s *Server) handleEnqueueCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	var req enqueueReq
	if err := decodeJSON(r, &req); err != nil || req.Kind == "" {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	payload := string(req.Payload)
	if payload == "" {
		payload = "{}"
	}
	cmdID, err := s.enqueueCommand(commandEnqueue{
		NodeID:  id,
		Kind:    req.Kind,
		Payload: payload,
		Audit: store.AuditEntry{
			Actor:    "panel",
			Risk:     riskLabel(req.Risky),
			Reason:   req.Reason,
			SourceIP: s.Trust.RealIP(r),
		},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": cmdID})
}

func riskLabel(risky bool) string {
	if risky {
		return "risky"
	}
	return "normal"
}

// --- primary IP pinning (design §14 手动主 IP) ---

type primaryIPReq struct {
	IP string `json:"ip"`
}

// handleSetPrimaryIP pins a manually chosen primary address. The IP must be
// one the node currently reports; store.SetManualPrimary flags the row so
// ReplaceNodeIPs restores it after every full agent report.
func (s *Server) handleSetPrimaryIP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req primaryIPReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	ip := strings.TrimSpace(req.IP)
	ips, err := s.Store.ListNodeIPs(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	reported := false
	for _, row := range ips {
		if row.IP == ip {
			reported = true
			break
		}
	}
	if !reported {
		writeErr(w, http.StatusBadRequest, "ip_not_reported")
		return
	}
	if err := s.Store.SetManualPrimary(id, ip); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", NodeID: id, Action: "primary_ip_set", Command: ip, SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("node_updated", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- 5s high-frequency probe stream (design §16) ---

type probeReq struct {
	Enabled *bool `json:"enabled"`
}

// handleNodeProbe toggles the temporary 5s metrics stream while a detail page
// is open: enabled on mount, disabled on leave (both fire-and-forget from the
// browser). Offline agents are a 409, not a queue — nothing is persisted.
func (s *Server) handleNodeProbe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req probeReq
	if err := decodeJSON(r, &req); err != nil || req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !s.Hub.IsOnline(id) {
		writeErr(w, http.StatusConflict, "node_offline")
		return
	}
	s.Hub.Send(id, protocol.NewEnvelope(protocol.TypeProbeMetr, "", protocol.ProbeMetrics{Enabled: *req.Enabled}))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func parseInt64Default(s string, def int64) int64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}

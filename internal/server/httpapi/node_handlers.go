package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/quota"
	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

// nodeView is the list/card shape the frontend renders (design §16).
type nodeView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Online      bool   `json:"online"`
	Note        string `json:"note"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Hostname    string `json:"hostname"`
	CPUCores    int    `json:"cpu_cores"`
	PrimaryIP   string `json:"primary_ip"`
	CountryCode string `json:"country_code"`
	// CountryManual marks a §14 operator-pinned flag (edit-page field); the
	// UI uses it to say "手动指定" instead of implying geoip said so.
	CountryManual bool `json:"country_manual"`
	// SubName is the §10.2 name clients see ('' = fall back to Name). Separate
	// from Name because "what the panel calls this probe" and "what the client
	// list shows" are different decisions.
	SubName      string `json:"sub_name"`
	AgentVersion string `json:"agent_version"`
	TZ           string `json:"tz"`
	// Linux distribution the agent detected from /etc/os-release (§16 基本信息).
	DistroID      string  `json:"distro_id"`
	DistroVersion string  `json:"distro_version"`
	LastSeen      *int64  `json:"last_seen,omitempty"`
	CPU           float64 `json:"cpu"`
	MemUsed       int64   `json:"mem_used"`
	MemTotal      int64   `json:"mem_total"`
	DiskUsed      int64   `json:"disk_used"`
	DiskTotal     int64   `json:"disk_total"`
	NetRxRate     float64 `json:"net_rx_rate"`
	NetTxRate     float64 `json:"net_tx_rate"`
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
	// BillingNote is the operator's free-text 费用 label (node_billing.note).
	// It rides in the list response so the overview card can tag it without a
	// per-node detail fetch.
	BillingNote string `json:"billing_note,omitempty"`

	// Agent self-update state (design §5.5): what the panel needs to answer
	// "why did this probe not follow the server?".
	AgentTargetVersion   string `json:"agent_target_version"`
	AgentUpdateState     string `json:"agent_update_state"`
	AgentUpdateAttempts  int    `json:"agent_update_attempts"`
	AgentUpdateError     string `json:"agent_update_error,omitempty"`
	AgentUpdatePlannedAt *int64 `json:"agent_update_planned_at,omitempty"`
	AgentUpdateDoneAt    *int64 `json:"agent_update_done_at,omitempty"`
	// AgentUpdateAfter is when the pending plan actually becomes actionable:
	// AgentUpdatePlannedAt (the stagger anchor) plus this node's deterministic
	// offset. This is the "计划时刻" the panel shows; the anchor is written here
	// for diagnostics only (it is the same second for every node after a server
	// restart, so displaying it looks like a late start — see agentupdate
	// .PlannedStart).
	AgentUpdateAfter *int64 `json:"agent_update_after,omitempty"`
	// AgentSelfUpdate is the capability bit an agent built before §5.5 never
	// sends; false is how the panel recognises "needs a manual reinstall".
	AgentSelfUpdate bool `json:"agent_self_update"`
	// AgentCapsSeen tells "no self-update support" apart from "never said
	// hello": a freshly registered node has no caps yet, and accusing it of
	// needing a reinstall would be wrong.
	AgentCapsSeen bool `json:"agent_caps_seen"`
	// SingboxReady: this node would actually render into a subscription (§10
	// predicate: primary IP + inbound port + reported certificate). The
	// subscription page's node picker lists only these, so selecting a node
	// always has an effect on the output.
	SingboxReady bool `json:"singbox_ready"`
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
		CountryManual: n.CountryManual,
		SubName:       n.SubName,
		AgentVersion:  n.AgentVersion,
		TZ:            n.TZ,
		DistroID:      n.DistroID, DistroVersion: n.DistroVersion,
		PeriodPct: -1,

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
	// §5.5: show the moment the probe will actually start, not the anchor it was
	// planned from. s.AgentUpdate is optional (dev builds without the volume),
	// and a nil manager simply has no plan to report.
	if s.AgentUpdate != nil {
		if after := s.AgentUpdate.PlannedStart(n); after > 0 {
			v.AgentUpdateAfter = &after
		}
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

	// §10: the same predicate the subscription renderer applies, so the picker
	// offers exactly the nodes that can appear in the output.
	if sb, err := s.Store.GetNodeSingbox(n.ID); err == nil {
		v.SingboxReady = s.nodeRenderable(n, sb)
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
		v.BillingNote = b.Note
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
			"desired_uninstall": sb.DesiredUninstall,
			"status":            sb.Status, "last_error": sb.LastError, "port": sb.Port,
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
	Name *string `json:"name,omitempty"`
	Note *string `json:"note,omitempty"`
	// SubName is the §10.2 subscription display name ('' clears it back to the
	// node name). Pointer = absent vs explicit, like every other field here.
	SubName *string `json:"sub_name,omitempty"`
	// CountryCode (§14 手动国旗): "XX" pins the flag; "" clears the pin and
	// re-derives from the current primary IP. Pointer = absent vs explicit.
	CountryCode      *string          `json:"country_code,omitempty"`
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
	if req.SubName != nil {
		sub := strings.TrimSpace(*req.SubName)
		if !store.ValidAlias(sub) {
			writeErr(w, http.StatusBadRequest, "bad_alias")
			return
		}
		if err := s.Store.SetNodeSubName(id, sub); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if req.CountryCode != nil {
		cc := strings.ToUpper(strings.TrimSpace(*req.CountryCode))
		if cc != "" && !isValidCountryCode(cc) {
			writeErr(w, http.StatusBadRequest, "invalid_country_code")
			return
		}
		if err := s.setNodeCountry(id, cc); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
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

// --- §14 手动国旗: pin or reset the flag shown for a node ---

func isValidCountryCode(cc string) bool {
	if len(cc) != 2 {
		return false
	}
	return cc[0] >= 'A' && cc[0] <= 'Z' && cc[1] >= 'A' && cc[1] <= 'Z'
}

// setNodeCountry applies one PATCH country_code value. Non-empty pins the
// code (country_manual=1, hub stops re-deriving); empty clears the pin and
// re-derives right away so "回到自动" takes effect without waiting for the next
// state report. A resolution miss keeps the stored code.
func (s *Server) setNodeCountry(id, cc string) error {
	if cc != "" {
		return s.Store.SetNodeCountry(id, cc, true)
	}
	n, err := s.Store.GetNode(id)
	if err != nil {
		return err
	}
	// Unpin first, keeping the current value: the pin is a property of the row,
	// and the hub refuses to touch a pinned one.
	if err := s.Store.SetNodeCountry(id, n.CountryCode, false); err != nil {
		return err
	}
	s.refreshCountry(id)
	return nil
}

// refreshCountry re-derives the §14 flag through the hub, which owns that rule
// (primary address first, then the node's public addresses).
func (s *Server) refreshCountry(id string) {
	if s.Hub != nil {
		s.Hub.RefreshCountry(id)
	}
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

type commandEnqueue struct {
	NodeID  string
	Kind    string
	Payload string
	TTL     int64
	Audit   store.AuditEntry
}

// enqueueCommand queues a node command and writes the audit line. The panel's
// arbitrary-shell entry point (POST /api/nodes/{id}/commands) was removed with
// the detail-page commands card; the remaining callers are panel actions
// (sing-box restart) and the AI execution path (design §12), which set their
// own actor/risk on the audit entry.
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
	// §14: the flag follows the primary address, so re-point it now instead of
	// leaving the old country on screen until the next state report.
	s.refreshCountry(id)
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

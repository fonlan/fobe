// Package agentupdate owns the server half of agent self-update (design §5.5).
//
// The contract is deliberately one-sided: the server states which agent build
// it wants ("the version this server was built as"), the agent compares that
// with its own build and replaces itself. Nothing here may become a second
// installation path — this package only decides *whether* a target is offered,
// *when* a node may start (the §5.5 stagger), what the agents reported, and
// when a non-converging node deserves an alert.
//
// Two properties are load-bearing:
//
//   - The target is only offered when the artifact for it is actually on the
//     artifact volume. Handing out a version nobody can download turns every
//     probe into a 404 retry loop.
//   - The stagger deadline is derived from a per-(node, target) anchor stored
//     in the nodes table, not from "now". A server restart reconnects every
//     agent in the same instant; without a stable offset they would all fetch
//     the same artifact simultaneously.
package agentupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// SettingAutoUpdate is the panel switch (design §5.5 开关). Absent means on:
// the default is "follow the server", and turning it off is the deliberate act.
const SettingAutoUpdate = "agent.auto_update"

// Alert kinds (design §15).
const (
	// AlertFailed: one node hit a terminal failure (sha256 mismatch, cannot
	// exec, version mismatch) — it will not retry by itself.
	AlertFailed = "agent_update_failed"
	// AlertTransient: repeated environment failures (unreachable, 404, disk).
	AlertTransient = "agent_update_transient"
	// AlertStale: aggregate — nodes that still report another version after the
	// convergence window (same 15 min / 1 h dedupe shape as §9.5).
	AlertStale = "agent_update_stale"
)

// Node states mirrored into nodes.agent_update_state for the panel.
const (
	StatePlanned     = "planned"
	StateDownloading = "downloading"
	StateVerifying   = "verifying"
	StateCommitted   = "committed"
	StateFailed      = "failed"
	StateTransient   = "transient"
	StateSuppressed  = "suppressed"
	StateUnsupported = "unsupported"
)

const (
	// DefaultStagger spreads the post-restart stampede over five minutes.
	DefaultStagger = 5 * time.Minute
	// DefaultConvergeAfter mirrors §9.5's 15-minute convergence window.
	DefaultConvergeAfter = 15 * time.Minute
	// DefaultSweepEvery is how often the convergence sweep runs.
	DefaultSweepEvery = time.Minute
	// transientAlertAfter is the consecutive-failure count that raises the
	// transient alert (§5.5: 连续 3 次).
	transientAlertAfter = 3
	// staleAlertDedupeSecs keeps the aggregate from being delivered twice
	// inside an hour, matching §9.5.
	staleAlertDedupeSecs = 3600
	// nodeAlertDedupeSecs is the per-node dedupe for the terminal/transient
	// kinds.
	nodeAlertDedupeSecs = 3600
)

// OffReason explains why no target is offered (the panel shows it verbatim as
// an error code).
type OffReason string

const (
	ReasonOffSwitchOff   OffReason = "switch_off"
	ReasonOffKillSwitch  OffReason = "kill_switch"
	ReasonOffNoDLDir     OffReason = "no_dl_dir"
	ReasonOffNotReleased OffReason = "not_released"
	ReasonOffNoArtifact  OffReason = "artifact_missing"
)

// Status is the cluster-wide view the settings page renders.
type Status struct {
	// Enabled is true when a target would be offered right now.
	Enabled bool `json:"enabled"`
	// Reason is set when Enabled is false.
	Reason string `json:"reason,omitempty"`
	// ServerVersion is what this server was built as (the target).
	ServerVersion string `json:"server_version"`
	// ArtifactPresent reports whether the agent build for ServerVersion is on
	// the artifact volume.
	ArtifactPresent bool `json:"artifact_present"`
	// StaggerSeconds is the spread applied to the per-node deadline.
	StaggerSeconds int `json:"stagger_seconds"`
	// NodesTotal / NodesBehind count nodes whose reported version differs.
	NodesTotal  int `json:"nodes_total"`
	NodesBehind int `json:"nodes_behind"`
}

// Config wires a Manager.
type Config struct {
	Store *store.Store
	Log   *slog.Logger
	// ServerVersion is main.version: the build this server is (design §5.5 判据).
	ServerVersion string
	// DLDir is FOBE_DL_DIR — where <dl>/agent/<version>/ artifacts live.
	DLDir string
	// KillSwitch reports ai.kill_switch (design §12.3: freeze wins over follow).
	KillSwitch func() bool
	// Enabled reports the panel switch; nil means "always on".
	Enabled func() bool
	// Now overrides the clock in tests; nil = time.Now().Unix().
	Now func() int64
	// Stagger / ConvergeAfter / SweepEvery default to the constants above.
	Stagger       time.Duration
	ConvergeAfter time.Duration
	SweepEvery    time.Duration
}

// Manager implements the server-side decisions. All methods are safe for
// concurrent use; state lives in SQLite and the artifact volume.
type Manager struct {
	cfg Config
}

// New builds a Manager, filling in the documented defaults.
func New(cfg Config) *Manager {
	if cfg.Stagger <= 0 {
		cfg.Stagger = DefaultStagger
	}
	if cfg.ConvergeAfter <= 0 {
		cfg.ConvergeAfter = DefaultConvergeAfter
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = DefaultSweepEvery
	}
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().Unix() }
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Manager{cfg: cfg}
}

func (m *Manager) now() int64 { return m.cfg.Now() }

// KillSwitchOn reports the current freeze state.
func (m *Manager) KillSwitchOn() bool {
	return m.cfg.KillSwitch != nil && m.cfg.KillSwitch()
}

// IsReleaseVersion decides whether a build identifier may serve as a target.
//
// dev and compose are build shapes without a release meaning: a dev server has
// no artifact to hand out and "compose" is what an unversioned local image
// build calls itself, so promising to follow it would promise nothing. Any
// other identifier that contains a digit is accepted (20260915.054229, v1.2.3).
func IsReleaseVersion(v string) bool {
	v = strings.TrimSpace(v)
	switch v {
	case "", "dev", "compose", "latest", "unknown":
		return false
	}
	for _, r := range v {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// ArtifactPresent reports whether the published agent build is downloadable
// from the artifact volume (<dl>/agent/<version>/{linux-amd64,.sha256}).
func ArtifactPresent(dlDir, version string) bool {
	if dlDir == "" || version == "" {
		return false
	}
	for _, name := range artifactFiles {
		if !fileNonEmpty(artifactPath(dlDir, version, name)) {
			return false
		}
	}
	return true
}

// status computes why a target is or is not offered right now.
func (m *Manager) status() (ok bool, reason OffReason) {
	switch {
	case m.cfg.Enabled != nil && !m.cfg.Enabled():
		return false, ReasonOffSwitchOff
	case m.KillSwitchOn():
		return false, ReasonOffKillSwitch
	case m.cfg.DLDir == "":
		return false, ReasonOffNoDLDir
	case !IsReleaseVersion(m.cfg.ServerVersion):
		return false, ReasonOffNotReleased
	case !ArtifactPresent(m.cfg.DLDir, m.cfg.ServerVersion):
		return false, ReasonOffNoArtifact
	}
	return true, ""
}

// Target is the §5.5 decision for one node: which build it should run and the
// earliest unix second at which it may start. ok is false when the node is
// already on the target, or when no target is offered at all.
//
// Called on every handshake, so it must stay cheap: one node read plus (only on
// a target change) one write.
func (m *Manager) Target(nodeID string) (version string, after int64, ok bool) {
	if ok, _ := m.status(); !ok {
		return "", 0, false
	}
	n, err := m.cfg.Store.GetNode(nodeID)
	if err != nil {
		return "", 0, false
	}
	target := m.cfg.ServerVersion

	if n.AgentVersion == target {
		// Converged. Close a plan left over from before the agent restarted
		// onto the new build so the panel stops saying "planned".
		if n.AgentUpdateState != StateCommitted || n.AgentUpdatePlannedAt != 0 {
			_ = m.cfg.Store.RecordAgentUpdate(nodeID, target, StateCommitted, n.AgentUpdateAttempts, "", m.now())
			_ = m.cfg.Store.SetAgentUpdatePlanned(nodeID, target, 0)
		}
		return "", 0, false
	}

	// A plan is anchored the first time this (node, target) pair is seen and
	// then stays put: recomputing "now + offset" per handshake would defeat the
	// stagger whenever the agent reconnects.
	anchor := n.AgentUpdatePlannedAt
	if n.AgentTargetVersion != target || anchor == 0 {
		anchor = m.now()
		if err := m.cfg.Store.SetAgentUpdatePlanned(nodeID, target, anchor); err != nil {
			m.cfg.Log.Warn("agentupdate: record plan", "node", nodeID, "err", err)
		}
	}
	after = anchor + int64(m.staggerOffset(nodeID, target)/time.Second)
	if after < 0 {
		after = 0
	}
	return target, after, true
}

// staggerOffset is the deterministic per-(node, target) spread inside the
// stagger window. Deterministic on purpose: the same node keeps the same slot
// across reconnects, so the panel's "planned at" does not jump around.
func (m *Manager) staggerOffset(nodeID, target string) time.Duration {
	span := int64(m.cfg.Stagger / time.Second)
	if span <= 0 {
		return 0
	}
	h := fnv1a(nodeID + "\x00" + target)
	return time.Duration(h%uint64(span)) * time.Second
}

func fnv1a(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// Desired fills the §5.5 fields of a desired state / hello_ack. Both carriers
// are always written by the same call so they cannot disagree.
func (m *Manager) Desired(nodeID string, d *protocol.DesiredState) (flat string, after int64) {
	version, when, ok := m.Target(nodeID)
	if !ok {
		return "", 0
	}
	d.AgentTargetVersion = version
	d.AgentUpdateAfter = when
	return version, when
}

// OnReport records what an agent said about one attempt. Repeats are cheap and
// harmless: the audit line and the alert only fire when the state actually
// changed, because an agent that is merely reconnecting re-sends its last
// verdict.
func (m *Manager) OnReport(nodeID string, r *protocol.AgentUpdate) {
	if r.Target == "" || r.Phase == "" {
		return
	}
	n, err := m.cfg.Store.GetNode(nodeID)
	if err != nil {
		return
	}

	state := r.Phase
	if r.Phase == protocol.UpdateFailed && r.Class == protocol.ClassTransient {
		state = StateTransient
	}
	doneAt := int64(0)
	if state == StateCommitted {
		doneAt = m.now()
	}

	changed := state != n.AgentUpdateState || r.Attempts != n.AgentUpdateAttempts || r.Error != n.AgentUpdateError
	if err := m.cfg.Store.RecordAgentUpdate(nodeID, r.Target, state, r.Attempts, truncate(r.Error, 500), doneAt); err != nil {
		m.cfg.Log.Warn("agentupdate: record report", "node", nodeID, "err", err)
		return
	}
	if !changed {
		return
	}

	m.cfg.Log.Info("agent self-update",
		"node", nodeID, "target", r.Target, "phase", state, "class", r.Class, "attempts", r.Attempts, "err", r.Error)

	// Audit every distinct transition (actor=system: nobody clicked anything).
	_ = m.cfg.Store.InsertAudit(&store.AuditEntry{
		Actor:   "system",
		NodeID:  nodeID,
		Action:  "agent_update_" + state,
		Command: fmt.Sprintf("target=%s attempts=%d", r.Target, r.Attempts),
		Reason:  truncate(r.Error, 300),
		Risk:    alertRisk(state),
	})

	switch state {
	case StateCommitted:
		_ = m.cfg.Store.RecoverAlert(AlertFailed, nodeID)
		_ = m.cfg.Store.RecoverAlert(AlertTransient, nodeID)
	case StateFailed:
		m.raiseNodeAlert(AlertFailed, nodeID, "terminal failure", r)
	case StateSuppressed:
		m.raiseNodeAlert(AlertFailed, nodeID, "gave up after repeated failures", r)
	case StateUnsupported:
		m.raiseNodeAlert(AlertFailed, nodeID, "no supervisor to restart the agent", r)
	case StateTransient:
		if r.Attempts >= transientAlertAfter {
			m.raiseNodeAlert(AlertTransient, nodeID, "repeated environment failures", r)
		}
	}
}

func alertRisk(state string) string {
	switch state {
	case StateFailed, StateSuppressed, StateUnsupported:
		return "high"
	case StateTransient:
		return "medium"
	default:
		return "low"
	}
}

func (m *Manager) raiseNodeAlert(kind, nodeID, why string, r *protocol.AgentUpdate) {
	payload, _ := json.Marshal(map[string]any{
		"target":   r.Target,
		"phase":    r.Phase,
		"class":    r.Class,
		"attempts": r.Attempts,
		"reason":   truncate(r.Error, 500),
		"why":      why,
	})
	if _, _, err := m.cfg.Store.CreateAlert(kind, nodeID, string(payload), nodeAlertDedupeSecs); err != nil {
		m.cfg.Log.Warn("agentupdate: create alert", "kind", kind, "node", nodeID, "err", err)
	}
}

// Retry is the operator escape hatch: forget the attempt counters so the next
// handshake starts a fresh budget. Returns the node's target so the caller can
// nudge an online agent immediately.
func (m *Manager) Retry(nodeID string) error {
	if err := m.cfg.Store.ClearAgentUpdate(nodeID); err != nil {
		return err
	}
	return nil
}

// Status is the cluster-wide view for the settings page.
func (m *Manager) Status() Status {
	ok, reason := m.status()
	st := Status{
		Enabled:         ok,
		ServerVersion:   m.cfg.ServerVersion,
		ArtifactPresent: ArtifactPresent(m.cfg.DLDir, m.cfg.ServerVersion),
		StaggerSeconds:  int(m.cfg.Stagger / time.Second),
	}
	if !ok {
		st.Reason = string(reason)
	}
	nodes, err := m.cfg.Store.ListNodes()
	if err != nil {
		return st
	}
	st.NodesTotal = len(nodes)
	for i := range nodes {
		if nodes[i].AgentVersion != m.cfg.ServerVersion {
			st.NodesBehind++
		}
	}
	return st
}

// Start runs the convergence sweep until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(m.cfg.SweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.sweep()
			}
		}
	}()
}

// sweep raises the single aggregated alert for nodes that were told to move and
// still have not (design §5.5: 分发后 15 分钟仍未收敛 → 汇总一条).
func (m *Manager) sweep() {
	if m.cfg.Store == nil {
		return
	}
	deadline := m.now() - int64(m.cfg.ConvergeAfter/time.Second)
	nodes, err := m.cfg.Store.StaleAgentUpdates(deadline)
	if err != nil {
		m.cfg.Log.Warn("agentupdate: stale sweep", "err", err)
		return
	}
	// Terminal states are excluded by the query, but a node can also be stale
	// because the whole feature is off (kill switch / dev version): saying
	// "not converged" then is noise, not signal.
	if ok, _ := m.status(); !ok {
		return
	}
	if len(nodes) == 0 {
		_ = m.cfg.Store.RecoverAlert(AlertStale, "")
		return
	}
	type staleView struct {
		NodeID  string `json:"node_id"`
		Name    string `json:"name,omitempty"`
		Version string `json:"version,omitempty"`
		Target  string `json:"target,omitempty"`
		State   string `json:"state,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	views := make([]staleView, 0, len(nodes))
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		views = append(views, staleView{
			NodeID: nodes[i].ID, Name: nodes[i].Name, Version: nodes[i].AgentVersion,
			Target: nodes[i].AgentTargetVersion, State: nodes[i].AgentUpdateState,
			Error: nodes[i].AgentUpdateError,
		})
		names = append(names, nodes[i].Name)
	}
	sort.Strings(names)
	payload, err := json.Marshal(map[string]any{
		"target_version": m.cfg.ServerVersion,
		"checked_at":     m.now(),
		"count":          len(views),
		"nodes":          views,
	})
	if err != nil {
		return
	}
	if _, _, err := m.cfg.Store.CreateAlert(AlertStale, "", string(payload), staleAlertDedupeSecs); err != nil {
		m.cfg.Log.Warn("agentupdate: create stale alert", "err", err)
		return
	}
	m.cfg.Log.Warn("agent self-update not converged",
		"target", m.cfg.ServerVersion, "count", len(views), "nodes", strings.Join(names, ","))
	_ = m.cfg.Store.InsertAudit(&store.AuditEntry{
		Actor:   "system",
		Action:  "agent_update_stale",
		Command: fmt.Sprintf("%s not_converged=%d nodes=%s", m.cfg.ServerVersion, len(views), strings.Join(names, ",")),
		Risk:    "medium",
	})
}

// AutoUpdateEnabled reads the panel switch. Absent = on (§5.5 default).
func AutoUpdateEnabled(st *store.Store) bool {
	v, err := st.GetSetting(SettingAutoUpdate)
	if err != nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// SetAutoUpdate writes the panel switch.
func SetAutoUpdate(st *store.Store, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return st.SetSetting(SettingAutoUpdate, v, false)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

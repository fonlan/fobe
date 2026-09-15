// Package singboxupdate distributes one cached sing-box release to every node
// that already runs sing-box (design §9.2 「一键批量更新」).
//
// The flow stays declarative: the panel states the target version, the manager
// writes it into every node's desired_version (keeping the node's existing port
// and config), and pushes the desired frame to online agents. Offline agents
// converge from hello_ack when they reconnect — nothing is replayed. The job
// lives in settings (singbox.last_update), so a page refresh — or a server
// restart — shows the same progress and the same per-node outcome.
//
// Fifteen minutes after a distribution the manager re-reads the agents'
// reported versions and, when some nodes have not converged, writes one
// aggregated alert (kind singbox_update_stale). The existing
// scheduler→notify path picks that alert up and delivers it to
// Telegram/Webhook, so this package never talks to a notifier itself.
package singboxupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// SettingLastUpdate holds the JSON-encoded Job below. It is server-owned state:
// the panel reads it, and PUT /api/settings never exposes it.
const SettingLastUpdate = "singbox.last_update"

// DefaultConvergeAfter is the post-distribution convergence window (design
// §9.2 一键批量更新: 15 分钟后复查未收敛的节点).
const DefaultConvergeAfter = 15 * time.Minute

// AlertSingboxUpdateStale is the §15 alert kind raised when nodes did not
// converge onto the distributed version in time.
const AlertSingboxUpdateStale = "singbox_update_stale"

// staleAlertDedupeSecs is the second line of defence behind Job.StaleAlerted:
// the same aggregate is never delivered twice inside the window.
const staleAlertDedupeSecs = 3600

// Job states. pending/downloading/pushing are non-terminal.
const (
	StatePending     = "pending"
	StateDownloading = "downloading"
	StatePushing     = "pushing"
	StateDone        = "done"
	StateFailed      = "failed"
)

// Per-node outcomes (the buckets the panel renders).
const (
	// OutcomeAlreadyCurrent: the node already reports the target version and
	// already desires it — nothing was written and nothing was pushed.
	OutcomeAlreadyCurrent = "already_current"
	// OutcomePushed: desired_version was written and the desired frame went out.
	OutcomePushed = "pushed"
	// OutcomeOfflinePending: desired_version was written; the agent converges
	// on reconnect (§7 declarative desired state).
	OutcomeOfflinePending = "offline_pending"
	// OutcomeFailed: the node could not be updated; Reason explains why.
	OutcomeFailed = "failed"
)

// ErrInProgress is returned when an update job is already running.
var ErrInProgress = errors.New("singboxupdate: an update is already running")

// InUseError reports that a cached version is still referenced by nodes, so
// deleting it needs an explicit force.
type InUseError struct {
	Version string
	Refs    int
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("singboxupdate: version %s is still referenced by %d node(s)", e.Version, e.Refs)
}

// Online reports whether an agent is connected right now. *hub.Hub satisfies it.
type Online interface {
	IsOnline(nodeID string) bool
}

// Settings is the subset of *store.Store the manager persists through.
type Settings interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string, encrypted bool) error
}

// Counts is the per-bucket tally of a job.
type Counts struct {
	Total          int `json:"total"`
	AlreadyCurrent int `json:"already_current"`
	Pushed         int `json:"pushed"`
	OfflinePending int `json:"offline_pending"`
	Failed         int `json:"failed"`
}

// NodeResult is one node's place in a job.
type NodeResult struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name,omitempty"`
	// Online is the agent's connectivity at push time.
	Online bool `json:"online"`
	// Outcome is one of the Outcome* constants.
	Outcome string `json:"outcome"`
	// Reason explains the outcome (always set for failed/offline_pending).
	Reason string `json:"reason,omitempty"`
	// Version is the version the agent reported when the batch ran.
	Version string `json:"version,omitempty"`
	// DesiredVersion is what the node now desires (the target version).
	DesiredVersion string `json:"desired_version,omitempty"`
	// Converged is nil until the 15-minute check ran.
	Converged *bool `json:"converged,omitempty"`
	// CheckedAt is when Converged was set (unix seconds).
	CheckedAt int64 `json:"checked_at,omitempty"`
}

// Job is one distribution batch. It is persisted verbatim under
// SettingLastUpdate so a refreshed page (or a restarted server) sees it.
type Job struct {
	ID    string `json:"id"`
	State string `json:"state"`
	// Requested is what the operator asked for ("latest" or a version).
	Requested string `json:"requested,omitempty"`
	// TargetVersion is the concrete version the whole batch converges to.
	TargetVersion string `json:"target_version,omitempty"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at,omitempty"`
	// Deadline is when the convergence check is due (unix seconds).
	Deadline int64 `json:"deadline,omitempty"`
	// Error carries the job-level failure (download/resolve/internal).
	Error    string       `json:"error,omitempty"`
	Actor    string       `json:"actor,omitempty"`
	SourceIP string       `json:"source_ip,omitempty"`
	Nodes    []NodeResult `json:"nodes"`
	Counts   Counts       `json:"counts"`
	// ConvergenceChecked is true once the deadline check ran.
	ConvergenceChecked bool  `json:"convergence_checked"`
	CheckedAt          int64 `json:"checked_at,omitempty"`
	// Stale lists the node ids that had not converged at Deadline.
	Stale []string `json:"stale,omitempty"`
	// StaleAlerted is true once the aggregate alert was written.
	StaleAlerted bool `json:"stale_alerted,omitempty"`

	// release is the already-resolved upstream release, when the caller has it.
	release *singboxdl.Release
}

// UpdateRequest describes one distribution.
type UpdateRequest struct {
	// Version is the concrete, canonical target version. Required.
	Version string
	// Requested is what the operator asked for ("latest" or the version).
	Requested string
	// Release is the already-resolved release for Version; nil makes the job
	// resolve it (only needed when the version is not cached yet).
	Release  *singboxdl.Release
	Actor    string
	SourceIP string
}

// ImpactNode is one affected node in the pre-flight confirmation list.
type ImpactNode struct {
	NodeID         string `json:"node_id"`
	Name           string `json:"name,omitempty"`
	Online         bool   `json:"online"`
	Status         string `json:"status,omitempty"`
	Version        string `json:"version,omitempty"`
	DesiredVersion string `json:"desired_version,omitempty"`
	AlreadyCurrent bool   `json:"already_current"`
}

// Impact is the pre-flight view behind the confirmation dialog.
type Impact struct {
	TargetVersion  string       `json:"target_version"`
	Requested      string       `json:"requested,omitempty"`
	Cached         bool         `json:"cached"`
	DownloadNeeded bool         `json:"download_needed"`
	Count          int          `json:"count"`
	Online         int          `json:"online"`
	Offline        int          `json:"offline"`
	AlreadyCurrent int          `json:"already_current"`
	Nodes          []ImpactNode `json:"nodes"`
}

// Config configures a Manager. Store and DL are required for real work; every
// callback is optional and nil-safe.
type Config struct {
	Store *store.Store
	DL    *singboxdl.Client
	// Online reports live agent connectivity; when nil the node status column
	// is used instead.
	Online Online
	// Settings receives singbox.last_update; defaults to Store.
	Settings Settings
	Log      *slog.Logger
	// Now overrides time.Now (tests).
	Now func() time.Time
	// Push sends the node's current desired state to a connected agent and
	// reports whether the frame went out (false = the agent is gone).
	Push func(nodeID string) bool
	// Publish reports a job snapshot (SSE singbox_update).
	Publish func(job Job)
	// CacheChanged reports that the artifact cache changed on disk
	// (SSE singbox_cache).
	CacheChanged func()
	// ConvergeAfter overrides DefaultConvergeAfter (tests).
	ConvergeAfter time.Duration
}

// Manager owns the batch-update job. It is safe for concurrent use; at most one
// job runs at a time.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	job     *Job
	loaded  bool
	ctx     context.Context
	armedAt map[string]bool
}

// New returns a Manager with defaults applied. It never performs I/O.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Settings == nil && cfg.Store != nil {
		cfg.Settings = cfg.Store
	}
	if cfg.ConvergeAfter <= 0 {
		cfg.ConvergeAfter = DefaultConvergeAfter
	}
	return &Manager{cfg: cfg, armedAt: map[string]bool{}}
}

// --- reads ---

// Current returns a copy of the persisted job, or nil when none ever ran. The
// first call loads the state written by an earlier process.
func (m *Manager) Current() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	return cloneJob(m.job)
}

// InProgress returns the running job, or nil.
func (m *Manager) InProgress() *Job {
	j := m.Current()
	if j == nil || isTerminal(j.State) {
		return nil
	}
	return j
}

// Impact lists every node with a desired state and how the batch would treat
// it. target may be empty (the caller could not resolve one yet).
func (m *Manager) Impact(target string) (*Impact, error) {
	if m.cfg.Store == nil {
		return &Impact{TargetVersion: target, Nodes: []ImpactNode{}}, nil
	}
	targets, err := m.cfg.Store.ListSingboxTargets()
	if err != nil {
		return nil, err
	}
	imp := &Impact{
		TargetVersion: target,
		Cached:        target != "" && m.cached(target),
		Nodes:         make([]ImpactNode, 0, len(targets)),
	}
	imp.DownloadNeeded = target != "" && !imp.Cached
	for _, t := range targets {
		online := m.isOnline(t.NodeID, t.Status)
		already := target != "" && norm(t.Version) == target && norm(t.DesiredVersion) == target
		node := ImpactNode{
			NodeID: t.NodeID, Name: t.Name, Online: online, Status: t.Status,
			Version: t.Version, DesiredVersion: t.DesiredVersion, AlreadyCurrent: already,
		}
		if online {
			imp.Online++
		} else {
			imp.Offline++
		}
		if already {
			imp.AlreadyCurrent++
		}
		imp.Nodes = append(imp.Nodes, node)
	}
	imp.Count = len(imp.Nodes)
	return imp, nil
}

// --- lifecycle ---

// Start records the run context and re-arms the convergence check of a job
// persisted by an earlier process (design §9.2: 服务端重启时按持久化的 deadline
// 重新武装). A job that was still running when the process died is marked
// failed so the panel never shows a phantom "in progress".
func (m *Manager) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	m.ctx = ctx
	m.loadLocked()
	job := cloneJob(m.job)
	interrupted := job != nil && !isTerminal(job.State)
	if interrupted {
		job.State = StateFailed
		job.Error = "interrupted by server restart"
		job.FinishedAt = m.cfg.Now().Unix()
		m.job = job
		m.persistLocked(job)
	}
	m.mu.Unlock()

	if interrupted {
		m.publish(job)
	}
	if job != nil && job.TargetVersion != "" && job.Deadline > 0 && !job.ConvergenceChecked {
		m.armTimer(job)
	}
}

// StartUpdate creates the job and runs it in the background. It returns as soon
// as the job exists; progress lands in settings and on the SSE channel. The
// request context is deliberately not used for the work: the job outlives the
// HTTP response.
func (m *Manager) StartUpdate(req UpdateRequest) (*Job, error) {
	target, err := canonical(req.Version)
	if err != nil {
		return nil, err
	}
	req.Version = target
	if req.Requested == "" {
		req.Requested = target
	}

	m.mu.Lock()
	m.loadLocked()
	if m.job != nil && !isTerminal(m.job.State) {
		j := cloneJob(m.job)
		m.mu.Unlock()
		return j, ErrInProgress
	}
	now := m.cfg.Now()
	job := &Job{
		ID:            newJobID(),
		State:         StatePending,
		Requested:     req.Requested,
		TargetVersion: target,
		StartedAt:     now.Unix(),
		Actor:         req.Actor,
		SourceIP:      req.SourceIP,
		Nodes:         []NodeResult{},
		release:       req.Release,
	}
	m.job = job
	m.persistLocked(job)
	out := cloneJob(job)
	m.mu.Unlock()

	m.publish(out)
	go m.runJob(m.baseCtx(), out.ID)
	return cloneJob(out), nil
}

// runJob performs one distribution: ensure the artifact is cached, then move
// every node with a desired state onto the target version.
func (m *Manager) runJob(ctx context.Context, jobID string) {
	m.setState(jobID, StateDownloading)

	m.mu.Lock()
	job := cloneJob(m.job)
	m.mu.Unlock()
	if job == nil || job.ID != jobID {
		return
	}
	target := job.TargetVersion

	if !m.cached(target) {
		if m.cfg.DL == nil || m.cfg.DL.DLDir() == "" {
			m.fail(jobID, "download_failed: FOBE_DL_DIR is not configured")
			return
		}
		rel := job.release
		if rel == nil {
			resolved, err := m.cfg.DL.ReleaseByVersion(ctx, target)
			if err != nil {
				m.fail(jobID, "release_unavailable: "+err.Error())
				return
			}
			rel = &resolved
		}
		if _, err := m.cfg.DL.Install(ctx, *rel, false); err != nil && !errors.Is(err, singboxdl.ErrVersionExists) {
			m.fail(jobID, "download_failed: "+err.Error())
			return
		}
		m.cacheChanged()
	}

	targets, err := m.listTargets()
	if err != nil {
		m.fail(jobID, "internal: "+err.Error())
		return
	}

	m.setState(jobID, StatePushing)
	for _, t := range targets {
		res := m.pushOne(t, target)
		m.mu.Lock()
		if m.job != nil && m.job.ID == jobID {
			m.job.Nodes = append(m.job.Nodes, res)
			m.job.Counts = countResults(m.job.Nodes)
			m.persistLocked(m.job)
		}
		snapshot := cloneJob(m.job)
		m.mu.Unlock()
		m.publish(snapshot)
	}

	m.mu.Lock()
	if m.job != nil && m.job.ID == jobID {
		now := m.cfg.Now()
		m.job.State = StateDone
		m.job.FinishedAt = now.Unix()
		m.job.Deadline = now.Add(m.cfg.ConvergeAfter).Unix()
		m.persistLocked(m.job)
	}
	done := cloneJob(m.job)
	m.mu.Unlock()
	m.publish(done)

	if done != nil && done.ID == jobID {
		m.audit(&store.AuditEntry{
			Actor: actorOf(done), Action: "singbox_update_done", SourceIP: done.SourceIP,
			Command: fmt.Sprintf("%s total=%d pushed=%d offline_pending=%d already_current=%d failed=%d",
				done.TargetVersion, done.Counts.Total, done.Counts.Pushed, done.Counts.OfflinePending,
				done.Counts.AlreadyCurrent, done.Counts.Failed),
		})
		if done.Deadline > 0 {
			m.armTimer(done)
		}
	}
}

// pushOne moves a single node onto the target version, keeping its port and its
// already-generated config (the update only moves the version).
func (m *Manager) pushOne(t store.SingboxTarget, target string) NodeResult {
	online := m.isOnline(t.NodeID, t.Status)
	res := NodeResult{
		NodeID: t.NodeID, Name: t.Name, Online: online,
		Version: t.Version, DesiredVersion: target,
	}
	if norm(t.Version) == target && norm(t.DesiredVersion) == target {
		res.Outcome = OutcomeAlreadyCurrent
		res.Reason = "already on the target version"
		return res
	}
	if m.cfg.Store == nil {
		res.Outcome = OutcomeFailed
		res.Reason = "store unavailable"
		return res
	}
	sb, err := m.cfg.Store.GetNodeSingbox(t.NodeID)
	if err != nil {
		res.Outcome = OutcomeFailed
		res.Reason = "desired state unreadable: " + err.Error()
		return res
	}
	// keep port + config: only the desired version moves
	sb.DesiredVersion = target
	if norm(t.Version) != target {
		sb.Status = "installing"
	}
	if err := m.cfg.Store.UpsertNodeSingbox(sb); err != nil {
		res.Outcome = OutcomeFailed
		res.Reason = "persist desired state: " + err.Error()
		return res
	}
	if !online {
		res.Outcome = OutcomeOfflinePending
		res.Reason = "node offline; the agent converges on reconnect"
		return res
	}
	if m.cfg.Push != nil {
		if !m.cfg.Push(t.NodeID) {
			res.Outcome = OutcomeFailed
			res.Reason = "the agent disconnected before the desired frame was sent"
			return res
		}
	}
	res.Outcome = OutcomePushed
	return res
}

// --- convergence check (design §9.2: 分发后 15 分钟复查) ---

// CheckConvergence re-reads every pushed node's reported version and raises one
// aggregated alert when some have not converged. It is idempotent: the job's
// ConvergenceChecked flag makes a repeated call (restart + timer race) a no-op.
func (m *Manager) CheckConvergence(jobID string) {
	m.mu.Lock()
	m.loadLocked()
	if m.job == nil || m.job.ID != jobID || m.job.ConvergenceChecked || m.job.TargetVersion == "" {
		m.mu.Unlock()
		return
	}
	job := cloneJob(m.job)
	m.mu.Unlock()

	now := m.cfg.Now().Unix()
	target := job.TargetVersion
	stale := []NodeResult{}
	for i := range job.Nodes {
		n := &job.Nodes[i]
		if n.Outcome == OutcomeAlreadyCurrent || n.Outcome == OutcomeFailed {
			continue
		}
		converged := false
		reason := ""
		if m.cfg.Store != nil {
			if sb, err := m.cfg.Store.GetNodeSingbox(n.NodeID); err == nil {
				converged = norm(sb.Version) == target
				if !converged {
					reason = "reports " + displayVersion(sb.Version)
				}
			} else {
				reason = "node state unreadable: " + err.Error()
			}
		} else {
			reason = "store unavailable"
		}
		v := converged
		n.Converged = &v
		n.CheckedAt = now
		if !converged {
			if reason != "" {
				n.Reason = reason
			}
			stale = append(stale, *n)
		}
	}

	job.ConvergenceChecked = true
	job.CheckedAt = now
	job.Stale = make([]string, 0, len(stale))
	for _, n := range stale {
		job.Stale = append(job.Stale, n.NodeID)
	}

	m.mu.Lock()
	if m.job != nil && m.job.ID == jobID {
		m.job = job
		m.persistLocked(job)
	}
	m.mu.Unlock()
	m.publish(job)

	if len(stale) == 0 {
		return
	}
	m.raiseStaleAlert(job, stale, now)
}

// raiseStaleAlert writes the single aggregated alert. CreateAlert dedupes on
// kind+node ("", the alert is cluster-wide) and StaleAlerted keeps a re-armed
// timer from writing a second row for the same job.
func (m *Manager) raiseStaleAlert(job *Job, stale []NodeResult, now int64) {
	if m.cfg.Store == nil {
		return
	}
	type staleView struct {
		NodeID         string `json:"node_id"`
		Name           string `json:"name,omitempty"`
		Version        string `json:"version,omitempty"`
		DesiredVersion string `json:"desired_version,omitempty"`
		Reason         string `json:"reason,omitempty"`
	}
	nodes := make([]staleView, 0, len(stale))
	for _, n := range stale {
		nodes = append(nodes, staleView{n.NodeID, n.Name, n.Version, n.DesiredVersion, n.Reason})
	}
	payload, err := json.Marshal(map[string]any{
		"job_id":         job.ID,
		"target_version": job.TargetVersion,
		"deadline":       job.Deadline,
		"checked_at":     now,
		"count":          len(nodes),
		"nodes":          nodes,
	})
	if err != nil {
		m.logWarn("singboxupdate: encode stale alert", "err", err)
		return
	}
	if _, _, err := m.cfg.Store.CreateAlert(AlertSingboxUpdateStale, "", string(payload), staleAlertDedupeSecs); err != nil {
		m.logWarn("singboxupdate: create stale alert", "err", err)
		return
	}
	m.mu.Lock()
	if m.job != nil && m.job.ID == job.ID {
		m.job.StaleAlerted = true
		m.persistLocked(m.job)
	}
	m.mu.Unlock()
	m.audit(&store.AuditEntry{
		Actor: actorOf(job), Action: "singbox_update_stale", SourceIP: job.SourceIP,
		Command: fmt.Sprintf("%s not_converged=%d nodes=%s", job.TargetVersion, len(stale), strings.Join(job.Stale, ",")),
	})
}

// armTimer fires CheckConvergence at the job's persisted deadline. A deadline
// in the past fires immediately; that is what re-arms a check after a restart.
func (m *Manager) armTimer(job *Job) {
	m.mu.Lock()
	if m.armedAt == nil {
		m.armedAt = map[string]bool{}
	}
	if m.armedAt[job.ID] {
		m.mu.Unlock()
		return
	}
	m.armedAt[job.ID] = true
	ctx := m.ctx
	m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	delay := time.Unix(job.Deadline, 0).Sub(m.cfg.Now())
	if delay < 0 {
		delay = 0
	}
	go func(id string, d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			m.mu.Lock()
			delete(m.armedAt, id)
			m.mu.Unlock()
			return
		case <-t.C:
			m.CheckConvergence(id)
			m.mu.Lock()
			delete(m.armedAt, id)
			m.mu.Unlock()
		}
	}(job.ID, delay)
}

// --- artifact cache ---

// DeleteVersion removes one cached release. A version still referenced by a
// node's desired_version needs force (§9.2: 被引用时给出确认).
func (m *Manager) DeleteVersion(version string, force bool) error {
	canonicalVersion, err := canonical(version)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.loadLocked()
	running := m.job != nil && !isTerminal(m.job.State)
	m.mu.Unlock()
	if running {
		return ErrInProgress
	}
	if m.cfg.DL == nil || m.cfg.DL.DLDir() == "" {
		return singboxdl.ErrNoDLDir
	}
	refs := 0
	if m.cfg.Store != nil {
		if all, err := m.cfg.Store.SingboxDesiredVersionRefs(); err == nil {
			refs = all[canonicalVersion]
		}
	}
	if refs > 0 && !force {
		return &InUseError{Version: canonicalVersion, Refs: refs}
	}
	if err := m.cfg.DL.DeleteVersion(canonicalVersion); err != nil {
		return err
	}
	m.cacheChanged()
	return nil
}

// --- internals ---

func (m *Manager) baseCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *Manager) listTargets() ([]store.SingboxTarget, error) {
	if m.cfg.Store == nil {
		return []store.SingboxTarget{}, nil
	}
	return m.cfg.Store.ListSingboxTargets()
}

func (m *Manager) cached(version string) bool {
	if m.cfg.DL == nil || m.cfg.DL.DLDir() == "" || version == "" {
		return false
	}
	versions, err := m.cfg.DL.ScanCache()
	if err != nil {
		return false
	}
	for _, v := range versions {
		if v.Version == version {
			return true
		}
	}
	return false
}

func (m *Manager) isOnline(nodeID, fallbackStatus string) bool {
	if m.cfg.Online != nil {
		return m.cfg.Online.IsOnline(nodeID)
	}
	return fallbackStatus == "online"
}

func (m *Manager) setState(jobID, state string) {
	m.mu.Lock()
	if m.job != nil && m.job.ID == jobID {
		m.job.State = state
		m.persistLocked(m.job)
	}
	snapshot := cloneJob(m.job)
	m.mu.Unlock()
	m.publish(snapshot)
}

func (m *Manager) fail(jobID, reason string) {
	m.mu.Lock()
	if m.job != nil && m.job.ID == jobID {
		m.job.State = StateFailed
		m.job.Error = reason
		m.job.FinishedAt = m.cfg.Now().Unix()
		m.persistLocked(m.job)
	}
	snapshot := cloneJob(m.job)
	m.mu.Unlock()
	m.publish(snapshot)
	m.logWarn("singboxupdate: update failed", "reason", reason)
	if snapshot != nil && snapshot.ID == jobID {
		m.audit(&store.AuditEntry{
			Actor: actorOf(snapshot), Action: "singbox_update_failed",
			Command: snapshot.TargetVersion, Reason: reason, SourceIP: snapshot.SourceIP,
		})
	}
}

func (m *Manager) publish(job *Job) {
	if m.cfg.Publish == nil || job == nil {
		return
	}
	m.cfg.Publish(*job)
}

func (m *Manager) cacheChanged() {
	if m.cfg.CacheChanged != nil {
		m.cfg.CacheChanged()
	}
}

func (m *Manager) audit(a *store.AuditEntry) {
	if m.cfg.Store == nil {
		return
	}
	if err := m.cfg.Store.InsertAudit(a); err != nil {
		m.logWarn("singboxupdate: insert audit", "action", a.Action, "err", err)
	}
}

func (m *Manager) logWarn(msg string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log.Warn(msg, args...)
	}
}

// loadLocked reads the persisted job once per process.
func (m *Manager) loadLocked() {
	if m.loaded {
		return
	}
	m.loaded = true
	if m.cfg.Settings == nil {
		return
	}
	raw, err := m.cfg.Settings.GetSetting(SettingLastUpdate)
	if err != nil || strings.TrimSpace(raw) == "" {
		return
	}
	var job Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		m.logWarn("singboxupdate: decode persisted job", "err", err)
		return
	}
	if job.ID == "" {
		return
	}
	if job.Nodes == nil {
		job.Nodes = []NodeResult{}
	}
	m.job = &job
}

func (m *Manager) persistLocked(job *Job) {
	if m.cfg.Settings == nil || job == nil {
		return
	}
	raw, err := json.Marshal(job)
	if err != nil {
		m.logWarn("singboxupdate: encode job", "err", err)
		return
	}
	if err := m.cfg.Settings.SetSetting(SettingLastUpdate, string(raw), false); err != nil {
		m.logWarn("singboxupdate: persist job", "err", err)
	}
}

func cloneJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	out := *j
	out.Nodes = make([]NodeResult, len(j.Nodes))
	copy(out.Nodes, j.Nodes)
	if j.Stale != nil {
		out.Stale = make([]string, len(j.Stale))
		copy(out.Stale, j.Stale)
	}
	return &out
}

func countResults(nodes []NodeResult) Counts {
	c := Counts{Total: len(nodes)}
	for _, n := range nodes {
		switch n.Outcome {
		case OutcomeAlreadyCurrent:
			c.AlreadyCurrent++
		case OutcomePushed:
			c.Pushed++
		case OutcomeOfflinePending:
			c.OfflinePending++
		case OutcomeFailed:
			c.Failed++
		}
	}
	return c
}

func isTerminal(state string) bool { return state == StateDone || state == StateFailed }

func actorOf(job *Job) string {
	if job == nil || job.Actor == "" {
		return "panel"
	}
	return job.Actor
}

func newJobID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sbupd-%d", time.Now().UnixNano())
	}
	return "sbupd-" + hex.EncodeToString(b)
}

// canonical normalizes a version tag (strips a leading v/V, validates the
// shape). It is the gate for every version that reaches the store or a path.
func canonical(version string) (string, error) {
	v, err := singboxdl.ParseVersion(strings.TrimSpace(version))
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// norm compares versions the way the agents report them (§9: the v prefix is
// conventional, never semantic).
func norm(version string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "V"))
}

func displayVersion(version string) string {
	if strings.TrimSpace(version) == "" {
		return "(unknown)"
	}
	return version
}

// Package scheduler runs the periodic server-side jobs (design.md §6/§7/§15/§17):
// metrics retention, offline detection, command TTL expiry, alert delivery and
// the §15 triggers (traffic thresholds, billing due reminders), daily backups.
package scheduler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/server/notify"
	"github.com/fonlan/fobe/internal/server/quota"
	"github.com/fonlan/fobe/internal/server/store"
)

type Scheduler struct {
	store      *store.Store
	log        *slog.Logger
	backupDir  string
	retainDays int64
	notifiers  []notify.Notifier
}

// New builds the scheduler. notifiers are the §15 delivery channels; passing
// none just disables outbound notifications (the alerts table stays authoritative).
func New(st *store.Store, log *slog.Logger, backupDir string, retainDays int64, notifiers ...notify.Notifier) *Scheduler {
	if retainDays <= 0 {
		retainDays = 7 // design §0.16: detail rows only for 7 days
	}
	return &Scheduler{store: st, log: log, backupDir: backupDir, retainDays: retainDays, notifiers: notifiers}
}

// Start launches all loops; cancel the context to stop.
func (s *Scheduler) Start(stop <-chan struct{}) {
	go s.loop(stop, 10*time.Minute, s.pruneSamples)  // §6: every 10 minutes
	go s.loop(stop, 30*time.Second, s.detectOffline) // §7: 90s heartbeat window
	go s.loop(stop, 1*time.Minute, s.expireCommands) // §19.1: 10min command TTL
	go s.loop(stop, 1*time.Minute, s.deliverAlerts)  // §15: notification channels
	go s.loop(stop, 5*time.Minute, s.checkTraffic)   // §15: quota thresholds
	go s.loop(stop, 6*time.Hour, s.checkBillingDue)  // §15: 7/3/1-day reminders
	if s.backupDir != "" {
		// Snapshot once at boot, not only on the 24h tick: a panel that gets
		// recreated often (deploy churn) never lives long enough for the
		// ticker to fire — the 2026-09-16 corruption incident found the
		// backup dir empty despite the loop "running" for days. Timestamped
		// names make restart churn additive; store.PruneBackups caps the pool.
		go s.backup()
		go s.loop(stop, 24*time.Hour, s.backup)
	}
}

func (s *Scheduler) loop(stop <-chan struct{}, every time.Duration, job func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			job()
		}
	}
}

func (s *Scheduler) pruneSamples() {
	cutoff := time.Now().Unix() - s.retainDays*86400
	for _, table := range []string{"metrics_samples", "latency_samples"} {
		n, err := s.store.PruneOlderThan(table, cutoff)
		if err != nil {
			s.log.Warn("prune", "table", table, "err", err)
		} else if n > 0 {
			s.log.Info("pruned rows", "table", table, "count", n)
		}
	}
}

// detectOffline flips nodes whose last_seen is older than 90s to offline and
// raises the alert (§15: 90s 无心跳). Recovery notice when it comes back.
func (s *Scheduler) detectOffline() {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		lastSeen := n.CreatedAt
		if n.LastSeen.Valid {
			lastSeen = n.LastSeen.Int64
		}
		if now-lastSeen > 90 {
			if err := s.store.MarkNodeOffline(n.ID); err != nil {
				s.log.Warn("mark offline", "node", n.ID, "err", err)
			}
			_, created, err := s.store.CreateAlert("node_offline", n.ID, "{}", 3600)
			if err == nil && created {
				s.log.Warn("node offline", "node", n.Name)
			}
		}
	}
}

func (s *Scheduler) expireCommands() {
	n, err := s.store.TimeoutStaleCommands()
	if err != nil {
		s.log.Warn("expire commands", "err", err)
	} else if n > 0 {
		s.log.Info("expired stale commands", "count", n)
	}
}

// --- alert delivery + §15 triggers (design.md §15) ---

// deliverAlerts pushes every not-yet-delivered alert to the configured
// channels. An alert recovered before its first delivery goes out as a
// recovery event; failures stay undelivered and retry on the next pass.
func (s *Scheduler) deliverAlerts() {
	targets := make([]notify.Notifier, 0, len(s.notifiers))
	for _, n := range s.notifiers {
		if n.Configured() {
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		return
	}
	alerts, err := s.store.UndeliveredAlerts()
	if err != nil {
		s.log.Warn("list undelivered alerts", "err", err)
		return
	}
	names := s.nodeNames()
	for _, a := range alerts {
		ev := notify.Event{
			Kind:      a.Kind,
			NodeID:    a.NodeID,
			NodeName:  names[a.NodeID],
			Payload:   a.Payload,
			CreatedAt: a.CreatedAt,
			Event:     notify.EventAlert,
		}
		if a.RecoveredAt != nil {
			ev.Event = notify.EventRecovery
		}
		if s.deliverOne(targets, ev) {
			if err := s.store.MarkAlertDelivered(a.ID); err != nil {
				s.log.Warn("mark alert delivered", "id", a.ID, "err", err)
			}
		}
	}
}

// deliverOne sends ev to every target that accepts it. true means the alert
// needs no further attempt: either at least one channel took it and none of
// the accepting ones failed, or no channel wanted it at all (§15 event
// switches). The second case matters: leaving a switched-off alert in the
// queue would replay the whole silenced period the moment the switch goes back
// on, which is never what "don't notify me about traffic" means.
func (s *Scheduler) deliverOne(targets []notify.Notifier, ev notify.Event) bool {
	failed := 0
	for _, n := range targets {
		if !n.Accepts(ev) {
			continue // channel or event type switched off: intentional silence
		}
		err := n.Deliver(ev)
		switch {
		case err == nil:
		case errors.Is(err, notify.ErrNotConfigured):
			// channel lost its settings mid-pass; ignore
		default:
			failed++
			s.log.Warn("deliver alert", "channel", n.Name(), "kind", ev.Kind,
				"node", ev.NodeID, "event", ev.Event, "err", err)
		}
	}
	return failed == 0
}

func (s *Scheduler) nodeNames() map[string]string {
	names := map[string]string{}
	nodes, err := s.store.ListNodes()
	if err != nil {
		return names
	}
	for _, n := range nodes {
		names[n.ID] = n.Name
	}
	return names
}

// Traffic alert kinds + the default thresholds (§15: 80% / 100%).
const (
	AlertTrafficWarn  = "traffic_warn"
	AlertTrafficCrit  = "traffic_crit"
	AlertBillingDue   = "billing_due"
	AlertBillingOver  = "due_overdue"
	defaultWarnPct    = 80.0
	defaultCritPct    = 100.0
	trafficDedupeSecs = 3600 // §15: same node+kind once per hour
	billingDedupeSecs = 7 * 86400
)

// checkTraffic raises traffic_warn / traffic_crit when the period usage
// reaches the configured thresholds. Dedupe (1h) is CreateAlert's job; there
// is no recovery delivery for traffic alerts (§15: 去重即可).
func (s *Scheduler) checkTraffic() {
	warn, crit := s.trafficThresholds()
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	now := time.Now()
	for _, n := range nodes {
		net, err := s.store.GetNodeNetwork(n.ID)
		if err != nil { // no node_network row: nothing to check
			continue
		}
		if net.QuotaBytes == nil || *net.QuotaBytes <= 0 { // §8.3: no quota → no percent
			continue
		}
		start := quota.PeriodStart(net.CycleType, net.NextResetAt, net.TZ, now)
		rx, tx, err := s.store.SumTrafficSince(n.ID, quota.LocalDate(start, net.TZ))
		if err != nil {
			s.log.Warn("sum traffic", "node", n.ID, "err", err)
			continue
		}
		used, pct := quota.Usage(net.Mode, rx, tx, *net.QuotaBytes)
		if pct < 0 {
			continue
		}
		kind := trafficKind(pct, warn, crit)
		if kind == "" {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"mode":        net.Mode,
			"used_bytes":  int64(used),
			"quota_bytes": *net.QuotaBytes,
			"pct":         math.Round(pct*1000) / 10, // percent, 1 decimal
		})
		if err != nil {
			payload = []byte("{}")
		}
		_, created, err := s.store.CreateAlert(kind, n.ID, string(payload), trafficDedupeSecs)
		if err != nil {
			s.log.Warn("create traffic alert", "node", n.ID, "err", err)
		} else if created {
			s.log.Warn("traffic threshold reached", "node", n.Name, "kind", kind, "pct", pct)
		}
	}
}

func trafficKind(pct, warn, crit float64) string {
	switch {
	case crit > 0 && pct >= crit:
		return AlertTrafficCrit
	case warn > 0 && pct >= warn:
		return AlertTrafficWarn
	default:
		return ""
	}
}

// trafficThresholds reads alert.traffic_warn_pct / alert.traffic_crit_pct
// (percent numbers, e.g. "80") and converts them into the fractions that
// quota.Usage compares against; empty or invalid falls back to 80/100.
func (s *Scheduler) trafficThresholds() (warn, crit float64) {
	warn, crit = defaultWarnPct/100, defaultCritPct/100
	if v, ok := s.setting("alert.traffic_warn_pct"); ok {
		if p, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && p > 0 {
			warn = p / 100
		}
	}
	if v, ok := s.setting("alert.traffic_crit_pct"); ok {
		if p, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && p > 0 {
			crit = p / 100
		}
	}
	return warn, crit
}

func (s *Scheduler) setting(key string) (string, bool) {
	v, err := s.store.GetSetting(key)
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

// checkBillingDue reminds 7/3/1 days before node_billing.next_due_at and once
// more after the date passed (due_overdue). Stage-aware dedupe: each reminder
// fires once per countdown stage — a new stage closes the previous alert
// (which also resolves stale reminders after a renewal pushes the due date out).
func (s *Scheduler) checkBillingDue() {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, n := range nodes {
		b, err := s.store.GetNodeBilling(n.ID)
		if err != nil { // no billing row
			continue
		}
		if b.NextDueAt == nil || *b.NextDueAt == 0 {
			continue
		}
		due := *b.NextDueAt
		days := int64(math.Ceil(float64(due-now) / 86400))
		kind := billingKind(days)
		if kind == "" {
			if days > 7 { // renewed: due pushed beyond the reminder horizon
				s.closeBillingAlerts(n.ID)
			}
			continue
		}
		stage := days
		if stage < 0 {
			stage = 0 // every overdue day is the same stage
		}
		if s.billingStageOpen(kind, n.ID, stage) {
			continue
		}
		payload, err := json.Marshal(map[string]any{"days": days, "due_at": due})
		if err != nil {
			payload = []byte("{}")
		}
		s.closeBillingAlerts(n.ID)
		_, created, err := s.store.CreateAlert(kind, n.ID, string(payload), billingDedupeSecs)
		if err != nil {
			s.log.Warn("create billing alert", "node", n.ID, "err", err)
		} else if created {
			s.log.Info("billing reminder", "node", n.Name, "kind", kind, "days", days)
		}
	}
}

// billingKind maps days-until-due to the §15 reminder stage
// (7/3/1 days ahead, overdue after; empty between stages).
func billingKind(days int64) string {
	switch {
	case days <= 0:
		return AlertBillingOver
	case days == 7 || days == 3 || days == 1:
		return AlertBillingDue
	default:
		return ""
	}
}

// billingStageOpen reports whether an open alert for this exact stage already
// exists (prevents duplicate reminders on every 6h pass).
func (s *Scheduler) billingStageOpen(kind, nodeID string, stage int64) bool {
	a, err := s.store.OpenAlert(kind, nodeID)
	if err != nil {
		return false
	}
	var p struct {
		Days *int64 `json:"days"`
	}
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil || p.Days == nil {
		return true // unreadable payload: fall back to plain dedupe
	}
	d := *p.Days
	if d < 0 {
		d = 0
	}
	return d == stage
}

func (s *Scheduler) closeBillingAlerts(nodeID string) {
	// recover both billing kinds; the previous stage is superseded
	_ = s.store.RecoverAlert(AlertBillingDue, nodeID)
	_ = s.store.RecoverAlert(AlertBillingOver, nodeID)
}

// backup snapshots the DB via VACUUM INTO (§17/§19.3). Timestamped names:
// re-running (boot, 24h tick, restart churn) always adds a fresh restore
// point instead of dying on "file exists"; the pool is capped by
// store.PruneBackups.
func (s *Scheduler) backup() {
	if err := s.store.BackupDir(s.backupDir); err != nil {
		s.log.Warn("backup", "err", err)
	}
}

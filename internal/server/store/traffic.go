package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// --- node_network / node_billing (design §8.3, §0.14/15) ---

type NodeNetwork struct {
	NodeID      string `json:"node_id"`
	Iface       string `json:"iface"`
	Mode        string `json:"mode"`        // in|out|both|max
	QuotaBytes  *int64 `json:"quota_bytes"` // nil = no quota
	CycleType   string `json:"cycle_type"`  // none|month|quarter|year
	NextResetAt *int64 `json:"next_reset_at"`
	TZ          string `json:"tz"`
}

func (s *Store) GetNodeNetwork(nodeID string) (*NodeNetwork, error) {
	n := &NodeNetwork{NodeID: nodeID}
	err := s.db.QueryRow(
		`SELECT nn.iface, nn.mode, nn.quota_bytes, nn.cycle_type, nn.next_reset_at,
		        COALESCE(NULLIF(n.tz, ''), 'UTC')
		 FROM node_network nn JOIN nodes n ON n.id = nn.node_id WHERE nn.node_id = ?`, nodeID,
	).Scan(&n.Iface, &n.Mode, &n.QuotaBytes, &n.CycleType, &n.NextResetAt, &n.TZ)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get node_network: %w", err)
	}
	return n, nil
}

// UpsertNodeNetwork creates the row with defaults if missing.
func (s *Store) UpsertNodeNetwork(n *NodeNetwork) error {
	_, err := s.db.Exec(
		`INSERT INTO node_network (node_id, iface, mode, quota_bytes, cycle_type, next_reset_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   iface = excluded.iface, mode = excluded.mode, quota_bytes = excluded.quota_bytes,
		   cycle_type = excluded.cycle_type, next_reset_at = excluded.next_reset_at`,
		n.NodeID, n.Iface, n.Mode, n.QuotaBytes, n.CycleType, n.NextResetAt,
	)
	if err != nil {
		return fmt.Errorf("upsert node_network: %w", err)
	}
	return nil
}

type NodeInterface struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"default"`
}

// ReplaceNodeInterfaces stores the complete interface inventory reported by a
// probe. It is a snapshot, just like the node IP inventory.
func (s *Store) ReplaceNodeInterfaces(nodeID string, interfaces []NodeInterface) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin replace node interfaces: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_interfaces WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("delete node interfaces: %w", err)
	}
	for _, iface := range interfaces {
		if iface.Name == "" {
			continue
		}
		isDefault := 0
		if iface.IsDefault {
			isDefault = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO node_interfaces (node_id, name, is_default, updated_at) VALUES (?, ?, ?, ?)`,
			nodeID,
			iface.Name,
			isDefault,
			now(),
		); err != nil {
			return fmt.Errorf("insert node interface: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit node interfaces: %w", err)
	}
	return nil
}

func (s *Store) ListNodeInterfaces(nodeID string) ([]NodeInterface, error) {
	rows, err := s.db.Query(
		`SELECT name, is_default FROM node_interfaces WHERE node_id = ? ORDER BY is_default DESC, name`, nodeID,
	)
	if err != nil {
		return nil, fmt.Errorf("list node interfaces: %w", err)
	}
	defer rows.Close()
	out := []NodeInterface{}
	for rows.Next() {
		var iface NodeInterface
		var isDefault int
		if err := rows.Scan(&iface.Name, &isDefault); err != nil {
			return nil, fmt.Errorf("scan node interface: %w", err)
		}
		iface.IsDefault = isDefault != 0
		out = append(out, iface)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node interfaces: %w", err)
	}
	return out, nil
}

// NodeBilling is serialized straight into the node-detail response
// (httpapi), so the json tags are wire contract, not decoration — without
// them the keys marshal as Go field names and the web form reads back blank.
type NodeBilling struct {
	NodeID    string `json:"node_id"`
	CycleType string `json:"cycle_type"`
	CycleDays *int64 `json:"cycle_days"`
	NextDueAt *int64 `json:"next_due_at"`
	Note      string `json:"note"`
}

func (s *Store) GetNodeBilling(nodeID string) (*NodeBilling, error) {
	b := &NodeBilling{NodeID: nodeID}
	err := s.db.QueryRow(
		`SELECT cycle_type, cycle_days, next_due_at, note FROM node_billing WHERE node_id = ?`, nodeID,
	).Scan(&b.CycleType, &b.CycleDays, &b.NextDueAt, &b.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func (s *Store) UpsertNodeBilling(b *NodeBilling) error {
	_, err := s.db.Exec(
		`INSERT INTO node_billing (node_id, cycle_type, cycle_days, next_due_at, note)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   cycle_type = excluded.cycle_type, cycle_days = excluded.cycle_days,
		   next_due_at = excluded.next_due_at, note = excluded.note`,
		b.NodeID, b.CycleType, b.CycleDays, b.NextDueAt, b.Note,
	)
	return err
}

// --- traffic counters & daily rollup (design §8.2) ---

type TrafficCounter struct {
	NodeID      string
	Iface       string
	Direction   string // rx|tx
	LastRaw     int64
	LastTS      int64
	PeriodStart int64
	PeriodUsed  int64
}

func (s *Store) GetTrafficCounter(nodeID, iface, direction string) (*TrafficCounter, error) {
	c := &TrafficCounter{NodeID: nodeID, Iface: iface, Direction: direction}
	err := s.db.QueryRow(
		`SELECT last_raw, last_ts, period_start, period_used FROM traffic_counters
		 WHERE node_id = ? AND iface = ? AND direction = ?`, nodeID, iface, direction,
	).Scan(&c.LastRaw, &c.LastTS, &c.PeriodStart, &c.PeriodUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// UpsertTrafficCounter writes the new baseline after reset detection.
func (s *Store) UpsertTrafficCounter(c *TrafficCounter) error {
	_, err := s.db.Exec(
		`INSERT INTO traffic_counters (node_id, iface, direction, last_raw, last_ts, period_start, period_used)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(node_id, iface, direction) DO UPDATE SET
		   last_raw = excluded.last_raw, last_ts = excluded.last_ts,
		   period_start = excluded.period_start, period_used = excluded.period_used`,
		c.NodeID, c.Iface, c.Direction, c.LastRaw, c.LastTS, c.PeriodStart, c.PeriodUsed,
	)
	return err
}

// AddTrafficDaily adds delta bytes to the node's day bucket (date in node tz).
func (s *Store) AddTrafficDaily(nodeID, date string, rxDelta, txDelta int64) error {
	_, err := s.db.Exec(
		`INSERT INTO traffic_daily (node_id, date, rx_bytes, tx_bytes) VALUES (?, ?, ?, ?)
		 ON CONFLICT(node_id, date) DO UPDATE SET
		   rx_bytes = rx_bytes + excluded.rx_bytes, tx_bytes = tx_bytes + excluded.tx_bytes`,
		nodeID, date, rxDelta, txDelta,
	)
	return err
}

func (s *Store) SumTrafficSince(nodeID, sinceDate string) (rx, tx int64, err error) {
	err = s.db.QueryRow(
		`SELECT COALESCE(SUM(rx_bytes),0), COALESCE(SUM(tx_bytes),0)
		 FROM traffic_daily WHERE node_id = ? AND date >= ?`, nodeID, sinceDate,
	).Scan(&rx, &tx)
	return
}

type TrafficDay struct {
	Date    string `json:"date"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

func (s *Store) ListTrafficDaily(nodeID, fromDate string) ([]TrafficDay, error) {
	rows, err := s.db.Query(
		`SELECT date, rx_bytes, tx_bytes FROM traffic_daily WHERE node_id = ? AND date >= ? ORDER BY date`,
		nodeID, fromDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrafficDay{}
	for rows.Next() {
		var d TrafficDay
		if err := rows.Scan(&d.Date, &d.RxBytes, &d.TxBytes); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- metrics_samples (7d retention, design §16.4) ---

type MetricsSample struct {
	TS        int64   `json:"ts"`
	CPU       float64 `json:"cpu"`
	MemUsed   int64   `json:"mem_used"`
	MemTotal  int64   `json:"mem_total"`
	DiskUsed  int64   `json:"disk_used"`
	DiskTotal int64   `json:"disk_total"`
	NetRxRate float64 `json:"net_rx_rate"`
	NetTxRate float64 `json:"net_tx_rate"`
	Load1     float64 `json:"load1"`
	Uptime    int64   `json:"uptime"`
}

func (s *Store) InsertMetricsSample(nodeID string, m *MetricsSample) error {
	_, err := s.db.Exec(
		`INSERT INTO metrics_samples
		 (node_id, ts, cpu, mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate, load1, uptime)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, m.TS, m.CPU, m.MemUsed, m.MemTotal, m.DiskUsed, m.DiskTotal,
		m.NetRxRate, m.NetTxRate, m.Load1, m.Uptime,
	)
	return err
}

func (s *Store) ListMetrics(nodeID string, fromTS int64) ([]MetricsSample, error) {
	rows, err := s.db.Query(
		`SELECT ts, cpu, mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate, load1, uptime
		 FROM metrics_samples WHERE node_id = ? AND ts >= ? ORDER BY ts`, nodeID, fromTS,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricsSample{}
	for rows.Next() {
		var m MetricsSample
		if err := rows.Scan(&m.TS, &m.CPU, &m.MemUsed, &m.MemTotal, &m.DiskUsed, &m.DiskTotal,
			&m.NetRxRate, &m.NetTxRate, &m.Load1, &m.Uptime); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) LatestMetrics(nodeID string) (*MetricsSample, error) {
	m := &MetricsSample{}
	err := s.db.QueryRow(
		`SELECT ts, cpu, mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate, load1, uptime
		 FROM metrics_samples WHERE node_id = ? ORDER BY ts DESC LIMIT 1`, nodeID,
	).Scan(&m.TS, &m.CPU, &m.MemUsed, &m.MemTotal, &m.DiskUsed, &m.DiskTotal,
		&m.NetRxRate, &m.NetTxRate, &m.Load1, &m.Uptime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// PruneOlderThan deletes retention-expired rows, returns rows removed.
func (s *Store) PruneOlderThan(table string, cutoffTS int64) (int64, error) {
	// table names come only from our own scheduler; still validated here.
	// audit_logs prunes on its own longer cutoff (§4.4, 2026-09-19 revision),
	// passed by the caller — same job, different window.
	allowed := map[string]bool{"metrics_samples": true, "latency_samples": true, "audit_logs": true}
	if !allowed[table] {
		return 0, fmt.Errorf("table %q is not prunable", table)
	}
	res, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE ts < ?`, table), cutoffTS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- latency (design §13) ---

type LatencyTarget struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (s *Store) CreateLatencyTarget(name, kind, host string, port int) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO latency_targets (name, kind, host, port, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, kind, host, port, now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteLatencyTarget(id int64) error {
	_, err := s.db.Exec(`DELETE FROM latency_targets WHERE id = ?`, id)
	return err
}

// UpdateLatencyTarget rewrites a target's definition and reports whether the
// measured endpoint (kind/host/port) moved. When it did, the target's samples
// are deleted in the same transaction: latency_samples is keyed by target_id,
// so keeping them would splice two different endpoints into one chart line —
// after a rename they are still the same endpoint and must survive (§13).
func (s *Store) UpdateLatencyTarget(id int64, name, kind, host string, port int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var curKind, curHost string
	var curPort int
	err = tx.QueryRow(`SELECT kind, host, port FROM latency_targets WHERE id = ?`, id).
		Scan(&curKind, &curHost, &curPort)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read latency target: %w", err)
	}
	// icmp targets created before the port normalization existed carry the
	// form's default port (443). An icmp row never dials a port, so a port-only
	// difference between two icmp rows is not an endpoint move — counting it
	// would purge the history of an existing ICMP target on a mere rename.
	portMoved := curPort != port && !(curKind == "icmp" && kind == "icmp")
	endpointChanged := curKind != kind || curHost != host || portMoved

	if _, err := tx.Exec(
		`UPDATE latency_targets SET name = ?, kind = ?, host = ?, port = ? WHERE id = ?`,
		name, kind, host, port, id,
	); err != nil {
		return false, fmt.Errorf("update latency target: %w", err)
	}
	if endpointChanged {
		if _, err := tx.Exec(`DELETE FROM latency_samples WHERE target_id = ?`, id); err != nil {
			return false, fmt.Errorf("purge latency samples: %w", err)
		}
	}
	return endpointChanged, tx.Commit()
}

func (s *Store) ListLatencyTargets() ([]LatencyTarget, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, host, port FROM latency_targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LatencyTarget{}
	for rows.Next() {
		var t LatencyTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, &t.Host, &t.Port); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SetNodeLatencyTargets(nodeID string, targetIDs []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_latency_targets WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	for _, id := range targetIDs {
		if _, err := tx.Exec(
			`INSERT INTO node_latency_targets (node_id, target_id) VALUES (?, ?)`, nodeID, id,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NodeIDsForLatencyTarget lists the nodes currently probing the given target.
// The delete path needs them *before* the FK cascade removes the link rows, so
// it can push each node an updated target list (§13 实现修订 2026-09-17k).
func (s *Store) NodeIDsForLatencyTarget(targetID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT node_id FROM node_latency_targets WHERE target_id = ? ORDER BY node_id`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TargetsForNode returns the enabled target specs for a node.
func (s *Store) TargetsForNode(nodeID string) ([]LatencyTarget, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.name, t.kind, t.host, t.port FROM latency_targets t
		 JOIN node_latency_targets n ON n.target_id = t.id WHERE n.node_id = ? ORDER BY t.id`, nodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LatencyTarget{}
	for rows.Next() {
		var t LatencyTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, &t.Host, &t.Port); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) InsertLatencySamples(nodeID string, samples []LatencySampleRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sm := range samples {
		if _, err := tx.Exec(
			`INSERT INTO latency_samples (node_id, target_id, ts, icmp_ms, tcp_ms, loss) VALUES (?, ?, ?, ?, ?, ?)`,
			nodeID, sm.TargetID, sm.TS, sm.ICMPMs, sm.TCPMs, sm.Loss,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type LatencySampleRow struct {
	TargetID int64
	TS       int64
	ICMPMs   float64
	TCPMs    float64
	Loss     float64
}

func (s *Store) ListLatency(nodeID string, targetID int64, fromTS int64) ([]LatencySampleRow, error) {
	rows, err := s.db.Query(
		`SELECT target_id, ts, icmp_ms, tcp_ms, loss FROM latency_samples
		 WHERE node_id = ? AND target_id = ? AND ts >= ? ORDER BY ts`, nodeID, targetID, fromTS,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LatencySampleRow{}
	for rows.Next() {
		var r LatencySampleRow
		if err := rows.Scan(&r.TargetID, &r.TS, &r.ICMPMs, &r.TCPMs, &r.Loss); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListLatencyBucketed returns per-bucket averages for every target enabled on
// the node, restricted to targets in node_latency_targets. Design §13 requires
// server-side downsampling for the 7d view: raw 5s points are ~121k rows per
// target, and the multi-target chart would multiply that by N. A bucket with
// no successful probe keeps the -1 sentinel (COALESCE over the CASE), loss
// becomes the bucket's mean failure rate. TS is the bucket start.
func (s *Store) ListLatencyBucketed(nodeID string, fromTS, bucket int64) ([]LatencySampleRow, error) {
	rows, err := s.db.Query(
		`SELECT target_id, (ts / ?) * ? AS bts,
		        COALESCE(AVG(CASE WHEN icmp_ms >= 0 THEN icmp_ms END), -1),
		        COALESCE(AVG(CASE WHEN tcp_ms >= 0 THEN tcp_ms END), -1),
		        AVG(loss)
		 FROM latency_samples
		 WHERE node_id = ? AND ts >= ?
		   AND target_id IN (SELECT target_id FROM node_latency_targets WHERE node_id = ?)
		 GROUP BY target_id, bts
		 ORDER BY target_id, bts`, bucket, bucket, nodeID, fromTS, nodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LatencySampleRow{}
	for rows.Next() {
		var r LatencySampleRow
		if err := rows.Scan(&r.TargetID, &r.TS, &r.ICMPMs, &r.TCPMs, &r.Loss); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

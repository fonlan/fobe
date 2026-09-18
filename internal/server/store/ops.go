package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/singbox"
)

// --- encrypted columns (design §9.3 实现修订 2026-09-17) ---
//
// Unlike settings rows, a node_singbox column has no `encrypted` flag beside
// it, so the ciphertext carries its own marker. That keeps the reader honest:
// a value written before this existed (or by a dev build with no master key)
// is plaintext and stays readable, while anything this process writes is
// explicitly tagged and must decrypt.
const encPrefix = "enc:v1:"

// EncryptSettingValue returns the storable form of a sensitive column value.
// A nil cryptor (dev builds, unit tests) leaves the value readable rather than
// refusing to store it: the product's fail-closed rule is about the *server*
// starting without a master key, not about a store helper.
func EncryptSettingValue(crypt *security.Cryptor, plain string) (string, error) {
	if plain == "" || crypt == nil {
		return plain, nil
	}
	ct, err := crypt.Encrypt(plain)
	if err != nil {
		return "", err
	}
	return encPrefix + ct, nil
}

// DecryptSettingValue reverses EncryptSettingValue; untagged values pass
// through untouched (see encPrefix).
func DecryptSettingValue(crypt *security.Cryptor, stored string) (string, error) {
	if stored == "" || !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	if crypt == nil {
		return "", errors.New("stored value is encrypted but no master key is available")
	}
	return crypt.Decrypt(strings.TrimPrefix(stored, encPrefix))
}

// --- commands (offline queue, TTL 10min, design §7/§19.1) ---

type Command struct {
	ID          string
	NodeID      string
	Kind        string
	Payload     string
	Status      string // pending|sent|ok|failed|timeout
	CreatedAt   int64
	SentAt      sql.NullInt64
	FinishedAt  sql.NullInt64
	Result      string
	TTLSeconds  int64
	Actor       string
	AISessionID string
	Reason      string
	Risk        string
}

type CommandMeta struct {
	Actor       string
	AISessionID string
	Reason      string
	Risk        string
}

func (s *Store) CreateCommand(id, nodeID, kind, payload string, ttlSeconds int64) error {
	return s.CreateCommandWithMeta(id, nodeID, kind, payload, ttlSeconds, CommandMeta{Actor: "panel"})
}

func (s *Store) CreateCommandWithMeta(id, nodeID, kind, payload string, ttlSeconds int64, meta CommandMeta) error {
	_, err := s.db.Exec(
		`INSERT INTO commands
		 (id, node_id, kind, payload, status, created_at, ttl_seconds, actor, ai_session_id, reason, risk)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?)`,
		id, nodeID, kind, payload, now(), ttlSeconds, meta.Actor, meta.AISessionID, meta.Reason, meta.Risk,
	)
	return err
}

// NextPendingCommand claims the oldest pending command for a node, marking it sent.
func (s *Store) NextPendingCommand(nodeID string) (*Command, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	c := &Command{}
	err = tx.QueryRow(
		`SELECT id, node_id, kind, payload, status, created_at, sent_at, finished_at, result,
			        ttl_seconds, actor, ai_session_id, reason, risk
			 FROM commands WHERE node_id = ? AND status = 'pending' ORDER BY created_at LIMIT 1`, nodeID,
	).Scan(&c.ID, &c.NodeID, &c.Kind, &c.Payload, &c.Status, &c.CreatedAt, &c.SentAt,
		&c.FinishedAt, &c.Result, &c.TTLSeconds, &c.Actor, &c.AISessionID, &c.Reason, &c.Risk)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE commands SET status = 'sent', sent_at = ? WHERE id = ?`, now(), c.ID); err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

func (s *Store) FinishCommand(id, status, result string) error {
	_, err := s.db.Exec(`UPDATE commands SET status = ?, result = ?, finished_at = ? WHERE id = ?`,
		status, result, now(), id)
	return err
}

// TimeoutStaleCommands expires pending commands past their TTL (design §7).
func (s *Store) TimeoutStaleCommands() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE commands SET status = 'timeout', finished_at = ?
		 WHERE status IN ('pending','sent') AND created_at + ttl_seconds < ?`, now(), now(),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) ListCommands(nodeID string, limit int) ([]Command, error) {
	rows, err := s.db.Query(
		`SELECT id, node_id, kind, payload, status, created_at, sent_at, finished_at, result,
			        ttl_seconds, actor, ai_session_id, reason, risk
			 FROM commands WHERE node_id = ? ORDER BY created_at DESC LIMIT ?`, nodeID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Command{}
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.NodeID, &c.Kind, &c.Payload, &c.Status, &c.CreatedAt,
			&c.SentAt, &c.FinishedAt, &c.Result, &c.TTLSeconds, &c.Actor, &c.AISessionID,
			&c.Reason, &c.Risk); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- alerts (design §15) ---

type Alert struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	NodeID      string `json:"node_id,omitempty"`
	Payload     string `json:"payload,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	DeliveredAt *int64 `json:"delivered_at,omitempty"`
	RecoveredAt *int64 `json:"recovered_at,omitempty"`
}

// CreateAlert dedupes: same node+kind within window returns existing id and created=false.
func (s *Store) CreateAlert(kind, nodeID, payload string, dedupeWindowSeconds int64) (int64, bool, error) {
	if dedupeWindowSeconds > 0 {
		var id int64
		err := s.db.QueryRow(
			`SELECT id FROM alerts WHERE kind = ? AND node_id = ? AND created_at > ? AND recovered_at IS NULL
			 ORDER BY id DESC LIMIT 1`, kind, nodeID, now()-dedupeWindowSeconds,
		).Scan(&id)
		if err == nil {
			return id, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	res, err := s.db.Exec(
		`INSERT INTO alerts (kind, node_id, payload, created_at) VALUES (?, ?, ?, ?)`,
		kind, nodeID, payload, now(),
	)
	if err != nil {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (s *Store) MarkAlertDelivered(id int64) error {
	_, err := s.db.Exec(`UPDATE alerts SET delivered_at = ? WHERE id = ?`, now(), id)
	return err
}

// RecoverAlert closes the matching open alert (recovery notice, design §15).
func (s *Store) RecoverAlert(kind, nodeID string) error {
	_, err := s.db.Exec(
		`UPDATE alerts SET recovered_at = ? WHERE kind = ? AND node_id = ? AND recovered_at IS NULL`,
		now(), kind, nodeID,
	)
	return err
}

// OpenAlert returns the newest unrecovered alert of kind+node; ErrNotFound
// when none is open. Lets jobs dedupe on their own stage (e.g. billing days).
func (s *Store) OpenAlert(kind, nodeID string) (*Alert, error) {
	a := &Alert{}
	err := s.db.QueryRow(
		`SELECT id, kind, node_id, payload, created_at, delivered_at, recovered_at
		 FROM alerts WHERE kind = ? AND node_id = ? AND recovered_at IS NULL
		 ORDER BY id DESC LIMIT 1`, kind, nodeID,
	).Scan(&a.ID, &a.Kind, &a.NodeID, &a.Payload, &a.CreatedAt, &a.DeliveredAt, &a.RecoveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Store) ListAlerts(limit int) ([]Alert, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, node_id, payload, created_at, delivered_at, recovered_at
		 FROM alerts ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.Kind, &a.NodeID, &a.Payload, &a.CreatedAt, &a.DeliveredAt, &a.RecoveredAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) UndeliveredAlerts() ([]Alert, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, node_id, payload, created_at, delivered_at, recovered_at
		 FROM alerts WHERE delivered_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.Kind, &a.NodeID, &a.Payload, &a.CreatedAt, &a.DeliveredAt, &a.RecoveredAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- audit logs (design §4.4) ---

type AuditEntry struct {
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	NodeID string `json:"node_id,omitempty"`
	// NodeName is resolved at read time (LEFT JOIN nodes) so renamed nodes
	// show their current name; it stays empty for nodes since deleted, and
	// audit_logs itself is never written back to.
	NodeName    string `json:"node_name,omitempty"`
	Action      string `json:"action"`
	Command     string `json:"command,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Risk        string `json:"risk,omitempty"`
	SourceIP    string `json:"source_ip,omitempty"`
	AISessionID string `json:"ai_session_id,omitempty"`
}

func (s *Store) InsertAudit(a *AuditEntry) error {
	if a.TS == 0 {
		a.TS = now()
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_logs (ts, actor, node_id, action, command, reason, risk, source_ip, ai_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Actor, a.NodeID, a.Action, a.Command, a.Reason, a.Risk, a.SourceIP, a.AISessionID,
	)
	return err
}

func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(
		`SELECT a.ts, a.actor, a.node_id, COALESCE(n.name, ''), a.action, a.command, a.reason, a.risk, a.source_ip, a.ai_session_id
		 FROM audit_logs a LEFT JOIN nodes n ON n.id = a.node_id
		 ORDER BY a.id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.TS, &a.Actor, &a.NodeID, &a.NodeName, &a.Action, &a.Command, &a.Reason,
			&a.Risk, &a.SourceIP, &a.AISessionID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- node_singbox (design §9.1) ---

type NodeSingbox struct {
	NodeID         string `json:"node_id"`
	Version        string `json:"version"`
	DesiredVersion string `json:"desired_version"`
	// DesiredUninstall is the operator's persistent "remove sing-box from this
	// probe" intent (design §9.2 实现修订 2026-09-16). It is the desired half,
	// not a status: the agent clears it by reporting that nothing is installed.
	// A node with it set has an empty DesiredVersion, which is what keeps it out
	// of every batch-update target list — and it survives with the probe offline.
	DesiredUninstall bool   `json:"desired_uninstall"`
	ConfigHash       string `json:"config_hash"`
	Status           string `json:"status"`
	LastError        string `json:"last_error"`
	CertPEM          string `json:"cert_pem"`
	CertSHA256       string `json:"cert_sha256"`
	CertNotAfter     int64  `json:"cert_not_after"`
	Port             int    `json:"port"`
	// Local discovery (§9.3 实现修订 2026-09-17): only the fingerprint lives in
	// this struct, because it is what the hub compares to decide "the operator
	// touched his own config.json". The report itself (version, running flag,
	// inbound list, raw bytes) is read on demand via GetNodeSingboxLocal, so no
	// hot path carries a blob that may be tens of kilobytes.
	LocalHash string `json:"local_hash"`
	// ExtrasPresent is "this node has adopted inbounds". Only the fact, never
	// the ciphertext: the column itself is encrypted, and callers that need the
	// contents go through GetNodeSingboxExtraInbounds. It is here because
	// "would this node appear in a subscription" must not cost a decrypt.
	ExtrasPresent bool  `json:"extras_present"`
	UpdatedAt     int64 `json:"updated_at"`
}

// NodeSingboxInbound is the panel-facing lifecycle record for one listener.
// The agent is authoritative for Running: desired config alone is never proof
// that sing-box actually bound a port.
type NodeSingboxInbound struct {
	Port   int
	Type   string
	Tag    string
	Status string
}

// ListNodeSingboxInbounds returns the known listener lifecycle records keyed
// by port. Empty is an ordinary result for a probe that predates the report.
func (s *Store) ListNodeSingboxInbounds(nodeID string) ([]NodeSingboxInbound, error) {
	rows, err := s.db.Query(
		`SELECT port, type, tag, status FROM node_singbox_inbounds WHERE node_id = ? ORDER BY port`,
		nodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeSingboxInbound{}
	for rows.Next() {
		var inbound NodeSingboxInbound
		if err := rows.Scan(&inbound.Port, &inbound.Type, &inbound.Tag, &inbound.Status); err != nil {
			return nil, err
		}
		out = append(out, inbound)
	}
	return out, rows.Err()
}

// SetNodeSingboxInboundPending records a panel-created listener immediately,
// before the agent has had a chance to apply and report the new document.
func (s *Store) SetNodeSingboxInboundPending(nodeID string, inbound NodeSingboxInbound) error {
	_, err := s.db.Exec(
		`INSERT INTO node_singbox_inbounds (node_id, port, type, tag, status, reported, updated_at)
		 VALUES (?, ?, ?, ?, 'pending', 0, ?)
		 ON CONFLICT(node_id, port) DO UPDATE SET
		   type = excluded.type, tag = excluded.tag, status = 'pending',
		   reported = 0, updated_at = excluded.updated_at`,
		nodeID,
		inbound.Port,
		inbound.Type,
		inbound.Tag,
		now(),
	)
	return err
}

// DeleteNodeSingboxInbound drops a lifecycle record when the panel removes its
// listener from the desired document.
func (s *Store) DeleteNodeSingboxInbound(nodeID string, port int) error {
	_, err := s.db.Exec(`DELETE FROM node_singbox_inbounds WHERE node_id = ? AND port = ?`, nodeID, port)
	return err
}

// RestoreNodeSingboxInbound writes a lifecycle row back to the state a report
// implies: the probe lists the port, and the status is the one the caller
// derived from that report (running when the probe confirmed the port accepts a
// local connection, pending otherwise). It is the undo of a panel intent mark,
// needed by two paths: a push that never left the server must not leave a
// retirement behind, and an edit that keeps a port an earlier edit retired
// cancels that removal. Without the latter, a listener that is serving would
// read 删除中 until the next report — and no report is coming, because an edit
// that merged back to the same document changes nothing on the probe.
//
// Every caller restores a port the last report lists, hence `reported = 1`
// unconditionally: this is not how a listener the probe has never seen is
// recorded (`SetNodeSingboxInboundPending`).
func (s *Store) RestoreNodeSingboxInbound(nodeID string, inbound NodeSingboxInbound) error {
	_, err := s.db.Exec(
		`INSERT INTO node_singbox_inbounds (node_id, port, type, tag, status, reported, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?)
		 ON CONFLICT(node_id, port) DO UPDATE SET
		   type = excluded.type, tag = excluded.tag, status = excluded.status,
		   reported = 1, updated_at = excluded.updated_at`,
		nodeID,
		inbound.Port,
		inbound.Type,
		inbound.Tag,
		inbound.Status,
		now(),
	)
	return err
}

// PruneNodeSingboxInboundOptimism drops the panel's optimistic rows — listeners
// it declared but the probe has never reported (`reported = 0`) — for ports the
// document it just pushed no longer declares.
//
// A port move that is edited again, or an add a later edit takes back out,
// would otherwise park a 添加中 row forever: reconciliation only sweeps rows the
// probe has reported, and a port that never reached the probe never appears in
// a report. Rows with `reported = 1` are deliberately out of reach — they mirror
// the probe's file, and a listener the panel's document does not mention is
// one-sing.sh's, not a stale intent (§9.3 实现修订 2026-09-17h).
func (s *Store) PruneNodeSingboxInboundOptimism(nodeID string, declaredPorts []int) error {
	args := []any{nodeID}
	q := `DELETE FROM node_singbox_inbounds
	      WHERE node_id = ? AND reported = 0 AND status = 'pending'`
	if len(declaredPorts) > 0 {
		ph := make([]string, 0, len(declaredPorts))
		for _, port := range declaredPorts {
			ph = append(ph, "?")
			args = append(args, port)
		}
		q += ` AND port NOT IN (` + strings.Join(ph, ", ") + `)`
	}
	_, err := s.db.Exec(q, args...)
	return err
}

// SetNodeSingboxInboundDeleting records that the panel removed this listener
// from the desired document while the probe is still serving it. The row
// survives — marked `deleting` — until an agent report no longer lists the
// port: that is the confirmation that turns the panel's 删除中 into "gone",
// and it is why the row must not be dropped at edit time (dropping it made the
// still-listed listener render as plain `pending` — 添加中 — for the whole
// apply window).
//
// A port move uses this too: the listener is not deleted, but the document the
// panel just pushed no longer serves it, and the probe keeps it up until it
// applies that document.
//
// A listener the probe never acknowledged (reported=0: an add whose desired
// state has not been applied yet) has nothing to delete on the probe, so its
// row is dropped outright. A listener with no row at all still gets one, so
// the deleting state is visible even when reconciliation has never run.
func (s *Store) SetNodeSingboxInboundDeleting(nodeID string, inbound NodeSingboxInbound) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The normal case: a row the probe has acknowledged flips to deleting.
	res, err := tx.Exec(
		`UPDATE node_singbox_inbounds SET status = 'deleting', type = ?, tag = ?, updated_at = ?
		 WHERE node_id = ? AND port = ? AND reported = 1`,
		inbound.Type, inbound.Tag, now(), nodeID, inbound.Port,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return tx.Commit()
	}
	// No acknowledged row. A row the probe never reported (an add still in
	// flight) describes nothing on the probe — drop it rather than park a
	// `deleting` ghost nobody will ever confirm.
	drop, err := tx.Exec(
		`DELETE FROM node_singbox_inbounds WHERE node_id = ? AND port = ? AND reported = 0`,
		nodeID, inbound.Port,
	)
	if err != nil {
		return err
	}
	if n, err := drop.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return tx.Commit()
	}
	// A live listener reconciliation has never rowed still gets a row, so its
	// removal is visible as 删除中 from the first report on.
	if _, err := tx.Exec(
		`INSERT INTO node_singbox_inbounds (node_id, port, type, tag, status, reported, updated_at)
		 VALUES (?, ?, ?, ?, 'deleting', 1, ?)`,
		nodeID, inbound.Port, inbound.Type, inbound.Tag, now(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileNodeSingboxInbounds records the config that the agent actually saw.
// An older report that lacks a freshly added listener must not erase that
// pending row (reported=0); confirmed, missing rows from a prior report are
// removed. Only effective ports become running. effectiveKnown is false for an
// older agent that did not send the wire field, whose report must not demote a
// listener that a newer agent had already confirmed.
//
// A `deleting` row whose port is still reported keeps its state: the report
// describes the file as it is *now*, but the panel's delete has not been
// applied until the port stops appearing — flipping back to running there is
// exactly what turned a pending removal into a phantom 添加中/运行中 row. The
// same stale cleanup that removes confirmed-but-vanished rows is what finally
// deletes it.
func (s *Store) ReconcileNodeSingboxInbounds(
	nodeID string,
	inbounds []NodeSingboxInbound,
	effectivePorts []int,
	effectiveKnown bool,
) error {
	effective := map[int]bool{}
	for _, port := range effectivePorts {
		effective[port] = true
	}
	reported := map[int]bool{}
	for _, inbound := range inbounds {
		if inbound.Port > 0 {
			reported[inbound.Port] = true
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, inbound := range inbounds {
		if inbound.Port <= 0 {
			continue
		}
		status := "pending"
		if effective[inbound.Port] {
			status = "running"
		}
		if !effectiveKnown {
			if _, err := tx.Exec(
				`INSERT INTO node_singbox_inbounds (node_id, port, type, tag, status, reported, updated_at)
				 VALUES (?, ?, ?, ?, 'pending', 1, ?)
				 ON CONFLICT(node_id, port) DO UPDATE SET
				   type = excluded.type, tag = excluded.tag, reported = 1,
				   updated_at = excluded.updated_at`,
				nodeID,
				inbound.Port,
				inbound.Type,
				inbound.Tag,
				now(),
			); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO node_singbox_inbounds (node_id, port, type, tag, status, reported, updated_at)
			 VALUES (?, ?, ?, ?, ?, 1, ?)
			 ON CONFLICT(node_id, port) DO UPDATE SET
			   type = excluded.type, tag = excluded.tag,
			   status = CASE WHEN node_singbox_inbounds.status = 'deleting'
			                 THEN 'deleting' ELSE excluded.status END,
			   reported = 1, updated_at = excluded.updated_at`,
			nodeID,
			inbound.Port,
			inbound.Type,
			inbound.Tag,
			status,
			now(),
		); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT port FROM node_singbox_inbounds WHERE node_id = ? AND reported = 1`, nodeID)
	if err != nil {
		return err
	}
	stale := []int{}
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			rows.Close()
			return err
		}
		if !reported[port] {
			stale = append(stale, port)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, port := range stale {
		if _, err := tx.Exec(`DELETE FROM node_singbox_inbounds WHERE node_id = ? AND port = ?`, nodeID, port); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetNodeSingbox(nodeID string) (*NodeSingbox, error) {
	n := &NodeSingbox{}
	err := s.db.QueryRow(
		`SELECT node_id, version, desired_version, desired_uninstall, config_hash, status, last_error,
		        cert_pem, cert_sha256, cert_not_after, port, updated_at, local_config_hash,
		        extra_inbounds <> ''
		 FROM node_singbox WHERE node_id = ?`, nodeID,
	).Scan(&n.NodeID, &n.Version, &n.DesiredVersion, &n.DesiredUninstall, &n.ConfigHash, &n.Status, &n.LastError,
		&n.CertPEM, &n.CertSHA256, &n.CertNotAfter, &n.Port, &n.UpdatedAt, &n.LocalHash, &n.ExtrasPresent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func (s *Store) UpsertNodeSingbox(n *NodeSingbox) error {
	_, err := s.db.Exec(
		`INSERT INTO node_singbox
		 (node_id, version, desired_version, desired_uninstall, config_hash, status, last_error,
		  cert_pem, cert_sha256, cert_not_after, port, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   version = excluded.version, desired_version = excluded.desired_version,
		   desired_uninstall = excluded.desired_uninstall,
		   config_hash = excluded.config_hash, status = excluded.status,
		   last_error = excluded.last_error,
		   cert_pem = excluded.cert_pem, cert_sha256 = excluded.cert_sha256,
		   cert_not_after = excluded.cert_not_after, port = excluded.port,
		   updated_at = excluded.updated_at`,
		n.NodeID, n.Version, n.DesiredVersion, n.DesiredUninstall, n.ConfigHash, n.Status, n.LastError,
		n.CertPEM, n.CertSHA256, n.CertNotAfter, n.Port, now(),
	)
	return err
}

// SingboxDesiredVersionRefs counts node_singbox rows per non-empty
// desired_version. The panel uses it to annotate the cached release list
// (§9.2: deleting a version that nodes still point at needs a warning).
func (s *Store) SingboxDesiredVersionRefs() (map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT desired_version, COUNT(*) FROM node_singbox
		 WHERE desired_version <> '' GROUP BY desired_version`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var version string
		var n int
		if err := rows.Scan(&version, &n); err != nil {
			return nil, err
		}
		out[version] = n
	}
	return out, rows.Err()
}

// SingboxTarget is one node the panel already manages sing-box on (its
// desired_version is set). It is the input to the §9.2 batch update: a node
// without a desired state is not touched.
type SingboxTarget struct {
	NodeID         string
	Name           string
	Status         string
	Version        string
	DesiredVersion string
	Port           int
	LastError      string
}

// ListSingboxTargets lists every node whose desired_version is non-empty,
// joined with the node's name/status so a distribution batch can bucket its
// per-node outcome without a second query per node.
//
// A node the operator uninstalled sing-box from (desired_uninstall) is *not* a
// target: §9.5.4 rewrites desired_version on every target, which would silently
// reinstall the binary the operator just removed (§9.2 实现修订 2026-09-16).
func (s *Store) ListSingboxTargets() ([]SingboxTarget, error) {
	rows, err := s.db.Query(
		`SELECT n.id, n.name, n.status, sb.version, sb.desired_version, sb.port, sb.last_error
		 FROM node_singbox sb JOIN nodes n ON n.id = sb.node_id
		 WHERE sb.desired_version <> '' AND sb.desired_uninstall = 0 ORDER BY n.created_at, n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SingboxTarget{}
	for rows.Next() {
		var t SingboxTarget
		if err := rows.Scan(&t.NodeID, &t.Name, &t.Status, &t.Version, &t.DesiredVersion,
			&t.Port, &t.LastError); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetNodeSingboxFirewallHint mirrors the agent's firewall hint (§9.2) into
// node_singbox without disturbing the other columns (the agent owns the
// value; the empty string clears it once the port is allowed). Creates the row if absent.
func (s *Store) SetNodeSingboxFirewallHint(nodeID, hint string) error {
	_, err := s.db.Exec(
		`INSERT INTO node_singbox (node_id, firewall_hint, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   firewall_hint = excluded.firewall_hint, updated_at = excluded.updated_at`,
		nodeID, hint, now(),
	)
	return err
}

// GetNodeSingboxFirewallHint returns the stored §9.2 manual command (empty
// when the agent allowed the port or never attempted).
func (s *Store) GetNodeSingboxFirewallHint(nodeID string) (string, error) {
	var hint string
	err := s.db.QueryRow(
		`SELECT firewall_hint FROM node_singbox WHERE node_id = ?`, nodeID,
	).Scan(&hint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return hint, err
}

// GetNodeSingboxPasswordOverride returns the per-node anytls password
// override (§19.9). Since §10.1 实现修订 2026-09-17e it is only a *fallback*:
// a node's credential normally lives in its own config.json (the panel's
// copy of it, or the probe's report), and this column is read last — it has no
// writer in the panel any more, but a value set by hand must still win over
// minting a new password.
func (s *Store) GetNodeSingboxPasswordOverride(nodeID string) (string, error) {
	var pw string
	err := s.db.QueryRow(
		`SELECT password_override FROM node_singbox WHERE node_id = ?`, nodeID,
	).Scan(&pw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return pw, err
}

// SetNodeSingboxPasswordOverride stores the per-node anytls password
// override (§19.9). Creates the row if absent.
func (s *Store) SetNodeSingboxPasswordOverride(nodeID, password string) error {
	_, err := s.db.Exec(
		`INSERT INTO node_singbox (node_id, password_override, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   password_override = excluded.password_override, updated_at = excluded.updated_at`,
		nodeID, password, now(),
	)
	return err
}

// --- local sing-box discovery (design §9.3 实现修订 2026-09-17) ---

// NodeSingboxLocal is one node's discovery snapshot.
//
// ConfigJSON is the probe's own config.json, byte for byte: it is stored so an
// adoption can be applied later (the operator may click while neither the probe
// nor its report is in flight), and so the panel can show what fobe found
// without asking the probe again. It is a file an operator wrote by hand and
// may contain credentials, so both halves of this table are Cryptor
// ciphertext at rest (see SetNodeSingboxLocal).
type NodeSingboxLocal struct {
	LocalHash  string `json:"local_hash"`
	ConfigPath string `json:"config_path"`
	// AnytlsCerts maps an anytls inbound's port to its certificate PEM, as the
	// probe reported it. Config.json only names a path; a subscription client
	// needs the bytes to pin the server, so the probe ships them (§9.3 实现修订
	// 2026-09-17b).
	AnytlsCerts map[int]string `json:"anytls_certs,omitempty"`
	// EffectiveInboundPorts is the agent's local TCP acknowledgement for each
	// listener. Missing means an older agent rather than a failed listener.
	EffectiveInboundPorts []int  `json:"effective_inbound_ports,omitempty"`
	InboundChecksKnown    bool   `json:"inbound_checks_known,omitempty"`
	LocalVersion          string `json:"local_version"`
	LocalRunning          bool   `json:"local_running"`
	LocalUnit             bool   `json:"local_unit_active"`
	LocalUnitOK           bool   `json:"local_unit_known"`
	LocalPresent          bool   `json:"local_present"`
	ConfigJSON            string `json:"config_json"`
	Error                 string `json:"error"`
}

// GetNodeSingboxLocal reads the discovery snapshot (metadata + config bytes),
// or ErrNotFound when the row or the snapshot is absent.
//
// Both halves are stored inside one encrypted blob: the metadata names a
// version and a running state, the payload is the operator's file. Keeping
// them together means "there is a report" has exactly one representation —
// no half-written row can claim a version without bytes.
func (s *Store) GetNodeSingboxLocal(nodeID string, crypt *security.Cryptor) (NodeSingboxLocal, error) {
	var out NodeSingboxLocal
	var blob string
	err := s.db.QueryRow(
		`SELECT local_config FROM node_singbox WHERE node_id = ?`, nodeID,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	plain, err := DecryptSettingValue(crypt, blob)
	if err != nil {
		return out, fmt.Errorf("decrypt local config: %w", err)
	}
	if strings.TrimSpace(plain) == "" {
		return out, ErrNotFound
	}
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		return out, fmt.Errorf("parse local config snapshot: %w", err)
	}
	return out, nil
}

// SetNodeSingboxLocal stores the discovery snapshot. local_config is encrypted
// at rest: it is the operator's own file and it may carry credentials in
// clear (one-sing.sh writes SS passwords and VLESS UUIDs into it).
func (s *Store) SetNodeSingboxLocal(nodeID string, local NodeSingboxLocal, crypt *security.Cryptor) error {
	raw, err := json.Marshal(local)
	if err != nil {
		return fmt.Errorf("marshal local config snapshot: %w", err)
	}
	blob, err := EncryptSettingValue(crypt, string(raw))
	if err != nil {
		return fmt.Errorf("encrypt local config: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO node_singbox (node_id, local_config_hash, local_config, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   local_config_hash = excluded.local_config_hash, local_config = excluded.local_config,
		   updated_at = excluded.updated_at`,
		nodeID, local.LocalHash, blob, now(),
	)
	return err
}

// GetNodeSingboxExtraInbounds returns the adopted inbounds, decrypted.
func (s *Store) GetNodeSingboxExtraInbounds(nodeID string, crypt *security.Cryptor) ([]singbox.ExtraInbound, error) {
	var blob string
	err := s.db.QueryRow(
		`SELECT extra_inbounds FROM node_singbox WHERE node_id = ?`, nodeID,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := DecryptSettingValue(crypt, blob)
	if err != nil {
		return nil, fmt.Errorf("decrypt adopted inbounds: %w", err)
	}
	if strings.TrimSpace(plain) == "" {
		return nil, nil
	}
	var out []singbox.ExtraInbound
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		return nil, fmt.Errorf("parse adopted inbounds: %w", err)
	}
	return out, nil
}

// SetNodeSingboxExtraInbounds stores the adopted inbounds (Cryptor ciphertext:
// the set carries UUIDs, passwords and a REALITY private key).
func (s *Store) SetNodeSingboxExtraInbounds(nodeID string, extras []singbox.ExtraInbound, crypt *security.Cryptor) error {
	plain := ""
	if len(extras) > 0 {
		raw, err := json.Marshal(extras)
		if err != nil {
			return fmt.Errorf("marshal adopted inbounds: %w", err)
		}
		plain = string(raw)
	}
	blob, err := EncryptSettingValue(crypt, plain)
	if err != nil {
		return fmt.Errorf("encrypt adopted inbounds: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO node_singbox (node_id, extra_inbounds, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   extra_inbounds = excluded.extra_inbounds, updated_at = excluded.updated_at`,
		nodeID, blob, now(),
	)
	return err
}

// --- subscriptions & templates (design §10; rendering lands in M3) ---

// CreateSubscription stores a new subscription. tokenEnc is the Cryptor
// ciphertext of the plaintext token (design §10 实现修订 2026-09-16: the panel
// re-shows the URL on demand instead of only once); the hash remains the only
// thing /sub/<token> looks up.
func (s *Store) CreateSubscription(id, name, tokenHash, tokenEnc string) error {
	_, err := s.db.Exec(
		`INSERT INTO subscriptions (id, name, token_hash, token_enc, enabled, created_at) VALUES (?, ?, ?, ?, 1, ?)`,
		id, name, tokenHash, tokenEnc, now(),
	)
	return err
}

func (s *Store) GetSubscriptionByTokenHash(tokenHash string) (id, name string, enabled bool, err error) {
	err = s.db.QueryRow(`SELECT id, name, enabled FROM subscriptions WHERE token_hash = ?`, tokenHash).
		Scan(&id, &name, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, ErrNotFound
	}
	return
}

// SetSubscriptionNodes is the pre-§10.2 binding API (node ids only). It now
// writes through subscription_entries — the single source of truth — and only
// owns the direct half, so relay entries bound by the reconciler survive a
// legacy save (design §10.2).
func (s *Store) SetSubscriptionNodes(subID string, nodeIDs []string) error {
	return s.SetSubscriptionDirectNodes(subID, nodeIDs)
}

func (s *Store) SubscriptionNodeIDs(subID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT node_id FROM subscription_nodes WHERE subscription_id = ? ORDER BY node_id`, subID)
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

// InsertSubAccess appends one /sub/<token> hit. reason is ” when the fetch was
// served, otherwise a refusal code (see the subAccess* constants in httpapi) —
// a 404 that the operator cannot see anywhere is a 404 they cannot debug
// (§10 实现修订 2026-09-16).
func (s *Store) InsertSubAccess(subID, ip, ua, reason string) {
	s.db.Exec(`INSERT INTO sub_access_logs (subscription_id, ts, ip, ua, reason) VALUES (?, ?, ?, ?, ?)`,
		subID, now(), ip, ua, reason)
}

// Subscription is one subscription row (design §10). The token hash is never
// exposed over HTTP; the plaintext itself lives on encrypted in TokenEnc so
// the panel can show the URL again at any time (§10 实现修订 2026-09-16) —
// it is empty for rows created before that revision.
type Subscription struct {
	ID         string
	Name       string
	TokenHash  string
	TokenEnc   string // AES-GCM ciphertext; '' = unrecoverable (legacy row)
	Enabled    bool
	UAFilter   string         // comma-separated UA substrings; empty = allow all (§10)
	TemplateID sql.NullString // nullable: empty → built-in default template
	// Format pins the output format (§10 实现修订 2026-09-16): 'singbox' /
	// 'clash', or '' for auto (bound template's format, else the request).
	Format    string
	CreatedAt int64
}

const subscriptionCols = `id, name, token_hash, token_enc, enabled, ua_filter, template_id, format, created_at`

func scanSubscription(rs rowScanner) (*Subscription, error) {
	sub := &Subscription{}
	var enabled int
	if err := rs.Scan(&sub.ID, &sub.Name, &sub.TokenHash, &sub.TokenEnc, &enabled, &sub.UAFilter, &sub.TemplateID, &sub.Format, &sub.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sub.Enabled = enabled != 0
	return sub, nil
}

func (s *Store) ListSubscriptions() ([]Subscription, error) {
	rows, err := s.db.Query(`SELECT ` + subscriptionCols + ` FROM subscriptions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

func (s *Store) GetSubscription(id string) (*Subscription, error) {
	return scanSubscription(s.db.QueryRow(
		`SELECT `+subscriptionCols+` FROM subscriptions WHERE id = ?`, id))
}

// GetSubscriptionByToken resolves a subscription from a token hash for
// /sub/<token> rendering (design §10: the DB only ever sees the hash).
func (s *Store) GetSubscriptionByToken(tokenHash string) (*Subscription, error) {
	return scanSubscription(s.db.QueryRow(
		`SELECT `+subscriptionCols+` FROM subscriptions WHERE token_hash = ?`, tokenHash))
}

// RotateSubscriptionToken replaces the token hash and its ciphertext
// (design §10 一键轮换): old URLs stop resolving, and the new URL stays
// copyable from the panel afterwards.
func (s *Store) RotateSubscriptionToken(id, tokenHash, tokenEnc string) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET token_hash = ?, token_enc = ? WHERE id = ?`, tokenHash, tokenEnc, id)
	return err
}

func (s *Store) SetSubscriptionEnabled(id string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET enabled = ? WHERE id = ?`, b2i(enabled), id)
	return err
}

func (s *Store) SetSubscriptionMeta(id, name string, templateID *string) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET name = ?, template_id = ? WHERE id = ?`, name, templateID, id)
	return err
}

// SetSubscriptionUAFilter updates the §10 UA allow-list (comma-separated
// substrings, already normalized by the caller; empty = every client allowed).
func (s *Store) SetSubscriptionUAFilter(id, filter string) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET ua_filter = ? WHERE id = ?`, filter, id)
	return err
}

// SetSubscriptionFormat pins the output format (§10 实现修订 2026-09-16);
// ” means auto. Validated by the caller (validFormat).
func (s *Store) SetSubscriptionFormat(id, format string) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET format = ? WHERE id = ?`, format, id)
	return err
}

func (s *Store) DeleteSubscription(id string) error {
	_, err := s.db.Exec(`DELETE FROM subscriptions WHERE id = ?`, id)
	return err
}

// SubAccess is one /sub/<token> hit (time/IP/UA + outcome, design §10 访问日志).
type SubAccess struct {
	TS int64  `json:"ts"`
	IP string `json:"ip"`
	UA string `json:"ua"`
	// Reason is '' for a served fetch, else the refusal code.
	Reason string `json:"reason"`
}

func (s *Store) ListSubAccess(subID string, limit int) ([]SubAccess, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT ts, ip, ua, reason FROM sub_access_logs WHERE subscription_id = ? ORDER BY id DESC LIMIT ?`,
		subID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubAccess{}
	for rows.Next() {
		var a SubAccess
		if err := rows.Scan(&a.TS, &a.IP, &a.UA, &a.Reason); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- templates (design §10: one complete config template per format) ---

type Template struct {
	ID        string
	Name      string
	Format    string // singbox | clash
	Content   string // must contain {{nodes}}; routing rules are written in directly (§10)
	CreatedAt int64
	UpdatedAt int64
}

func (s *Store) ListTemplates() ([]Template, error) {
	rows, err := s.db.Query(
		`SELECT id, name, format, content, created_at, updated_at FROM templates ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.Format, &t.Content, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTemplate(id string) (*Template, error) {
	t := &Template{}
	err := s.db.QueryRow(
		`SELECT id, name, format, content, created_at, updated_at FROM templates WHERE id = ?`, id,
	).Scan(&t.ID, &t.Name, &t.Format, &t.Content, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) InsertTemplate(t *Template) error {
	t.CreatedAt, t.UpdatedAt = now(), now()
	_, err := s.db.Exec(
		`INSERT INTO templates (id, name, format, content, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Format, t.Content, t.CreatedAt, t.UpdatedAt)
	return err
}

func (s *Store) UpdateTemplate(t *Template) error {
	t.UpdatedAt = now()
	_, err := s.db.Exec(
		`UPDATE templates SET name = ?, format = ?, content = ?, updated_at = ? WHERE id = ?`,
		t.Name, t.Format, t.Content, t.UpdatedAt, t.ID)
	return err
}

func (s *Store) DeleteTemplate(id string) error {
	_, err := s.db.Exec(`DELETE FROM templates WHERE id = ?`, id)
	return err
}

func (s *Store) BackupTo(path string) error {
	// VACUUM INTO produces a consistent snapshot (design §17).
	_, err := s.db.Exec(fmt.Sprintf(`VACUUM INTO %q`, path))
	if err != nil {
		return fmt.Errorf("vacuum into %s: %w", path, err)
	}
	return nil
}

// backupKeep is how many snapshots survive pruning (design §17: 3, the
// operator's call — sample tables are pruned to 7 days but traffic_daily is
// permanent, so the db only grows with node count; keep the pool small).
// The daily scheduler and the ad-hoc BackupNow share one naming scheme
// (fobe-*.db), so they prune from the same pool.
const backupKeep = 3

// BackupNow writes one timestamped snapshot into <db dir>/backup and prunes
// old ones. Used at boot (scheduler fires it immediately) and before schema
// migrations — the snapshot that saves you is the one taken *before* the
// thing that breaks.
func (s *Store) BackupNow() error {
	return s.BackupDir(filepath.Join(filepath.Dir(s.path), "backup"))
}

// BackupDir writes one timestamped snapshot into dir and prunes to backupKeep.
func (s *Store) BackupDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}
	// Millisecond precision: boot fires two snapshots back-to-back (the
	// migration snapshot inside Open, then the scheduler's boot run) and a
	// 1s timestamp collided. VACUUM INTO refuses to overwrite, so clear any
	// same-named leftover first — a collision means our own earlier attempt,
	// and the fresh snapshot is strictly better than the stale one.
	name := fmt.Sprintf("fobe-%s.db", time.Now().Format("20060102-150405.000"))
	target := filepath.Join(dir, name)
	_ = os.Remove(target)
	if err := s.BackupTo(target); err != nil {
		return err
	}
	return PruneBackups(dir, backupKeep)
}

// PruneBackups removes the oldest fobe-*.db files in dir, keeping the newest
// keep. Errors on individual files are non-fatal (best effort).
func PruneBackups(dir string, keep int) error {
	entries, err := filepath.Glob(filepath.Join(dir, "fobe-*.db"))
	if err != nil || len(entries) <= keep {
		return err
	}
	sort.Strings(entries)
	for _, old := range entries[:len(entries)-keep] {
		if err := os.Remove(old); err != nil {
			return err
		}
	}
	return nil
}

// SoleAnytlsPortInDoc returns the single anytls port a config document declares,
// or 0 when there is none or several. It is exported so the HTTP layer answers
// "which listener is this document's anytls" the same way the store does.
func SoleAnytlsPortInDoc(doc string) int { return soleAnytlsPortInDoc(doc) }

// SolePanelPort returns the one anytls port when a panel-edited config has
// exactly one, 0 otherwise.
//
// It exists so the panel's "接管" write does not depend on reading the config
// back (the report arrives at the agent's cadence, so the document it just
// pushed is ahead of what anyone can read): the operator ticked a listener,
// and if that listener is the file's only anytls inbound it is now the node's
// own (§9.3 实现修订 2026-09-17b).
func (s *Store) SolePanelPort(nodeID string) int {
	local, err := s.GetNodeSingboxLocal(nodeID, nil)
	if err != nil || local.ConfigJSON == "" {
		return 0
	}
	return soleAnytlsPortInDoc(local.ConfigJSON)
}

// Same answers "is this snapshot identical to that one" without using ==, which
// a map field makes illegal.
func (l NodeSingboxLocal) Same(other NodeSingboxLocal) bool {
	if l.LocalHash != other.LocalHash || l.ConfigPath != other.ConfigPath ||
		l.LocalVersion != other.LocalVersion || l.LocalRunning != other.LocalRunning ||
		l.LocalUnit != other.LocalUnit || l.LocalUnitOK != other.LocalUnitOK ||
		l.LocalPresent != other.LocalPresent || l.ConfigJSON != other.ConfigJSON ||
		l.InboundChecksKnown != other.InboundChecksKnown || l.Error != other.Error ||
		len(l.AnytlsCerts) != len(other.AnytlsCerts) ||
		len(l.EffectiveInboundPorts) != len(other.EffectiveInboundPorts) {
		return false
	}
	for port, pem := range l.AnytlsCerts {
		if other.AnytlsCerts[port] != pem {
			return false
		}
	}
	for i, port := range l.EffectiveInboundPorts {
		if other.EffectiveInboundPorts[i] != port {
			return false
		}
	}
	return true
}

// soleAnytlsPortInDoc mirrors the httpapi helper; it lives here so the store can
// answer without importing the HTTP layer (which imports it).
func soleAnytlsPortInDoc(doc string) int {
	var parsed struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		return 0
	}
	port := 0
	for _, in := range parsed.Inbounds {
		if t, _ := in["type"].(string); t != "anytls" {
			continue
		}
		switch n := in["listen_port"].(type) {
		case float64:
			if p := int(n); p > 0 {
				if port != 0 {
					return 0
				}
				port = p
			}
		}
	}
	return port
}

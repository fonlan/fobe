package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// --- settings (key/value, optional AES-GCM at rest, design §4.4) ---

// GetSetting returns the raw stored value; ErrNotFound when unset.
func (s *Store) GetSetting(key string) (string, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return val, err
}

// GetSettingValue returns the stored value plus the encrypted-at-rest flag so
// callers with a Cryptor can decrypt sensitive keys (design §4.4: outbound
// integrations read bot tokens / webhook secrets without httpapi).
func (s *Store) GetSettingValue(key string) (value string, encrypted bool, err error) {
	var enc int
	err = s.db.QueryRow(`SELECT value, encrypted FROM settings WHERE key = ?`, key).Scan(&value, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return value, enc != 0, err
}

// SetSetting upserts; encrypted marks the value as ciphertext (see security.Cryptor).
func (s *Store) SetSetting(key, value string, encrypted bool) error {
	e := 0
	if encrypted {
		e = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value, encrypted) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, encrypted = excluded.encrypted`,
		key, value, e,
	)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// DeleteSetting removes a key entirely — used when a channel is unbound
// (feishu QR/config clear), where an empty row would still read as "set".
func (s *Store) DeleteSetting(key string) error {
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete setting %s: %w", key, err)
	}
	return nil
}

// --- registration tokens (design §4.2: single-use, TTL 30min) ---

type RegToken struct {
	ID        int64
	Name      string // node name applied at registration (required, panel-enforced)
	Note      string
	CreatedAt int64
	ExpiresAt int64
	UsedAt    sql.NullInt64
	UsedBy    sql.NullString
}

// CreateRegToken stores the hash of a token; the plaintext never touches the DB.
func (s *Store) CreateRegToken(tokenHash, name, note string, ttlSeconds int64) error {
	_, err := s.db.Exec(
		`INSERT INTO reg_tokens (token_hash, name, note, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		tokenHash, name, note, now(), now()+ttlSeconds,
	)
	if err != nil {
		return fmt.Errorf("create reg token: %w", err)
	}
	return nil
}

// ConsumeRegToken atomically marks the token (by hash) used; returns the
// node name and note bound to it. Caller passes the token hash, not the plaintext.
func (s *Store) ConsumeRegToken(tokenHash string) (name, note string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	var id int64
	var expires sql.NullInt64
	err = tx.QueryRow(`SELECT id, name, note, expires_at FROM reg_tokens
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, tokenHash, now(),
	).Scan(&id, &name, &note, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("look up reg token: %w", err)
	}
	if _, err := tx.Exec(`UPDATE reg_tokens SET used_at = ? WHERE id = ?`, now(), id); err != nil {
		return "", "", fmt.Errorf("consume reg token: %w", err)
	}
	return name, note, tx.Commit()
}

func (s *Store) ListRegTokens(activeOnly bool) ([]RegToken, error) {
	q := `SELECT id, name, note, created_at, expires_at, used_at, used_by FROM reg_tokens`
	if activeOnly {
		q += ` WHERE used_at IS NULL AND expires_at > ` + fmt.Sprint(now())
	}
	q += ` ORDER BY created_at DESC LIMIT 100`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegToken{}
	for rows.Next() {
		var t RegToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Note, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt, &t.UsedBy); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- nodes ---

type Node struct {
	ID            string
	Name          string
	MachineID     string
	Status        string
	Note          string
	CreatedAt     int64
	LastSeen      sql.NullInt64
	AgentVersion  string
	OS            string
	Arch          string
	Kernel        string
	DistroID      string
	DistroVersion string
	Hostname      string
	CPUCores      int
	PrimaryIP     string
	CountryCode   string
	// CountryManual is the §14 operator pin: when set, hub never re-derives
	// country_code from the primary IP, same contract as node_ips.manual_primary.
	CountryManual bool
	TZ            string
	Caps          json.RawMessage
	// Agent self-update bookkeeping (design §5.5).
	AgentTargetVersion   string
	AgentUpdateState     string
	AgentUpdateAttempts  int
	AgentUpdateError     string
	AgentUpdatePlannedAt int64
	AgentUpdateDoneAt    int64
}

// nodeColumns is the single column list every node query shares. Adding a
// column in one place and forgetting the others used to be the easy mistake
// (scanNode would then fail at runtime, not at compile time).
const nodeColumns = `id, name, machine_id, status, note, created_at, last_seen, agent_version,
		        os, arch, kernel, distro_id, distro_version, hostname, cpu_cores, primary_ip, country_code, country_manual, tz, caps,
		        agent_target_version, agent_update_state, agent_update_attempts, agent_update_error,
		        agent_update_planned_at, agent_update_done_at`

func (s *Store) CreateNode(n *Node, secretHash string) error {
	_, err := s.db.Exec(
		`INSERT INTO nodes (id, name, machine_id, node_secret_hash, status, note, created_at,
		 agent_version, os, arch, kernel, distro_id, distro_version, hostname, cpu_cores, primary_ip, tz, caps)
		 VALUES (?, ?, ?, ?, 'offline', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.MachineID, secretHash, n.Note, now(),
		n.AgentVersion, n.OS, n.Arch, n.Kernel, n.DistroID, n.DistroVersion,
		n.Hostname, n.CPUCores, n.PrimaryIP, n.TZ, "{}",
	)
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	return nil
}

func (s *Store) GetNodeByMachineID(machineID string) (*Node, error) {
	return s.scanNode(s.db.QueryRow(
		`SELECT `+nodeColumns+` FROM nodes WHERE machine_id = ?`, machineID,
	))
}

func (s *Store) GetNode(id string) (*Node, error) {
	return s.scanNode(s.db.QueryRow(
		`SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id,
	))
}

func (s *Store) ListNodes() ([]Node, error) {
	rows, err := s.db.Query(
		`SELECT ` + nodeColumns + ` FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := s.scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) scanNode(rs rowScanner) (*Node, error) {
	n := &Node{}
	var caps string
	err := rs.Scan(&n.ID, &n.Name, &n.MachineID, &n.Status, &n.Note, &n.CreatedAt, &n.LastSeen,
		&n.AgentVersion, &n.OS, &n.Arch, &n.Kernel, &n.DistroID, &n.DistroVersion, &n.Hostname, &n.CPUCores,
		&n.PrimaryIP, &n.CountryCode, &n.CountryManual, &n.TZ, &caps,
		&n.AgentTargetVersion, &n.AgentUpdateState, &n.AgentUpdateAttempts, &n.AgentUpdateError,
		&n.AgentUpdatePlannedAt, &n.AgentUpdateDoneAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan node: %w", err)
	}
	if caps == "" {
		caps = "{}"
	}
	n.Caps = json.RawMessage(caps)
	return n, nil
}

// --- agent self-update bookkeeping (design §5.5) ---

// SetAgentUpdatePlanned stores the target version and the stagger deadline the
// server just handed to this agent. done_at is cleared only when the target
// changes, so the panel keeps showing the last completed update's timestamp.
func (s *Store) SetAgentUpdatePlanned(id, target string, plannedAt int64) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET agent_target_version = ?, agent_update_planned_at = ?,
		   agent_update_done_at = CASE WHEN agent_target_version = ? THEN agent_update_done_at ELSE 0 END
		 WHERE id = ?`,
		target, plannedAt, target, id,
	)
	return err
}

// RecordAgentUpdate stores what the agent reported about an attempt. A target
// change resets the attempt counter, so a new server version starts from a
// clean slate instead of inheriting an old exhaustion.
func (s *Store) RecordAgentUpdate(id, target, state string, attempts int, lastErr string, doneAt int64) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET
		   agent_update_attempts = CASE WHEN agent_target_version = ? THEN ? ELSE ? END,
		   agent_update_state    = ?,
		   agent_update_error    = ?,
		   agent_update_done_at  = CASE WHEN ? > 0 THEN ? ELSE agent_update_done_at END,
		   agent_target_version  = CASE WHEN ? = '' THEN agent_target_version ELSE ? END
		 WHERE id = ?`,
		target, attempts, attempts, state, lastErr, doneAt, doneAt, target, target, id,
	)
	return err
}

// ClearAgentUpdate is the operator escape hatch (POST /api/nodes/{id}/agent/retry):
// forget the attempts and the previous error, and let the next handshake hand
// out a fresh stagger deadline.
func (s *Store) ClearAgentUpdate(id string) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET agent_update_attempts = 0, agent_update_error = '',
		   agent_update_state = '', agent_update_planned_at = 0 WHERE id = ?`, id,
	)
	return err
}

// StaleAgentUpdates lists nodes that were told to move to a target and still
// report something else (design §5.5: 分发后 15 分钟仍未收敛). Nodes already
// in a terminal state are excluded — they are surfaced by their own alert and
// are not "still converging".
func (s *Store) StaleAgentUpdates(deadline int64) ([]Node, error) {
	rows, err := s.db.Query(
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE agent_target_version != '' AND agent_target_version != agent_version
		   AND agent_update_planned_at > 0 AND agent_update_planned_at <= ?
		   AND agent_update_state NOT IN ('failed', 'suppressed', 'unsupported')
		 ORDER BY created_at`, deadline,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := s.scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// GetNodeSecretHash returns the stored argon2/bcrypt hash of node_secret.
func (s *Store) GetNodeSecretHash(id string) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT node_secret_hash FROM nodes WHERE id = ?`, id).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

// TouchNode updates liveness after a frame; empty version keeps the current one.
func (s *Store) TouchNode(id string, version string, at int64) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET status = 'online', last_seen = ?,
		 agent_version = CASE WHEN ? = '' THEN agent_version ELSE ? END WHERE id = ?`,
		at, version, version, id,
	)
	return err
}

func (s *Store) MarkNodeOffline(id string) error {
	_, err := s.db.Exec(`UPDATE nodes SET status = 'offline' WHERE id = ?`, id)
	return err
}

func (s *Store) UpdateNodeInfo(id string, os, arch, kernel, distroID, distroVersion, hostname, tz string, cpuCores int) error {
	_, err := s.db.Exec(
		`UPDATE nodes SET os = ?, arch = ?, kernel = ?, distro_id = ?, distro_version = ?,
		   hostname = ?, cpu_cores = ?, tz = ? WHERE id = ?`,
		os, arch, kernel, distroID, distroVersion, hostname, cpuCores, tz, id,
	)
	return err
}

func (s *Store) RenameNode(id, name string) error {
	_, err := s.Exec(`UPDATE nodes SET name = ? WHERE id = ?`, name, id)
	return err
}

func (s *Store) SetNodeNote(id, note string) error {
	_, err := s.Exec(`UPDATE nodes SET note = ? WHERE id = ?`, note, id)
	return err
}

func (s *Store) DeleteNode(id string) error {
	_, err := s.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	return err
}

// SetNodePrimaryIP re-pins the primary address. country_manual=1 (§14 手动
// 国旗) keeps the stored country_code no matter what the caller passes — the
// pin is a property of the row, not of every caller remembering the guard.
func (s *Store) SetNodePrimaryIP(id, ip, country string) error {
	_, err := s.Exec(`UPDATE nodes SET primary_ip = ?,
		country_code = CASE WHEN country_manual = 1 THEN country_code ELSE ? END WHERE id = ?`, ip, country, id)
	return err
}

// SetNodeCountry writes the §14 display country; manual=true pins it against
// geoip re-derivation (the flag survives primary-IP changes until cleared).
func (s *Store) SetNodeCountry(id, code string, manual bool) error {
	_, err := s.Exec(`UPDATE nodes SET country_code = ?, country_manual = ? WHERE id = ?`, code, manual, id)
	return err
}

func (s *Store) SetNodeCaps(id string, caps json.RawMessage) error {
	_, err := s.Exec(`UPDATE nodes SET caps = ? WHERE id = ?`, string(caps), id)
	return err
}

// Exec exposes a plain exec for the few admin/CLI paths that need it.
func (s *Store) Exec(query string, args ...any) (sql.Result, error) {
	return s.db.Exec(query, args...)
}

// --- node_ips (design §14: full address report) ---

// ReplaceNodeIPs swaps in the full reported set, preserving is_primary
// for addresses that survive, keeping a §14 manual primary pick alive across
// the rebuild, and pinning the first public IPv4 as primary when no primary
// exists.
func (s *Store) ReplaceNodeIPs(nodeID string, ips []IPRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// §14 手动主 IP: remember the pinned address; if the agent still reports
	// it, it stays primary no matter what the auto heuristic would pick.
	var manualIP string
	_ = tx.QueryRow(
		`SELECT ip FROM node_ips WHERE node_id = ? AND manual_primary = 1 LIMIT 1`, nodeID,
	).Scan(&manualIP)
	manualKept := false
	for _, ip := range ips {
		if ip.IP == manualIP {
			manualKept = true
			break
		}
	}

	if _, err := tx.Exec(`DELETE FROM node_ips WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	for _, ip := range ips {
		manual := 0
		if manualKept && ip.IP == manualIP {
			manual = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO node_ips (node_id, ip, family, scope, is_primary, manual_primary, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			nodeID, ip.IP, ip.Family, ip.Scope, b2i(ip.IsPrimary), manual, now(),
		); err != nil {
			return err
		}
	}
	if manualKept {
		// the manual pick wins over the auto heuristic
		if _, err := tx.Exec(`UPDATE node_ips SET is_primary = 0 WHERE node_id = ?`, nodeID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE node_ips SET is_primary = 1 WHERE node_id = ? AND ip = ?`, nodeID, manualIP); err != nil {
			return err
		}
		// nodes.primary_ip is what the list API, node cards and subscription
		// rendering actually read; node_ips flags alone never reach them. A pin
		// made before the guard existed (or drift left by an older build that
		// overwrote primary_ip with the agent's suggestion) must heal here on
		// the next full report, otherwise the panel shows the wrong "主 IP"
		// forever — the drift is in the reported set, so the hub never re-pins.
		if _, err := tx.Exec(`UPDATE nodes SET primary_ip = ? WHERE id = ?`, manualIP, nodeID); err != nil {
			return err
		}
		return tx.Commit()
	}
	// keep exactly one primary: first public v4, else first public v6, else first row
	if _, err := tx.Exec(`UPDATE node_ips SET is_primary = 0 WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_ips SET is_primary = 1 WHERE node_id = ? AND rowid = (
		SELECT rowid FROM node_ips WHERE node_id = ?
		ORDER BY is_primary DESC, family = 4 DESC, scope = 'public' DESC, ip LIMIT 1)`, nodeID, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetManualPrimary pins one of the node's reported addresses as primary
// (§14 手动主 IP). The caller must have verified the IP belongs to the node.
// manual_primary survives later ReplaceNodeIPs rebuilds, so the agent cannot
// unpick it on the next full report.
func (s *Store) SetManualPrimary(nodeID, ip string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE node_ips SET manual_primary = CASE WHEN ip = ? THEN 1 ELSE 0 END WHERE node_id = ?`,
		ip, nodeID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_ips SET is_primary = 0 WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_ips SET is_primary = 1 WHERE node_id = ? AND ip = ?`, nodeID, ip); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nodes SET primary_ip = ? WHERE id = ?`, ip, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

type IPRow struct {
	IP        string
	Family    int
	Scope     string
	IsPrimary bool
}

func (s *Store) ListNodeIPs(nodeID string) ([]IPRow, error) {
	rows, err := s.db.Query(
		`SELECT ip, family, scope, is_primary FROM node_ips WHERE node_id = ? ORDER BY family, ip`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IPRow{}
	for rows.Next() {
		var r IPRow
		var prim int
		if err := rows.Scan(&r.IP, &r.Family, &r.Scope, &prim); err != nil {
			return nil, err
		}
		r.IsPrimary = prim != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

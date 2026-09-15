package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// --- users (single row, design §4.1) ---

var ErrNotFound = errors.New("not found")

type User struct {
	ID           int64
	PasswordHash string
	MustChange   bool
}

func (s *Store) GetUser() (*User, error) {
	u := &User{}
	var must int
	err := s.db.QueryRow(`SELECT id, password_hash, must_change FROM users ORDER BY id LIMIT 1`).
		Scan(&u.ID, &u.PasswordHash, &must)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	u.MustChange = must != 0
	return u, nil
}

func (s *Store) CreateUser(passwordHash string, mustChange bool) (int64, error) {
	m := 0
	if mustChange {
		m = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO users (password_hash, must_change, created_at) VALUES (?, ?, ?)`,
		passwordHash, m, now(),
	)
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) UpdatePassword(userID int64, passwordHash string, mustChange bool) error {
	m := 0
	if mustChange {
		m = 1
	}
	_, err := s.db.Exec(`UPDATE users SET password_hash = ?, must_change = ? WHERE id = ?`,
		passwordHash, m, userID)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	return nil
}

// --- sessions (design §4.1) ---

type Session struct {
	ID        string
	CreatedAt int64
	LastSeen  int64
	UA        string
	IP        string
	Revoked   bool
}

func (s *Store) CreateSession(id, ua, ip string, createdAt int64) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, created_at, last_seen, ua, ip) VALUES (?, ?, ?, ?, ?)`,
		id, createdAt, createdAt, ua, ip,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (s *Store) GetSession(id string) (*Session, error) {
	sess := &Session{}
	var revoked int
	err := s.db.QueryRow(`SELECT id, created_at, last_seen, ua, ip, revoked FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.CreatedAt, &sess.LastSeen, &sess.UA, &sess.IP, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	sess.Revoked = revoked != 0
	return sess, nil
}

func (s *Store) TouchSession(id string, at int64) {
	s.db.Exec(`UPDATE sessions SET last_seen = ? WHERE id = ?`, at, id)
}

func (s *Store) RevokeSession(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET revoked = 1 WHERE id = ?`, id)
	return err
}

func (s *Store) RevokeAllSessions() (int64, error) {
	res, err := s.db.Exec(`UPDATE sessions SET revoked = 1 WHERE revoked = 0`)
	if err != nil {
		return 0, fmt.Errorf("revoke all sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) ListSessions(activeOnly bool) ([]Session, error) {
	q := `SELECT id, created_at, last_seen, ua, ip, revoked FROM sessions`
	if activeOnly {
		q += ` WHERE revoked = 0`
	}
	q += ` ORDER BY last_seen DESC LIMIT 200`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var sess Session
		var revoked int
		if err := rows.Scan(&sess.ID, &sess.CreatedAt, &sess.LastSeen, &sess.UA, &sess.IP, &revoked); err != nil {
			return nil, err
		}
		sess.Revoked = revoked != 0
		out = append(out, sess)
	}
	return out, rows.Err()
}

// --- login-failure blacklist (design §4.3) ---

type BlacklistEntry struct {
	IP        string
	Reason    string
	FailCount int
	CreatedAt int64
	ExpiresAt int64 // 0 = never
}

// IsBlacklisted returns the ACTIVE block for ip (expires_at in the future),
// or nil. Rows with expires_at = 0 are just failure counters, not blocks.
func (s *Store) IsBlacklisted(ip string) (*BlacklistEntry, error) {
	e := &BlacklistEntry{}
	var expires sql.NullInt64
	err := s.db.QueryRow(
		`SELECT ip, reason, fail_count, created_at, COALESCE(expires_at, 0) FROM ip_blacklist
		 WHERE ip = ? AND expires_at > ?`, ip, now(),
	).Scan(&e.IP, &e.Reason, &e.FailCount, &e.CreatedAt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check blacklist: %w", err)
	}
	e.ExpiresAt = expires.Int64
	return e, nil
}

// RecordLoginFail bumps the failure counter and blacklists ip once it
// reaches maxFails. Returns the fail count after the bump.
func (s *Store) RecordLoginFail(ip, reason string, maxFails int, blockSeconds int64) (int, error) {
	nowTs := now()
	var count int
	err := s.db.QueryRow(`SELECT fail_count FROM ip_blacklist WHERE ip = ?`, ip).Scan(&count)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(
			`INSERT INTO ip_blacklist (ip, reason, fail_count, created_at, expires_at)
			 VALUES (?, ?, 1, ?, 0)`, ip, reason, nowTs,
		)
		count = 1
	case err != nil:
		return 0, fmt.Errorf("record login fail: %w", err)
	default:
		count++
		_, err = s.db.Exec(`UPDATE ip_blacklist SET fail_count = ?, reason = ? WHERE ip = ?`, count, reason, ip)
	}
	if err != nil {
		return 0, fmt.Errorf("record login fail: %w", err)
	}
	if count >= maxFails && blockSeconds > 0 {
		if _, err := s.db.Exec(`UPDATE ip_blacklist SET expires_at = ? WHERE ip = ?`, nowTs+blockSeconds, ip); err != nil {
			return count, err
		}
	}
	return count, nil
}

func (s *Store) ClearLoginFails(ip string) {
	s.db.Exec(`DELETE FROM ip_blacklist WHERE ip = ? AND expires_at = 0`, ip)
}

func (s *Store) UnblockIP(ip string) error {
	if ip == "all" {
		_, err := s.db.Exec(`DELETE FROM ip_blacklist`)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM ip_blacklist WHERE ip = ?`, ip)
	return err
}

func (s *Store) ListBlacklist() ([]BlacklistEntry, error) {
	rows, err := s.db.Query(
		`SELECT ip, reason, fail_count, created_at, COALESCE(expires_at, 0)
		 FROM ip_blacklist WHERE expires_at > ? ORDER BY created_at DESC`, now(),
	)
	if err != nil {
		return nil, fmt.Errorf("list blacklist: %w", err)
	}
	defer rows.Close()
	out := []BlacklistEntry{}
	for rows.Next() {
		var e BlacklistEntry
		if err := rows.Scan(&e.IP, &e.Reason, &e.FailCount, &e.CreatedAt, &e.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

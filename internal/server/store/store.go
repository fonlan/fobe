// Package store wraps the SQLite database (WAL) and all queries.
// Schema lives in schema.sql; every table from design.md §6 is created here.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// SchemaVersion is the schema generation this binary knows how to serve,
// tracked in PRAGMA user_version. Bump it with every schema change (schema.sql
// or migrateAdditive): it gates the pre-migration backup and the downgrade
// warning. It does NOT gate the migrations themselves — those stay unconditional
// and idempotent, so forgetting a bump only loses the backup-on-change
// guarantee, never correctness. Upgrade and downgrade semantics: §6 "schema
// 兼容策略" in design.md.
const SchemaVersion = 1

// Store is the database handle. Safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the database at path with WAL enabled.
func Open(path string) (*Store, error) {
	// busy_timeout: single writer, but brief contention with the retention sweeper.
	// synchronous=FULL: WAL+NORMAL is the usual recommendation, but this file
	// typically lives on a Docker bind mount whose fsync ordering is only as
	// strong as the host filesystem (OrbStack/virtiofs et al). A 2026-09-16
	// host reboot corrupted the main db mid-write and took the panel down.
	// FULL fsyncs every commit; at this panel's write rate (second-spaced
	// batches) the cost is unmeasurable, and a crash then costs at most the
	// last commit instead of file integrity.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// modernc/sqlite serializes internally; multiple conns only add contention.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.quickCheck(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// quickCheck refuses to open a damaged file. A corrupt db used to surface as
// scattered 500s across the panel while every write dug the hole deeper;
// failing at startup names the file and points at the recovery path instead.
// quick_check walks every b-tree, O(db size) — fine here, retention keeps the
// sample tables bounded.
func (s *Store) quickCheck() error {
	rows, err := s.db.Query(`PRAGMA quick_check(8)`)
	if err != nil {
		return s.corruptDBErr(err)
	}
	defer rows.Close()
	first, n := "", 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return s.corruptDBErr(err)
		}
		if line == "ok" {
			return nil
		}
		n++
		if first == "" {
			first = line
		}
	}
	if err := rows.Err(); err != nil {
		// drivers may abort the scan on the first unreadable page instead of
		// reporting it as a row — same verdict either way
		return s.corruptDBErr(err)
	}
	return s.corruptDBErr(fmt.Errorf("%d reported problem(s), first: %s", n, first))
}

func (s *Store) corruptDBErr(detail error) error {
	return fmt.Errorf("database file %s is corrupt (%v) — restore a snapshot from <data>/backup/ or rebuild with `sqlite3 <db> .recover`", s.path, detail)
}

func (s *Store) migrate() error {
	dbVer, err := s.userVersion()
	if err != nil {
		return err
	}
	if dbVer > SchemaVersion {
		// Downgrade (§5.5 version pinning makes these real, not hypothetical):
		// the additive-only policy means an older binary finds a consistent
		// subset — every query names its columns, so extra columns/tables from
		// a newer build are inert. Serve without touching the file and without
		// lowering user_version: this build doesn't know what the newer one
		// applied, and re-setting it could let a half-known upgrade re-run.
		slog.Warn("database schema is newer than this build; serving in downgrade-compatible mode (additive-only schema)",
			"db_schema", dbVer, "build_schema", SchemaVersion)
		return nil
	}
	if dbVer < SchemaVersion {
		fresh, err := s.isEmpty()
		if err != nil {
			return err
		}
		if !fresh {
			// Snapshot before the first tracked upgrade (and every later one):
			// a bad migration then costs minutes of restore, not the panel.
			// A failed backup must not wedge startup — migrations here are
			// additive and idempotent, so the uncovered risk is small.
			if err := s.BackupNow(); err != nil {
				slog.Warn("pre-migration backup failed; continuing", "err", err)
			} else {
				slog.Info("pre-migration backup written", "dir", filepath.Join(filepath.Dir(s.path), "backup"))
			}
			slog.Info("migrating database schema", "from", dbVer, "to", SchemaVersion)
		}
	}
	src, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read embedded schema: %w", err)
	}
	// strip comments first: a split on ";" would otherwise cut statements
	// in half at a semicolon inside a comment (no string literals in the
	// schema contain "--", so line-level stripping is safe)
	var clean strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		clean.WriteString(line)
		clean.WriteString("\n")
	}
	for _, stmt := range strings.Split(clean.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("apply schema statement %.60q: %w", stmt, err)
		}
	}
	if err := s.migrateAdditive(); err != nil {
		return err
	}
	// Only write the version when it actually moved — user_version shares the
	// header page with the change counter, rewriting it on every boot would
	// dirty the file for nothing.
	if dbVer != SchemaVersion {
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	return nil
}

func (s *Store) userVersion() (int, error) {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// isEmpty reports whether the file has no user tables yet (fresh install —
// nothing worth snapshotting before "migrating" it).
func (s *Store) isEmpty() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// migrateAdditive adds columns introduced after the initial schema to
// databases created by older builds. Idempotent: existing columns are skipped.
func (s *Store) migrateAdditive() error {
	// Web Terminal is agent-proxied and no longer uses SSH credentials. Remove
	// legacy ciphertext rather than retaining secrets that the product no
	// longer needs. This is idempotent for fresh and upgraded databases.
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS node_ssh`); err != nil {
		return fmt.Errorf("remove legacy node ssh credentials: %w", err)
	}

	migrations := []struct{ table, column, ddl string }{
		{"reg_tokens", "name", `ALTER TABLE reg_tokens ADD COLUMN name TEXT NOT NULL DEFAULT ''`},
		{"commands", "actor", `ALTER TABLE commands ADD COLUMN actor TEXT NOT NULL DEFAULT 'panel'`},
		{"commands", "ai_session_id", `ALTER TABLE commands ADD COLUMN ai_session_id TEXT NOT NULL DEFAULT ''`},
		{"commands", "reason", `ALTER TABLE commands ADD COLUMN reason TEXT NOT NULL DEFAULT ''`},
		{"commands", "risk", `ALTER TABLE commands ADD COLUMN risk TEXT NOT NULL DEFAULT ''`},
		{"audit_logs", "reason", `ALTER TABLE audit_logs ADD COLUMN reason TEXT NOT NULL DEFAULT ''`},
		{"subscriptions", "ua_filter", `ALTER TABLE subscriptions ADD COLUMN ua_filter TEXT NOT NULL DEFAULT ''`},
		{"node_ips", "manual_primary", `ALTER TABLE node_ips ADD COLUMN manual_primary INTEGER NOT NULL DEFAULT 0`},
		{"node_singbox", "firewall_hint", `ALTER TABLE node_singbox ADD COLUMN firewall_hint TEXT NOT NULL DEFAULT ''`},
		{"node_singbox", "password_override", `ALTER TABLE node_singbox ADD COLUMN password_override TEXT NOT NULL DEFAULT ''`},
		// §5.5 agent self-update bookkeeping (migrateAdditive is idempotent).
		{"nodes", "agent_target_version", `ALTER TABLE nodes ADD COLUMN agent_target_version TEXT NOT NULL DEFAULT ''`},
		{"nodes", "agent_update_state", `ALTER TABLE nodes ADD COLUMN agent_update_state TEXT NOT NULL DEFAULT ''`},
		{"nodes", "agent_update_attempts", `ALTER TABLE nodes ADD COLUMN agent_update_attempts INTEGER NOT NULL DEFAULT 0`},
		{"nodes", "agent_update_error", `ALTER TABLE nodes ADD COLUMN agent_update_error TEXT NOT NULL DEFAULT ''`},
		{"nodes", "agent_update_planned_at", `ALTER TABLE nodes ADD COLUMN agent_update_planned_at INTEGER NOT NULL DEFAULT 0`},
		{"nodes", "agent_update_done_at", `ALTER TABLE nodes ADD COLUMN agent_update_done_at INTEGER NOT NULL DEFAULT 0`},
		{"node_network", "cycle_type", `ALTER TABLE node_network ADD COLUMN cycle_type TEXT NOT NULL DEFAULT 'none'`},
		{"node_network", "next_reset_at", `ALTER TABLE node_network ADD COLUMN next_reset_at INTEGER`},
	}
	for _, m := range migrations {
		if s.columnExists(m.table, m.column) {
			continue
		}
		if _, err := s.db.Exec(m.ddl); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
	}
	// Existing anchors were monthly by default. Preserve that behaviour for
	// upgraded panels while new rows explicitly default to no traffic cycle.
	if _, err := s.db.Exec(`UPDATE node_network SET cycle_type = 'month', next_reset_at = anchor_at
		WHERE cycle_type = 'none' AND next_reset_at IS NULL AND anchor_at IS NOT NULL`); err != nil {
		return fmt.Errorf("migrate legacy traffic cycles: %w", err)
	}
	return nil
}

func (s *Store) columnExists(table, column string) bool {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

func (s *Store) Close() error { return s.db.Close() }

// now is unix seconds everywhere in the DB.
func now() int64 { return time.Now().Unix() }

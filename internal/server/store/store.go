// Package store wraps the SQLite database (WAL) and all queries.
// Schema lives in schema.sql; every table from design.md §6 is created here.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// Store is the database handle. Safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path with WAL enabled.
func Open(path string) (*Store, error) {
	// busy_timeout: single writer, but brief contention with the retention sweeper.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// modernc/sqlite serializes internally; multiple conns only add contention.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
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
	return s.migrateAdditive()
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
	}
	for _, m := range migrations {
		if s.columnExists(m.table, m.column) {
			continue
		}
		if _, err := s.db.Exec(m.ddl); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
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

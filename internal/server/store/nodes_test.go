package store

import (
	"path/filepath"
	"testing"
)

func TestRegTokenNameColumnMigratesLegacyDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a database created before the name column existed. SQLite cannot
	// drop a column portably in all supported versions, so use a separate table
	// copy with the legacy shape and restore its name before reopening.
	if _, err := legacy.db.Exec(`CREATE TABLE legacy_reg_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT, token_hash TEXT NOT NULL UNIQUE,
		note TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL, used_at INTEGER, used_by TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`DROP TABLE reg_tokens`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`ALTER TABLE legacy_reg_tokens RENAME TO reg_tokens`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	migrated, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen legacy db: %v", err)
	}
	defer migrated.Close()
	if err := migrated.CreateRegToken("hash", "named-node", "note", 1800); err != nil {
		t.Fatalf("create token after migration: %v", err)
	}
	name, note, err := migrated.ConsumeRegToken("hash")
	if err != nil {
		t.Fatalf("consume token after migration: %v", err)
	}
	if name != "named-node" || note != "note" {
		t.Fatalf("got name=%q note=%q", name, note)
	}
}
func TestLegacyNodeSSHCredentialsArePurged(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-ssh.db")
	legacy, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`CREATE TABLE node_ssh (
		node_id TEXT PRIMARY KEY,
		password_enc TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`INSERT INTO node_ssh (node_id, password_enc) VALUES ('node', 'ciphertext')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen legacy db: %v", err)
	}
	defer migrated.Close()
	var exists int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'node_ssh'`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("legacy node_ssh table remains after migration")
	}
}

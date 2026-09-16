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
	if err := migrated.CreateRegToken("hash", "named-node", "note", "", 1800); err != nil {
		t.Fatalf("create token after migration: %v", err)
	}
	name, note, bound, err := migrated.ConsumeRegToken("hash")
	if err != nil {
		t.Fatalf("consume token after migration: %v", err)
	}
	if name != "named-node" || note != "note" || bound != "" {
		t.Fatalf("got name=%q note=%q bound=%q", name, note, bound)
	}
}

// A node-bound token (§4.2 revision) must survive both storage and listing, and
// must still be single-use.
func TestRegTokenNodeBindingRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "bound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateRegToken("h-bound", "probe", "reinstall n-1", "n-1", 1800); err != nil {
		t.Fatal(err)
	}
	name, note, bound, err := st.ConsumeRegToken("h-bound")
	if err != nil {
		t.Fatal(err)
	}
	if name != "probe" || note != "reinstall n-1" || bound != "n-1" {
		t.Fatalf("got name=%q note=%q bound=%q", name, note, bound)
	}
	if _, _, _, err := st.ConsumeRegToken("h-bound"); err == nil {
		t.Fatal("consumed token accepted a second time")
	}
	if err := st.CreateRegToken("h-2", "probe", "", "n-1", 1800); err != nil {
		t.Fatal(err)
	}
	tokens, err := st.ListRegTokens(true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tok := range tokens {
		if tok.NodeID == "n-1" {
			found = true
		}
		if tok.CreatedAt == 0 || tok.ExpiresAt == 0 {
			t.Fatalf("token %d lost its timestamps", tok.ID)
		}
	}
	if !found {
		t.Fatalf("bound token not listed with its node id: %+v", tokens)
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

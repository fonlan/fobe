package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fobe.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

// A damaged file must be refused at Open — the 2026-09-16 incident had the
// panel serving 500s from a corrupt db for an hour, every write digging the
// hole deeper. Open is the one place where "refuse to start" is cheap.
func TestOpenRejectsCorruptDatabase(t *testing.T) {
	st, path := openTemp(t)
	if _, err := st.db.Exec(`CREATE TABLE canary (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// splatter every page past the header with noise: some pages may be free,
	// but with a whole schema in the file live pages are guaranteed hit.
	// Zero bytes would be a legal free-page pattern to sqlite, hence noise.
	noise := []byte("\xDE\xAD\xBE\xEF\xCA\xFE\xBA\xBE")
	for off := int64(4096); off < 4096+8192; off += int64(len(noise)) {
		if _, err := f.WriteAt(noise, off); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a corrupt database")
	} else if !strings.Contains(err.Error(), "corrupt") && !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("error should name the corruption, got: %v", err)
	}
}

func TestFreshDatabaseGetsSchemaVersion(t *testing.T) {
	st, _ := openTemp(t)
	v, err := st.userVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Fatalf("fresh db schema version = %d, want %d", v, SchemaVersion)
	}
}

// Databases created before version tracking (user_version = 0, non-empty) are
// the "untracked legacy" case: reopening must snapshot them, migrate, and stamp
// the version — without losing a row.
func TestUntrackedDatabaseBacksUpBeforeMigrating(t *testing.T) {
	st, path := openTemp(t)
	if _, err := st.db.Exec(`INSERT INTO settings (key, value, encrypted) VALUES ('server.public_url', 'http://x', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	v, err := st2.userVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, SchemaVersion)
	}
	var val string
	if err := st2.db.QueryRow(`SELECT value FROM settings WHERE key = 'server.public_url'`).Scan(&val); err != nil || val != "http://x" {
		t.Fatalf("data lost across migration: %q, %v", val, err)
	}
	snaps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "backup", "fobe-*.db"))
	if len(snaps) != 1 {
		t.Fatalf("pre-migration backup count = %d, want 1", len(snaps))
	}
}

// Downgrade: a file written by a newer build must open on an older binary.
// The additive-only policy is what makes this safe; the version stamp must be
// left untouched so the newer build's history is not rewritten.
func TestNewerSchemaOpensWithoutMigration(t *testing.T) {
	st, path := openTemp(t)
	future := SchemaVersion + 5
	if _, err := st.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, future)); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("downgraded binary refused a newer database: %v", err)
	}
	defer st2.Close()
	v, err := st2.userVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != future {
		t.Fatalf("downgrade rewrote schema version: got %d, want %d", v, future)
	}
}

func TestBackupDirSnapshotsAndPrunes(t *testing.T) {
	st, _ := openTemp(t)
	dir := filepath.Join(t.TempDir(), "backup")
	for i := 0; i < backupKeep+4; i++ {
		name := filepath.Join(dir, fmt.Sprintf("fobe-202601%02d-000000.db", i+1))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.BackupDir(dir); err != nil {
		t.Fatal(err)
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "fobe-*.db"))
	if len(snaps) != backupKeep {
		t.Fatalf("backup pool size = %d, want %d (pruned)", len(snaps), backupKeep)
	}
	// the fresh snapshot must be among the survivors, not pruned as "oldest":
	// today's timestamp sorts after every fake January name
	today := time.Now().Format("20060102")
	found := false
	for _, s := range snaps {
		if strings.Contains(filepath.Base(s), today) {
			found = true
		}
	}
	if !found {
		t.Fatal("fresh snapshot missing after prune")
	}
}

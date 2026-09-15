package geoip

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oschwald/geoip2-golang"
)

// Installing a database is one shared path for both writers — the panel upload
// (§14 手动上传) and the mirror download (geoipupdate) — because both must end
// with the same guarantees: the live file is never half-written, and whatever
// replaces it is actually parseable.

const (
	// MaxMMDBBytes caps a database payload. GeoLite2-Country is ~9MB today;
	// 64MB leaves headroom without letting a request or a mirror buffer
	// without bound.
	MaxMMDBBytes = 64 << 20

	// tempPattern keeps in-progress writes recognisable next to the database.
	tempPattern = ".mmdb-*.tmp"
)

// Errors callers match with errors.Is.
var (
	// ErrInvalidMMDB reports a payload that does not parse as a MaxMind DB.
	ErrInvalidMMDB = errors.New("geoip: not a valid mmdb database")
	// ErrTooLarge reports a payload above MaxMMDBBytes.
	ErrTooLarge = errors.New("geoip: database exceeds the size limit")
)

// CreateMMDBTemp opens a temp file in the database's directory, so a later
// rename stays on one filesystem and is therefore atomic.
func CreateMMDBTemp(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return nil, fmt.Errorf("temp file in %s: %w", dir, err)
	}
	return f, nil
}

// CommitMMDB validates the database at tmpPath and moves it onto path. On any
// error path is left untouched, so a bad download can never displace a working
// database. The caller removes tmpPath afterwards (the rename makes that a
// no-op on success).
func CommitMMDB(tmpPath, path string) error {
	if err := ValidateMMDB(tmpPath); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}

// ValidateMMDB reports whether path holds a readable MaxMind database.
//
// The format carries no leading magic — a real database starts with its search
// tree and ends with the "\xab\xcd\xefMaxMind.com" metadata marker — so the
// only honest check is to open it. (An earlier version tested for a leading
// "MMDB" string, which no genuine database has: every real upload was rejected
// with bad_mmdb while the tests passed against a fake header.)
func ValidateMMDB(path string) error {
	r, err := geoip2.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMMDB, err)
	}
	defer r.Close()
	if r.Metadata().NodeCount == 0 {
		return fmt.Errorf("%w: empty search tree", ErrInvalidMMDB)
	}
	return nil
}

// WriteMMDB is the whole install path for a stream (the panel upload, or a
// mirror body): temp file → size check → validate → rename. It returns the
// number of bytes written, which is also the size of the installed database.
func WriteMMDB(path string, r io.Reader) (int64, error) {
	f, err := CreateMMDBTemp(path)
	if err != nil {
		return 0, err
	}
	name := f.Name()
	defer os.Remove(name) // no-op once the rename below succeeded

	// One byte over the cap is enough to know the payload is too big.
	n, err := io.Copy(f, io.LimitReader(r, MaxMMDBBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("write %s: %w", name, err)
	}
	if n > MaxMMDBBytes {
		return n, ErrTooLarge
	}
	if err := CommitMMDB(name, path); err != nil {
		return n, err
	}
	return n, nil
}

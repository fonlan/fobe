package httpapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// --- GeoIP MMDB upload (design §14: refresh the local GeoLite2-Country
// database from the panel; no restart needed) ---

// maxMMDBBytes caps the upload. GeoLite2-Country is ~10MB today; 64MB leaves
// headroom without letting a request buffer without bound.
const maxMMDBBytes = 64 << 20

// mmdbMagic is the file signature every upload must start with (0x4D 0x4D 0x44 0x42).
var mmdbMagic = []byte("MMDB")

// reloadableResolver matches the explicit reload hook of the concrete MMDB
// resolver without depending on it: after a successful upload the Server's
// resolver is asserted to this interface and reloaded. Resolvers without a
// Reload method still pick the file up on their next lookup (the mmdb
// implementation re-stats the path per query).
type reloadableResolver interface {
	Reload() bool
}

// handleUploadMMDB accepts raw MMDB bytes, validates the signature, swaps the
// database in atomically (temp file + rename) and reloads the live resolver.
func (s *Server) handleUploadMMDB(w http.ResponseWriter, r *http.Request) {
	if s.GeoIPMMDBPath == "" {
		writeErr(w, http.StatusServiceUnavailable, "mmdb_not_configured")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMMDBBytes+1))
	if err != nil || len(data) > maxMMDBBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "mmdb_too_large")
		return
	}
	if !bytes.HasPrefix(data, mmdbMagic) {
		writeErr(w, http.StatusBadRequest, "bad_mmdb")
		return
	}
	if err := writeFileAtomic(s.GeoIPMMDBPath, data); err != nil {
		s.Log.Error("write mmdb", "path", s.GeoIPMMDBPath, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if rr, ok := s.GeoIPResolver.(reloadableResolver); ok {
		rr.Reload()
	}
	s.audit("geoip_mmdb_uploaded", fmt.Sprintf("%s (%d bytes)", s.GeoIPMMDBPath, len(data)), s.Trust.RealIP(r))
	s.publishEvent("geoip_updated", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeFileAtomic replaces path with data via a temp file in the same
// directory + rename, so readers never observe a half-written database.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".mmdb-*.tmp")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}

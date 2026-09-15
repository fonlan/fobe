package singboxdl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CachedVersion is one installed version as the panel sees it.
type CachedVersion struct {
	// Version is the canonical version (no leading "v").
	Version string `json:"version"`
	// Size is the binary size in bytes.
	Size int64 `json:"size"`
	// SHA256 is the binary digest.
	SHA256 string `json:"sha256,omitempty"`
	// DownloadedAt is when the version was published, unix seconds.
	DownloadedAt int64 `json:"downloaded_at"`
	// Refs counts nodes whose desired_version is this version.
	Refs int `json:"refs"`
	// Asset is the upstream asset file name.
	Asset string `json:"asset,omitempty"`
	// Path is the version directory (never serialized).
	Path string `json:"-"`
}

// ScanCache lists every valid cached version, newest first by semver. A
// directory only counts when it holds a regular "linux-amd64" file; leftover
// temp dirs and half-written versions are ignored.
//
// The cache root not existing yet is not an error (empty list).
func (c *Client) ScanCache() ([]CachedVersion, error) {
	if c.cfg.DLDir == "" {
		return nil, ErrNoDLDir
	}
	entries, err := os.ReadDir(c.SingboxDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []CachedVersion{}, nil
		}
		return nil, err
	}
	out := []CachedVersion{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, tempPrefix) {
			continue
		}
		v, verr := ParseVersion(name)
		if verr != nil {
			continue
		}
		dir := filepath.Join(c.SingboxDir(), name)
		st, err := os.Stat(filepath.Join(dir, BinaryName))
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		cv := CachedVersion{
			Version:      v.String(),
			Size:         st.Size(),
			DownloadedAt: st.ModTime().Unix(),
			Path:         dir,
		}
		if m, err := readManifest(filepath.Join(dir, ManifestName)); err == nil {
			if mv, err := ParseVersion(m.Version); err == nil {
				cv.Version = mv.String()
			}
			if m.Size > 0 {
				cv.Size = m.Size
			}
			if m.DownloadedAt > 0 {
				cv.DownloadedAt = m.DownloadedAt
			}
			cv.SHA256 = m.SHA256
			cv.Asset = m.Asset
		}
		if cv.SHA256 == "" {
			if raw, err := os.ReadFile(filepath.Join(dir, ChecksumName)); err == nil {
				if fields := strings.Fields(string(raw)); len(fields) > 0 {
					if sum, ok := normalizeHex(fields[0]); ok {
						cv.SHA256 = sum
					}
				}
			}
		}
		if c.cfg.Refs != nil {
			cv.Refs = c.cfg.Refs(cv.Version)
		}
		out = append(out, cv)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].Version, out[j].Version) > 0
	})
	return out, nil
}

// LatestCached returns the newest cached version, or ErrNotFound when the
// cache is empty. The startup auto-download uses this as its "already have
// something" check.
func (c *Client) LatestCached() (*CachedVersion, error) {
	versions, err := c.ScanCache()
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, ErrNotFound
	}
	v := versions[0]
	return &v, nil
}

// CacheEmpty reports whether no valid version is cached.
func (c *Client) CacheEmpty() (bool, error) {
	versions, err := c.ScanCache()
	if err != nil {
		return false, err
	}
	return len(versions) == 0, nil
}

// DeleteVersion removes one cached version from disk. Unknown versions
// return ErrNotFound.
func (c *Client) DeleteVersion(version string) error {
	if c.cfg.DLDir == "" {
		return ErrNoDLDir
	}
	_, canonical, err := normalizeVersion(version)
	if err != nil {
		return err
	}
	dir := c.VersionDir(canonical)
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, canonical)
		}
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: %s", ErrNotFound, canonical)
	}
	return os.RemoveAll(dir)
}

// readManifest decodes a version manifest. A missing file is an error.
func readManifest(path string) (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	return m, nil
}

package singboxdl

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// binaryNameInArchive is the member name inside the official tarball.
const binaryNameInArchive = "sing-box"

// Manifest describes one cached version; it is the source of truth for the
// panel's version list (<DLDir>/singbox/<version>/manifest.json).
type Manifest struct {
	Version string `json:"version"`
	Asset   string `json:"asset"`
	// SHA256 is the digest of the extracted linux-amd64 binary — the file the
	// agent downloads from /dl and verifies with linux-amd64.sha256.
	SHA256 string `json:"sha256"`
	// Size is the extracted binary size in bytes.
	Size int64 `json:"size"`
	// ArchiveSHA256 / ArchiveSize describe the upstream tar.gz we verified.
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	ArchiveSize   int64  `json:"archive_size,omitempty"`
	DownloadedAt  int64  `json:"downloaded_at"`
	SourceURL     string `json:"source_url,omitempty"`
}

// Install downloads, verifies and atomically publishes one release into
// <DLDir>/singbox/<version>/. On any failure the cache is left untouched and
// the temporary directory is removed (fail-closed: a missing checksum aborts
// the install).
//
// An already-cached version returns ErrVersionExists unless force is set; a
// forced install replaces the directory.
//
// Installs made through one Client are serialized: a caller that arrives while
// another download runs waits (reporting PhaseWaiting) and then finds the
// version already published, so the same tarball is never fetched twice
// (design §9.5.3). Sharing one Client per process is therefore load-bearing,
// not an optimization.
func (c *Client) Install(ctx context.Context, rel Release, force bool) (*CachedVersion, error) {
	if c.cfg.DLDir == "" {
		return nil, ErrNoDLDir
	}
	_, version, err := normalizeVersion(rel.Version)
	if err != nil {
		return nil, err
	}
	started := time.Now().Unix()
	emit := func(p Progress) {
		p.Version = version
		p.StartedAt = started
		c.emit(p)
	}
	// A failure past this point is reported once, so the panel never shows a
	// bar stuck at "downloading" for a download that is already over.
	failed := func(err error) error {
		emit(Progress{Phase: PhaseFailed, Error: err.Error()})
		return err
	}

	// TryLock only tells us whether we are queued; the panel then shows
	// "waiting" instead of a stalled bar. The lock itself is what guarantees a
	// single download (§9.5.3).
	if !c.installMu.TryLock() {
		emit(Progress{Phase: PhaseWaiting})
		c.installMu.Lock()
	}
	defer c.installMu.Unlock()

	target := c.VersionDir(version)
	if _, err := os.Stat(target); err == nil {
		if !force {
			// Not a failure: whoever held the lock (or an earlier run) already
			// published this version.
			return nil, fmt.Errorf("%w: %s", ErrVersionExists, version)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	assetName := AssetName(version)
	asset, ok := rel.Asset(assetName)
	if !ok {
		return nil, failed(fmt.Errorf("%w: %s", ErrAssetNotFound, assetName))
	}
	assetURL := c.assetURL(rel, asset)

	base := c.SingboxDir()
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, failed(fmt.Errorf("singboxdl: mkdir %s: %w", base, err))
	}
	tmp, err := os.MkdirTemp(base, tempPrefix+version+"-")
	if err != nil {
		return nil, failed(fmt.Errorf("singboxdl: mkdir temp: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			if rerr := os.RemoveAll(tmp); rerr != nil {
				c.logWarn("singboxdl: clean temp dir", "dir", tmp, "err", rerr)
			}
		}
	}()

	// 1. resolve the expected digest first: an artifact with no checksum
	// anywhere is refused before we waste a download on it (fail-closed)
	emit(Progress{Phase: PhaseResolving})
	want, err := c.resolveDigest(ctx, asset, assetURL)
	if err != nil {
		return nil, failed(err)
	}

	// 2. download into the temp dir, hashing as we go
	archive := filepath.Join(tmp, assetName)
	emit(Progress{Phase: PhaseDownloading})
	sum, size, err := c.download(ctx, assetURL, archive, func(downloaded, total int64) {
		emit(Progress{Phase: PhaseDownloading, Downloaded: downloaded, Total: total})
	})
	if err != nil {
		return nil, failed(err)
	}
	emit(Progress{Phase: PhaseVerifying})
	if !strings.EqualFold(sum, want) {
		return nil, failed(fmt.Errorf("%w: %s: got %s want %s", ErrChecksumMismatch, assetName, sum, want))
	}

	// 3. unpack the single static binary
	emit(Progress{Phase: PhaseExtracting})
	binTmp := filepath.Join(tmp, BinaryName)
	if err := extractBinary(archive, binTmp); err != nil {
		return nil, failed(fmt.Errorf("singboxdl: extract %s: %w", assetName, err))
	}
	if err := os.Chmod(binTmp, 0o755); err != nil {
		return nil, failed(fmt.Errorf("singboxdl: chmod binary: %w", err))
	}
	// The upstream digest covers the archive; the agent verifies the binary
	// it downloads from /dl, so the published sidecar hashes the binary.
	binSum, binSize, err := hashFile(binTmp)
	if err != nil {
		return nil, failed(fmt.Errorf("singboxdl: hash binary: %w", err))
	}

	// 4. the two sidecars the agent/panel read
	sidecar := binSum + "  " + BinaryName
	if err := os.WriteFile(filepath.Join(tmp, ChecksumName), []byte(sidecar), 0o644); err != nil {
		return nil, failed(fmt.Errorf("singboxdl: write checksum: %w", err))
	}
	now := time.Now()
	manifest := Manifest{
		Version:       version,
		Asset:         assetName,
		SHA256:        binSum,
		Size:          binSize,
		ArchiveSHA256: sum,
		ArchiveSize:   size,
		DownloadedAt:  now.Unix(),
		SourceURL:     assetURL,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, failed(fmt.Errorf("singboxdl: encode manifest: %w", err))
	}
	if err := os.WriteFile(filepath.Join(tmp, ManifestName), append(raw, byte(10)), 0o644); err != nil {
		return nil, failed(fmt.Errorf("singboxdl: write manifest: %w", err))
	}
	// the archive is an intermediate: /dl must only expose the binary
	_ = os.Remove(archive)

	// 5. publish atomically
	emit(Progress{Phase: PhasePublishing})
	if force {
		if err := os.RemoveAll(target); err != nil {
			return nil, failed(fmt.Errorf("singboxdl: replace %s: %w", target, err))
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		return nil, failed(fmt.Errorf("singboxdl: publish %s: %w", target, err))
	}
	committed = true
	emit(Progress{Phase: PhaseDone})

	out := CachedVersion{
		Version:      version,
		Size:         binSize,
		SHA256:       binSum,
		DownloadedAt: manifest.DownloadedAt,
		Path:         target,
	}
	if c.cfg.Refs != nil {
		out.Refs = c.cfg.Refs(version)
	}
	if c.cfg.Log != nil {
		c.cfg.Log.Info("singboxdl: installed", "version", version,
			"size", binSize, "sha256", binSum, "archive_sha256", sum)
	}
	return &out, nil
}

// download streams a URL to dst, returning its sha256 and byte count. The
// size cap is enforced against the actual body.
//
// report (optional) receives byte-level progress; the final call always fires
// with the exact size, so a finished download never reports a stale count.
func (c *Client) download(ctx context.Context, u, dst string, report func(downloaded, total int64)) (string, int64, error) {
	if report == nil {
		report = func(int64, int64) {}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, fmt.Errorf("singboxdl: request %s: %w", u, err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("singboxdl: download %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("singboxdl: download %s: %s", u, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("singboxdl: create %s: %w", dst, err)
	}
	defer f.Close()

	// ContentLength is -1 when upstream does not say; 0 means "unknown size"
	// to the panel, which then shows bytes instead of a percentage.
	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	h := sha256.New()
	body := &progressReader{r: resp.Body, total: total, report: report}
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, c.cfg.MaxDownloadBytes+1))
	if err != nil {
		return "", 0, fmt.Errorf("singboxdl: download %s: %w", u, err)
	}
	if n > c.cfg.MaxDownloadBytes {
		return "", 0, fmt.Errorf("singboxdl: %s exceeds %d bytes", u, c.cfg.MaxDownloadBytes)
	}
	report(n, total)
	if err := f.Sync(); err != nil {
		return "", 0, fmt.Errorf("singboxdl: sync %s: %w", dst, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// resolveDigest implements the checksum chain: API asset digest first, then
// the sibling <asset>.sha256 file, then refuse to install.
func (c *Client) resolveDigest(ctx context.Context, a Asset, assetURL string) (string, error) {
	if d := strings.TrimSpace(a.Digest); d != "" {
		if i := strings.Index(d, ":"); i >= 0 {
			if strings.EqualFold(strings.TrimSpace(d[:i]), "sha256") {
				if sum, ok := normalizeHex(d[i+1:]); ok {
					return sum, nil
				}
			}
		} else if sum, ok := normalizeHex(d); ok {
			return sum, nil
		}
		c.logWarn("singboxdl: unusable API digest, falling back to .sha256", "digest", d)
	}

	var candidates []string
	if assetURL != "" {
		candidates = append(candidates, assetURL+".sha256")
	}
	if a.BrowserDownloadURL != "" {
		if u := a.BrowserDownloadURL + ".sha256"; u != assetURL+".sha256" {
			candidates = append(candidates, u)
		}
	}
	var lastErr error
	for _, u := range candidates {
		sum, err := c.fetchChecksum(ctx, u)
		if err == nil {
			return sum, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", fmt.Errorf("%w: %v", ErrChecksumUnavailable, lastErr)
	}
	return "", ErrChecksumUnavailable
}

// fetchChecksum downloads a sha256sum-style sidecar and returns the digest.
func (c *Client) fetchChecksum(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("%s: %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s: empty checksum file", u)
	}
	sum, ok := normalizeHex(fields[0])
	if !ok {
		return "", fmt.Errorf("%s: invalid checksum %q", u, fields[0])
	}
	return sum, nil
}

// hashFile returns the sha256 and size of an on-disk file.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// normalizeHex validates a sha256 hex digest (optionally prefixed with
// "sha256:") and returns it lower-cased.
func normalizeHex(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", false
	}
	return strings.ToLower(s), true
}

// extractBinary pulls the sing-box binary member out of a tar.gz archive.
func extractBinary(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	// The compressed stream is capped upstream, but tar.Next() walks every
	// member and decompresses the ones this loop skips, so a small archive with
	// many members still burns CPU. Cap the DECOMPRESSED total as well (§9.2
	// 实现修订 2026-09-20).
	tr := tar.NewReader(&cappedReader{r: gz, max: maxArchiveDecompressedBytes})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		if filepath.Base(hdr.Name) != binaryNameInArchive {
			continue
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxBinaryExtractBytes)); err != nil {
			_ = out.Close()
			_ = os.Remove(dst)
			return err
		}
		return out.Close()
	}
	return ErrBinaryNotFound
}

// maxBinaryExtractBytes caps the unpacked binary size (zip-bomb guard).
const maxBinaryExtractBytes = 256 << 20

// maxArchiveDecompressedBytes caps the TOTAL decompressed bytes read from an
// archive, which is what a gzip bomb actually costs (§9.2 实现修订 2026-09-20).
const maxArchiveDecompressedBytes = 512 << 20

var errArchiveTooLarge = errors.New("archive expands beyond the allowed size")

// cappedReader fails the read once max bytes have been produced, so the limit
// surfaces through tar.Next()/io.Copy as an error instead of an endless
// decompression.
type cappedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		return 0, errArchiveTooLarge
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, errArchiveTooLarge
	}
	return n, err
}

// CleanTemps removes leftover in-progress directories (crash recovery on
// startup). It never touches published versions.
func (c *Client) CleanTemps() (int, error) {
	if c.cfg.DLDir == "" {
		return 0, ErrNoDLDir
	}
	entries, err := os.ReadDir(c.SingboxDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.SingboxDir(), e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

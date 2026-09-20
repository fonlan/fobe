// Artifact install: overlay space gate (§5.4) plus download + sha256 verify
// (gate ① of §9.2).
package agent

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// --- overlay space gate (§5.4) ---

// statfsFunc is swappable so tests can inject free-space results.
var statfsFunc = statfsFree

// checkInstallSpace refuses a binary install when the filesystem holding the
// sing-box binary is nearly full (§5.4 OpenWrt 专项: never write the router's
// overlay full). It is the *gross* floor: at converge time the artifact size is
// still unknown, so it only refuses a filesystem that cannot hold anything at
// all. installArtifact() repeats the check with the real size once the response
// headers are in. Undeterminable space does not block (fail open).
func checkInstallSpace(dir string) error {
	return checkInstallSpaceFor(dir, 0, false)
}

// checkInstallSpaceFor is the size-aware gate. The artifact is written as a
// temp file next to the binary it replaces, and installArtifact keeps the
// binary it displaced as .prev (§9.2 rollback) — so replacing an existing copy
// needs 2× the artifact while a fresh install needs one, both plus headroom for
// config/certs/logs. Asking for a flat 64 MiB let a ~90 MiB artifact start on a
// filesystem that could not hold the swap, which is the fill-the-overlay
// outcome §5.4 exists to prevent (same 2× rule as §5.5).
func checkInstallSpaceFor(dir string, artifactBytes uint64, replacing bool) error {
	freeBytes, freeInodes, totalInodes, err := statfsFunc(dir)
	if err != nil {
		return nil
	}
	needBytes := uint64(minInstallFreeBytes) + artifactBytes
	if replacing {
		needBytes += artifactBytes
	}
	// btrfs and friends allocate inodes on demand, so statfs on them reports
	// f_files = f_ffree = 0 (`df -i` shows 0 0). That is "no inode budget",
	// not "out of inodes" — refusing there blocked every install on a btrfs
	// data volume with "0 inodes free" while gigabytes were actually usable.
	// The inode floor only applies when the filesystem publishes a budget
	// (ext4/f2fs/overlay-on-ext4...); a genuinely starved one still does.
	inodesOk := totalInodes == 0 || freeInodes >= minInstallFreeInodes
	if freeBytes >= needBytes && inodesOk {
		return nil
	}
	if totalInodes == 0 {
		// Inode numbers are meaningless on this fs; don't let the message
		// point the operator at a nonexistent inode shortage.
		return fmt.Errorf(
			"insufficient disk space on %s: %d MiB free, need ≥ %d MiB — install refused",
			dir, freeBytes>>20, needBytes>>20)
	}
	return fmt.Errorf(
		"insufficient disk space on %s: %d MiB / %d inodes free, need ≥ %d MiB / %d inodes — install refused",
		dir, freeBytes>>20, freeInodes, needBytes>>20, minInstallFreeInodes)
}

// --- download + verify ---

// installArtifact fetches the artifact and its <url>.sha256 sidecar, verifies
// the digest (mismatch aborts), backs up the current binary as .prev, then
// moves the new one in place with 0755. No shell involved: http.Get +
// io.LimitReader + rename (§9.2).
func installArtifact(hc *http.Client, artifactURL, dest string, maxSize int64) error {
	want, err := fetchExpectedSHA256(hc, artifactURL+".sha256")
	if err != nil {
		return err
	}

	resp, err := hc.Get(artifactURL)
	if err != nil {
		return fmt.Errorf("get artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get artifact: %s", resp.Status)
	}
	// Refuse from the headers: a file the cap rejects must not be transferred
	// (the probe pays for every byte), and the space gate below can only be
	// honest once the size is known. Replacing an existing binary also parks a
	// second copy as .prev (§9.2 rollback).
	if resp.ContentLength > 0 {
		if resp.ContentLength > maxSize {
			return fmt.Errorf("artifact exceeds %d bytes: %d", maxSize, resp.ContentLength)
		}
		replacing := fileExists(dest)
		if err := checkInstallSpaceFor(filepath.Dir(dest), uint64(resp.ContentLength), replacing); err != nil {
			return err
		}
	}

	tmp := dest + ".download"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxSize+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("download: %w", errors.Join(copyErr, closeErr))
	}
	if n > maxSize {
		_ = os.Remove(tmp)
		return fmt.Errorf("artifact exceeds %d bytes", maxSize)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		_ = os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: want %s, got %s", want, got)
	}

	if fileExists(dest) { // §9.2: keep the current binary for rollback
		_ = os.Rename(dest, dest+".prev")
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod artifact: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install artifact: %w", err)
	}
	return nil
}

// fetchExpectedSHA256 reads the .sha256 sidecar ("<hex>  <filename>" or bare).
func fetchExpectedSHA256(hc *http.Client, shaURL string) (string, error) {
	resp, err := hc.Get(shaURL)
	if err != nil {
		return "", fmt.Errorf("get sha256: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get sha256: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("read sha256: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(raw)))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("malformed sha256 file %q", truncateStr(string(raw), 80))
	}
	return strings.ToLower(fields[0]), nil
}

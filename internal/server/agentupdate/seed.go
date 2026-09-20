package agentupdate

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// artifactFiles are the files that must exist for a published agent build to
// be downloadable (design §5.5 发布门槛). manifest.json is copied when present
// but is not part of the gate: the agent only needs the binary and its sha256.
var artifactFiles = []string{"linux-amd64", "linux-amd64.sha256"}

// seedExtraFiles are copied along with the artifacts when the seed has them.
var seedExtraFiles = []string{"manifest.json"}

// DefaultSeedDir is where the image keeps its own agent builds, deliberately
// *outside* the artifact volume: compose bind-mounts ./data/dl over /srv/dl, so
// anything the image put in /srv/dl is invisible at runtime (design §17).
const DefaultSeedDir = "/srv/agent-seed"

// artifactPath is the on-disk location of one artifact file.
func artifactPath(dlDir, version, name string) string {
	return filepath.Join(dlDir, "agent", version, name)
}

func fileNonEmpty(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// Seed copies the agent builds that shipped inside the image into the artifact
// volume, then points agent/latest at the build of the running server.
//
// Without this, a standard compose deployment has an empty <dl>/agent: the
// bind mount hides the image's own /srv/dl copy, so both /install.sh and §5.5
// self-update would 404 forever. Copying is additive and idempotent — an
// existing version directory is never touched, so a manually placed artifact
// (or one from an older image) survives, and the runtime volume stays the one
// source of truth the agents download from.
//
// latest is only repointed when it already is a symlink (the image layout) or
// absent; a real directory left there by local tooling is never deleted.
func Seed(seedDir, dlDir, serverVersion string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if seedDir == "" || dlDir == "" {
		return nil
	}
	seedAgent := filepath.Join(seedDir, "agent")
	entries, err := os.ReadDir(seedAgent)
	if os.IsNotExist(err) {
		log.Debug("agentupdate: no seed directory", "dir", seedAgent)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read seed dir %s: %w", seedAgent, err)
	}

	copied := []string{}
	for _, e := range entries {
		version := e.Name()
		if !e.IsDir() || version == "latest" || !SafeVersionPart(version) {
			continue
		}
		src := filepath.Join(seedAgent, version, "linux-amd64")
		if !fileNonEmpty(src) {
			continue
		}
		if fileNonEmpty(artifactPath(dlDir, version, "linux-amd64")) {
			continue // the volume already has this build; never overwrite it
		}
		dstDir := filepath.Join(dlDir, "agent", version)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dstDir, err)
		}
		for _, name := range append(append([]string{}, artifactFiles...), seedExtraFiles...) {
			s := filepath.Join(seedAgent, version, name)
			if !fileNonEmpty(s) {
				continue
			}
			mode := os.FileMode(0o644)
			if name == "linux-amd64" {
				mode = 0o755
			}
			if err := copyFile(s, filepath.Join(dstDir, name), mode); err != nil {
				return fmt.Errorf("seed %s/%s: %w", version, name, err)
			}
		}
		copied = append(copied, version)
	}

	if len(copied) > 0 {
		sort.Strings(copied)
		log.Info("agentupdate: seeded agent artifacts into the artifact volume",
			"dir", filepath.Join(dlDir, "agent"), "versions", copied)
	}

	if !SafeVersionPart(serverVersion) || !fileNonEmpty(artifactPath(dlDir, serverVersion, "linux-amd64")) {
		return nil
	}
	return pointLatest(dlDir, serverVersion, log)
}

// pointLatest makes <dl>/agent/latest/<file> a symlink to the running server's
// build, so the install command always installs the agent that matches the
// panel that served it (a stale symlink would install an old probe binary on a
// new panel after a container upgrade).
func pointLatest(dlDir, version string, log *slog.Logger) error {
	latestDir := filepath.Join(dlDir, "agent", "latest")
	if !ownedLatest(latestDir, log) {
		return nil
	}
	if err := os.MkdirAll(latestDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", latestDir, err)
	}
	for _, name := range append(append([]string{}, artifactFiles...), seedExtraFiles...) {
		if !fileNonEmpty(filepath.Join(dlDir, "agent", version, name)) {
			continue
		}
		link := filepath.Join(latestDir, name)
		want := filepath.Join("..", version, name)
		if cur, err := os.Readlink(link); err == nil && cur == want {
			continue
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("replace %s: %w", link, err)
		}
		if err := os.Symlink(want, link); err != nil {
			return fmt.Errorf("link %s: %w", link, err)
		}
	}
	return nil
}

// ownedLatest reports whether <dl>/agent/latest is ours to rewrite. Two shapes
// exist in the wild: the image layout (a directory holding per-file symlinks
// into a version directory — ours) and a locally staged directory with real
// files (scripts/build.sh / manual drops — not ours, never touch).
func ownedLatest(latestDir string, log *slog.Logger) bool {
	st, err := os.Lstat(latestDir)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	if st.Mode()&os.ModeSymlink != 0 {
		// A symlinked "latest" directory is not a layout this code creates;
		// replacing it could break whatever set it up.
		log.Debug("agentupdate: agent/latest is a symlink, not touching it", "path", latestDir)
		return false
	}
	entries, err := os.ReadDir(latestDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		info, err := os.Lstat(filepath.Join(latestDir, e.Name()))
		if err == nil && info.Mode()&os.ModeSymlink == 0 {
			log.Debug("agentupdate: agent/latest holds real files, not touching it", "path", latestDir)
			return false
		}
	}
	return true
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".seed-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

package service

import (
	"os"
	"path/filepath"
	"testing"
)

// mkdir/writeFile build a fake root so the §5.3 detection table can be exercised
// on a host that is none of the three platforms (a dev Mac).
func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The regression this file exists for: /run/systemd/system is a *directory*, and
// fileExists() answers false for directories. Probing it with the file predicate
// made every systemd host report KindFallback — which disabled agent self-update
// (the panel then said "no service manager to restart the agent" on a host whose
// agent was running under systemd) and routed sing-box to the fallback branch.
func TestDetectSeesSystemdMarkerDirectory(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "run", "systemd", "system"))

	if got := detectAt(root); got != KindSystemd {
		t.Fatalf("detectAt with a /run/systemd/system directory = %v, want systemd", got)
	}
}

func TestDetectProcd(t *testing.T) {
	for _, marker := range []string{"sbin/procd", "etc/rc.common"} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, filepath.FromSlash(marker)))
			if got := detectAt(root); got != KindProcd {
				t.Fatalf("detectAt with %s = %v, want procd", marker, got)
			}
		})
	}
}

func TestDetectFallsBackOnAnEmptyRoot(t *testing.T) {
	if got := detectAt(t.TempDir()); got != KindFallback {
		t.Fatalf("detectAt on an empty root = %v, want fallback", got)
	}
}

// systemd wins over procd markers, matching the §5.3 table order.
func TestDetectPrefersSystemdOverProcd(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "run", "systemd", "system"))
	writeFile(t, filepath.Join(root, "sbin", "procd"))

	if got := detectAt(root); got != KindSystemd {
		t.Fatalf("detectAt with both markers = %v, want systemd", got)
	}
}

// The two predicates must stay distinguishable: this is the trap that caused the
// bug, so it is asserted rather than left as a comment.
func TestPredicatesDifferOnDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "marker")
	mkdir(t, dir)

	if !pathExists(dir) {
		t.Fatal("pathExists must see a directory")
	}
	if fileExists(dir) {
		t.Fatal("fileExists must not call a directory a file")
	}
	if pathExists(filepath.Join(dir, "nope")) || fileExists(filepath.Join(dir, "nope")) {
		t.Fatal("a missing path must answer false from both predicates")
	}
}

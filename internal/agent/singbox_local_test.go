package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/agent/service"
	"github.com/fonlan/fobe/internal/protocol"
)

// The local discovery scan is what makes a one-sing.sh host visible to the
// panel at all (§9.3 实现修订 2026-09-17). It runs against the effective
// layout, so every case here installs a fake root through service.SetWorkDir.

// fakeSingbox writes a stand-in binary that answers `version` like the real
// one. A shell script is enough: detectLocal only ever execs `version`.
func fakeSingbox(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "sing-box")
	script := "#!/bin/sh\necho 'sing-box version 1.13.0-beta.7'\necho\necho 'Environment: go1.25.6 linux/amd64'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return bin
}

func useFakeRoot(t *testing.T, dir string) {
	t.Helper()
	service.SetWorkDir(dir)
	t.Cleanup(func() { service.SetWorkDir(service.SingboxWorkDir) })
}

func TestDetectLocalReportsOneSingInstallation(t *testing.T) {
	dir := t.TempDir()
	useFakeRoot(t, dir)
	fakeSingbox(t, dir)

	config := `{"inbounds":[{"type":"anytls","listen_port":28711}]}`
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got := detectLocal()
	if !got.Present {
		t.Fatalf("Present = false, want true for an installed binary")
	}
	if got.Version != "1.13.0-beta.7" {
		t.Fatalf("Version = %q", got.Version)
	}
	if got.ConfigJSON != config {
		t.Fatalf("ConfigJSON = %q", got.ConfigJSON)
	}
	if got.ConfigSHA256 != sha256Hex([]byte(config)) {
		t.Fatalf("ConfigSHA256 = %q", got.ConfigSHA256)
	}
	if got.ConfigPath != configPath {
		t.Fatalf("ConfigPath = %q, want %q", got.ConfigPath, configPath)
	}
	if got.Error != "" {
		t.Fatalf("Error = %q, want empty", got.Error)
	}
	// Running is a process/unit question, and this test's fake binary is not
	// exec'd as a daemon: the report must not claim it is up.
	if got.Running {
		t.Fatal("Running = true for a binary that was never started")
	}
}

// A probe with no sing-box at all still reports: "nothing here" is the answer
// the panel needs to render "not installed" without guessing.
func TestDetectLocalOnCleanProbe(t *testing.T) {
	dir := t.TempDir()
	useFakeRoot(t, dir)

	got := detectLocal()
	if got.Present {
		t.Fatal("Present = true on a probe with no sing-box")
	}
	if got.Version != "" || got.ConfigJSON != "" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

// The binary exists but the config does not (or is unreadable): the report
// keeps Present and names the reason instead of pretending there is nothing.
func TestDetectLocalMissingConfig(t *testing.T) {
	dir := t.TempDir()
	useFakeRoot(t, dir)
	fakeSingbox(t, dir)

	got := detectLocal()
	if !got.Present {
		t.Fatal("Present = false, want true")
	}
	if got.ConfigJSON != "" {
		t.Fatalf("ConfigJSON = %q, want empty", got.ConfigJSON)
	}
	if !strings.Contains(got.Error, "config file does not exist") {
		t.Fatalf("Error = %q, want it to name the missing config", got.Error)
	}
}

// Cap: an oversized file is not shipped on the wire at all (and says so).
func TestDetectLocalCapsOversizedConfig(t *testing.T) {
	dir := t.TempDir()
	useFakeRoot(t, dir)
	fakeSingbox(t, dir)

	big := make([]byte, maxLocalConfigBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), big, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	got := detectLocal()
	if got.ConfigJSON != "" {
		t.Fatal("ConfigJSON should be empty for an oversized file")
	}
	if !strings.Contains(got.Error, "not reported") {
		t.Fatalf("Error = %q, want the cap mentioned", got.Error)
	}
}

// localEqual drives "send a frame only when something an operator could see
// changed" — a rescan of an untouched host must be silent.
func TestLocalEqual(t *testing.T) {
	a := &protocol.SingboxLocal{Present: true, Version: "1.13.0-beta.7"}
	if !localEqual(a, a) {
		t.Fatal("identical pointers must compare equal")
	}
	b := *a
	if !localEqual(a, &b) {
		t.Fatal("equal payloads must compare equal")
	}
	b.Version = "other"
	if localEqual(a, &b) {
		t.Fatal("a changed version must compare unequal")
	}
	if localEqual(nil, a) || localEqual(a, nil) {
		t.Fatal("nil vs non-nil must compare unequal")
	}
	if !localEqual(nil, nil) {
		t.Fatal("nil vs nil must compare equal")
	}
}

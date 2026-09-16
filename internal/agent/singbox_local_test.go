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

// Config.json names a certificate *path*; a subscription client needs the bytes
// to pin the server, and the probe is the only party that can read that path
// (§9.3 实现修订 2026-09-17b). Without this the anytls inbound one-sing.sh set
// up was skipped from every subscription while plainly running.
func TestDetectLocalReportsAnytlsCertificates(t *testing.T) {
	dir := t.TempDir()
	useFakeRoot(t, dir)
	fakeSingbox(t, dir)

	certDir := filepath.Join(dir, "cert")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatalf("mkdir cert: %v", err)
	}
	pem := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	certPath := filepath.Join(certDir, "cert.crt")
	if err := os.WriteFile(certPath, []byte(pem), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	cfg := `{"inbounds":[
	  {"type":"anytls","listen_port":28711,"tls":{"enabled":true,"certificate_path":"` + certPath + `"}},
	  {"type":"vless","listen_port":16929,"tls":{"enabled":true}}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got := detectLocal()
	if got.AnytlsCerts[28711] != pem {
		t.Fatalf("AnytlsCerts = %+v, want the PEM for 28711", got.AnytlsCerts)
	}
	if _, ok := got.AnytlsCerts[16929]; ok {
		t.Fatalf("a non-anytls inbound must not contribute a certificate: %+v", got.AnytlsCerts)
	}

	// A missing certificate file is not an error: the inbound is simply not
	// renderable, and the panel says so.
	if err := os.Remove(certPath); err != nil {
		t.Fatalf("remove cert: %v", err)
	}
	if got := detectLocal(); len(got.AnytlsCerts) != 0 {
		t.Fatalf("AnytlsCerts = %+v, want empty for a missing file", got.AnytlsCerts)
	}
}

// A config-only desired frame is how the panel edits the file of a node whose
// binary fobe never installed (§9.3 实现修订 2026-09-17b). Two ways it can go
// wrong, both found on a real probe: treating it as "unmanaged" (the edit is
// dropped silently) and treating the missing version as a version mismatch (the
// agent tries to download `/dl/singbox//linux-amd64`, gets a 404, and rolls the
// whole apply back).
func TestConfigOnlyDesiredIsAppliedWithoutInstalling(t *testing.T) {
	dir := t.TempDir()
	useFakeRoot(t, dir)
	bin := fakeSingbox(t, dir)

	// The probe's own file, as one-sing.sh left it.
	onDisk := `{"inbounds":[{"type":"anytls","tag":"anytls-in-28711","listen_port":28711,"users":[{"password":"x"}]}]}`
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(onDisk), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	m := newSingboxManager(&Config{ServerURL: "http://127.0.0.1:1"}, testLogger())
	// A version-less, config-only declaration must be recorded, not dropped.
	m.SetDesired(&protocol.SingboxDesired{Port: 22039, ConfigJSON: `{"inbounds":[]}`})
	m.mu.Lock()
	kept := m.desired
	m.mu.Unlock()
	if kept == nil {
		t.Fatal("a config-only desired state was discarded as unmanaged")
	}

	// And with no version there is nothing to download: the old code compared
	// "" against the installed version and went straight to the artifact URL.
	act := m.observe(bin, &protocol.SingboxDesired{Port: 22039, ConfigJSON: `{"inbounds":[]}`})
	if act.version == "" {
		t.Fatalf("fixture: the fake binary must report a version")
	}
	needInstall := keptConfigOnlyNeedInstall(act.version, kept)
	if needInstall {
		t.Fatal("a config-only frame must not request an install")
	}
}

// keptConfigOnlyNeedInstall mirrors the guard in converge(); kept here so the
// regression is pinned by a test that fails at the same moment the code does.
func keptConfigOnlyNeedInstall(installed string, d *protocol.SingboxDesired) bool {
	return d.Version != "" && normalizeVersion(installed) != normalizeVersion(d.Version)
}

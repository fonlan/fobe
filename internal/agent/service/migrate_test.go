package service

import (
	"os"
	"path/filepath"
	"testing"
)

// tempLayout builds a legacy → current layout entirely under t.TempDir(), so
// the migration can be tested without touching the real /etc (§9.3 实现修订).
func tempLayout(t *testing.T) singboxLayout {
	t.Helper()
	root := t.TempDir()
	legacyDir := filepath.Join(root, "etc", "sing-box")
	legacyBinDir := filepath.Join(root, "usr", "local", "bin")
	cur := filepath.Join(root, "etc", "one-sing")
	return singboxLayout{
		bin:          filepath.Join(cur, "sing-box"),
		config:       filepath.Join(cur, "config.json"),
		certDir:      filepath.Join(cur, "cert"),
		cert:         filepath.Join(cur, "cert", SingboxCertFile),
		key:          filepath.Join(cur, "cert", SingboxKeyFile),
		legacyBins:   []string{filepath.Join(legacyBinDir, "sing-box")},
		legacyConfig: filepath.Join(legacyDir, "config.json"),
		legacyCert:   filepath.Join(legacyDir, "cert", "cert.pem"),
		legacyKey:    filepath.Join(legacyDir, "cert", "key.pem"),
	}
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateSingboxLayoutMovesLegacyTree covers the upgrade path: binary,
// rollback copy, config and certificate all move, the old directories go away,
// and a second run is a no-op.
func TestMigrateSingboxLayoutMovesLegacyTree(t *testing.T) {
	l := tempLayout(t)
	write(t, l.legacyBins[0], "BIN", 0o755)
	write(t, l.legacyBins[0]+".prev", "OLD-BIN", 0o755)
	write(t, l.legacyConfig, `{"inbounds":[]}`, 0o600)
	write(t, l.legacyCert, "CERT-PEM", 0o600)
	write(t, l.legacyKey, "KEY-PEM", 0o600)

	notes, err := migrateSingboxLayout(l)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 5 moves + the two now-empty legacy directories.
	if len(notes) != 7 {
		t.Fatalf("notes = %v, want 7 (5 moves + 2 removals)", notes)
	}
	for path, want := range map[string]string{
		l.bin:           "BIN",
		l.bin + ".prev": "OLD-BIN",
		l.config:        `{"inbounds":[]}`,
		l.cert:          "CERT-PEM",
		l.key:           "KEY-PEM",
		filepath.Join(l.certDir, SingboxCertFile): "CERT-PEM",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(raw) != want {
			t.Fatalf("%s = %q, want %q", path, raw, want)
		}
	}
	// The binary stays executable: a copy fallback must not drop the mode.
	if st, err := os.Stat(l.bin); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode = %v (%v)", st.Mode(), err)
	}
	// Empty legacy directories are removed.
	if _, err := os.Stat(filepath.Dir(l.legacyConfig)); !os.IsNotExist(err) {
		t.Fatalf("legacy config dir survived: %v", err)
	}

	// Idempotent: nothing left to move, nothing to report.
	again, err := migrateSingboxLayout(l)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run = %v, %v", again, err)
	}
}

// TestMigrateSingboxLayoutKeepsNewerFiles: the migration never overwrites a
// current-layout file, so running it on an already-upgraded (or one-sing.sh
// managed) probe cannot clobber the live binary or certificate.
func TestMigrateSingboxLayoutKeepsNewerFiles(t *testing.T) {
	l := tempLayout(t)
	write(t, l.bin, "CURRENT", 0o755)
	write(t, l.cert, "CURRENT-CERT", 0o600)
	write(t, l.legacyBins[0], "OLD", 0o755)
	write(t, l.legacyCert, "OLD-CERT", 0o600)

	notes, err := migrateSingboxLayout(l)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
	raw, _ := os.ReadFile(l.bin)
	if string(raw) != "CURRENT" {
		t.Fatalf("binary overwritten: %q", raw)
	}
	cert, _ := os.ReadFile(l.cert)
	if string(cert) != "CURRENT-CERT" {
		t.Fatalf("certificate overwritten: %q", cert)
	}
}

// TestMigrateSingboxLayoutOnCleanProbe: nothing to do on a probe that never
// ran sing-box, and no error either.
func TestMigrateSingboxLayoutOnCleanProbe(t *testing.T) {
	l := tempLayout(t)
	notes, err := migrateSingboxLayout(l)
	if err != nil || len(notes) != 0 {
		t.Fatalf("clean probe = %v, %v", notes, err)
	}
}

// TestRetireLegacySingboxUnit covers the §9.3 rename: fobe's own old unit is
// stopped, disabled and deleted exactly once, the init script is only removed
// when it is fobe's, and everything else is a no-op.
func TestRetireLegacySingboxUnit(t *testing.T) {
	var calls [][]string
	old := serviceRun
	serviceRun = func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	t.Cleanup(func() { serviceRun = old })

	// systemd: our own old unit file is unambiguously ours — retire it.
	unit := filepath.Join(t.TempDir(), legacySingboxUnitName)
	write(t, unit, "[Unit]\nDescription=sing-box (managed by fobe-agent)\n", 0o644)
	retired, err := retireSystemdUnit(unit, legacySingboxUnitName)
	if err != nil || !retired {
		t.Fatalf("retire = %v, %v", retired, err)
	}
	if fileExists(unit) {
		t.Fatalf("legacy unit file survived retirement")
	}
	if len(calls) != 2 || calls[0][3] != legacySingboxUnitName ||
		calls[1][0] != "systemctl" || calls[1][1] != "daemon-reload" {
		t.Fatalf("calls = %v", calls)
	}

	// A missing unit is not a retirement.
	missing := filepath.Join(t.TempDir(), "absent.service")
	if retired, err := retireSystemdUnit(missing, legacySingboxUnitName); retired || err != nil {
		t.Fatalf("missing unit = %v, %v", retired, err)
	}
	if len(calls) != 2 {
		t.Fatalf("unexpected extra systemctl calls: %v", calls)
	}
}

// The procd half must never delete a stranger's /etc/init.d/sing-box: on
// OpenWrt that name usually belongs to the distribution's own sing-box package.
func TestOwnsLegacyInitScript(t *testing.T) {
	ours := filepath.Join(t.TempDir(), "sing-box")
	write(t, ours, "#!/bin/sh /etc/rc.common\nUSE_PROCD=1\nprocd_set_param command /etc/one-sing/sing-box run -c /etc/one-sing/config.json\n", 0o755)
	if !ownsLegacyInitScript(ours) {
		t.Fatalf("fobe's own generated init script not recognised")
	}

	distro := filepath.Join(t.TempDir(), "sing-box")
	write(t, distro, "#!/bin/sh /etc/rc.common\nUSE_PROCD=1\nprocd_set_param command /usr/bin/sing-box run -c /etc/sing-box/config.json\n", 0o755)
	if ownsLegacyInitScript(distro) {
		t.Fatalf("a distribution's sing-box init script must not be claimed as ours")
	}
	if ownsLegacyInitScript(filepath.Join(t.TempDir(), "absent")) {
		t.Fatalf("a missing init script is not ours")
	}
}

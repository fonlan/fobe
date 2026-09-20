// Package service abstracts the platform service manager (design.md §5.3):
// systemd on regular Linux, procd on OpenWrt, foreground fallback elsewhere.
// The agent writes its own service files so behavior is identical everywhere.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type Kind int

const (
	KindSystemd Kind = iota
	KindProcd
	KindFallback
)

func (k Kind) String() string {
	switch k {
	case KindSystemd:
		return "systemd"
	case KindProcd:
		return "procd"
	default:
		return "fallback"
	}
}

// Detect picks the service manager for this host (§5.3 table).
func Detect() Kind { return detectAt("/") }

// detectAt is Detect with the filesystem root spelled out so the table can be
// unit-tested somewhere that is none of the three platforms (a dev Mac).
//
// /run/systemd/system is a *directory* — the marker systemd creates for itself
// — so it must be probed with pathExists, NOT fileExists. Using the file
// predicate here was a silent, system-wide bug: every systemd host (i.e. every
// normal Debian/Ubuntu VPS, and the dev VM this was caught on) answered
// KindFallback, which then
//
//   - reported caps.systemd=false / fallback=true to the panel,
//   - disabled agent self-update (§5.5 hangs off Detect() != KindFallback),
//     so the panel showed "no service manager to restart the agent" on hosts
//     that were running the agent under systemd the whole time,
//   - pushed sing-box onto the spawn-and-reap fallback branch instead of a
//     systemd unit, and made the panel's start/stop/restart errors say
//     "no service manager detected".
func detectAt(root string) Kind {
	switch {
	case pathExists(filepath.Join(root, "/run/systemd/system")):
		return KindSystemd
	case fileExists(filepath.Join(root, "/sbin/procd")), fileExists(filepath.Join(root, "/etc/rc.common")):
		return KindProcd
	default:
		return KindFallback
	}
}

// sing-box on-disk layout (design §9.3 实现修订).
//
// The paths follow one-sing.sh — the server-side script this project mirrors
// for service management and the anytls inbound — so a probe already running
// one-sing.sh can be taken over without re-downloading a binary or
// regenerating a certificate:
//
//	/etc/one-sing/sing-box      the static binary
//	/etc/one-sing/config.json   the config pushed by the panel
//	/etc/one-sing/cert/         cert.crt + private.key
//
// /etc is overlay-persistent on OpenWrt too, so one layout covers systemd,
// procd and the foreground fallback.
//
// The *service* name is one-sing's as well, on purpose (§9.3 实现修订
// 2026-09-16): one-sing.sh manages `one-sing.service` over exactly this binary
// and config, so an operator's script and the panel must address the same unit
// instead of each disabling the other's. The cost is the mirror image of the
// old choice — the unit is shared, so whichever side writes it last wins, and
// fobe's uninstall/upgrade touches a unit one-sing.sh also knows about.
const (
	// SingboxWorkDir is the one-sing-compatible working directory — the
	// *default* layout root. An unprivileged deployment moves it at startup
	// via SetWorkDir (§5.3 实现修订 2026-09-16); EffectiveWorkDir reports what
	// is actually in effect.
	SingboxWorkDir = "/etc/one-sing"
	// SingboxCertFile / SingboxKeyFile are the certificate names inside the
	// cert directory. The server's generated config.json refers to these
	// paths, so the two must agree (see internal/server/singbox/config.go);
	// a relocated layout remaps the prefix agent-side (relocateConfig).
	SingboxCertFile = "cert.crt"
	SingboxKeyFile  = "private.key"

	// SingboxUnitName is the systemd unit fobe owns, and SingboxUnitPath is
	// where it lives. Both come from one-sing.sh verbatim (SING_SERVICE).
	SingboxUnitName = "one-sing.service"
	SingboxUnitPath = "/etc/systemd/system/one-sing.service"
	// SingboxInitPath is the procd equivalent. one-sing.sh is Debian-only, so
	// this name is fobe's own — and it deliberately stops using
	// `/etc/init.d/sing-box`, which on OpenWrt often belongs to the
	// distribution's own sing-box package (fobe used to overwrite it).
	SingboxInitPath = "/etc/init.d/one-sing"
	// legacySingboxUnitPath / legacySingboxInitPath are fobe's pre-revision
	// names. A probe installed before the rename has them enabled and running;
	// they are retired (stop + disable + remove) before fobe writes its own, or
	// two supervisors would fight over the same binary, config and port.
	legacySingboxUnitName = "fobe-singbox.service"
	legacySingboxUnitPath = "/etc/systemd/system/fobe-singbox.service"
	legacySingboxInitPath = "/etc/init.d/sing-box"
)

// The layout paths are package state rather than consts so SetWorkDir can
// relocate them. They are written once at agent startup, before the sing-box
// manager goroutine exists, and read-only afterwards — no locking on purpose.
var (
	singboxWorkDir    = SingboxWorkDir
	singboxBinPath    = SingboxWorkDir + "/sing-box"
	singboxConfigPath = SingboxWorkDir + "/config.json"
	singboxCertDir    = SingboxWorkDir + "/cert"
)

// SetWorkDir redirects the one-sing layout root. Call it once at startup,
// before the first SingboxPaths()/MigrateSingboxLayout() use. Passing the
// default restores it (tests rely on that to clean up).
func SetWorkDir(dir string) {
	singboxWorkDir = dir
	singboxBinPath = filepath.Join(dir, "sing-box")
	singboxConfigPath = filepath.Join(dir, "config.json")
	singboxCertDir = filepath.Join(dir, "cert")
}

// EffectiveWorkDir is the layout root actually in effect.
func EffectiveWorkDir() string { return singboxWorkDir }

// legacySingbox holds the pre-revision locations, so an installed probe
// upgrades in place instead of re-downloading its binary.
var legacySingbox = struct {
	bins    []string
	config  string
	certDir string
	cert    string
	key     string
}{
	bins:    []string{"/usr/local/bin/sing-box", "/usr/bin/sing-box"},
	config:  "/etc/sing-box/config.json",
	certDir: "/etc/sing-box/cert",
	cert:    "/etc/sing-box/cert/cert.pem",
	key:     "/etc/sing-box/cert/key.pem",
}

// SingboxPaths returns the binary, config and certificate directory (§5.4).
func SingboxPaths() (bin, config, certDir string) {
	return singboxBinPath, singboxConfigPath, singboxCertDir
}

// InstallSingbox writes the sing-box service definition. The config file is
// written separately by the agent when applying desired state.
//
// The unit mirrors one-sing.sh's proven settings — capability bounding set,
// ExecReload on SIGHUP, unlimited file descriptors, 10 s restart backoff —
// under fobe's own unit name. NoNewPrivileges stays: the agent never needs to
// gain privileges after exec, and the ambient capabilities it does need are
// granted explicitly.
func InstallSingbox(binPath, configPath string) error {
	switch Detect() {
	case KindSystemd:
		unit := fmt.Sprintf(`[Unit]
Description=one-sing service (managed by fobe-agent)
After=network-online.target
Wants=network-online.target

[Service]
User=root
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_SYS_PTRACE CAP_DAC_READ_SEARCH
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_SYS_PTRACE CAP_DAC_READ_SEARCH
ExecStart=%s run -c %s
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=10s
LimitNOFILE=infinity
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, binPath, configPath)
		if err := os.WriteFile(SingboxUnitPath, []byte(unit), 0o644); err != nil {
			return fmt.Errorf("write unit: %w", err)
		}
		if err := run("systemctl", "daemon-reload"); err != nil {
			return err
		}
		// WantedBy in the file does NOT create the multi-user.target.wants
		// symlink — only `enable` does. Without it sing-box comes up only
		// because the agent converges after boot, so a host whose agent is
		// broken (or stopped) would reboot without its inbound.
		return run("systemctl", "enable", SingboxUnitName)

	case KindProcd:
		init := fmt.Sprintf(`#!/bin/sh /etc/rc.common
USE_PROCD=1
START=99

start_service() {
	procd_open_instance
	procd_set_param command %s run -c %s
	procd_set_param respawn 3600 5 5
	procd_set_param stdout 0
	procd_set_param stderr 0
	procd_close_instance
}
`, binPath, configPath)
		if err := os.WriteFile(SingboxInitPath, []byte(init), 0o755); err != nil {
			return fmt.Errorf("write init.d: %w", err)
		}
		return nil // enable/start happens on first apply

	default:
		return fmt.Errorf("no service manager detected")
	}
}

// EnableSingbox marks the sing-box service as enabled. systemd is handled by
// InstallSingbox (it runs `systemctl enable`); this is the procd half.
func EnableSingbox() error {
	switch Detect() {
	case KindProcd:
		return run(SingboxInitPath, "enable")
	default:
		return nil // systemd: nothing to do; fallback: no boot persistence
	}
}

// RestartSingbox restarts the managed sing-box service.
func RestartSingbox() error {
	switch Detect() {
	case KindSystemd:
		return run("systemctl", "restart", SingboxUnitName)
	case KindProcd:
		return run(SingboxInitPath, "restart")
	default:
		return fmt.Errorf("no service manager detected")
	}
}

// StopSingbox / StartSingbox for the panel's start/stop actions.
func StopSingbox() error {
	switch Detect() {
	case KindSystemd:
		return run("systemctl", "stop", SingboxUnitName)
	case KindProcd:
		return run(SingboxInitPath, "stop")
	default:
		return fmt.Errorf("no service manager detected")
	}
}

func StartSingbox() error {
	switch Detect() {
	case KindSystemd:
		return run("systemctl", "start", SingboxUnitName)
	case KindProcd:
		return run(SingboxInitPath, "start")
	default:
		return fmt.Errorf("no service manager detected")
	}
}

// SingboxActive reports whether the platform service manager currently owns a
// live sing-box service (§5.3). The fallback path answers false by definition:
// there the agent owns the process itself (singboxManager.proc).
func SingboxActive() bool {
	switch Detect() {
	case KindSystemd:
		return run("systemctl", "is-active", "--quiet", SingboxUnitName) == nil
	case KindProcd:
		return run(SingboxInitPath, "status") == nil
	default:
		return false
	}
}

// UninstallSingbox removes everything fobe installed for sing-box: the service
// definition, the binary (with its .prev/.download siblings), the config and
// the self-signed certificate (§9.2 实现修订 2026-09-16).
//
// The kind is passed in rather than re-detected because an unprivileged agent
// must never call systemctl: Detect() still answers systemd there, and every
// invocation would come back as a polkit refusal that looks like a failed
// uninstall while the service was never touched (same trap as
// retireLegacyUnit). The caller passes what the sing-box lifecycle actually
// drives (singboxKind).
//
// Steps are best-effort and joined: a missing unit file or an already-absent
// binary is not a failure (the desired state may be re-delivered), but a real
// permission error has to reach the panel instead of being swallowed.
func UninstallSingbox(kind Kind) error {
	var errs []error
	removeFile := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	switch kind {
	case KindSystemd:
		// --now also stops it; leaving the unit enabled would keep a unit
		// pointing at a deleted binary, which systemd restarts forever.
		_ = run("systemctl", "disable", "--now", SingboxUnitName)
		removeFile(SingboxUnitPath)
		_ = run("systemctl", "daemon-reload")
	case KindProcd:
		_ = run(SingboxInitPath, "stop")
		_ = run(SingboxInitPath, "disable")
		removeFile(SingboxInitPath)
	}
	// The pre-rename unit lives on machines installed before §9.3's revision;
	// it supervises the same binary we are about to delete.
	if kind != KindFallback {
		if _, err := RetireLegacySingboxUnit(); err != nil {
			errs = append(errs, fmt.Errorf("retire legacy unit: %w", err))
		}
	}

	bin, config, certDir := SingboxPaths()
	for _, p := range []string{bin, bin + ".prev", bin + ".download", config, config + ".prev"} {
		removeFile(p)
	}
	// The certificate is fobe's own (self-signed, generated on the probe), so
	// it goes with the rest: a reinstall mints a new pair and the subscription
	// re-renders it. Doing so is what makes the node disappear from §10
	// subscriptions — they list a node only while it has a pinned certificate.
	if err := os.RemoveAll(certDir); err != nil {
		errs = append(errs, fmt.Errorf("remove %s: %w", certDir, err))
	}
	// Only when empty: /etc/one-sing may hold an operator's other files.
	if err := os.Remove(filepath.Dir(bin)); err != nil && !os.IsNotExist(err) && !isNotEmpty(err) {
		errs = append(errs, fmt.Errorf("remove %s: %w", filepath.Dir(bin), err))
	}
	return errors.Join(errs...)
}

// isNotEmpty reports the ENOTEMPTY/EEXIST family, i.e. "there is still
// something there and that is fine".
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// archIdent returns the artifact triple used in /dl paths.
func ArchIdent() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// --- layout migration & service retirement (design §9.3 实现修订) ----------

// singboxLayout names every path the migration touches. Keeping it a value
// (rather than reading the package constants inline) is what makes the
// migration testable: the test runs it against a t.TempDir() tree instead of
// the real /etc.
type singboxLayout struct {
	bin     string
	config  string
	certDir string
	cert    string
	key     string

	legacyBins   []string
	legacyConfig string
	legacyCert   string
	legacyKey    string
}

func currentLayout() singboxLayout {
	return singboxLayout{
		bin:          singboxBinPath,
		config:       singboxConfigPath,
		certDir:      singboxCertDir,
		cert:         filepath.Join(singboxCertDir, SingboxCertFile),
		key:          filepath.Join(singboxCertDir, SingboxKeyFile),
		legacyBins:   legacySingbox.bins,
		legacyConfig: legacySingbox.config,
		legacyCert:   legacySingbox.cert,
		legacyKey:    legacySingbox.key,
	}
}

// MigrateSingboxLayout moves an installed probe from the pre-revision layout
// (/usr/local/bin/sing-box, /etc/sing-box/{config.json,cert/}) to the
// one-sing-compatible one, and returns one note per moved item.
//
// It is idempotent and safe on every start: a move happens only when the
// destination is missing and the source exists, so a probe that never ran
// sing-box is untouched and a migrated one is a no-op. The running process
// keeps its inode across a rename; the next apply rewrites the unit with the
// new ExecStart path.
//
// Errors do not roll anything back — the convergence loop re-downloads what it
// needs anyway — so the caller treats them as a WARN.
func MigrateSingboxLayout() ([]string, error) {
	return migrateSingboxLayout(currentLayout())
}

func migrateSingboxLayout(l singboxLayout) ([]string, error) {
	var notes []string
	move := func(src, dst string, mode os.FileMode, what string) error {
		if fileExists(dst) || !fileExists(src) {
			return nil
		}
		if err := moveFile(src, dst, mode); err != nil {
			return fmt.Errorf("migrate %s: %w", what, err)
		}
		notes = append(notes, what+" → "+dst)
		return nil
	}

	for _, legacyBin := range l.legacyBins {
		if err := move(legacyBin, l.bin, 0o755, "binary"); err != nil {
			return notes, err
		}
		// The rollback copy and an interrupted download follow the binary, or
		// the first apply after the upgrade would start from nothing.
		for _, suffix := range []string{".prev", ".download"} {
			if err := move(legacyBin+suffix, l.bin+suffix, 0o755, "binary"+suffix); err != nil {
				return notes, err
			}
		}
	}
	if err := move(l.legacyConfig, l.config, 0o600, "config"); err != nil {
		return notes, err
	}
	if err := move(l.legacyConfig+".prev", l.config+".prev", 0o600, "config.prev"); err != nil {
		return notes, err
	}
	if err := move(l.legacyCert, l.cert, 0o600, "certificate"); err != nil {
		return notes, err
	}
	if err := move(l.legacyKey, l.key, 0o600, "private key"); err != nil {
		return notes, err
	}

	// Drop the old directories once they are empty; a non-empty one is left
	// alone (an operator may have put something there).
	for _, dir := range []string{filepath.Dir(l.legacyCert), filepath.Dir(l.legacyConfig)} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			if err := os.Remove(dir); err == nil {
				notes = append(notes, "removed empty "+dir)
			}
		}
	}
	return notes, nil
}

// moveFile renames src onto dst, falling back to a copy when the two live on
// different filesystems (a /usr/local/bin → /etc move on a split-root host).
func moveFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, raw, mode); err != nil {
		return err
	}
	return os.Remove(src)
}

// serviceRun is the systemctl seam: the retirement path is otherwise impossible
// to test without touching a live service manager.
var serviceRun = run

// RetireLegacySingboxUnit stops and removes the service fobe used before the
// §9.3 rename (fobe-singbox.service / /etc/init.d/sing-box), so it cannot fight
// the unit fobe owns now: both supervise the same binary, config and inbound
// port, and two supervisors restarting one process flap forever while the panel
// addresses a unit it does not control.
//
// It reports whether anything was retired. Only systemd is handled for the unit
// file; on procd the legacy init script is removed **only when it is fobe's
// own** (it carries fobe's one-sing paths) — `/etc/init.d/sing-box` on OpenWrt
// frequently belongs to the distribution's sing-box package, and deleting a
// stranger's service would be far worse than leaving a dead file behind.
func RetireLegacySingboxUnit() (bool, error) {
	switch Detect() {
	case KindSystemd:
		return retireSystemdUnit(legacySingboxUnitPath, legacySingboxUnitName)
	case KindProcd:
		if !ownsLegacyInitScript(legacySingboxInitPath) {
			return false, nil
		}
		_ = serviceRun(legacySingboxInitPath, "stop")
		_ = serviceRun(legacySingboxInitPath, "disable")
		if err := os.Remove(legacySingboxInitPath); err != nil && !os.IsNotExist(err) {
			return true, fmt.Errorf("remove %s: %w", legacySingboxInitPath, err)
		}
		return true, nil
	default:
		return false, nil
	}
}

func retireSystemdUnit(unitPath, unitName string) (bool, error) {
	if !fileExists(unitPath) {
		return false, nil
	}
	if err := serviceRun("systemctl", "disable", "--now", unitName); err != nil {
		return true, fmt.Errorf("disable %s: %w", unitName, err)
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return true, fmt.Errorf("remove %s: %w", unitPath, err)
	}
	_ = serviceRun("systemctl", "daemon-reload")
	return true, nil
}

// ownsLegacyInitScript reports whether the procd script at path was written by
// fobe: ours always names the one-sing layout (the unit is generated with
// absolute binary/config paths under the work dir), which a distribution
// package's script never does.
func ownsLegacyInitScript(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(raw, []byte("one-sing"))
}

// pathExists reports whether anything at all (file or directory) is at p.
//
// The two predicates are not interchangeable, and mixing them up is exactly the
// bug documented on detectAt: /run/systemd/system is a directory, so only this
// one can see it.
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// fileExists reports whether a *regular-ish file* is at p; a directory answers
// false on purpose (it is the right question for a unit file, a binary or a
// config — see callers).
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "),
			err, truncate(string(out), 400))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

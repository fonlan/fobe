package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fonlan/fobe/internal/agent/service"
	"github.com/fonlan/fobe/internal/protocol"
)

// sing-box lifecycle (design §9): the server only ships a desired state
// ({version, config, port}); the agent converges to it through the three
// gates (check → start → observe) and rolls back to .prev on any failure.
const (
	// maxDownloadBytes caps the artifact the agent is willing to write. /dl
	// serves the *extracted* binary, and modern sing-box linux-amd64-musl
	// builds are ~90 MiB (1.14.1 = 91,891,552 B, 1.15.0-alpha.4 = 92,895,232 B):
	// the old 64 MiB cap rejected every current release — after transferring
	// the whole file — and then rolled back, so no install ever succeeded.
	// Kept in step with the server's own defaultMaxDownloadBytes (256 MiB).
	maxDownloadBytes = 256 << 20
	// downloadTimeout is the whole-artifact budget. 5 min was calibrated when
	// artifacts were a third of today's size; a ~90 MiB download needs
	// >2.5 Mbit/s sustained to fit, which a home uplink on a remote probe
	// does not always deliver (and the failure looks exactly like a rejected
	// artifact: download error → rollback).
	downloadTimeout = 15 * time.Minute
	checkTimeout    = 30 * time.Second
	versionTimeout  = 10 * time.Second
	observeWindow   = 30 * time.Second // gate ③
	observeStep     = time.Second      // per-second TCP retry
	tcpDialTimeout  = time.Second
	convergeEvery   = 60 * time.Second // periodic convergence/report tick
	retryBackoff    = 5 * time.Minute  // min spacing between watchdog retries

	fwCmdTimeout         = 10 * time.Second // per firewall command budget (§9.2)
	nftablesConfPath     = "/etc/nftables.conf"
	minInstallFreeBytes  = 64 << 20 // §5.4: refuse installs under 64 MiB free…
	minInstallFreeInodes = 100      // §5.4: …or under 100 free inodes
)

type singboxManager struct {
	cfg *Config
	log *slog.Logger
	hc  *http.Client // injectable for tests

	mu               sync.Mutex
	desired          *protocol.SingboxDesired
	desiredUninstall bool // panel asked for removal (§9.2 实现修订 2026-09-16)
	uninstallPort    int  // port carried through a removal into the absent report
	state            *protocol.SingboxState
	proc             *os.Process           // fallback-mode child
	lastAttempt      time.Time             // last convergence that reached the change path
	lastErr          string                // last reported error; sticky while off-target (see reportOnly)
	lastFwPort       int                   // port the firewall pass already ran for (§9.2)
	lastFwHint       string                // manual command when that pass failed (""=allowed)
	rollbackSeen     bool                  // one-shot: a rollback happened since last report (§15)
	retiredUnit      bool                  // one-shot: the pre-rename fobe-singbox unit was removed (§9.3)
	openFw           func(port int) string // firewall pass, injectable for tests

	kick    chan struct{} // nudge: desired changed / watchdog fired (buffered 1)
	changed chan struct{} // nudge: reported state changed (buffered 1)

	// local / changedLocal carry the *discovery* half (§9.3 实现修订
	// 2026-09-17): what sing-box already runs on this host, whether or not fobe
	// manages it. Kept separate from state so a discovery change cannot be
	// mistaken for a managed-state change (the panel's alerts key off the
	// latter).
	local        *protocol.SingboxLocal
	changedLocal chan struct{}
}

func newSingboxManager(cfg *Config, log *slog.Logger) *singboxManager {
	m := &singboxManager{
		cfg:          cfg,
		log:          log,
		hc:           &http.Client{Timeout: downloadTimeout},
		kick:         make(chan struct{}, 1),
		changed:      make(chan struct{}, 1),
		changedLocal: make(chan struct{}, 1),
	}
	m.openFw = m.openFirewallPort
	return m
}

// singboxKind is the service-manager kind the sing-box lifecycle actually
// drives. An unprivileged agent cannot write /etc/systemd/system units nor
// talk to the system bus (polkit refuses), so it always owns the process
// itself — the fallback spawn branch, which is a complete implementation
// (spawn + watchdog + orphan cleanup). The inbound port is panel-assigned in
// 10000–60000 (§9.3), so no bind privilege is needed (§5.3 实现修订
// 2026-09-16).
func singboxKind() service.Kind {
	if !privileged() {
		return service.KindFallback
	}
	return service.Detect()
}

// relocateConfig remaps the absolute paths the server bakes into the desired
// config onto the effective layout. BuildNodeConfig (§9.3) declares the
// certificate pair under the default /etc/one-sing; a probe whose sing-box
// home moved (unprivileged mode, FOBE_SINGBOX_HOME) rewrites that prefix so
// sing-box reads the agent-owned copies. Write and hash-compare must both use
// the relocated form, or every 60s tick would see a mismatch and restart
// sing-box forever.
func relocateConfig(configJSON string) string {
	if service.EffectiveWorkDir() == service.SingboxWorkDir {
		return configJSON
	}
	return strings.ReplaceAll(configJSON, service.SingboxWorkDir, service.EffectiveWorkDir())
}

// SetDesired records the latest declaration and nudges convergence. A nil
// (or empty-Version) desired means "not managed": the manager reports
// nothing and never touches a running sing-box (§9 版本显式).
//
// A desired state carrying Uninstall is the operator's removal request
// (§9.2 实现修订 2026-09-16). It is recorded rather than executed here: the
// read loop must not block on systemctl and file removal, and the flag has to
// survive a failed attempt so the 60s tick retries it.
func (m *singboxManager) SetDesired(d *protocol.SingboxDesired) {
	m.mu.Lock()
	if d != nil && d.Uninstall {
		// The declared port is remembered rather than acted on: the absent
		// report carries it back, so an operator who uninstalls and reinstalls
		// keeps the inbound port (and whatever firewall rule goes with it)
		// instead of being handed a fresh random one.
		m.desiredUninstall = true
		if d.Port > 0 {
			m.uninstallPort = d.Port
		}
		m.mu.Unlock()
		m.nudge(m.kick)
		return
	}
	m.desiredUninstall = false
	// "No version" means unmanaged (§9 版本显式) — but only when there is no
	// config either. A config-only declaration is how the panel edits the file
	// of a node it never installed (an operator's one-sing.sh setup): the
	// version is empty because fobe does not own the binary, while the config is
	// the whole point of the frame. Treating that as "unmanaged" is what made
	// every edit to such a node vanish without a trace.
	if d != nil && normalizeVersion(d.Version) == "" && d.ConfigJSON == "" {
		d = nil
	}
	// A different target invalidates the stored error: it described the
	// previous attempt, and repeating it would put the wrong cause on screen.
	if d != nil && (m.desired == nil || normalizeVersion(m.desired.Version) != normalizeVersion(d.Version)) {
		m.lastErr = ""
	}
	m.desired = d
	m.mu.Unlock()
	m.nudge(m.kick)
}

// Nudge asks for an observe/report cycle (e.g. after a panel start/stop cmd).
func (m *singboxManager) Nudge() { m.nudge(m.kick) }

func (m *singboxManager) nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Snapshot returns the last reported state, nil while sing-box is unmanaged.
func (m *singboxManager) Snapshot() *protocol.SingboxState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return nil
	}
	cp := *m.state
	return &cp
}

// Changed is the state-change notification stream (buffered, never closed —
// sessions come and go, the manager outlives them).
func (m *singboxManager) Changed() <-chan struct{} { return m.changed }

// Run drives convergence forever: kicks (desired updates, watchdog) and a
// 60s periodic tick. A single goroutine ⇒ at most one convergence at a time.
//
// The local discovery scan (§9.3 实现修订 2026-09-17) hangs off the same tick
// but is *not* conditional on the desired state: a probe managed by
// one-sing.sh has no desired state at all, and that is exactly the probe the
// panel used to be blind to. State, not search: the scan is read-only (no
// systemctl writes, no file writes), so running it every minute on a node fobe
// does not manage cannot disturb the operator's service.
func (m *singboxManager) Run() {
	tick := time.NewTicker(convergeEvery)
	defer tick.Stop()
	m.scanLocal() // first report without waiting a minute
	for {
		select {
		case <-m.kick:
			m.converge()
			// A panel edit changes both managed state and the local config snapshot.
			// Scan before the next 60s tick so a newly added inbound can promptly
			// acknowledge that it is reachable.
			m.scanLocal()
		case <-tick.C:
			if m.needWatch() {
				m.converge()
			} else {
				m.reportOnly()
			}
			m.scanLocal()
		}
	}
}

func (m *singboxManager) needWatch() bool {
	m.mu.Lock()
	d, last, uninstall := m.desired, m.lastAttempt, m.desiredUninstall
	m.mu.Unlock()
	if time.Since(last) < retryBackoff {
		return false
	}
	if uninstall {
		// Removal is idempotent and cheap; retry it on the same backoff as a
		// failed install so a half-removed probe converges without operator
		// action (§9.2 实现修订 2026-09-16).
		return true
	}
	if d == nil {
		return false
	}
	bin, _, _ := service.SingboxPaths()
	if !m.processAlive(bin) {
		return true
	}
	act := m.observe(bin, d)
	return normalizeVersion(act.version) != normalizeVersion(d.Version)
}

// reportOnly re-observes and refreshes the state frame without converging.
//
// The last error is sticky until the node is *healthy* — the same condition
// under which the server clears it and calls the node running: a periodic
// "nothing new" report used to send last_error="" while `status` stayed
// `degraded`, leaving an unexplained failure on the panel. Matching the binary
// version alone is not enough: the artifact can be in place and the node still
// failing (a `check` gate rejection leaves exactly that state).
func (m *singboxManager) reportOnly() {
	m.mu.Lock()
	d, lastErr := m.desired, m.lastErr
	m.mu.Unlock()
	if d == nil {
		return
	}
	bin, _, certDir := service.SingboxPaths()
	act := m.observe(bin, d)
	if act.running && normalizeVersion(act.version) == normalizeVersion(d.Version) {
		lastErr = "" // healthy on the target: whatever failed before is history
	}
	pair, err := ensureSelfSignedCert(certDir)
	if err != nil {
		m.report(d, act.version, false, fmt.Sprintf("certificate: %v", err), certPair{})
		return
	}
	m.report(d, act.version, act.running, lastErr, pair)
}

// converge brings local reality in line with the desired state (§9.2).
func (m *singboxManager) converge() {
	m.mu.Lock()
	uninstall := m.desiredUninstall
	d := m.desired
	m.mu.Unlock()
	if uninstall {
		m.uninstall()
		return
	}
	if d == nil {
		return // unmanaged: never touch a running sing-box
	}
	m.mu.Lock()
	m.lastAttempt = time.Now()
	m.mu.Unlock()

	m.retireLegacyUnit()

	bin, configPath, certDir := service.SingboxPaths()
	act := m.observe(bin, d)

	pair, err := ensureSelfSignedCert(certDir)
	if err != nil {
		m.report(d, act.version, act.running, fmt.Sprintf("certificate: %v", err), certPair{})
		return
	}

	// "No desired version" is not "install version nothing": a config-only
	// declaration (the panel editing a node whose binary fobe never installed)
	// must not touch the artifact at all. Without this guard the agent tried to
	// download `/dl/singbox//linux-amd64` — a 404 — and rolled the whole apply
	// back, which is how every panel edit to a one-sing.sh node ended as
	// "download : get sha256: 404 Not Found" in the log and nothing on disk.
	needInstall := d.Version != "" && normalizeVersion(act.version) != normalizeVersion(d.Version)
	// configHashMatch is a comparison of the file on disk against the desired
	// bytes (see observe), not a cached "what I wrote last" — which is what
	// makes an operator's hand edit visible here instead of being overwritten
	// only when fobe happens to change something.
	needConfig := d.ConfigJSON != "" && !act.configHashMatch
	if !needInstall && !needConfig {
		if act.running {
			m.report(d, act.version, true, "", pair)
			return
		}
		// Right version, right config, just not running: gate ① still runs.
		// Skipping it here is how a config sing-box refuses to *load* — the
		// removed address-based DNS section (§9.4) — camped on disk for good:
		// the file never changed, so the change path (the only caller of
		// `check`) was never entered, and every round reported gate ③'s
		// "process exited" instead of the reason. Nothing has been replaced at
		// this point, so a failure needs no rollback.
		if err := checkConfig(bin, configPath); err != nil {
			m.report(d, act.version, false, err.Error(), pair)
			return
		}
		if err := m.start(bin, configPath); err != nil {
			m.report(d, act.version, false, err.Error(), pair)
			return
		}
		// Nothing was running before this start, so gate ③ holds the config to
		// its full promise: every listener it declares must answer (§9.2 实现
		// 修订 2026-09-17).
		if err := m.verifyWindow(bin, gate3TargetPorts(d, configPath, false)); err != nil {
			m.rollback(bin, configPath, d, false, false, err, pair)
			return
		}
		m.maybeAllowFirewall(effectivePort(d))
		m.report(d, act.version, true, "", pair)
		return
	}

	// §5.4: refuse installs onto a nearly-full overlay — without touching the
	// running service (only install/update is blocked, never start/stop).
	if needInstall {
		if err := checkInstallSpace(filepath.Dir(bin)); err != nil {
			m.log.Warn("sing-box install refused", "err", err)
			m.report(d, act.version, act.running, err.Error(), pair)
			return
		}
	}

	if err := m.apply(bin, configPath, d, needInstall, needConfig); err != nil {
		m.rollback(bin, configPath, d, needInstall, needConfig, err, pair)
		return
	}
	m.maybeAllowFirewall(effectivePort(d))
	m.report(d, d.Version, true, "", pair)
}

// uninstall removes sing-box from this probe on the panel's request (§9.2
// 实现修订 2026-09-16).
//
// Nothing in this path is guarded by a rollback: the operator asked for the
// binary to be gone, so there is no previous state worth restoring. It is
// idempotent — the desired flag stays set until the removal succeeds, which is
// what lets a failed attempt retry on the next tick and lets an offline probe
// converge from hello_ack whenever it reconnects.
//
// port carries the last desired inbound port into the absent report so the
// operator's port choice survives the round trip (see SetDesired).
func (m *singboxManager) uninstall() {
	m.mu.Lock()
	m.lastAttempt = time.Now()
	port := m.uninstallPort
	m.mu.Unlock()

	bin, config, certDir := service.SingboxPaths()
	kind := singboxKind()
	// A probe that already converged gets the declaration again on every
	// reconnect: nothing on disk means nothing to do, and calling `systemctl
	// stop` for a unit that no longer exists would report a failure for a
	// perfectly clean state (which would keep the flag set forever).
	if anyExists(bin, config, certDir, service.SingboxUnitPath, service.SingboxInitPath) {
		var errs []error
		// Stop before removing the definition: the fallback branch owns the
		// child process, so only the manager can kill it, and stopping after
		// the unit file is gone would leave an instance running from a deleted
		// path. `SingboxActive` guards the systemd/procd call for the same
		// reason — stop on an unloaded unit is an error, not a no-op.
		if kind == service.KindFallback || service.SingboxActive() {
			if err := m.stop(bin); err != nil {
				errs = append(errs, err)
			}
		}
		// A sing-box started outside this manager's life (a reparented
		// fallback child, or one-sing.sh's own unit) still holds the binary
		// and the inbound port.
		if kind == service.KindFallback {
			if pid := findProcByExe(bin); pid > 0 {
				killPID(pid)
			}
		} else {
			m.dropOrphan(bin)
		}
		if err := service.UninstallSingbox(kind); err != nil {
			errs = append(errs, err)
		}
		if err := errors.Join(errs...); err != nil {
			m.log.Warn("sing-box uninstall failed", "err", err)
			// Report what is actually left, with the cause: a removal that
			// failed halfway must not look like a clean uninstall on the panel.
			d := &protocol.SingboxDesired{Port: port}
			act := m.observe(bin, d)
			m.report(d, act.version, act.running, "uninstall: "+err.Error(), certPair{})
			return
		}
	}

	m.mu.Lock()
	m.desiredUninstall = false
	m.desired = nil
	m.uninstallPort = 0
	m.lastErr = ""
	m.lastFwHint = ""
	m.lastFwPort = 0
	m.rollbackSeen = false
	m.mu.Unlock()

	// The confirmation the server needs to clear the reported half (version,
	// certificate, status → absent): nothing installed, and nothing wrong.
	m.log.Info("sing-box uninstalled", "port", port)
	m.report(&protocol.SingboxDesired{Port: port}, "", false, "", certPair{})
}

// anyExists reports whether anything at all (file or directory) is at any of
// the paths. The uninstall needs the directory-tolerant question: an empty-but-
// present cert directory is still leftover state to remove.
func anyExists(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// apply runs the change path: download+verify → backup → write → the three
// gates. Any error hands over to rollback.
func (m *singboxManager) apply(bin, configPath string, d *protocol.SingboxDesired, needInstall, needConfig bool) error {
	if needInstall {
		// The binary lives in a directory of its own now (§9.3 实现修订:
		// /etc/one-sing), not in an always-present system bin dir — a fresh
		// probe has to create it before the download can write its temp file.
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return fmt.Errorf("create sing-box dir: %w", err)
		}
		artifactURL := fmt.Sprintf("%s/dl/singbox/%s/%s",
			strings.TrimRight(m.cfg.ServerURL, "/"), url.PathEscape(d.Version), service.ArchIdent())
		if err := installArtifact(m.hc, artifactURL, bin, maxDownloadBytes); err != nil {
			return fmt.Errorf("download %s: %w", d.Version, err)
		}
	}

	if d.ConfigJSON != "" {
		// config lands before `sing-box check`: the gate needs the new file.
		// Written in the relocated form (§5.3 实现修订 2026-09-16) so the
		// certificate paths match the effective layout.
		cfgJSON := relocateConfig(d.ConfigJSON)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
		backupPrevConfig(configPath, cfgJSON)
		if err := writeFileAtomic(configPath, []byte(cfgJSON), 0o600); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
	}
	// gate ①: the config must parse before anything touches the running service
	if err := checkConfig(bin, configPath); err != nil {
		return err
	}

	// Fix what gate ③ may demand *before* the restart replaces the listeners
	// that would form the baseline (§9.2 实现修订 2026-09-17).
	wasRunning := m.processAlive(bin)
	demand := gate3TargetPorts(d, configPath, wasRunning)

	// gate ②: write/refresh the service definition and start
	if err := m.start(bin, configPath); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// gate ③: 30s observation — process alive && every demanded listener
	// answers TCP on 127.0.0.1
	if err := m.verifyWindow(bin, demand); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}

// rollback restores what this apply touched: the .prev binary and config are
// only swapped back when this round actually replaced them (a stale .prev
// from an earlier upgrade must not resurrect), then the service restarts.
// The agent only reports the resulting state + last_error — alerting
// semantics are the server's job (§15).
func (m *singboxManager) rollback(bin, configPath string, d *protocol.SingboxDesired,
	changedBin, changedCfg bool, cause error, pair certPair) {
	m.log.Warn("sing-box apply failed, rolling back", "err", cause)

	_ = m.stop(bin)
	if changedBin {
		restoreFile(bin)
	}
	if changedCfg {
		restoreFile(configPath)
	}
	_ = os.Remove(bin + ".download")

	version, running := "", false
	if _, err := os.Stat(configPath); err == nil {
		if err := m.start(bin, configPath); err == nil {
			time.Sleep(2 * time.Second) // give a crashing config time to die
			running = m.processAlive(bin)
		}
	}
	if act := m.observe(bin, d); act.version != "" {
		version = act.version
	} else if running {
		version = d.Version
	}
	m.mu.Lock()
	m.rollbackSeen = true // one-shot §15 event: the next report carries it
	m.mu.Unlock()
	m.report(d, version, running, fmt.Sprintf("%v (rolled back)", cause), pair)
}

func restoreFile(path string) {
	prev := path + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return
	}
	_ = os.Remove(path)
	_ = os.Rename(prev, path)
}

// backupPrevConfig preserves the current config as .prev before it is
// replaced (§9.2); a no-op on first install or when content is unchanged.
func backupPrevConfig(configPath, incoming string) {
	raw, err := os.ReadFile(configPath)
	if err != nil || string(raw) == incoming {
		return
	}
	_ = os.WriteFile(configPath+".prev", raw, 0o600)
}

// singboxState assembles the reportable state (§7 SingboxState). PEM and
// SHA256 always travel together: the hub stores them as one pair.
func singboxState(d *protocol.SingboxDesired, version string, running bool, lastErr string, pair certPair) protocol.SingboxState {
	return protocol.SingboxState{
		Running:      running,
		Version:      version,
		Port:         d.Port,
		CertPEM:      pair.PEM,
		CertSHA256:   pair.SHA256,
		CertNotAfter: pair.NotAfter,
		LastError:    lastErr,
	}
}

// checkConfig is gate ① (§9.2): the config must parse before anything touches
// the running service. It is a gate of its own because both paths that start
// sing-box have to pass it — the change path and the "already on target, just
// not running" path.

func (m *singboxManager) report(d *protocol.SingboxDesired, version string, running bool, lastErr string, pair certPair) {
	st := singboxState(d, version, running, lastErr, pair)
	m.mu.Lock()
	m.lastErr = lastErr
	st.FirewallHint = m.lastFwHint
	st.RollbackHappened = m.rollbackSeen
	m.rollbackSeen = false
	m.mu.Unlock()
	m.setState(st)
}

func (m *singboxManager) setState(st protocol.SingboxState) {
	changed := true
	m.mu.Lock()
	if m.state != nil && *m.state == st {
		changed = false
	} else {
		cp := st
		m.state = &cp
	}
	m.mu.Unlock()
	if changed {
		m.nudge(m.changed)
	}
}

// --- version helpers ---

// parseSingboxVersion picks the version token from `sing-box version` output
// ("sing-box version 1.11.5" on the first line).
func parseSingboxVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		for i, w := range fields {
			if strings.EqualFold(strings.Trim(w, ":"), "version") && i+1 < len(fields) {
				return normalizeVersion(fields[i+1])
			}
		}
	}
	return ""
}

// normalizeVersion strips the conventional v/V prefix and whitespace.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// --- small shared helpers ---

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic writes via a temp file + rename so a crash never leaves a
// half-written config behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runCmd runs a short-lived management command and returns combined output.
func runCmd(name string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

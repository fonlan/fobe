package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	if d != nil && normalizeVersion(d.Version) == "" {
		d = nil // empty version = not managed (§9 版本显式)
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

// --- local discovery (§9.3 实现修订 2026-09-17) ---

// scanLocal reads the on-disk sing-box and publishes it when it changed.
// Cheap enough for the 60s tick: two stats, one `version` exec and one file
// read (capped). Errors are reported in the payload rather than logged away —
// "the file is there but unreadable" is operator-relevant on a host whose
// config an operator wrote by hand.
func (m *singboxManager) scanLocal() {
	got := detectLocal()
	m.mu.Lock()
	same := localEqual(m.local, got)
	m.local = got
	m.mu.Unlock()
	if !same {
		m.nudge(m.changedLocal)
	}
}

// Local returns the last discovery report, or nil before the first scan.
func (m *singboxManager) Local() *protocol.SingboxLocal {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.local == nil {
		return nil
	}
	cp := *m.local
	return &cp
}

// LocalChanged is the discovery-side notification stream (buffered, never
// closed — like Changed, the manager outlives sessions).
func (m *singboxManager) LocalChanged() <-chan struct{} { return m.changedLocal }

// localEqual compares two reports, ignoring nothing: the caller only wants a
// frame when something an operator could see has changed, and the config hash
// covers edits inside the file.
func localEqual(a, b *protocol.SingboxLocal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// maxLocalConfigBytes caps the config file the agent will read into a report.
// one-sing.sh's own output is a few KiB; anything past this is either not a
// sing-box config or an operator's hand-written monster, and shipping it in
// every state frame would be pure wire cost.
const maxLocalConfigBytes = 1 << 20

// detectLocal is the read-only probe behind SingboxLocal.
//
// It answers from the *effective* layout (service.SingboxPaths), so an
// unprivileged probe reports its own relocation and a root probe reports
// /etc/one-sing — the same paths one-sing.sh uses, which is what makes the
// takeover possible at all.
func detectLocal() *protocol.SingboxLocal {
	bin, configPath, _ := service.SingboxPaths()
	out := &protocol.SingboxLocal{ConfigPath: configPath}
	var problems []string

	if _, err := os.Stat(bin); err != nil {
		out.Present = false
		if !os.IsNotExist(err) {
			problems = append(problems, fmt.Sprintf("stat %s: %v", bin, err))
		}
		out.Error = strings.Join(problems, "; ")
		// A binary that is not there cannot be running: skip the rest rather
		// than reporting a unit state for someone else's install.
		return out
	}
	out.Present = true

	if raw, err := runCmd(bin, versionTimeout, "version"); err == nil {
		out.Version = parseSingboxVersion(raw)
	} else {
		problems = append(problems, fmt.Sprintf("version: %v", err))
	}

	// The process scan needs no privileges and is exactly how the fallback
	// branch already decides liveness, so it is the portable half of the
	// answer; the unit state below is the accurate half when we may ask.
	if findProcByExe(bin) > 0 {
		out.Running = true
	}
	if privileged() {
		switch service.Detect() {
		case service.KindSystemd:
			out.UnitKnown = true
			out.UnitActive = service.SingboxActive()
		case service.KindProcd:
			out.UnitKnown = true
			out.UnitActive = service.SingboxActive()
		}
	}
	if out.UnitActive {
		out.Running = true
	}

	switch raw, err := os.ReadFile(configPath); {
	case err == nil:
		if len(raw) > maxLocalConfigBytes {
			problems = append(problems, fmt.Sprintf("config %s is %d bytes (cap %d), not reported",
				configPath, len(raw), maxLocalConfigBytes))
			break
		}
		out.ConfigJSON = string(raw)
		out.ConfigSHA256 = sha256Hex(raw)
	case os.IsNotExist(err):
		problems = append(problems, "config file does not exist")
	default:
		problems = append(problems, fmt.Sprintf("read config: %v", err))
	}

	out.Error = strings.Join(problems, "; ")
	return out
}

// needWatch reports whether the desired instance lost its process (fallback
// watchdog, §5.3) or drifted from the desired version. Failed attempts back
// off so a broken download does not retry every 60s.
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

	needInstall := normalizeVersion(act.version) != normalizeVersion(d.Version)
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
		if err := m.verifyWindow(bin, effectivePort(d)); err != nil {
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

	// gate ②: write/refresh the service definition and start
	if err := m.start(bin, configPath); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// gate ③: 30s observation — process alive && inbound port answers TCP
	if err := m.verifyWindow(bin, effectivePort(d)); err != nil {
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
func checkConfig(bin, configPath string) error {
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("no config to check: %w", err)
	}
	if out, err := runCmd(bin, checkTimeout, "check", "-c", configPath); err != nil {
		return fmt.Errorf("config check: %w: %s", err, tailStr(out, 200))
	}
	return nil
}

// report assembles the reportable state and publishes it, attaching the
// sticky firewall hint (§9.2) and draining the one-shot rollback flag so
// rollback is reported exactly once (§15 告警"回滚已执行"). The error text is
// remembered for reportOnly (see there): while the node is off its desired
// version, "no news" must not be reported as "no problem".
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

// --- firewall auto-allow (§9.2) ---

// fwProbes bundles the environment lookups the firewall pass needs so tests
// can inject fake detection and command execution.
type fwProbes struct {
	lookPath   func(string) (string, error)
	fileExists func(string) bool
	run        func(name string, args ...string) error // bounded by fwCmdTimeout
}

func (p fwProbes) available(name string) bool {
	_, err := p.lookPath(name)
	return err == nil
}

// hasNft is deliberately cautious (§9.2): the nft rule is only attempted when
// both the binary and an existing ruleset are present, and failure is never
// fatal — an "inet filter input" chain may simply not exist.
func (p fwProbes) hasNft() bool {
	return p.available("nft") && p.fileExists(nftablesConfPath)
}

// fwAttempt is one firewall front-end: the commands to run (all must succeed,
// in order) and the raw command text suggested to the operator when nothing
// worked (§9.2 失败返回命令原文).
type fwAttempt struct {
	name string
	cmds [][]string
	hint string
}

func (a fwAttempt) apply(run func(name string, args ...string) error) bool {
	for _, cmd := range a.cmds {
		if err := run(cmd[0], cmd[1:]...); err != nil {
			return false
		}
	}
	return true
}

// firewallCandidates lists the port allow rules in priority order (§9.2):
// ufw → firewalld → nft → iptables (OpenWrt/兜底). Pure over injected probe
// results so the selection is unit-testable.
func firewallCandidates(port int, hasUfw, hasFirewalld, hasNft, hasIptables bool) []fwAttempt {
	p := strconv.Itoa(port)
	proto := p + "/tcp"
	out := []fwAttempt{}
	if hasUfw {
		out = append(out, fwAttempt{
			name: "ufw",
			cmds: [][]string{{"ufw", "allow", proto}},
			hint: "ufw allow " + proto,
		})
	}
	if hasFirewalld {
		out = append(out, fwAttempt{
			name: "firewalld",
			cmds: [][]string{
				{"firewall-cmd", "--add-port=" + proto, "--permanent"},
				{"firewall-cmd", "--reload"},
			},
			hint: "firewall-cmd --add-port=" + proto + " --permanent && firewall-cmd --reload",
		})
	}
	if hasNft {
		out = append(out, fwAttempt{
			name: "nft",
			cmds: [][]string{{"nft", "add", "rule", "inet", "filter", "input", "tcp", "dport", p, "accept"}},
			hint: "nft add rule inet filter input tcp dport " + p + " accept",
		})
	}
	if hasIptables {
		out = append(out, fwAttempt{
			name: "iptables",
			cmds: [][]string{{"iptables", "-I", "INPUT", "-p", "tcp", "--dport", p, "-j", "ACCEPT"}},
			hint: "iptables -I INPUT -p tcp --dport " + p + " -j ACCEPT",
		})
	}
	return out
}

// maybeAllowFirewall opens the inbound port once per distinct port (§9.2:
// 首次安装/端口变化时尝试一次,不随每次收敛重跑). On total failure the
// manual command is kept and reported in every state frame until the next
// port resets it.
func (m *singboxManager) maybeAllowFirewall(port int) {
	if port <= 0 {
		return
	}
	m.mu.Lock()
	attempted := port == m.lastFwPort
	m.lastFwPort = port
	m.mu.Unlock()
	if attempted {
		return
	}
	hint := m.openFw(port)
	m.mu.Lock()
	m.lastFwHint = hint
	m.mu.Unlock()
	if hint != "" {
		m.log.Warn("firewall auto-allow failed; manual command required", "port", port, "cmd", hint)
		return
	}
	m.log.Info("firewall allow ok", "port", port)
}

// openFirewallPort runs every available front-end in order, each command
// directly via exec (no shell) with a 10s budget. Returns the raw manual
// command when all candidates failed, "" when a rule was applied — or when
// the host has no firewall manager at all (nothing to suggest).
func (m *singboxManager) openFirewallPort(port int) string {
	probe := fwProbes{
		lookPath:   exec.LookPath,
		fileExists: fileExists,
		run: func(name string, args ...string) error {
			_, err := runCmd(name, fwCmdTimeout, args...)
			return err
		},
	}
	return runFirewallCandidates(port, probe)
}

func runFirewallCandidates(port int, probe fwProbes) string {
	candidates := firewallCandidates(
		port,
		probe.available("ufw"),
		probe.available("firewall-cmd"),
		probe.hasNft(),
		probe.available("iptables"),
	)
	if len(candidates) == 0 {
		return ""
	}
	for _, c := range candidates {
		if c.apply(probe.run) {
			return ""
		}
	}
	return candidates[0].hint // suggest the highest-priority front-end's command
}

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
	freeBytes, freeInodes, err := statfsFunc(dir)
	if err != nil {
		return nil
	}
	needBytes := uint64(minInstallFreeBytes) + artifactBytes
	if replacing {
		needBytes += artifactBytes
	}
	if freeBytes >= needBytes && freeInodes >= minInstallFreeInodes {
		return nil
	}
	return fmt.Errorf(
		"insufficient disk space on %s: %d MiB / %d inodes free, need ≥ %d MiB / %d inodes — install refused",
		dir, freeBytes>>20, freeInodes, needBytes>>20, minInstallFreeInodes)
}

// --- observation ---

type singboxActual struct {
	version         string
	configHashMatch bool
	running         bool
}

// retireLegacyUnit removes fobe's pre-rename service (fobe-singbox.service /
// /etc/init.d/sing-box) the first time this manager is about to drive sing-box.
// It is not optional housekeeping: the old unit is still enabled and running,
// and it supervises the same binary, config and port as the one-sing.service
// written below — two supervisors restarting one process flap forever while the
// panel addresses a unit it does not control (§9.3 实现修订 2026-09-16).
// Failure is a WARN: the change path below still runs, and the operator sees
// the flapping in the logs.
func (m *singboxManager) retireLegacyUnit() {
	if !privileged() {
		// 非特权模式不可能 systemctl disable——若机器上有 root 装的旧单元，
		// 端口冲突会在闸门③暴露并如实上报，而不是在这里收获一串 polkit 拒绝
		// （§5.3 实现修订 2026-09-16）。
		return
	}
	m.mu.Lock()
	done := m.retiredUnit
	m.retiredUnit = true
	m.mu.Unlock()
	if done {
		return
	}
	retired, err := service.RetireLegacySingboxUnit()
	if err != nil {
		m.log.Warn("could not retire the legacy sing-box service", "err", err)
		return
	}
	if retired {
		m.log.Info("retired the legacy fobe-singbox service so one-sing.service can own the process")
	}
}

// observe reads the local truth: installed binary version, config.json
// sha256 versus the desired config, and the running flag.
func (m *singboxManager) observe(bin string, d *protocol.SingboxDesired) singboxActual {
	act := singboxActual{}
	if out, err := runCmd(bin, versionTimeout, "version"); err == nil {
		act.version = parseSingboxVersion(out)
	}
	if d.ConfigJSON != "" {
		_, configPath, _ := service.SingboxPaths()
		if raw, err := os.ReadFile(configPath); err == nil {
			act.configHashMatch = sha256Hex(raw) == sha256Hex([]byte(relocateConfig(d.ConfigJSON)))
		}
	}
	act.running = m.processAlive(bin)
	return act
}

func (m *singboxManager) processAlive(bin string) bool {
	switch singboxKind() {
	case service.KindFallback:
		m.mu.Lock()
		p := m.proc
		m.mu.Unlock()
		if p == nil {
			return false
		}
		return p.Signal(syscall.Signal(0)) == nil
	case service.KindSystemd:
		return service.SingboxActive()
	default: // procd — and anything else: find the process directly
		return findProcByExe(bin) > 0
	}
}

// start writes/refreshes the service definition and starts sing-box (§5.3).
//
// It restarts rather than starts, on purpose: the convergence path reaches here
// whenever the config changed while the service is already up, and
// `systemctl start` / `/etc/init.d/... start` on a running service is a no-op —
// the new config would never be loaded, yet gate ③ would pass (the port is
// answering, just from the old process) and the panel would report a version
// that is not actually serving. `restart` also starts a stopped unit, so the
// fresh-install case is covered by the same call (design §5.5 同一处修订).
func (m *singboxManager) start(bin, configPath string) error {
	switch singboxKind() {
	case service.KindSystemd:
		m.dropOrphan(bin)
		if err := service.InstallSingbox(bin, configPath); err != nil {
			return err
		}
		return service.RestartSingbox()
	case service.KindProcd:
		m.dropOrphan(bin)
		if err := service.InstallSingbox(bin, configPath); err != nil {
			return err
		}
		if err := service.EnableSingbox(); err != nil {
			return err
		}
		return service.RestartSingbox()
	default:
		return m.spawn(bin, configPath)
	}
}

// dropOrphan kills a sing-box that the service manager does not know about.
//
// This is the fallback → native migration (§5.3 实现修订): the fallback branch
// spawns sing-box as a child of the agent, and such a child is reparented to
// init when the agent restarts (reinstall, self-update, or the first start of
// a build whose Detect() finally sees systemd) while still holding the inbound
// port. The unit we are about to start would fail to bind, flap under
// Restart=always, and gate ③ would blame the freshly installed version. A
// sing-box the manager does own is left alone: `restart` handles it.
func (m *singboxManager) dropOrphan(bin string) {
	if service.SingboxActive() {
		return
	}
	if pid := findProcByExe(bin); pid > 0 {
		m.log.Info("killing a sing-box left over from the fallback supervisor", "pid", pid, "exe", bin)
		killPID(pid)
	}
}

func (m *singboxManager) stop(bin string) error {
	switch singboxKind() {
	case service.KindSystemd, service.KindProcd:
		return service.StopSingbox()
	default:
		m.mu.Lock()
		p := m.proc
		m.proc = nil
		m.mu.Unlock()
		if p == nil {
			return nil
		}
		_ = p.Kill()
		return nil
	}
}

// spawn is the fallback path (§5.3): the agent owns the process outright and
// reaps it in the background.
func (m *singboxManager) spawn(bin, configPath string) error {
	// a leftover sing-box from a previous agent life still holds the port
	if pid := findProcByExe(bin); pid > 0 {
		killPID(pid)
	}
	m.mu.Lock()
	old := m.proc
	m.proc = nil
	m.mu.Unlock()
	if old != nil {
		_ = old.Kill()
	}

	cmd := exec.Command(bin, "run", "-c", configPath)
	cmd.Stdout = io.Discard // §5.4: no on-disk logs
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", bin, err)
	}
	proc := cmd.Process
	m.mu.Lock()
	m.proc = proc
	m.mu.Unlock()
	go func() { _ = cmd.Wait() }() // reap so the child never zombifies
	return nil
}

// killPID terminates a foreign sing-box: SIGTERM, then SIGKILL after a grace
// period (used only by the fallback watchdog for orphans we can no longer track).
func killPID(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.Kill()
}

// verifyWindow is gate ③: within the observation window the process must
// stay alive and TCP 127.0.0.1:port must answer; retried every second.
func (m *singboxManager) verifyWindow(bin string, port int) error {
	deadline := time.Now().Add(observeWindow)
	for {
		if !m.processAlive(bin) {
			return errors.New("sing-box process exited during observation window")
		}
		if port <= 0 || tcpConnectable(port) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("port %d not reachable within %s", port, observeWindow)
		}
		time.Sleep(observeStep)
	}
}

func tcpConnectable(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), tcpDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// effectivePort prefers the desired port; when absent it falls back to the
// first inbound listen_port in the config so gate ③ still has a target.
func effectivePort(d *protocol.SingboxDesired) int {
	if d.Port > 0 {
		return d.Port
	}
	return parseListenPort(d.ConfigJSON)
}

// parseListenPort extracts the first inbound listen_port from a config JSON.
func parseListenPort(configJSON string) int {
	var cfg struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return 0
	}
	for _, in := range cfg.Inbounds {
		if in.ListenPort > 0 {
			return in.ListenPort
		}
	}
	return 0
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

// compareVersions orders dotted numeric versions (1.10.0 > 1.9.9); components
// that are not plain numbers compare lexically as a tiebreak. Inputs may
// carry the conventional v/V prefix.
func compareVersions(a, b string) int {
	a, b = normalizeVersion(a), normalizeVersion(b)
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := versionPart(as, i), versionPart(bs, i)
		if an, bn := atoiSafe(av), atoiSafe(bv); an != bn {
			if an < bn {
				return -1
			}
			return 1
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionPart(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return "0" // missing components count as zero
}

// atoiSafe parses a plain decimal; -1 marks "not numeric" so lexical
// comparison takes over in compareVersions.
func atoiSafe(s string) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return n
		}
	}
	return n
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

func runQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
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

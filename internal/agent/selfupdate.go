package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/gorilla/websocket"
)

// Agent self-update (design §5.5).
//
// The server states the build it wants (hello_ack / desired); this file decides
// whether that is actionable and, if so, replaces the running binary. Three
// properties shape the implementation:
//
//   - Nothing is committed until a bypass self-check proves the new binary can
//     talk to this server *and* agrees that it is the requested build. The
//     running binary is only touched after that.
//   - The attempt budget lives on disk (update-state.json, beside the config),
//     not in memory: the dangerous loop is "replace → restart → still old →
//     replace again", and only a file survives that restart.
//   - .prev is deliberately absent (design §20.6): no local rollback, in
//     exchange for not doubling the probe's disk footprint.
const (
	// DefaultUpdateStatePath is the probe-local bookkeeping file (§5.5) for the
	// default config location; newSelfUpdater derives the real one from the
	// -config directory so a relocated deployment keeps its state with it.
	DefaultUpdateStatePath = "/etc/fobe-agent/update-state.json"

	// selfCheckTimeout bounds the bypass handshake; the server side closes its
	// own read after 20s, so a hung check must not hold the update forever.
	selfCheckTimeout = 60 * time.Second

	// updateTerminalLimit is the §5.5 circuit breaker: this many terminal
	// failures for the same target and the agent stops trying.
	updateTerminalLimit = 3

	// transient backoff (1m → 1h).
	transientBackoffMin = time.Minute
	transientBackoffMax = time.Hour

	// updateDownloadLimit caps the artifact we are willing to write.
	updateDownloadLimit = 64 << 20

	// updateFreeSpaceFactor is the §5.5 pre-flight gate: the temp file plus the
	// rename target are both on this filesystem, so 2× the artifact is the
	// honest requirement.
	updateFreeSpaceFactor = 2
)

// updateState is the probe-local half of the §5.5 double bookkeeping. It is
// written before an attempt that may end in a restart, so the next process
// start inherits the knowledge that this target already failed.
type updateState struct {
	Target string `json:"target"`
	// TerminalAttempts counts terminal failures for Target (sha256 mismatch,
	// cannot exec, self-check disagrees). At updateTerminalLimit the agent
	// stops until the target changes or an operator retries.
	TerminalAttempts int `json:"terminal_attempts,omitempty"`
	// TransientStreak counts consecutive environment failures (unreachable,
	// 404, disk) and drives the backoff.
	TransientStreak int    `json:"transient_streak,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	LastAttemptAt   int64  `json:"last_attempt_at,omitempty"`
	NextRetryAt     int64  `json:"next_retry_at,omitempty"`
	// Anchor is the plan deadline the server last handed out for Target. The
	// server only re-anchors a (node, target) pair when the target changes or an
	// operator hits retry, so a different Anchor for the same target *is* the
	// unlock signal — comparing values (not clocks) keeps a probe with a wrong
	// clock from resetting its own circuit breaker forever.
	Anchor int64 `json:"anchor,omitempty"`
	// CommittedVersion records that this binary IS the result of a successful
	// replacement, so the panel gets one "committed" report.
	CommittedVersion string `json:"committed_version,omitempty"`
}

// updateAction is the outcome of the pure decision function.
type updateAction int

const (
	actIdle updateAction = iota
	actWait
	actUpdate
	actSuppressed
	actUnsupported
)

// updateInput is everything decide() needs; keeping it a plain value makes the
// state machine unit-testable without touching the network or the disk.
type updateInput struct {
	SelfUpdate bool
	Current    string // this binary's build
	Target     string // what the server wants ("" = no target offered)
	After      int64  // earliest unix second the server allows
	Now        int64
	State      updateState
}

// decide is the §5.5 state machine, pure by construction.
func decide(in updateInput) (updateAction, time.Duration) {
	if in.Target == "" || in.Target == in.Current {
		return actIdle, 0
	}
	if !in.SelfUpdate {
		return actUnsupported, 0
	}
	// A different target always starts a fresh budget: the previous target's
	// failures say nothing about this build.
	if in.State.Target != in.Target {
		if in.Now < in.After {
			return actWait, time.Duration(in.After-in.Now) * time.Second
		}
		return actUpdate, 0
	}
	if in.State.TerminalAttempts >= updateTerminalLimit {
		return actSuppressed, 0
	}
	if in.State.NextRetryAt > in.Now {
		return actWait, time.Duration(in.State.NextRetryAt-in.Now) * time.Second
	}
	if in.Now < in.After {
		return actWait, time.Duration(in.After-in.Now) * time.Second
	}
	return actUpdate, 0
}

// selfUpdateCapBit is the Hello.Caps form of the §5.5 verdict (§5.5): linux,
// a resolvable executable, and a binary directory the current user can rename
// within. It resolves the executable itself because the hello may be built
// before the updater exists. Since the exec replace (实现修订 2026-09-16) a
// supervisor is no longer part of the question, which is what lets the
// nohup/fallback branch legitimately follow the server.
func selfUpdateCapBit() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exeDirWritable(exe)
}

// supported is the per-attempt form of the same verdict, evaluated against the
// updater's own resolved executable path.
func (u *selfUpdater) supported() bool {
	return runtime.GOOS == "linux" && exeDirWritable(u.exePath)
}

// exeDirWritable: rename-onto needs write permission on the directory, not on
// the file — a root-owned 0755 binary in a user-owned directory is replaceable.
func exeDirWritable(exePath string) bool {
	if exePath == "" {
		return false
	}
	return dirWritable(filepath.Dir(exePath))
}

// unsupportedReason explains supported() == false in words the panel shows
// verbatim. A wrong diagnosis sent to the operator is worse than no message:
// it points at the host instead of at the actual blocker.
func (u *selfUpdater) unsupportedReason() string {
	if runtime.GOOS != "linux" {
		return fmt.Sprintf("self-update is only implemented on linux (this is %s)", runtime.GOOS)
	}
	if u.exePath == "" {
		return "cannot resolve own executable"
	}
	return fmt.Sprintf("cannot write %s (the directory holding the agent binary)", filepath.Dir(u.exePath))
}

// reanchor folds a freshly received plan into the local bookkeeping. The server
// only re-anchors a (node, target) pair when the target changes or an operator
// hits retry, so a different anchor for the *same* target is the unlock signal:
// without this, "retry" in the panel would clear the server's copy and change
// nothing on the probe. Comparing anchors (not clocks) also keeps a probe with
// a wrong clock from resetting its own circuit breaker forever.
func reanchor(st updateState, target string, after int64) updateState {
	switch {
	case st.Target != target:
		// A different build: whatever was counted belonged to the other one.
		st = updateState{Target: target}
	case st.Anchor != 0 && after != st.Anchor:
		// Same build, new plan: an operator retry.
		st = updateState{Target: target}
	}
	if after != 0 {
		st.Anchor = after
	}
	return st
}

// selfUpdater owns the probe-local half of §5.5. One per process: the state file
// and the "am I already updating?" guard must outlive individual sessions.
type selfUpdater struct {
	cfg        *Config
	configPath string
	log        *slog.Logger
	statePath  string
	exePath    string

	mu      sync.Mutex
	state   updateState
	running bool
	// reporter is the current session's frame sink; nil when offline.
	reporter func(target, phase, class string, attempts int, errMsg string)
	// timer holds the pending wait (stagger deadline or transient backoff).
	timer *time.Timer

	// hooks for tests.
	execPath  func() (string, error)
	execCheck func(path string) error
	now       func() int64
	// exit is os.Exit in production; tests replace it so the replacement chain
	// can be asserted without killing the test process.
	exit func(int)
	// execSelf is syscall.Exec in production (re-exec into the committed
	// binary, §5.5 实现修订 2026-09-16); tests stub it to choose between the
	// happy path and the exec-failure fallback.
	execSelf func(self string, argv, env []string) error
}

// newSelfUpdater loads the probe-local bookkeeping. statePath is derived from
// the config path so a probe with a non-default -config keeps its state beside
// that config (the default config path yields the documented
// /etc/fobe-agent/update-state.json).
func newSelfUpdater(cfg *Config, configPath string, log *slog.Logger) *selfUpdater {
	statePath := DefaultUpdateStatePath
	if configPath != "" {
		statePath = filepath.Join(filepath.Dir(configPath), "update-state.json")
	}
	u := &selfUpdater{
		cfg:        cfg,
		configPath: configPath,
		log:        log,
		statePath:  statePath,
		now:        func() int64 { return time.Now().Unix() },
	}
	u.execPath = os.Executable
	u.execCheck = func(path string) error { return runSelfCheck(path, configPath) }
	u.exit = os.Exit
	u.execSelf = execSelfDefault
	u.state = loadUpdateState(statePath, log)
	if exe, err := u.execPath(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		u.exePath = exe
	} else {
		// A process we cannot locate is a process we must not try to replace.
		log.Warn("self-update disabled: cannot resolve own executable", "err", err)
	}
	return u
}

// SetReporter installs the current session's sink. Reports are best effort: a
// disconnected probe simply loses the narration, and the server learns the
// outcome from agent_version at the next handshake.
func (u *selfUpdater) SetReporter(fn func(target, phase, class string, attempts int, errMsg string)) {
	u.mu.Lock()
	u.reporter = fn
	u.mu.Unlock()
}

func (u *selfUpdater) report(target, phase, class string, attempts int, errMsg string) {
	u.mu.Lock()
	fn := u.reporter
	u.mu.Unlock()
	if fn != nil {
		fn(target, phase, class, attempts, errMsg)
	}
}

// Consider is called on every handshake (hello_ack) and on every desired push.
// It never blocks: waiting and updating both happen in the background.
func (u *selfUpdater) Consider(target string, after int64) {
	supported := u.supported()
	u.mu.Lock()
	cur := Version
	if target == cur {
		// Converged. Telling the server "committed" matters exactly once: when
		// this process is the result of a replacement it performed itself. A
		// probe that was installed at this version has nothing to report.
		if u.state.Target == target && u.state.CommittedVersion != cur {
			u.state = updateState{Target: target, CommittedVersion: cur}
			u.persistLocked()
			u.mu.Unlock()
			u.report(target, protocol.UpdateCommitted, "", 0, "")
			return
		}
		if u.state.Target != "" && u.state.Target != target {
			// The panel moved back to the build we already run: the abandoned
			// target's failures say nothing about this one.
			u.state = updateState{Target: target, CommittedVersion: cur}
			u.persistLocked()
		}
		u.mu.Unlock()
		return
	}
	if u.exePath == "" {
		u.mu.Unlock()
		return
	}
	if next := reanchor(u.state, target, after); next != u.state {
		if u.state.TerminalAttempts > 0 || u.state.TransientStreak > 0 {
			u.log.Info("self-update plan re-anchored by the server: resetting the local budget",
				"target", target, "was", u.state.Anchor, "now", after)
		}
		u.state = next
	}
	in := updateInput{
		SelfUpdate: supported,
		Current:    cur,
		Target:     target,
		After:      after,
		Now:        u.now(),
		State:      u.state,
	}
	action, wait := decide(in)
	switch action {
	case actIdle:
		u.mu.Unlock()
	case actUnsupported:
		u.mu.Unlock()
		reason := u.unsupportedReason()
		u.log.Info("self-update not available on this host", "target", target, "reason", reason)
		u.report(target, protocol.UpdateUnsupported, "", 0, reason)
	case actSuppressed:
		attempts := u.state.TerminalAttempts
		lastErr := u.state.LastError
		u.mu.Unlock()
		u.report(target, protocol.UpdateSuppressed, protocol.ClassTerminal, attempts, lastErr)
	case actWait:
		if u.running {
			u.mu.Unlock()
			return
		}
		if u.timer != nil {
			u.timer.Stop()
		}
		u.timer = time.AfterFunc(wait, func() { u.Consider(target, after) })
		u.mu.Unlock()
		u.log.Info("self-update scheduled", "target", target, "in", wait.String())
	case actUpdate:
		if u.running {
			u.mu.Unlock()
			return
		}
		u.running = true
		u.state.Target = target
		u.mu.Unlock()
		go u.perform(target)
	}
}

// perform runs the §5.5 chain: download → sha256 → self-check → atomic replace
// → re-exec into the new build (the supervisor, if any, never notices beyond
// the reconnect).
func (u *selfUpdater) perform(target string) {
	defer func() {
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
	}()

	u.report(target, protocol.UpdateDownloading, "", u.attempts(), "")
	fail := func(class, stage string, err error) {
		u.recordResult(target, class, stage+": "+err.Error())
	}

	tmp := filepath.Join(filepath.Dir(u.exePath), ".fobe-agent.tmp")
	defer os.Remove(tmp) // no-op once the rename succeeded

	size, err := downloadFile(u.httpClient(), u.artifactURL(target), tmp, u.exePath)
	if err != nil {
		fail(protocol.ClassTransient, "download", err)
		return
	}
	u.report(target, protocol.UpdateVerifying, "", u.attempts(), "")

	want, err := u.fetchChecksum(target)
	if err != nil {
		fail(protocol.ClassTransient, "checksum", err)
		return
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		fail(protocol.ClassTransient, "checksum", err)
		return
	}
	if !strings.EqualFold(want, got) {
		// The artifact is not what the panel published: retrying cannot help.
		fail(protocol.ClassTerminal, "sha256",
			fmt.Errorf("mismatch: server=%s downloaded=%s (%d bytes)", short(want), short(got), size))
		return
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		fail(protocol.ClassTerminal, "chmod", err)
		return
	}

	// Bypass self-check: the candidate binary must reach this server and agree
	// that it is the build the server asked for.
	if err := u.execCheck(tmp); err != nil {
		fail(protocol.ClassTerminal, "selfcheck", err)
		return
	}

	// Same directory, same filesystem: rename is atomic and cannot EXDEV.
	if err := os.Rename(tmp, u.exePath); err != nil {
		fail(protocol.ClassTerminal, "replace", err)
		return
	}
	u.mu.Lock()
	u.state = updateState{Target: target, CommittedVersion: target}
	u.persistLocked()
	u.mu.Unlock()
	u.report(target, protocol.UpdateCommitted, "", 0, "")
	u.log.Info("self-update committed; re-executing into the new build",
		"target", target, "path", u.exePath)
	// Give the frame a moment to leave the socket before the image is replaced.
	time.Sleep(250 * time.Millisecond)
	u.reexec()
}

// reexec replaces the process image with the committed binary. No supervisor
// is needed: the PID survives (systemd keeps tracking the same MainPID), Go's
// CLOEXEC fds close the old sockets, and a fallback-mode sing-box child keeps
// running — exec does not touch children, whereas the old exit flow let a
// systemd unit kill the whole cgroup (实现修订 2026-09-16).
func (u *selfUpdater) reexec() {
	execSelf := u.execSelf
	if execSelf == nil {
		execSelf = execSelfDefault
	}
	if err := execSelf(u.exePath, os.Args, os.Environ()); err != nil {
		// exec 的失败面很小（ENOEXEC/权限）；回退旧语义：退出，交给可能存在
		// 的 supervisor。没有 supervisor 的部署会掉线——supported() 的目录
		// 可写性探测就是要在事前挡住这种场景。
		if u.log != nil {
			u.log.Warn("re-exec failed; exiting for the supervisor instead", "err", err)
		}
		exit := u.exit
		if exit == nil {
			exit = os.Exit
		}
		exit(0)
	}
}

// recordResult writes the outcome to the state file and reports it. Terminal
// failures consume the attempt budget; transient ones only push the retry out.
func (u *selfUpdater) recordResult(target, class, errMsg string) {
	u.mu.Lock()
	if u.state.Target != target {
		u.state = updateState{Target: target}
	}
	u.state.LastError = errMsg
	u.state.LastAttemptAt = u.now()
	attempts := 0
	if class == protocol.ClassTerminal {
		u.state.TerminalAttempts++
		attempts = u.state.TerminalAttempts
		u.state.NextRetryAt = 0
	} else {
		u.state.TransientStreak++
		attempts = u.state.TransientStreak
		backoff := transientBackoffMin << (u.state.TransientStreak - 1)
		if backoff > transientBackoffMax || backoff <= 0 {
			backoff = transientBackoffMax
		}
		u.state.NextRetryAt = u.now() + int64(backoff/time.Second)
	}
	terminalAttempts := u.state.TerminalAttempts
	u.persistLocked()
	u.mu.Unlock()

	u.log.Warn("self-update attempt failed",
		"target", target, "class", class, "attempts", attempts, "err", errMsg)
	u.report(target, protocol.UpdateFailed, class, attempts, errMsg)
	if terminalAttempts >= updateTerminalLimit {
		u.report(target, protocol.UpdateSuppressed, protocol.ClassTerminal, terminalAttempts, errMsg)
	}
}

func (u *selfUpdater) attempts() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state.TerminalAttempts
}

func (u *selfUpdater) persistLocked() {
	if u.statePath == "" {
		return
	}
	raw, err := json.MarshalIndent(u.state, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(u.statePath), 0o700); err != nil {
		u.log.Warn("self-update state dir", "err", err)
		return
	}
	tmp := u.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		u.log.Warn("self-update state write", "err", err)
		return
	}
	if err := os.Rename(tmp, u.statePath); err != nil {
		u.log.Warn("self-update state rename", "err", err)
	}
}

func loadUpdateState(path string, log *slog.Logger) updateState {
	if path == "" {
		return updateState{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return updateState{}
	}
	var st updateState
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Warn("self-update state unreadable, starting fresh", "path", path, "err", err)
		return updateState{}
	}
	return st
}

func (u *selfUpdater) httpClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

func (u *selfUpdater) artifactURL(target string) string {
	return fmt.Sprintf("%s/dl/agent/%s/linux-amd64", strings.TrimRight(u.cfg.ServerURL, "/"), target)
}

// fetchChecksum delegates to the shared sidecar parser; the digest is
// length-checked and lowercased there.
func (u *selfUpdater) fetchChecksum(target string) (string, error) {
	return fetchExpectedSHA256(u.httpClient(), u.artifactURL(target)+".sha256")
}

// downloadFile streams the artifact into dir/tmp, refusing to start when the
// filesystem cannot hold the temp copy plus the replacement (a probe that runs
// out of space mid-write would end up with a truncated binary).
func downloadFile(client *http.Client, url, tmp, exePath string) (int64, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s: %s", url, resp.Status)
	}
	if resp.ContentLength > updateDownloadLimit {
		return 0, fmt.Errorf("artifact too large: %d bytes", resp.ContentLength)
	}
	need := uint64(resp.ContentLength) * updateFreeSpaceFactor
	if resp.ContentLength <= 0 {
		need = 64 << 20
	}
	// statfsFunc (not statfsFree) is the swappable hook sing-box tests use too.
	if free, _, _, err := statfsFunc(filepath.Dir(exePath)); err == nil && free < need {
		return 0, fmt.Errorf("not enough free space in %s: %d < %d bytes", filepath.Dir(exePath), free, need)
	}

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, updateDownloadLimit+1))
	if err != nil {
		out.Close()
		return n, err
	}
	if n > updateDownloadLimit {
		out.Close()
		return n, fmt.Errorf("artifact exceeds %d bytes", updateDownloadLimit)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return n, err
	}
	return n, out.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runSelfCheck executes the candidate binary in bypass mode (see SelfCheck).
func runSelfCheck(path, configPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), selfCheckTimeout)
	defer cancel()
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	cmd := exec.CommandContext(ctx, path, "-config", configPath, "-selfcheck")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("timed out after %s", selfCheckTimeout)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// SelfCheck is the `-selfcheck` entry point (design §5.5): connect to the panel
// without registering, complete a marked handshake, and require the server to
// agree that this binary is the build it asked for. Exit code 0 means "safe to
// commit"; any other outcome leaves the running agent untouched.
func SelfCheck(cfg *Config, log *slog.Logger) error {
	if cfg == nil || cfg.ServerURL == "" {
		return errors.New("self-check: no server configured")
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	headers := http.Header{
		"X-Fobe-Node-ID":     {cfg.NodeID},
		"X-Fobe-Node-Secret": {cfg.NodeSecret},
		"X-Fobe-Selfcheck":   {"1"},
	}
	ws, resp, err := dialer.Dial(wsEndpoint(cfg.ServerURL), headers)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("self-check dial %s: %s (%w)", wsEndpoint(cfg.ServerURL), resp.Status, err)
		}
		return fmt.Errorf("self-check dial %s: %w", wsEndpoint(cfg.ServerURL), err)
	}
	defer ws.Close()
	deadline := time.Now().Add(selfCheckTimeout)
	_ = ws.SetWriteDeadline(deadline)
	_ = ws.SetReadDeadline(deadline)

	hello := protocol.Hello{
		MachineID: cfg.MachineID,
		Version:   Version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		SelfCheck: true,
	}
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", hello)); err != nil {
		return fmt.Errorf("self-check write: %w", err)
	}
	for {
		var env protocol.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return fmt.Errorf("self-check read: %w", err)
		}
		if env.Type != protocol.TypeHelloAck {
			continue
		}
		var ack protocol.HelloAck
		if err := json.Unmarshal(env.Payload, &ack); err != nil {
			return fmt.Errorf("self-check hello_ack: %w", err)
		}
		target := ack.AgentTargetVersion
		if target == "" {
			target = ack.Desired.AgentTargetVersion
		}
		// The server stopped offering a target between download and check
		// (switch off, kill switch, a new deployment). Refusing here would burn
		// a terminal attempt for a reason that is not about this artifact.
		if target != "" && target != Version {
			return fmt.Errorf("server wants %s but this binary is %s", target, Version)
		}
		log.Info("self-check passed", "version", Version, "target", target)
		return nil
	}
}

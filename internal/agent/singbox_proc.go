// Process supervision: observe what is running and start/stop it through the
// detected service manager (design §9.3).
package agent

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/fonlan/fobe/internal/agent/service"
	"github.com/fonlan/fobe/internal/protocol"
)

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

// verifyWindow is gate ③: within the observation window the process must stay
// alive and every port in `ports` must answer TCP on 127.0.0.1; retried every
// second. An empty slice skips the TCP half — the process check still gates.

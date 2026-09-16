package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/fonlan/fobe/internal/protocol"
)

const (
	defaultTerminalCols = 80
	defaultTerminalRows = 24
	maxTerminalInput    = 1 << 20
	maxTerminalCols     = 1000
	maxTerminalRows     = 1000
)

type terminalManager struct {
	agent *agentSession

	mu      sync.Mutex
	active  *terminalSession
	stopped bool
}

type terminalSession struct {
	manager *terminalManager
	agent   *agentSession
	id      string
	cmd     *exec.Cmd
	pty     *os.File

	stopOnce sync.Once
	reasonMu sync.Mutex
	reason   string
}

func newTerminalManager(agent *agentSession) *terminalManager {
	return &terminalManager{agent: agent}
}

func (m *terminalManager) handle(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeTermOpen:
		var open protocol.TerminalOpen
		if err := json.Unmarshal(env.Payload, &open); err != nil {
			return
		}
		m.open(open)
	case protocol.TypeTermInput:
		var input protocol.TerminalInput
		if err := json.Unmarshal(env.Payload, &input); err != nil {
			return
		}
		m.input(input)
	case protocol.TypeTermResize:
		var resize protocol.TerminalResize
		if err := json.Unmarshal(env.Payload, &resize); err != nil {
			return
		}
		m.resize(resize)
	case protocol.TypeTermClose:
		var closeRequest protocol.TerminalClose
		if err := json.Unmarshal(env.Payload, &closeRequest); err != nil {
			return
		}
		m.closeSession(closeRequest.SessionID, "closed by server")
	}
}

func (m *terminalManager) open(request protocol.TerminalOpen) {
	if request.SessionID == "" {
		return
	}
	m.closeCurrent("replaced by new terminal")

	// Web Terminal always runs the agent's local PTY. Accept the legacy ssh
	// marker too, but deliberately ignore it: this keeps a newly installed
	// agent usable with a server that has not yet been upgraded while ensuring
	// the agent never initiates an SSH connection.
	if request.Mode != "" && request.Mode != "pty" && request.Mode != "ssh" {
		m.notifyClosed(request.SessionID, "terminal_unsupported_mode")
		return
	}
	cols, rows, ok := terminalSize(request.Cols, request.Rows)
	if !ok {
		m.notifyClosed(request.SessionID, "terminal_invalid_size")
		return
	}

	m.mu.Lock()
	stopped := m.stopped
	m.mu.Unlock()
	if stopped {
		return
	}

	term, err := startTerminal(m, request, cols, rows)
	if err != nil {
		m.agent.log.Warn("terminal start failed", "session_id", request.SessionID, "err", err)
		m.notifyClosed(request.SessionID, "terminal_start_failed")
		return
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		term.stop("agent session stopped")
		return
	}
	m.active = term
	m.mu.Unlock()

	go term.run()
}

func (m *terminalManager) input(input protocol.TerminalInput) {
	term := m.session(input.SessionID)
	if term == nil {
		return
	}
	if len(input.Data) > maxTerminalInput {
		m.agent.log.Warn("terminal input too large", "session_id", input.SessionID, "bytes", len(input.Data))
		return
	}
	if err := writeAll(term.pty, []byte(input.Data)); err != nil {
		m.agent.log.Warn("terminal input failed", "session_id", input.SessionID, "err", err)
		m.closeSession(input.SessionID, "terminal_input_failed")
	}
}

func (m *terminalManager) resize(request protocol.TerminalResize) {
	term := m.session(request.SessionID)
	if term == nil {
		return
	}
	cols, rows, ok := terminalSize(request.Cols, request.Rows)
	if !ok {
		m.agent.log.Warn("invalid terminal size", "session_id", request.SessionID, "cols", request.Cols, "rows", request.Rows)
		return
	}
	if err := pty.Setsize(term.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		m.agent.log.Warn("terminal resize failed", "session_id", request.SessionID, "err", err)
	}
}

func (m *terminalManager) notifyClosed(sessionID, reason string) {
	m.agent.sendTerminalClosed(protocol.TerminalClosed{
		SessionID: sessionID,
		Reason:    reason,
	})
}

func (m *terminalManager) closeSession(sessionID, reason string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	term := m.active
	if term == nil || term.id != sessionID {
		m.mu.Unlock()
		return
	}
	m.active = nil
	m.mu.Unlock()
	term.stop(reason)
}

func (m *terminalManager) closeCurrent(reason string) {
	m.mu.Lock()
	term := m.active
	m.active = nil
	m.mu.Unlock()
	if term != nil {
		term.stop(reason)
	}
}

func (m *terminalManager) close(reason string) {
	m.mu.Lock()
	m.stopped = true
	term := m.active
	m.active = nil
	m.mu.Unlock()
	if term != nil {
		term.stop(reason)
	}
}

func (m *terminalManager) session(sessionID string) *terminalSession {
	if sessionID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.id != sessionID {
		return nil
	}
	return m.active
}

func (m *terminalManager) finished(term *terminalSession, reason string) {
	m.mu.Lock()
	if m.active == term {
		m.active = nil
	}
	m.mu.Unlock()

	if reason == "" {
		reason = "terminal_exited"
	}
	m.agent.sendTerminalClosed(protocol.TerminalClosed{
		SessionID: term.id,
		Reason:    reason,
	})
}

func (t *terminalSession) run() {
	defer t.pty.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.agent.sendEnvelope(protocol.NewEnvelope(protocol.TypeTerminalOutput, "", protocol.TerminalOutput{
				SessionID: t.id,
				Data:      string(buf[:n]),
			}))
		}
		if err != nil {
			break
		}
	}

	waitErr := t.cmd.Wait()
	reason := t.requestedReason()
	if reason == "" {
		reason = terminalExitReason(waitErr)
	}
	t.manager.finished(t, reason)
}

func (t *terminalSession) stop(reason string) {
	t.reasonMu.Lock()
	if t.reason == "" {
		t.reason = reason
	}
	t.reasonMu.Unlock()

	t.stopOnce.Do(func() {
		if t.cmd.Process != nil {
			_ = killTerminalProcess(t.cmd)
		}
		_ = t.pty.Close()
	})
}

func (t *terminalSession) requestedReason() string {
	t.reasonMu.Lock()
	defer t.reasonMu.Unlock()
	return t.reason
}

func startTerminal(manager *terminalManager, request protocol.TerminalOpen, cols, rows int) (*terminalSession, error) {
	cmd := exec.Command(localShell(), "-i")
	cmd.Env = terminalEnvironment(os.Environ())
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &terminalSession{
		manager: manager,
		agent:   manager.agent,
		id:      request.SessionID,
		cmd:     cmd,
		pty:     master,
	}, nil
}

func localShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		if _, err := exec.LookPath(shell); err == nil {
			return shell
		}
	}
	if runtime.GOOS == "windows" {
		if shell := os.Getenv("COMSPEC"); shell != "" {
			return shell
		}
		return "cmd.exe"
	}
	return "/bin/sh"
}

func terminalEnvironment(environment []string) []string {
	out := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		if !strings.HasPrefix(value, "TERM=") {
			out = append(out, value)
		}
	}
	return append(out, "TERM=xterm-256color")
}

func terminalSize(cols, rows int) (int, int, bool) {
	if cols == 0 {
		cols = defaultTerminalCols
	}
	if rows == 0 {
		rows = defaultTerminalRows
	}
	if cols < 1 || cols > maxTerminalCols || rows < 1 || rows > maxTerminalRows {
		return 0, 0, false
	}
	return cols, rows, true
}

func terminalExitReason(err error) string {
	if err == nil {
		return "terminal_exited"
	}
	return fmt.Sprintf("terminal_exited: %v", err)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

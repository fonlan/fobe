package agent

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/fobe-panel/fobe/internal/agent/collect"
	"github.com/fobe-panel/fobe/internal/agent/service"
	"github.com/fobe-panel/fobe/internal/protocol"
)

const (
	cmdTimeout    = 30 * time.Second
	outputMaxKeep = 32 * 1024 // stdout/stderr truncated before reply (§7)
)

// executeCommand runs a server command and replies with cmd_result.
// The execMu channel serializes execution; duplicate IDs are ignored so a
// re-sent command after reconnect never runs twice (§7 幂等).
func (s *agentSession) executeCommand(env protocol.Envelope) {
	select {
	case s.execMu <- struct{}{}:
		defer func() { <-s.execMu }()
	case <-s.done:
		return
	}

	cmd, err := commandFromEnvelope(env)
	if err != nil {
		s.sendEnvelope(protocol.NewEnvelope(protocol.TypeCmdResult, env.ID, protocol.CmdResult{ID: env.ID, Error: err.Error()}))
		return
	}
	result := s.runCommand(cmd.ID, cmd.Kind, cmd.Payload)
	s.sendEnvelope(protocol.NewEnvelope(protocol.TypeCmdResult, cmd.ID, result))
}

// commandFromEnvelope unpacks the wire command (§7). The envelope type is
// always "cmd"; the kind and the kind-specific payload live inside the payload
// (protocol.Cmd). Reading env.Type here answered every panel/AI command with
// "unsupported command kind: cmd" — run_shell, restart/start/stop_singbox and
// tail_logs were all dead on arrival.
func commandFromEnvelope(env protocol.Envelope) (protocol.Cmd, error) {
	var cmd protocol.Cmd
	if err := json.Unmarshal(env.Payload, &cmd); err != nil {
		return protocol.Cmd{}, fmt.Errorf("decode cmd: %w", err)
	}
	if cmd.ID == "" {
		cmd.ID = env.ID
	}
	if cmd.Kind == "" {
		return cmd, fmt.Errorf("cmd %s: missing kind", cmd.ID)
	}
	return cmd, nil
}

func (s *agentSession) runCommand(id, kind string, payload json.RawMessage) protocol.CmdResult {
	res := protocol.CmdResult{ID: id}
	done := make(chan struct{})
	go func() {
		defer close(done)
		switch kind {
		case "run_shell":
			var p struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(payload, &p); err != nil || p.Command == "" {
				res.Error = "missing command"
				return
			}
			res.ExitCode, res.Stdout, res.Stderr = runShell(p.Command, cmdTimeout)
		case "restart_singbox":
			if err := service.RestartSingbox(); err != nil {
				res.Error = err.Error()
			}
			s.sbx.Nudge() // re-observe and report the new state (§9)
		case "start_singbox":
			if err := service.StartSingbox(); err != nil {
				res.Error = err.Error()
			}
			s.sbx.Nudge()
		case "stop_singbox":
			if err := service.StopSingbox(); err != nil {
				res.Error = err.Error()
			}
			s.sbx.Nudge()
		case "tail_logs":
			// §12.1: AI-triggered log read. Best-effort by design —
			// missing log sources answer empty with exit 0.
			var p struct {
				Lines int `json:"lines"`
			}
			_ = json.Unmarshal(payload, &p) // bad/empty payload → default lines
			res.Stdout = collect.TailSingboxLogs(service.Detect().String(), p.Lines)
		case protocol.CmdKindNftForwards:
			// §21: nftables port forwarding. The answer is a JSON
			// protocol.ForwardsResult in stdout — the panel needs structured
			// data (the resulting ruleset + a stable error token), and stdout is
			// the only result channel a command has.
			out, code := runForwardsCommand(s.fwdEnv, payload)
			if code != "" {
				res.Error = code
				return
			}
			// Report the observed truth on the state channel as well: the panel
			// HTTP call may have timed out or never happened (offline queue),
			// and the state frame is what keeps the server's snapshot correct.
			s.setForwards(out.State)
			blob, err := json.Marshal(out)
			if err != nil {
				res.Error = "encode_result"
				return
			}
			res.Stdout = string(blob)
		default:
			res.Error = "unsupported command kind: " + kind
		}
	}()
	select {
	case <-done:
	case <-s.done:
	}
	return res
}

func runShell(command string, timeout time.Duration) (exitCode int, stdout, stderr string) {
	shell := "/bin/sh"
	if runtime.GOOS == "windows" {
		shell = "cmd"
	}
	cmd := exec.Command(shell, "-c", command)
	var outBuf, errBuf limitedBuffer
	outBuf.limit = outputMaxKeep
	errBuf.limit = outputMaxKeep
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return -1, "", err.Error()
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), outBuf.String(), errBuf.String()
			}
			return -1, outBuf.String(), errBuf.String() + err.Error()
		}
		return 0, outBuf.String(), errBuf.String()
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return -1, outBuf.String(), errBuf.String() + "command timed out"
	}
}

// limitedBuffer caps memory when a command spews output.
type limitedBuffer struct {
	buf   []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(b.buf) < b.limit {
		room := b.limit - len(b.buf)
		if room > len(p) {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil // swallow the rest, report full write to the pipe
}

func (b *limitedBuffer) String() string { return string(b.buf) }

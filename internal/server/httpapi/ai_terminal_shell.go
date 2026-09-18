package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/hub"
	"github.com/fonlan/fobe/internal/server/store"
)

// Running a command in the operator's terminal (design §12.7.3).
//
// The shape of this is dictated by one fact: a PTY gives us no result channel.
// There is no exit status, no separated stdout/stderr (the tty merges fd1 and
// fd2), and no completion event — only a stream of rendered characters that the
// server does not even keep. So the command is wrapped in a SENTINEL that
// prints its own exit status onto the screen, and completion is observed by
// reading that screen back through the browser.
//
// The sentinel is the only reliable completion signal; everything else here is
// a fallback for the cases where no sentinel can appear (a full-screen program
// owns the terminal, or the output scrolled out of the buffer).

// aiTerminalRunCap bounds one command end to end.
//
// Deliberately the same 30s the deleted `cmdTimeout` used, but it no longer
// KILLS anything: it is the model's patience, not a leash on the command
// (§12.7.3). A command that outlives it is still running in the operator's
// shell, and the result says so. A var so tests need not wait half a minute.
var aiTerminalRunCap = 30 * time.Second

const (
	// aiTerminalQuietWindow is the fallback completion heuristic: no visible
	// change for this long and we hand back what is on screen with an unknown
	// status. Known to be wrong for silent commands — which is exactly why the
	// sentinel exists and why the result says "unknown" rather than guessing.
	aiTerminalQuietWindow = time.Second
	// aiTerminalPollInterval is how often the browser is asked for the window
	// while waiting. Also the resolution of change detection.
	aiTerminalPollInterval = 250 * time.Millisecond
	// aiTerminalSettleDelay lets the shell's prompt land after the sentinel so
	// the returned delta ends where the operator's screen does.
	aiTerminalSettleDelay = 150 * time.Millisecond
	// aiTerminalCommandWindow is the per-minute rate window for the terminal
	// tools (they never enqueue a command, so `commands` cannot count them).
	aiTerminalCommandWindow = 60
)

// aiTerminalSentinelPrefix is the marker's fixed part. The nonce makes it
// unique per call so a previous run's marker can never be mistaken for this
// one's; the format is documented in §12.7.3.
const aiTerminalSentinelPrefix = "__FOBE_"

// aiSentinelRE matches the marker as the shell prints it: the nonce plus the
// exit status. It deliberately does NOT match the echoed command line (that one
// still contains the literal `%d` of the printf format), so the exit status can
// never be read out of the echo.
var aiSentinelRE = regexp.MustCompile(`^__FOBE_([0-9a-f]+)_(-?[0-9]+)__$`)

// aiTerminalIO is the seam between the command lifecycle and the transport.
//
// The lifecycle is a state machine over "type these bytes" and "show me the
// screen", and both are round trips into infrastructure a unit test cannot
// stand up (an agent connection, a PTY, a browser). Narrowing them to two
// methods is what makes clear → type → wait → read testable end to end; the
// production implementation is hubTerminalIO below.
type aiTerminalIO interface {
	// Send types raw bytes into the terminal. false means nothing is attached.
	Send(data string) bool
	// Window returns the last lines of the buffer, `offset` lines above the
	// bottom; lines <= 0 means the browser's viewport height.
	Window(offset, lines int) (protocol.TerminalBuffer, error)
}

type hubTerminalIO struct {
	hub       *hub.Hub
	ctx       context.Context
	nodeID    string
	sessionID string
}

func (h hubTerminalIO) Send(data string) bool {
	return h.hub.SendTerminalKeys(h.nodeID, h.sessionID, data)
}

func (h hubTerminalIO) Window(offset, lines int) (protocol.TerminalBuffer, error) {
	return h.hub.AskTerminalBuffer(h.ctx, h.sessionID, offset, lines, aiTerminalQueryTimeout)
}

// newAITerminalIO builds the transport for one terminal call.
//
// A package-level var, like aiQueuedCommandWait, because the command lifecycle
// can only be tested end to end by standing in for infrastructure a unit test
// cannot bring up (an agent socket AND a browser holding an emulator). nil-ish
// overrides are read-only test seams — production always uses the hub.
var newAITerminalIO = func(h *hub.Hub, ctx context.Context, nodeID, sessionID string) aiTerminalIO {
	return hubTerminalIO{hub: h, ctx: ctx, nodeID: nodeID, sessionID: sessionID}
}

// aiShellResult is what one command produced.
type aiShellResult struct {
	output string
	// exitCode is nil when no sentinel was observed — the honest answer for a
	// full-screen program or output that scrolled away. It is NOT an error:
	// §12.3 records that "unknown" must not count as a failure, or ordinary
	// `vim`/`top` use would pause the node's AI for five minutes.
	exitCode *int
	timedOut bool
	// typed is what actually went into the terminal, audited verbatim.
	typed string
}

// aiShellSentinel wraps a command so its exit status lands on the screen as its
// own line.
//
// `printf` (not `echo`) because the leading newline has to be a real newline
// rather than an escape the shell might or might not interpret; `$?` is read
// immediately after the `;`, so it is the wrapped command's status. The text is
// built in a raw string on purpose: the `\n` must reach the shell intact.
func aiShellSentinel(command, nonce string) string {
	return fmt.Sprintf(`%s; printf '\n%s%s_%%d__\n' $?`, command, aiTerminalSentinelPrefix, nonce)
}

// aiSentinelExit finds this call's marker and returns the exit status it
// carries. Searched from the bottom so a re-used nonce (it cannot happen, but)
// would resolve to the newest line.
func aiSentinelExit(lines []string, nonce string) (int, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		match := aiSentinelRE.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if match == nil || match[1] != nonce {
			continue
		}
		code, err := strconv.Atoi(match[2])
		if err != nil {
			return 0, false
		}
		return code, true
	}
	return 0, false
}

// aiStripSentinel drops this package's bookkeeping from what the model reads.
//
// It matches the nonce ANYWHERE in the line, not just in the strict marker form,
// because the shell also ECHOES the wrapped command — and that echo carries the
// printf format, so it too contains the marker. Leaving either one in means the
// model reads our plumbing and, worse, sees a line that looks exactly like the
// completion signal it is supposed to trust. (A command whose own output happens
// to contain a freshly generated 4-byte nonce would be stripped too; the
// alternative is leaking the sentinel into every single result.)
func aiStripSentinel(lines []string, nonce string) []string {
	needle := aiTerminalSentinelPrefix + nonce + "_"
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, needle) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// aiTerminalNonce is the per-call marker suffix. Random so a marker already on
// the screen from an earlier command can never satisfy this call.
func aiTerminalNonce() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// aiPollWindow is how much of the screen each poll asks for: the visible rows
// plus a little history, enough to see the marker even after the prompt has
// pushed the command's output up. Kept small because it is requested several
// times a second.
func aiPollWindow(rows int) int {
	if rows <= 0 {
		rows = 24
	}
	return rows + 10
}

// execAIShell runs one command in the terminal: clear the line, type the
// command plus its sentinel, then wait for the sentinel (fast path) or for the
// screen to go quiet (slow path), and return what appeared.
//
// It NEVER kills the command (§12.7.3): there is no clean way to kill one
// command in an interactive shell, and the design accepted that a runaway
// command is the operator's to deal with.
func (s *Server) execAIShell(ctx context.Context, io aiTerminalIO, command string) (aiShellResult, error) {
	nonce, err := aiTerminalNonce()
	if err != nil {
		return aiShellResult{}, err
	}
	wrapped := aiShellSentinel(command, nonce)

	// The line count before typing is the delta anchor.
	before, err := io.Window(0, 1)
	if err != nil {
		return aiShellResult{}, err
	}
	startLines := before.Length
	rows := before.Rows

	// Clear whatever the operator left on the input line, then type. This is a
	// BLIND keystroke (§12.7.3): when the foreground is not a shell prompt,
	// Ctrl+U/Ctrl+C mean something else to whatever is running. The operator
	// accepted that; it is why the result of a failed read says "unknown"
	// instead of pretending the command ran.
	if !io.Send("\x15\x03") {
		return aiShellResult{}, hub.ErrNoTerminalBrowser
	}
	if !io.Send(wrapped + "\n") {
		return aiShellResult{}, hub.ErrNoTerminalBrowser
	}

	deadline := time.Now().Add(aiTerminalRunCap)
	quietSince := time.Now()
	lastProbe := ""
	seen := false
	var code int
	var last protocol.TerminalBuffer

	for {
		window, err := io.Window(0, aiPollWindow(rows))
		if err != nil {
			return aiShellResult{typed: wrapped}, err
		}
		last = window
		probe := strings.Join(window.Lines, "\n")
		if probe != lastProbe {
			lastProbe = probe
			quietSince = time.Now()
		}
		if found, ok := aiSentinelExit(window.Lines, nonce); ok {
			seen, code = true, found
			// Give the prompt a moment so the delta ends where the screen does.
			time.Sleep(aiTerminalSettleDelay)
			break
		}
		if time.Since(quietSince) >= aiTerminalQuietWindow {
			break
		}
		if time.Now().After(deadline) {
			return aiShellResult{
				output:   aiDelta(io, last, startLines, rows, nonce),
				timedOut: true,
				typed:    wrapped,
			}, nil
		}
		select {
		case <-ctx.Done():
			return aiShellResult{typed: wrapped}, ctx.Err()
		case <-time.After(aiTerminalPollInterval):
		}
	}

	result := aiShellResult{typed: wrapped}
	if seen {
		result.exitCode = &code
	}
	result.output = aiDelta(io, last, startLines, rows, nonce)
	return result, nil
}

// aiDelta renders the part of the screen this command produced.
//
// The window is sized from the buffer's own line counter (`Length`), which the
// browser reports on every answer, so the server still keeps no state. If the
// buffer hit its scrollback cap mid-command the counter stops growing and the
// delta degrades to "the last screenful" — a truncation, never a wrong answer.
func aiDelta(io aiTerminalIO, last protocol.TerminalBuffer, startLines, rows int, nonce string) string {
	if rows <= 0 {
		rows = 24
	}
	want := last.Length - startLines + rows + 2
	if want < rows+2 {
		want = rows + 2
	}
	if want > aiTerminalMaxLines {
		want = aiTerminalMaxLines
	}
	window, err := io.Window(0, want)
	if err != nil {
		window = last // the last successful read is better than nothing
	}
	lines := aiStripSentinel(window.Lines, nonce)
	// Trailing blanks are the empty rows under the prompt; they cost context and
	// say nothing.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	// Leading blanks are the tail of whatever was on screen before.
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// aiShellToolContent formats a finished command for the model. Shared by the
// direct path and the confirmation resume so the two cannot drift apart — the
// §12.6 mistake of writing "install sing-box" twice is the precedent.
func aiShellToolContent(res aiShellResult) string {
	screen := res.output
	if strings.TrimSpace(screen) == "" {
		screen = "(the command produced nothing visible on the terminal)"
	}
	switch {
	case res.timedOut:
		return fmt.Sprintf("the command did not finish within %s and is STILL RUNNING in the operator's terminal; nothing was killed. "+
			"Use read_terminal to see where it is, then decide whether to wait or to interrupt it with send_keys (Ctrl+C = \"\\u0003\").\n\n--- screen ---\n%s",
			aiTerminalRunCap, screen)
	case res.exitCode == nil:
		return "exit status unknown: no completion marker appeared (a full-screen program owns the terminal, or the output scrolled out of the buffer). " +
			"Use read_terminal to see the current state.\n\n--- screen ---\n" + screen
	default:
		return fmt.Sprintf("exit status %d\n\n--- screen ---\n%s", *res.exitCode, screen)
	}
}

// aiTerminalGate applies the §12.3 brakes that the terminal tools would
// otherwise slip past: the node-level failure pause (shared with queued
// commands) plus a per-minute count of terminal actions read back from the
// audit trail, because these tools write no `commands` row.
func (s *Server) aiTerminalGate(nodeID string) (bool, string) {
	allowed, reason, err := s.Store.CheckAICommandGate(nodeID, aiCommandLimit, nowUnix())
	if err != nil {
		return false, "internal"
	}
	if !allowed {
		return false, reason
	}
	count, err := s.Store.CountAITerminalActions(nodeID, nowUnix()-aiTerminalCommandWindow)
	if err != nil {
		return false, "internal"
	}
	if count >= aiCommandLimit {
		return false, "rate_limited"
	}
	return true, ""
}

// aiRunShellPayload is the confirmation payload for run_shell. It carries the
// terminal session id for the same reason send_keys does: a confirmed action is
// executed by a LATER request, when the turn's live binding is gone.
type aiRunShellPayload struct {
	Command           string `json:"command"`
	TerminalSessionID string `json:"terminal_session_id"`
}

// runAIShellNow performs the actual terminal run, shared by the direct path and
// the confirmation resume.
func (s *Server) runAIShellNow(r *http.Request, session *store.AISession, terminalSessionID, command, reason, risk string, toolCall map[string]any) map[string]any {
	if allowed, gateReason := s.aiTerminalGate(session.NodeID); !allowed {
		toolCall["status"] = "blocked"
		toolCall["reason"] = gateReason
		toolCall["result"] = "refused: " + gateReason
		return toolCall
	}
	if terminalSessionID == "" {
		return aiTerminalUnavailable(toolCall, "no terminal session is bound to this conversation")
	}
	io := newAITerminalIO(s.Hub, r.Context(), session.NodeID, terminalSessionID)
	result, err := s.execAIShell(r.Context(), io, command)
	if err != nil {
		return aiTerminalUnavailable(toolCall, "could not run the command in the terminal: "+err.Error())
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "ai", NodeID: session.NodeID, Action: store.AuditAITerminalRun,
		Command: truncateAIBytes(result.typed, 2048), Reason: reason, Risk: risk,
		SourceIP: s.Trust.RealIP(r), AISessionID: session.ID,
	})
	if result.exitCode != nil {
		// The loop surfaces this as the tool_result event's exit_code, which is
		// what the panel displays next to the command.
		toolCall["exit_code"] = *result.exitCode
	}
	if result.timedOut {
		toolCall["status"] = "timeout"
	} else {
		toolCall["status"] = "ok"
	}
	toolCall["result"] = aiShellToolContent(result)
	return toolCall
}

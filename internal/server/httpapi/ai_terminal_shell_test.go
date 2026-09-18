package httpapi

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/hub"
)

// --- a stand-in for "the agent + the browser holding the emulator" ---
//
// run_shell needs BOTH halves of the §12.7 topology: keystrokes travel
// server → agent → PTY (so the hub send path must succeed, which needs a live
// agent connection), while observation travels server → browser (so something
// must answer buffer queries). Neither exists in a unit test, and standing up a
// real agent socket would test the socket rather than the lifecycle.
//
// So the seam (newAITerminalIO) is swapped for this: it remembers what was
// typed and synthesizes the screen a real shell would have produced — including
// the sentinel, which is the whole point of the design.

type fakeTerminal struct {
	mu sync.Mutex
	// sent is every keystroke payload, in order.
	sent []string
	// lines is the synthesized screen, bottom = last.
	lines []string
	// exit is what the sentinel reports for a wrapped command.
	exit int
	// silent suppresses the sentinel, simulating a full-screen program that
	// never lets our printf run (§12.7.3's slow path).
	silent bool
	// hang makes the screen change on every read, so the quiet heuristic never
	// fires — the timeout path.
	hang bool
	// reads counts buffer queries (the poll cadence is observable).
	reads int
}

var fakeSentinelFormatRE = regexp.MustCompile(`__FOBE_([0-9a-f]+)_%d__`)

func (f *fakeTerminal) Send(data string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, data)
	if match := fakeSentinelFormatRE.FindStringSubmatch(data); match != nil {
		nonce := match[1]
		// A real shell echoes the wrapped command before running it.
		f.lines = append(f.lines, strings.TrimRight(data, "\n"))
		f.lines = append(f.lines, "uptime: load 0.1")
		if !f.silent {
			f.lines = append(f.lines, fmt.Sprintf("__FOBE_%s_%d__", nonce, f.exit))
		}
	}
	return true
}

func (f *fakeTerminal) Window(offset, lines int) (protocol.TerminalBuffer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.hang {
		f.lines = append(f.lines, fmt.Sprintf("tick %d", f.reads))
	}
	if lines <= 0 {
		lines = 24
	}
	all := f.lines
	start := 0
	if len(all) > lines {
		start = len(all) - lines
	}
	window := append([]string{}, all[start:]...)
	return protocol.TerminalBuffer{
		ID: "fake", OK: true, Cols: 80, Rows: 24,
		Length: len(all), Lines: window,
	}, nil
}

// wrappedCommands returns the sentinel-wrapped commands that were typed. This
// is the new ground truth for "what actually ran": nothing is queued any more,
// so the commands table can no longer answer that question (§12.7).
func (f *fakeTerminal) wrappedCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, payload := range f.sent {
		if strings.Contains(payload, "$?") {
			out = append(out, payload)
		}
	}
	return out
}

func (f *fakeTerminal) typed() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.sent, "")
}

// withFakeTerminal installs the fake for the rest of the test.
func withFakeTerminal(t *testing.T, exit int, output []string) *fakeTerminal {
	t.Helper()
	fake := &fakeTerminal{exit: exit, lines: append([]string{}, output...)}
	previous := newAITerminalIO
	newAITerminalIO = func(_ *hub.Hub, _ context.Context, _, _ string) aiTerminalIO {
		return fake
	}
	t.Cleanup(func() { newAITerminalIO = previous })
	return fake
}

// --- the lifecycle itself ---

// TestAIShellSentinelIsHowACommandFinishes: the fast path. The marker is the
// only reliable completion signal a PTY offers, and it is what makes an exit
// status recoverable at all.
func TestAIShellSentinelIsHowACommandFinishes(t *testing.T) {
	fake := withFakeTerminal(t, 7, []string{"root@probe:~# "})
	_, api := newTestServer(t)

	result, err := api.execAIShell(context.Background(), fake, "false")
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode == nil || *result.exitCode != 7 {
		t.Fatalf("exitCode = %v, want 7", result.exitCode)
	}
	if result.timedOut {
		t.Fatal("timed out, want the sentinel to end the wait")
	}
	if !strings.Contains(result.output, "uptime: load 0.1") {
		t.Fatalf("output = %q, want the command's screen", result.output)
	}
	// The marker is our bookkeeping and must not reach the model.
	if strings.Contains(result.output, "__FOBE_") {
		t.Fatalf("output leaked the sentinel: %q", result.output)
	}
	// The keystroke SEQUENCE is the contract: clear the operator's half-typed
	// line first, then the wrapped command. `result.typed` deliberately holds
	// only the command (it is what the audit row names), so the clear bytes are
	// checked on the transport instead.
	fake.mu.Lock()
	sent := append([]string{}, fake.sent...)
	fake.mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("sent %d payloads (%q), want exactly the clear and the command", len(sent), sent)
	}
	if sent[0] != "\x15\x03" {
		t.Fatalf("first payload = %q, want Ctrl+U Ctrl+C", sent[0])
	}
	if !strings.Contains(sent[1], "$?") || !strings.HasSuffix(sent[1], "\n") {
		t.Fatalf("second payload = %q, want the sentinel command plus Enter", sent[1])
	}
}

// TestAIShellWithoutSentinelReportsUnknownStatus: a full-screen program never
// lets our printf run, so the quiet heuristic ends the wait and the status is
// honestly unknown — §12.3 records that "unknown" must not count as a failure,
// or ordinary vim/top use would pause the node's AI.
func TestAIShellWithoutSentinelReportsUnknownStatus(t *testing.T) {
	fake := withFakeTerminal(t, 0, []string{"root@probe:~# "})
	fake.silent = true
	_, api := newTestServer(t)

	result, err := api.execAIShell(context.Background(), fake, "vim")
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode != nil {
		t.Fatalf("exitCode = %v, want nil (unknown)", result.exitCode)
	}
	if result.timedOut {
		t.Fatal("timed out, want the quiet heuristic to end the wait")
	}
}

// TestAIShellTimeoutNeverKillsTheCommand: the design's most deliberate
// subtraction. The old run_shell killed its process at 30s; §12.7.3 records
// that there is no clean way to kill one command in an interactive shell, so a
// runaway command keeps running and the model is told to look at it.
func TestAIShellTimeoutNeverKillsTheCommand(t *testing.T) {
	fake := withFakeTerminal(t, 0, []string{"root@probe:~# "})
	fake.silent = true
	fake.hang = true
	_, api := newTestServer(t)

	previous := aiTerminalRunCap
	aiTerminalRunCap = 300 * time.Millisecond
	t.Cleanup(func() { aiTerminalRunCap = previous })

	result, err := api.execAIShell(context.Background(), fake, "sleep 999")
	if err != nil {
		t.Fatal(err)
	}
	if !result.timedOut {
		t.Fatalf("timedOut = false, want the cap to fire (output %q)", result.output)
	}
	if len(fake.sent) == 0 {
		t.Fatal("nothing was typed")
	}
	// Nothing in the lifecycle may send a signal: the only bytes are the clear
	// and the command itself.
	for _, payload := range fake.sent {
		if strings.Contains(payload, "\x03") && !strings.HasPrefix(payload, "\x15\x03") {
			t.Fatalf("unexpected interrupt sent: %q", payload)
		}
	}
}

// TestAISentinelParsingIgnoresTheEcho pins the one subtlety of the marker
// format: the ECHOED command line also contains the nonce (it carries the
// printf format), so a parser that merely searched for the nonce would read a
// status out of the command text.
func TestAISentinelParsingIgnoresTheEcho(t *testing.T) {
	nonce := "deadbeef"
	echo := `uptime; printf '\n__FOBE_deadbeef_%d__\n' $?`
	if _, ok := aiSentinelExit([]string{echo}, nonce); ok {
		t.Fatal("the echoed command was mistaken for the marker")
	}
	if code, ok := aiSentinelExit([]string{echo, "__FOBE_deadbeef_3__"}, nonce); !ok || code != 3 {
		t.Fatalf("marker not read: ok=%v code=%d", ok, code)
	}
	// Another call's marker must not satisfy this one.
	if _, ok := aiSentinelExit([]string{"__FOBE_00000000_0__"}, nonce); ok {
		t.Fatal("a foreign nonce satisfied this call")
	}
}

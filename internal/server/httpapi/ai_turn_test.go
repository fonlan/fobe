package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAITurnBudgetChangeBudgetOnlyCountsChanges(t *testing.T) {
	now := time.Now()
	budget := newAITurnBudget(now)

	// Read-only calls are bounded by the read streak (§12.7.4), not by the
	// change budget; what matters here is that they do not consume it, or a turn
	// that investigated before acting would be cut off exactly when it is about
	// to fix something.
	for i := 0; i < 50; i++ {
		if aiToolIsChange(aiToolReadTerminal) {
			t.Fatal("read_terminal classified as a change")
		}
	}
	for i := 0; i < aiTurnChangeBudget; i++ {
		ok, reason := budget.NoteChange()
		if !ok {
			t.Fatalf("change %d refused: %q", i+1, reason)
		}
	}
	if ok, reason := budget.NoteChange(); ok || reason != "turn_budget" {
		t.Fatalf("budget not enforced: ok=%v reason=%q", ok, reason)
	}
	if budget.Changes() != aiTurnChangeBudget {
		t.Fatalf("changes = %d, want %d", budget.Changes(), aiTurnChangeBudget)
	}
}

func TestAITurnBudgetRepeatDetection(t *testing.T) {
	budget := newAITurnBudget(time.Now())

	first := json.RawMessage(`{"command":"uptime"}`)
	if ok, reason, _ := budget.NoteCall("run_shell", first); !ok {
		t.Fatalf("first call refused: %q", reason)
	}
	// Byte-identical repeat: the loop is stuck.
	if ok, reason, count := budget.NoteCall("run_shell", first); ok || reason != "repeat_call" || count != 2 {
		t.Fatalf("identical repeat allowed: ok=%v reason=%q count=%d", ok, reason, count)
	}
	// Same tool, different arguments: normal work, not a loop.
	if ok, reason, _ := budget.NoteCall("run_shell", json.RawMessage(`{"command":"date"}`)); !ok {
		t.Fatalf("different args refused as a repeat: %q", reason)
	}
	// Key order and whitespace are the model's formatting noise, not a new call.
	spaced := json.RawMessage(`{  "command" : "uptime" }`)
	if ok, reason, _ := budget.NoteCall("run_shell", spaced); ok || reason != "repeat_call" {
		t.Fatalf("reformatted repeat allowed: ok=%v reason=%q", ok, reason)
	}
	// Nested objects are compared structurally too, not as raw text.
	if ok, _, _ := budget.NoteCall(aiToolSendKeys, json.RawMessage(`{"data":{"beta":true,"pin":1}}`)); !ok {
		t.Fatal("first nested call refused")
	}
	if ok, reason, _ := budget.NoteCall(aiToolSendKeys, json.RawMessage(`{"data":{"pin":1,"beta":true}}`)); ok || reason != "repeat_call" {
		t.Fatalf("reordered nested repeat allowed: ok=%v reason=%q", ok, reason)
	}
	// Unparseable arguments still get fingerprinted instead of slipping through.
	budget.NoteCall("run_shell", json.RawMessage(`{"command":`))
	if ok, reason, _ := budget.NoteCall("run_shell", json.RawMessage(`{"command":`)); ok || reason != "repeat_call" {
		t.Fatalf("garbage repeat allowed: ok=%v reason=%q", ok, reason)
	}
}

func TestAITurnBudgetDeadline(t *testing.T) {
	start := time.Now()
	budget := newAITurnBudget(start)
	if ok, reason := budget.Check(start); !ok {
		t.Fatalf("fresh turn refused: %q", reason)
	}
	if ok, reason := budget.Check(start.Add(aiTurnDeadline - time.Second)); !ok {
		t.Fatalf("turn refused before the deadline: %q", reason)
	}
	if ok, reason := budget.Check(start.Add(aiTurnDeadline + time.Second)); ok || reason != "turn_timeout" {
		t.Fatalf("deadline not enforced: ok=%v reason=%q", ok, reason)
	}
}

// TestAIToolIsChangeFailsClosedForUnknownTools pins the classification's
// failure mode: a tool added to the set later must be treated as a change until
// someone classifies it, so a new mutating tool cannot silently escape the
// per-turn budget.
func TestAIToolIsChangeFailsClosedForUnknownTools(t *testing.T) {
	for _, tool := range []string{aiToolRunShell, aiToolSendKeys, "restart_singbox", "some_future_tool"} {
		if !aiToolIsChange(tool) {
			t.Fatalf("%s should count as a change", tool)
		}
	}
	if aiToolIsChange(aiToolReadTerminal) {
		t.Fatal("read_terminal should be read-only")
	}
	// Names that were never tools (the §12.2 table used to promise them) must
	// still fall on the CHANGE side: the whitelist is positive, so anything
	// unrecognised is treated as a mutation. That is the fail-closed direction,
	// and it is why removing them from the whitelist was safe.
	for _, tool := range []string{"list_nodes", "get_metrics", "tail_logs", "install_singbox"} {
		if !aiToolIsChange(tool) {
			t.Fatalf("%s is not a tool and must count as a change", tool)
		}
	}
}

func TestAISSEWriterEmitsNamedEventsAndFlushes(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, err := newAISSEWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q", got)
	}
	// The proxy-buffering hint is the difference between working streaming and
	// "the events all arrive at the end" behind the operator's own nginx.
	if got := recorder.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	if err := writer.Send(aiEventText, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Send(aiEventToolCall, map[string]any{"id": "call-1", "name": "run_shell"}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: text_delta\ndata: {\"text\":\"hello\"}\n\n") {
		t.Fatalf("text event malformed: %q", body)
	}
	if !strings.Contains(body, "event: tool_call\ndata: ") {
		t.Fatalf("tool_call event missing: %q", body)
	}
	if !recorder.Flushed {
		t.Fatal("events were not flushed: a buffered stream is indistinguishable from a broken one")
	}
}

// TestAISSEWriterRequiresFlusher: a ResponseWriter without Flush() must fail at
// construction rather than silently produce a stream that only arrives at the
// end of the turn.
func TestAISSEWriterRequiresFlusher(t *testing.T) {
	if _, err := newAISSEWriter(nonFlushingWriter{}); err == nil {
		t.Fatal("expected an error for a non-flushing writer")
	}
}

type nonFlushingWriter struct{ header http.Header }

func (w nonFlushingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (nonFlushingWriter) Write(b []byte) (int, error) { return len(b), nil }
func (nonFlushingWriter) WriteHeader(int)             {}

// TestAITurnBudgetReadStreakIsTheReadOnlyBrake covers the §12.7.4 trade: to make
// re-reading a screen possible at all, read_terminal is exempt from repeat
// detection (its fingerprint is constant, so the repeat rule would refuse the
// second read of a turn). The replacement brake is a cap on CONSECUTIVE reads,
// and any other tool call resets it — "read, change, read again" is work.
func TestAITurnBudgetReadStreakIsTheReadOnlyBrake(t *testing.T) {
	budget := newAITurnBudget(time.Now())

	// Identical calls are NOT refused by repeat detection: that is the exemption.
	for i := 1; i <= aiTurnReadLimit; i++ {
		if ok, reason := budget.NoteRead(); !ok {
			t.Fatalf("read %d refused (%q), want the first %d allowed", i, reason, aiTurnReadLimit)
		}
	}
	if ok, reason := budget.NoteRead(); ok || reason != "read_limit" {
		t.Fatalf("read %d = ok:%v reason:%q, want read_limit", aiTurnReadLimit+1, ok, reason)
	}

	// Any other call resets the streak.
	budget2 := newAITurnBudget(time.Now())
	for i := 0; i < aiTurnReadLimit; i++ {
		if ok, _ := budget2.NoteRead(); !ok {
			t.Fatal("read refused before the cap")
		}
	}
	if _, _, _ = budget2.NoteCall(aiToolRunShell, json.RawMessage(`{"command":"uptime"}`)); false {
		t.Fatal("unreachable")
	}
	if ok, reason := budget2.NoteRead(); !ok {
		t.Fatalf("read refused after an intervening tool call (%q), want the streak reset", reason)
	}
}

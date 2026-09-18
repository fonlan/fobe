package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/server/store"
)

// The autonomous tool loop (design.md §12.6): one user message drives
// ask → execute → feed back → ask again until the model stops or a limit trips.
//
// Before this file the loop had no end-to-end test at all — only its pure
// bookkeeping (ai_turn_test.go) was covered. Everything below drives it through
// real HTTP against a mock upstream that speaks the streaming contract, because
// that is the only place the three §12.6 controls, the confirmation handshake and
// the stop button actually meet the wire.

// TestAILoopRunsToolCallThenAnswers is the shape every other loop test builds
// on: a tool call in round one, a plain answer in round two.
func TestAILoopRunsToolCallThenAnswers(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-loop-multi")
	cookie := loginCookie(t, server.URL)
	upstream := newMockAIUpstream(t,
		openAIStreamToolCall(t, "call-tail", "read_terminal", `{}`),
		openAIStreamText(t, "sing-box exited after the cert check"),
	)
	configureAI(t, api, upstream.URL(), "k", "m")
	shrinkAICommandWait(t, 3*time.Second)

	// read_terminal is a round trip to the browser that holds the emulator
	// (§12.7.1), so the test stands in for that browser.
	answerBrowserQueries(t, api, "ts-loop", "loop-marker-1")

	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "check the terminals", TerminalSessionID: "ts-loop",
	})

	// One user message drove TWO upstream calls: the loop asked again because
	// the model had asked for a tool.
	if upstream.Calls() != 2 {
		t.Fatalf("upstream calls = %d, want 2", upstream.Calls())
	}
	if result.TurnEnd == nil || result.TurnEnd.Reason != "completed" {
		t.Fatalf("turn end = %+v, want reason=completed", result.TurnEnd)
	}
	if result.Text != "sing-box exited after the cert check" {
		t.Fatalf("text = %q", result.Text)
	}

	// The second request differs from the first in exactly one way: it carries
	// the tool result — paired with the model's own call id — plus the assistant
	// turn that asked for it (replayed so a tool loop stays legal).
	first, second := upstream.Request(t, 0), upstream.Request(t, 1)
	for _, message := range first.Messages {
		if message.Role == "tool" {
			t.Fatalf("the first request already carried a tool result: %+v", message)
		}
	}
	msg := toolMessageFor(second, "call-tail")
	if msg == nil || !strings.Contains(msg.Content, "loop-marker-1") {
		t.Fatalf("round-2 request = %+v", second.Messages)
	}
	replayed := false
	for _, message := range second.Messages {
		for _, call := range message.ToolCalls {
			if call.ID == "call-tail" {
				replayed = true
			}
		}
	}
	if !replayed {
		t.Fatalf("round-2 request lost the assistant's tool call: %+v", second.Messages)
	}

	messages, err := api.Store.ListAIMessages(result.SessionID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 4 || messages[0].Role != "user" {
		t.Fatalf("persisted transcript = %+v", messages)
	}
	// Skip the trailing `turn_end` marker (§12.6): the claim here is about the
	// last CONVERSATIONAL message, and the marker is panel metadata.
	last := messages[len(messages)-1]
	if last.Role == "turn_end" && len(messages) > 1 {
		last = messages[len(messages)-2]
	}
	if last.Role != "assistant" || last.Content != "sing-box exited after the cert check" {
		t.Fatalf("last message = %+v", last)
	}
	if !toolResultMessageHas(t, api, result.SessionID, "loop-marker-1") {
		t.Fatal("the tool output never reached the transcript")
	}
}

// TestAILoopChangeBudgetRefusesTheEleventhCall pins the per-turn change budget:
// a runaway model must not be able to burst damage inside one turn.
func TestAILoopChangeBudgetRefusesTheEleventhCall(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-loop-budget")
	cookie := loginCookie(t, server.URL)

	// A model that keeps asking for changes, each one DIFFERENT so only the
	// budget — not repeat detection — can stop it.
	upstream := newMockAIUpstreamFunc(t, func(call int) string {
		return openAIStreamToolCall(t, fmt.Sprintf("call-%d", call), "run_shell",
			fmt.Sprintf(`{"command":"echo step %d","reason":"probe","risky":false}`, call))
	})
	configureAI(t, api, upstream.URL(), "k", "m")
	shrinkAICommandWait(t, 50*time.Millisecond)
	fake := withFakeTerminal(t, 0, []string{"root@probe:~# "})

	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "fix it", TerminalSessionID: "ts-budget",
	})

	if result.TurnEnd == nil || result.TurnEnd.Reason != "turn_budget" {
		t.Fatalf("turn end = %+v, want reason=turn_budget", result.TurnEnd)
	}
	if result.TurnEnd.Changes != aiTurnChangeBudget {
		t.Fatalf("turn changes = %d, want %d", result.TurnEnd.Changes, aiTurnChangeBudget)
	}
	// The budget is checked before the call executes, and only after asking the
	// model once more: the call that tripped the limit was never carried out.
	if upstream.Calls() != aiTurnChangeBudget+1 {
		t.Fatalf("upstream calls = %d, want %d", upstream.Calls(), aiTurnChangeBudget+1)
	}
	refused := result.ToolResults[len(result.ToolResults)-1]
	if refused.Status != "refused" || refused.Reason != "turn_budget" {
		t.Fatalf("refusal event = %+v", refused)
	}

	// What was TYPED is the ground truth, not the loop's own counters: since
	// §12.7 nothing is queued, so the commands table cannot answer this. Exactly
	// the allowed changes reached the terminal and the eleventh did not.
	typed := fake.wrappedCommands()
	if len(typed) != aiTurnChangeBudget {
		t.Fatalf("commands typed = %d, want %d", len(typed), aiTurnChangeBudget)
	}
	for _, payload := range typed {
		if strings.Contains(payload, "echo step 11") {
			t.Fatalf("the refused call was typed anyway: %q", payload)
		}
	}
}

// TestAILoopRepeatCallStopsAfterOneExecution covers the ONLY automatic brake the
// read-only path has (§12.6 记账: no step cap there, so a stuck model would
// otherwise read forever).
func TestAILoopRepeatCallStopsAfterOneExecution(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-loop-repeat")
	cookie := loginCookie(t, server.URL)
	upstream := newMockAIUpstream(t, openAIStreamToolCall(t, "call-dup", "run_shell",
		`{"command":"uptime","reason":"probe","risky":false}`))
	configureAI(t, api, upstream.URL(), "k", "m")
	shrinkAICommandWait(t, 50*time.Millisecond)
	fake := withFakeTerminal(t, 0, []string{"root@probe:~# "})

	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "look at it", TerminalSessionID: "ts-repeat",
	})

	if result.TurnEnd == nil || result.TurnEnd.Reason != "repeat_call" {
		t.Fatalf("turn end = %+v, want reason=repeat_call", result.TurnEnd)
	}
	// Exactly two calls: the one that executed and the one that was recognised
	// as the same call.
	if upstream.Calls() != 2 {
		t.Fatalf("upstream calls = %d, want 2", upstream.Calls())
	}
	if len(result.ToolResults) != 2 {
		t.Fatalf("tool results = %+v", result.ToolResults)
	}
	if result.ToolResults[0].Status != "ok" {
		t.Fatalf("first result = %+v, want the call to have run", result.ToolResults[0])
	}
	if result.ToolResults[1].Status != "refused" || result.ToolResults[1].Reason != "repeat_call" {
		t.Fatalf("repeat refusal = %+v", result.ToolResults[1])
	}
	if typed := fake.wrappedCommands(); len(typed) != 1 {
		t.Fatalf("commands typed = %d, want 1 (the identical call must run once)", len(typed))
	}
}

// TestAILoopContinueAfterConfirmationPairsTheCallID drives the whole §12.6
// handshake: the stream ends at a risky call, and the continue endpoint applies
// the decision and resumes with the result under the ORIGINAL call id.
func TestAILoopContinueAfterConfirmationPairsTheCallID(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-loop-confirm")
	cookie := loginCookie(t, server.URL)
	upstream := newMockAIUpstream(t,
		openAIStreamToolCall(t, "call-risk", "run_shell",
			`{"command":"reboot","reason":"restart required","risky":true}`),
		openAIStreamText(t, "reboot requested"),
	)
	configureAI(t, api, upstream.URL(), "k", "m")
	shrinkAICommandWait(t, 300*time.Millisecond)
	fake := withFakeTerminal(t, 0, []string{"root@probe:~# "})

	first := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "reboot it", TerminalSessionID: "ts-confirm",
	})
	if first.Confirmation == nil || first.Confirmation.ActionID == "" {
		t.Fatalf("needs_confirmation = %+v", first.Confirmation)
	}
	if first.TurnEnd == nil || first.TurnEnd.Reason != "needs_confirmation" {
		t.Fatalf("turn end = %+v", first.TurnEnd)
	}
	if typed := fake.wrappedCommands(); len(typed) != 0 {
		t.Fatalf("a risky action reached the terminal before confirmation: %q", typed)
	}
	action, err := api.Store.GetAIPendingAction(first.Confirmation.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if action.Status != "pending" || action.Kind != "run_shell" || action.Risk != "risky" {
		t.Fatalf("pending action = %+v", action)
	}
	// Persisted at request time, which is what lets the resumed turn pair its
	// tool result: Anthropic rejects an orphan tool_result.
	if action.CallID != "call-risk" {
		t.Fatalf("pending action call id = %q, want call-risk", action.CallID)
	}

	cont := postAIChatContinue(t, server.URL, cookie, aiContinueRequest{
		SessionID: first.SessionID, ActionID: first.Confirmation.ActionID, Approved: true,
	})
	if cont.Status != http.StatusOK {
		t.Fatalf("continue status = %d (%v)", cont.Status, cont.JSONBody)
	}
	if cont.TurnEnd == nil || cont.TurnEnd.Reason != "completed" {
		t.Fatalf("continued turn end = %+v", cont.TurnEnd)
	}

	// Approving runs it in the operator's terminal (§12.7.3) — it is NOT queued
	// as a command for the agent any more, so the keystrokes are the evidence.
	typed := fake.wrappedCommands()
	if len(typed) != 1 || !strings.Contains(typed[0], "reboot") {
		t.Fatalf("typed = %q, want exactly the approved command", typed)
	}
	confirmed, err := api.Store.GetAIPendingAction(first.Confirmation.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != "confirmed" {
		t.Fatalf("confirmed action = %+v", confirmed)
	}

	// The resumed request must carry the outcome under "call-risk" — the id the
	// model used, which resolveAIConfirmation reads off the pending row.
	if upstream.Calls() < 2 {
		t.Fatalf("upstream calls = %d, want a resumed turn", upstream.Calls())
	}
	msg := toolMessageFor(upstream.Request(t, 1), "call-risk")
	// The resumed turn reports what the TERMINAL did (exit status from the
	// sentinel plus the screen), not "queued for the agent" — §12.7.3.
	if msg == nil || !strings.Contains(msg.Content, "exit status 0") || !strings.Contains(msg.Content, "uptime: load 0.1") {
		t.Fatalf("resumed request tool message = %+v", msg)
	}
}

// TestAILoopDoesNotStoreATurnWithNothingToSay pins that a turn which streamed
// nothing — the upstream died before the first delta, the stream was truncated,
// or the operator stopped straight away — leaves no assistant row behind, and
// therefore that the NEXT message of that session is still a legal request.
//
// The two halves belong together: an empty assistant row is not merely a blank
// bubble in the transcript, it re-encodes on the wire as a bare
// {"role":"assistant"}, and a gateway answers that with 400 "Invalid assistant
// message: content or tool_calls must be set" on every later request of the
// session. That is how the defect was found: the operator sees a failure about
// a message they never wrote, long after the turn that actually broke.
func TestAILoopDoesNotStoreATurnWithNothingToSay(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-loop-empty-turn")
	cookie := loginCookie(t, server.URL)

	// Call 1: the stream dies right after the role frame — no content, no tool
	// call, no finish_reason ⇒ the adapter calls it truncated.
	truncated := openAIData(openAIChoice(t, map[string]any{"role": "assistant"}, ""))
	upstream := newMockAIUpstream(t, truncated, openAIStreamText(t, "yes, I am here"))
	configureAI(t, api, upstream.URL(), "k", "m")

	first := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "hello"})
	if first.TurnEnd == nil || first.TurnEnd.Reason != "upstream_error" {
		t.Fatalf("turn end = %+v, want reason=upstream_error", first.TurnEnd)
	}

	stored, err := api.Store.ListAIMessages(first.SessionID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range stored {
		if message.Role == "assistant" {
			t.Fatalf("a turn that said nothing was stored as a message: %+v", message)
		}
	}

	// The next message of the SAME session must not replay the empty turn.
	second := postAIChat(t, server.URL, cookie, aiChatRequest{
		SessionID: first.SessionID, NodeID: nodeID, Message: "are you there?",
	})
	if second.Text != "yes, I am here" {
		t.Fatalf("text = %q", second.Text)
	}
	for _, message := range upstream.Request(t, 1).Messages {
		if message.Role == "assistant" {
			t.Fatalf("the replayed request carries an assistant turn that said nothing: %+v", message)
		}
	}
}

// TestAILoopStopPersistsTheHalfTurn is the stop button (§12.6): the client goes
// away mid-turn, the loop ends, and the deltas the operator already saw stay in
// the transcript instead of being rolled back.
func TestAILoopStopPersistsTheHalfTurn(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-loop-stop")
	cookie := loginCookie(t, server.URL)

	// An upstream that delivers one delta and then waits: only the client going
	// away can end this turn.
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openAIData(`{"choices":[{"delta":{"role":"assistant"}}]}`))
		_, _ = io.WriteString(w, openAIData(`{"choices":[{"delta":{"content":"half a turn"}}]}`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	// Registered after upstream.Close so it runs BEFORE it: a stuck handler must
	// never make httptest.Server.Close() hang the test.
	defer close(release)
	configureAI(t, api, upstream.URL, "k", "m") // httptest.Server: URL is a field

	ctx, cancel := context.WithCancel(context.Background())
	stream := openAIChatStream(t, ctx, server.URL+"/api/ai/chat", cookie, aiChatRequest{
		NodeID: nodeID, Message: "take your time",
	})
	defer stream.close()

	// Read up to the delta the operator would have seen, then press stop.
	result := &aiChatResult{}
	for {
		event, ok := stream.next()
		if !ok {
			t.Fatal("the stream ended before the stop button was pressed")
		}
		applyAIChatEvent(t, result, event)
		if strings.Contains(result.Text, "half a turn") {
			break
		}
	}
	if result.SessionID == "" {
		t.Fatal("the session event never arrived")
	}
	cancel()

	var half *store.AIMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := api.Store.ListAIMessages(result.SessionID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for i := range messages {
			if messages[i].Role == "assistant" && strings.Contains(messages[i].Content, "half a turn") {
				half = &messages[i]
			}
		}
		if half != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if half == nil {
		t.Fatal("the interrupted half turn was not persisted")
	}
	if half.Content != "half a turn" {
		t.Fatalf("persisted half turn = %q", half.Content)
	}
}

// TestAIGetSessionHistoryReturnsRawAndThinking covers the endpoint the panel
// restores a transcript from: messages in order, with the reasoning text already
// extracted server-side and the native blocks handed over verbatim (§12.5 keeps
// opaque blocks opaque outside the adapter).
func TestAIGetSessionHistoryReturnsRawAndThinking(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-history")
	cookie := loginCookie(t, server.URL)

	sessionID := "sess-history"
	if err := api.Store.CreateAISessionPinned(sessionID, nodeID, "term-1",
		testAIProviderID, "test-model", store.ProtocolOpenAICompletions); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.InsertAIMessage(sessionID, "user", "why is it unhealthy?"); err != nil {
		t.Fatal(err)
	}
	raw := `[{"type":"thinking","thinking":"check the cert first"},{"type":"text","text":"the cert expired"}]`
	if err := api.Store.InsertAIMessageBlocks(sessionID, "assistant", "the cert expired",
		`{"raw":`+raw+`}`); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.InsertAIMessageBlocks(sessionID, "tool", "cert expired at 03:00",
		`{"tool_results":[{"CallID":"call-1","Content":"cert expired at 03:00"}]}`); err != nil {
		t.Fatal(err)
	}

	resp, body := doAuthed(t, http.MethodGet, server.URL+"/api/ai/sessions/"+sessionID, cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}
	var view aiSessionHistoryView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.SessionID != sessionID || view.NodeID != nodeID {
		t.Fatalf("view = %+v", view)
	}
	if len(view.Messages) != 3 {
		t.Fatalf("messages = %+v", view.Messages)
	}
	// Order matters: the panel replays this list as the transcript.
	for i, want := range []string{"user", "assistant", "tool"} {
		if view.Messages[i].Role != want {
			t.Fatalf("message %d role = %q, want %q", i, view.Messages[i].Role, want)
		}
	}
	if !strings.Contains(string(view.Messages[1].Raw), `"thinking"`) {
		t.Fatalf("raw blocks = %s", view.Messages[1].Raw)
	}
	if view.Messages[1].Thinking != "check the cert first" {
		t.Fatalf("thinking = %q", view.Messages[1].Thinking)
	}
	if view.Messages[0].Thinking != "" || view.Messages[2].Thinking != "" {
		t.Fatalf("a message without reasoning blocks reported thinking: %+v", view.Messages)
	}
}

// TestAIChatRejectsUnknownReasoningLevelBeforeStreaming pins WHERE the §12.5
// level validation happens: before the 200, so it is a JSON error and not an
// `error` event (once the stream is open a status code can no longer be sent).
func TestAIChatRejectsUnknownReasoningLevelBeforeStreaming(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-bad-level")
	cookie := loginCookie(t, server.URL)
	upstream := newMockAIUpstream(t, openAIStreamText(t, "should not run"))
	configureAI(t, api, upstream.URL(), "k", "m")

	// configureAI declares off/low/medium/high for the model; "minimal" is not
	// one of them.
	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "hello", ReasoningLevel: "minimal",
	})
	if result.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", result.Status, result.JSONBody)
	}
	if code := result.errorCode(); code != "bad_reasoning_level" {
		t.Fatalf("error code = %q, want bad_reasoning_level", code)
	}
	if result.JSONBody == nil || len(result.Raw) != 0 {
		t.Fatalf("expected a plain JSON refusal, got %+v", result)
	}
	if upstream.Calls() != 0 {
		t.Fatalf("upstream called %d times for a refused reasoning level", upstream.Calls())
	}
}

// TestAIChatAppliesStoredDefaultReasoning pins the other half of the default
// trio on the real turn path: a request that carries NO reasoning level (the
// panel's picker left at "unset") must still reach the model at the level stored
// in ai.default_reasoning. TestAIDefaultReasoningLevel unit-checks the fallback
// itself; this one proves it is WIRED into the turn, because a fallback that no
// caller consults looks identical to no fallback at all from the panel's side.
func TestAIChatAppliesStoredDefaultReasoning(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-default-level")
	cookie := loginCookie(t, server.URL)
	upstream := newMockAIUpstream(t, openAIStreamText(t, "answered"))
	configureAI(t, api, upstream.URL(), "k", "m")
	// configureAI declares off/low/medium/high for the model, so "high" is a
	// level this pair really exposes.
	if err := api.Store.SetSetting("ai.default_reasoning", "high", false); err != nil {
		t.Fatal(err)
	}

	result := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "hello"})
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d (%s)", result.Status, result.Raw)
	}
	if got := upstream.Request(t, 0).ReasoningEffort; got != "high" {
		t.Fatalf("upstream reasoning_effort = %q, want the stored default high", got)
	}
}

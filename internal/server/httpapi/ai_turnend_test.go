package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAITurnEndReasonIsPersistedAndReplayed covers §12.6's requirement that a
// conversation reloaded later explains WHY a turn stopped.
//
// The reason exists in no other place: a completed-but-cut-short turn is
// indistinguishable from a finished one once the stream is gone, so without a
// persisted marker the operator sees half an answer and no way to tell whether
// the model stopped, the per-turn budget tripped, or the upstream died.
func TestAITurnEndReasonIsPersistedAndReplayed(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-turnend")
	cookie := loginCookie(t, server.URL)

	// Turn 1: a plain answer → reason=completed.
	upstream := newMockAIUpstream(t, openAIStreamText(t, "all good"))
	configureAI(t, api, upstream.URL(), "k", "m")
	first := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "status?"})
	if first.TurnEnd == nil || first.TurnEnd.Reason != "completed" {
		t.Fatalf("turn end = %+v, want completed", first.TurnEnd)
	}

	history := aiSessionHistory(t, server, cookie, first.SessionID)
	reasons := turnReasons(history)
	if len(reasons) != 1 || reasons[0] != "completed" {
		t.Fatalf("persisted turn reasons = %v, want [completed]", reasons)
	}

	// Turn 2 in the SAME session, driven past the per-turn change budget by
	// distinct run_shell calls → reason=turn_budget, also persisted.
	// Every call must be DISTINCT: a repeated command trips repeat detection
	// first (correctly, but that is a different reason than this test asserts).
	busy := newMockAIUpstreamFunc(t, func(call int) string {
		step := strconv.Itoa(call)
		return openAIStreamToolCall(t, "call-"+step, "run_shell",
			`{"command":"echo step `+step+`","reason":"step","risky":false}`)
	})
	configureAI(t, api, busy.URL(), "k", "m")
	shrinkAICommandWait(t, time.Second)
	second := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, SessionID: first.SessionID, Message: "fix everything",
	})
	if second.TurnEnd == nil || second.TurnEnd.Reason != "turn_budget" {
		t.Fatalf("turn end = %+v, want turn_budget", second.TurnEnd)
	}

	history = aiSessionHistory(t, server, cookie, first.SessionID)
	reasons = turnReasons(history)
	if len(reasons) != 2 || reasons[0] != "completed" || reasons[1] != "turn_budget" {
		t.Fatalf("persisted turn reasons = %v, want [completed turn_budget]", reasons)
	}
	total := 0
	for _, message := range history.Messages {
		if message.Role == "turn_end" {
			total += message.Changes
		}
	}
	if total != aiTurnChangeBudget {
		t.Fatalf("persisted changes = %d, want %d (the count is what tells the operator how far it got)", total, aiTurnChangeBudget)
	}
}

// TestAITurnEndMarkerIsNotSentUpstream is the guard that keeps the persisted
// reason from becoming part of the conversation: an unknown role is rejected
// outright by Anthropic, and the OpenAI dialects would hand the model a message
// that is not dialogue at all.
func TestAITurnEndMarkerIsNotSentUpstream(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-turnend-upstream")
	cookie := loginCookie(t, server.URL)

	upstream := newMockAIUpstream(t, openAIStreamText(t, "first answer"), openAIStreamText(t, "second answer"))
	configureAI(t, api, upstream.URL(), "k", "m")

	first := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "hello"})
	if first.SessionID == "" {
		t.Fatal("no session id")
	}
	// The marker is only written when a turn ENDS, so a second turn in the same
	// session is the first request that could carry the previous one.
	postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, SessionID: first.SessionID, Message: "and now?",
	})

	if upstream.Calls() < 2 {
		t.Fatalf("upstream calls = %d, want at least 2", upstream.Calls())
	}
	second := upstream.Request(t, 1)
	for _, message := range second.Messages {
		if message.Role == "turn_end" {
			t.Fatalf("the turn_end marker was sent upstream: %+v", second.Messages)
		}
	}
	// And the previous turn IS still there — filtering must drop only the marker.
	found := false
	for _, message := range second.Messages {
		if strings.Contains(message.Content, "first answer") {
			found = true
		}
	}
	if !found {
		t.Fatalf("history lost the previous answer while filtering markers: %+v", second.Messages)
	}
}

func aiSessionHistory(t *testing.T, server *httptest.Server, cookie, sessionID string) aiSessionHistoryView {
	t.Helper()
	resp, raw := doAuthed(t, "GET", server.URL+"/api/ai/sessions/"+sessionID, cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history: %d %s", resp.StatusCode, raw)
	}
	var view aiSessionHistoryView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func turnReasons(history aiSessionHistoryView) []string {
	out := []string{}
	for _, message := range history.Messages {
		if message.Role == "turn_end" {
			out = append(out, message.TurnReason)
		}
	}
	return out
}

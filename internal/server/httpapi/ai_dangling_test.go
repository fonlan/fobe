package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/fonlan/fobe/internal/server/aiprotocol"
)

// TestCloseDanglingToolCallsRepairsAnUnansweredTurn pins the fix for a bug that
// only bites Anthropic users, and only on the message AFTER the interrupted one.
//
// An assistant turn that asked for tools is persisted before the tools run. If
// the turn then ends without recording a result — a per-turn budget refusal, a
// stop, or the operator typing a new message instead of answering the
// confirmation — the transcript keeps a tool_use with no tool_result. Anthropic
// pairs the two strictly and 400s the NEXT request of that session, so the
// failure surfaces far from its cause: the operator sees an error about a block
// they never wrote.
func TestCloseDanglingToolCallsRepairsAnUnansweredTurn(t *testing.T) {
	_, api := newTestServer(t)
	// The session's node is a foreign key, so the node has to exist first.
	nodeID := createAINode(t, api, "node-dangling")
	if err := api.Store.CreateAISessionPinned("sess-dangling", nodeID, "", "aip-x", "model-x", "anthropic-messages"); err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(aiMessageEnvelope{ToolCalls: []aiprotocol.ToolCall{
		{ID: "call-open", Name: "run_shell", Arguments: json.RawMessage(`{"command":"true"}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.InsertAIMessageBlocks("sess-dangling", "assistant", "let me check", string(envelope)); err != nil {
		t.Fatal(err)
	}

	if err := api.closeDanglingToolCalls("sess-dangling"); err != nil {
		t.Fatal(err)
	}
	messages, err := api.Store.ListAIMessages("sess-dangling", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected the dangling call to be closed with one tool row, got %d rows", len(messages))
	}
	closing := messages[len(messages)-1]
	if closing.Role != "tool" {
		t.Fatalf("closing row role = %q, want tool", closing.Role)
	}
	var stored aiMessageEnvelope
	if err := json.Unmarshal([]byte(closing.Blocks), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.ToolResults) != 1 || stored.ToolResults[0].CallID != "call-open" {
		t.Fatalf("closing result does not pair with the open call: %+v", stored.ToolResults)
	}
	if !stored.ToolResults[0].IsError {
		t.Fatal("an unexecuted call must be reported as an error, not as success")
	}

	// Idempotent: a transcript that is already paired must not grow a second
	// synthetic result (which would itself be an orphan pair for Anthropic).
	if err := api.closeDanglingToolCalls("sess-dangling"); err != nil {
		t.Fatal(err)
	}
	after, err := api.Store.ListAIMessages("sess-dangling", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(messages) {
		t.Fatalf("second pass appended rows: %d -> %d", len(messages), len(after))
	}
}

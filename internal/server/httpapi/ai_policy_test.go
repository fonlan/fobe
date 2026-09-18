package httpapi

import (
	"testing"
	"time"
)

// TestAIDefaultPolicyConfirmGatesEveryChangeAction pins the §12.3 execution
// policy, which §12.4 sells as the mitigation ("tighten it with one config
// change").
//
// The setting was written by the settings page and read by NOTHING in the Go
// tree, so the escape hatch did not exist: the panel displayed the choice and
// the assistant ignored it. That mattered more once §12.6 made the assistant
// autonomous — the difference between one change at a time and a chain of them.
func TestAIDefaultPolicyConfirmGatesEveryChangeAction(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	nodeID := createAINode(t, api, "ai-policy")
	cookie := loginCookie(t, server.URL)

	// A NON-risky run_shell: the model does not flag it, so only the policy can
	// make the panel ask. Risky calls are gated regardless (covered elsewhere).
	//
	// The mock's call counter is global to the test and repeats the last body, so
	// the canned list spells out all three phases: allow (tool → text), confirm
	// (tool → text), then the approved continue (text). Using a call-indexed
	// closure here silently gave the second phase a text answer and made the test
	// pass for the wrong reason.
	policyToolCall := openAIStreamToolCall(t, "call-policy", "run_shell",
		`{"command":"uptime","reason":"check the load","risky":false}`)
	upstream := newMockAIUpstream(t,
		policyToolCall, openAIStreamText(t, "ok"),
		policyToolCall, openAIStreamText(t, "ok"),
		openAIStreamText(t, "done"),
	)
	configureAI(t, api, upstream.URL(), "k", "m")
	shrinkAICommandWait(t, 2*time.Second)
	fake := withFakeTerminal(t, 0, []string{"root@probe:~# "})
	ts := "ts-policy"

	// Default (unset) = allow: the command executes without a prompt.
	allowed := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "check uptime", TerminalSessionID: ts,
	})
	if allowed.Confirmation != nil {
		t.Fatalf("the default policy asked for confirmation: %+v", allowed.Confirmation)
	}
	if typed := fake.wrappedCommands(); len(typed) != 1 {
		t.Fatalf("commands typed after the default policy = %d, want 1 (the call should have run)", len(typed))
	}

	// confirm: the SAME call must now stop at a confirmation instead of running.
	if err := api.Store.SetSetting("ai.default_policy", "confirm", false); err != nil {
		t.Fatal(err)
	}
	gated := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "check uptime again", TerminalSessionID: ts,
	})
	if gated.Confirmation == nil {
		t.Fatalf("policy=confirm did not gate a change action (turn_end=%+v)", gated.TurnEnd)
	}
	if gated.TurnEnd == nil || gated.TurnEnd.Reason != "needs_confirmation" {
		t.Fatalf("turn end = %+v, want reason=needs_confirmation", gated.TurnEnd)
	}
	if typed := fake.wrappedCommands(); len(typed) != 1 {
		t.Fatalf("policy=confirm executed the command anyway: typed = %d, want 1", len(typed))
	}

	// And it is recoverable: approving through the continue path runs it, so the
	// policy is a prompt rather than a dead end.
	approve := postAIChatContinue(t, server.URL, cookie, aiContinueRequest{
		SessionID: gated.SessionID, ActionID: gated.Confirmation.ActionID, Approved: true,
	})
	if approve.Status != 200 {
		t.Fatalf("continue after a policy prompt: status %d", approve.Status)
	}
	if typed := fake.wrappedCommands(); len(typed) != 2 {
		t.Fatalf("approving the policy prompt did not run the command: typed = %d, want 2", len(typed))
	}
}

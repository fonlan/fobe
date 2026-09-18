package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The per-turn controls of the autonomous loop (design.md §12.6).
//
// Why this is a separate unit from both the tool executor and the upstream
// adapter: the loop's limits are pure bookkeeping over "what has this turn
// already done", and keeping them free of HTTP and provider types is what makes
// them testable without a fake model. The three limits answer three different
// questions:
//
//   - turn budget: how many CHANGE-class commands may one turn run (a runaway
//     loop must not be able to do damage in a burst);
//   - repeat detection: is the model stuck re-issuing the same call (the
//     read-only path has no step cap at all, so this is its ONLY automatic
//     brake — see §20.14);
//   - turn deadline: how long a single user message may occupy the loop.

// aiTurnBudget accumulates the counters for one user message.
type aiTurnBudget struct {
	changeBudget int
	changes      int
	deadline     time.Time
	// calls fingerprints "tool + arguments" seen this turn. The count is kept so
	// the refusal can say how many times, which is what tells an operator
	// whether the model was looping or merely re-reading after a change.
	calls map[string]int
	// readStreak counts CONSECUTIVE read_terminal calls (§12.7.4). It is
	// separate from `calls` because read_terminal is exempt from repeat
	// detection: its fingerprint is constant (it takes no arguments), so the
	// repeat rule would refuse the second read of a turn — and polling the
	// screen is exactly what reading a terminal means.
	readStreak int
}

const (
	// aiTurnChangeBudget is the §12.6 per-turn budget for CHANGE-class tools.
	// Deliberately NOT the same knob as aiCommandLimit (per node, per minute):
	// this one bounds one turn, that one bounds the rate.
	aiTurnChangeBudget = 10
	// aiTurnDeadline bounds one user message end to end. Long enough for
	// "look, change, restart, verify" with a slow model, short enough that a
	// stuck loop cannot burn an hour of tokens.
	aiTurnDeadline = 10 * time.Minute
	// aiTurnReadLimit bounds CONSECUTIVE read_terminal calls in one turn
	// (§12.7.4). read_terminal is exempt from repeat detection — re-reading the
	// same screen is how you poll a terminal, and its argument-free fingerprint
	// would otherwise make the second read a "repeat" — so without this cap a
	// pathological model could read until the turn deadline. §12.6 named repeat
	// detection as the ONLY brake on read-only steps; this is its replacement.
	//
	// A THIRD axis, deliberately named apart from aiTurnChangeBudget (changes
	// per turn) and aiCommandLimit (commands per node per minute): §12.6 already
	// records one bug caused by conflating two same-sounding limits.
	aiTurnReadLimit = 10
)

func newAITurnBudget(now time.Time) *aiTurnBudget {
	return &aiTurnBudget{
		changeBudget: aiTurnChangeBudget,
		deadline:     now.Add(aiTurnDeadline),
		calls:        map[string]int{},
	}
}

// aiChangeTools are the §12.2 tools that mutate a probe. Everything else is
// read-only, and read-only calls are bounded by the read streak cap (§12.7.4)
// rather than by the change budget.
var aiChangeTools = map[string]bool{
	aiToolRunShell: true,
	aiToolSendKeys: true,
}

// aiReadOnlyTools is the positive whitelist of tools that do NOT mutate the
// node. An UNKNOWN tool counts as a change: if the tool set gains a mutating
// entry and someone forgets this table, the failure mode must be "the budget
// applies", not "the budget is bypassed by a new name".
var aiReadOnlyTools = map[string]bool{
	aiToolReadTerminal: true,
}

// aiToolIsChange reports whether a tool mutates the node.
func aiToolIsChange(tool string) bool { return !aiReadOnlyTools[tool] }

// Check reports whether the turn may continue, and why not when it may not.
// Called before each upstream iteration so a deadline is noticed before another
// (expensive) model call rather than in the middle of one.
func (b *aiTurnBudget) Check(now time.Time) (bool, string) {
	if now.After(b.deadline) {
		return false, "turn_timeout"
	}
	return true, ""
}

// NoteCall records a tool call and refuses a repeated identical one. A repeat is
// judged on tool + arguments: the same tool with different arguments is normal
// work (tail_logs on two different units), the same call twice is a loop.
func (b *aiTurnBudget) NoteCall(tool string, args json.RawMessage) (bool, string, int) {
	// Any call that is not read_terminal breaks the read streak: "read, change,
	// read again" is real work, not a polling loop (§12.7.4).
	if tool != aiToolReadTerminal {
		b.readStreak = 0
	}
	key := tool + "\x00" + normalizeToolArgs(args)
	b.calls[key]++
	count := b.calls[key]
	if count > 1 {
		return false, "repeat_call", count
	}
	return true, "", count
}

// NoteRead records one read_terminal call and reports whether the turn may
// continue. Exempt from repeat detection (see aiTurnBudget.readStreak), so this
// streak — not the fingerprint — is what bounds a read-only loop.
func (b *aiTurnBudget) NoteRead() (bool, string) {
	b.readStreak++
	if b.readStreak > aiTurnReadLimit {
		return false, "read_limit"
	}
	return true, ""
}

// NoteChange consumes one unit of the change budget. Callers must only call it
// for aiToolIsChange tools, and only once the call is actually going to execute
// (a call that needs confirmation has not run yet).
func (b *aiTurnBudget) NoteChange() (bool, string) {
	if b.changes >= b.changeBudget {
		return false, "turn_budget"
	}
	b.changes++
	return true, ""
}

func (b *aiTurnBudget) Changes() int { return b.changes }

// normalizeToolArgs makes the fingerprint stable across the formatting noise a
// model produces (whitespace, key order) so "the same call" is recognised even
// when the JSON is re-serialized differently.
func normalizeToolArgs(args json.RawMessage) string {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" {
		return "{}"
	}
	var asMap map[string]any
	if err := json.Unmarshal([]byte(trimmed), &asMap); err != nil {
		// Not an object (bad JSON from the model): fingerprint the literal bytes
		// after whitespace squashing. Refusing an unparseable repeat is still
		// the right call.
		return strings.Join(strings.Fields(trimmed), " ")
	}
	canonical, err := json.Marshal(sortedAny(asMap))
	if err != nil {
		return strings.Join(strings.Fields(trimmed), " ")
	}
	return string(canonical)
}

// sortedAny recursively rebuilds maps with sorted keys. encoding/json already
// sorts map keys when marshalling, so this exists to normalize NESTED values
// into comparable forms (numbers arriving as float64 is fine: same value, same
// encoding).
func sortedAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[key] = sortedAny(inner)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = sortedAny(inner)
		}
		return out
	default:
		return value
	}
}

// --- SSE transport ---

// aiSSEWriter emits named server-sent events with an explicit flush per event.
//
// X-Accel-Buffering: no is set on purpose. §12.6 puts the reverse proxy outside
// the repo (the operator brings their own nginx), and a buffered SSE response
// looks exactly like "streaming is broken" — the events all arrive at the end.
// The header makes nginx skip buffering for this response, so the common
// configuration works without the operator reading the README first; the README
// still documents proxy_buffering off for the general case.
type aiSSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newAISSEWriter(w http.ResponseWriter) (*aiSSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// A handler that cannot flush cannot stream; failing loudly beats
		// sending an event stream that only arrives when the turn ends.
		return nil, fmt.Errorf("response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &aiSSEWriter{w: w, flusher: flusher}, nil
}

// Send writes one event. The payload is JSON so a client never has to parse
// prose, and the event name lets the panel route deltas without inspecting the
// body.
func (s *aiSSEWriter) Send(event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", event, err)
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, body); err != nil {
		return fmt.Errorf("write %s event: %w", event, err)
	}
	s.flusher.Flush()
	return nil
}

// SSE event names (§12.6). Kept as constants so the frontend's switch and the
// server's sends cannot drift apart silently.
const (
	aiEventText         = "text_delta"
	aiEventThinking     = "thinking_delta"
	aiEventToolCall     = "tool_call"
	aiEventToolResult   = "tool_result"
	aiEventNeedsConfirm = "needs_confirmation"
	aiEventTurnEnd      = "turn_end"
	aiEventError        = "error"
)

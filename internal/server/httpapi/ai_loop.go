package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/server/aiprotocol"
	"github.com/fonlan/fobe/internal/server/modelsdev"
	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

// The autonomous tool loop (design.md §12.6).
//
// One user message drives a server-side loop: ask the model → execute whatever
// tools it requested → feed the results back → ask again, until the model stops
// asking, a limit trips, or the operator presses stop. The loop is where the
// three §12.6 controls live (per-turn change budget, repeat detection, ten
// minute deadline — all in ai_turn.go).
//
// Everything protocol-specific is the aiprotocol package's job: this file only
// decides WHAT to send and WHEN to stop, never how a dialect spells it.

// aiMessageEnvelope is the JSON stored in ai_messages.blocks.
//
// `content` (a plain-text column) is what a human reads; the envelope is what
// the NEXT loop step must replay verbatim. Both exist because they answer
// different questions: the text is for the transcript, the envelope is for
// correctness — Anthropic rejects the next step of a tool loop whose assistant
// turn lost its thinking block, and Responses loses the reasoning item.
type aiMessageEnvelope struct {
	// Raw is the assistant turn's native blocks, exactly as the upstream
	// returned them (nil for dialects without such a concept).
	Raw json.RawMessage `json:"raw,omitempty"`
	// ToolCalls are the calls the model requested in this turn.
	ToolCalls []aiprotocol.ToolCall `json:"tool_calls,omitempty"`
	// ToolResults are the outcomes of calls the loop executed, attached to the
	// turn that reports them back.
	ToolResults []aiprotocol.ToolResult `json:"tool_results,omitempty"`
}

// aiProtocolTools converts the §12.2 tool set into the adapter's shape.
func aiProtocolTools() []aiprotocol.ToolSpec {
	specs := aiToolsSpec()
	out := make([]aiprotocol.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		schema, err := json.Marshal(spec.Function.Parameters)
		if err != nil {
			continue
		}
		out = append(out, aiprotocol.ToolSpec{
			Name:        spec.Function.Name,
			Description: spec.Function.Description,
			Schema:      schema,
		})
	}
	return out
}

// aiLoopMessages rebuilds the conversation from the store, preserving every
// turn's native blocks.
//
// It does NOT include a system message: the shared context travels in
// Request.System, and the adapters place it where their dialect wants it
// (messages[0] for OpenAI completions, a top-level field for the other two).
// Prepending one here as well sent the whole context TWICE — visible in a live
// run as roles=[system, system, user, ...] — which doubles the prompt cost and
// gives the model two copies of its instructions to reconcile.
func (s *Server) aiLoopMessages(session *store.AISession, system, userMessage string) ([]aiprotocol.Message, error) {
	messages := make([]aiprotocol.Message, 0, aiLoopHistoryLimit+1)
	history, err := s.Store.ListAIMessages(session.ID, aiLoopHistoryLimit)
	if err != nil {
		return nil, err
	}
	for _, item := range history {
		// The `turn_end` marker is panel metadata. Sending it upstream would put
		// an unknown role in the conversation — Anthropic rejects unknown roles
		// outright, and the OpenAI dialects would feed the model a message that
		// is not part of the dialogue.
		if item.Role == aiTurnEndRole {
			continue
		}
		message := aiprotocol.Message{Role: item.Role, Text: item.Content}
		if strings.TrimSpace(item.Blocks) != "" {
			var envelope aiMessageEnvelope
			if err := json.Unmarshal([]byte(item.Blocks), &envelope); err == nil {
				message.Raw = envelope.Raw
				message.ToolCalls = envelope.ToolCalls
				message.ToolResults = envelope.ToolResults
			}
			// An unparseable envelope degrades to plain text on purpose: the
			// transcript is still usable, and refusing the whole request over
			// one damaged history row would strand the operator.
		}
		messages = append(messages, message)
	}
	if userMessage != "" {
		messages = append(messages, aiprotocol.Message{Role: "user", Text: userMessage})
	}
	return messages, nil
}

const (
	// aiLoopHistoryLimit is how many stored messages feed the model. Bounded
	// because every tool result carries command output, and an unbounded
	// transcript is both a cost and a context-length failure waiting to happen.
	aiLoopHistoryLimit = 60
	// aiToolResultWait bounds how long one loop step waits for a queued command
	// to report back before telling the model "queued, no result yet" (the same
	// 8s ceiling §12.1 puts on tail_logs).
	aiToolResultWait = 8 * time.Second
	// aiToolResultBytes caps what one tool result contributes to the context.
	// Without a cap, one `run_shell` that dumps a log file eats the window and
	// evicts the conversation the operator cares about.
	aiToolResultBytes = 8 << 10
)

// aiReasoning builds the §12.5 thinking request. The budgets are the package's
// built-in constants (low 2k / medium 8k / high 16k): the operator asked for
// experience values rather than a settings surface, so there is no setting to
// read and no way for a hand-edited value to put the mapping out of range.
// (The LEVEL itself comes from the request or, when the picker is unset, from
// ai.default_reasoning — see effectiveAIReasoning.)
func (s *Server) aiReasoning(resolved *aiResolved, level string) aiprotocol.Reasoning {
	reasoning := aiprotocol.Reasoning{Level: level, Budgets: modelsdev.DefaultReasoningBudgets}
	// Only the model row knows how "off" is spelled on the wire (§12.5); the
	// adapter cannot tell from the level alone.
	reasoning.OffStyle = resolved.Model.ReasoningOffStyle
	return reasoning
}

// effectiveAIReasoning fills the panel's "默认(不指定)" with the stored
// ai.default_reasoning. A stored level the resolved model does not expose is
// IGNORED rather than rejected: the default is configured against the default
// pair, but the panel can send any pair, and a hand-picked model that lacks
// the level must not 400 the whole turn — "unset" is the honest fallback.
func (s *Server) effectiveAIReasoning(resolved *aiResolved, requested string) string {
	if requested != "" {
		return requested
	}
	stored, _ := s.Store.GetSetting("ai.default_reasoning")
	stored = strings.TrimSpace(stored)
	if stored == "" || validateAIReasoningLevel(resolved, stored) != nil {
		return ""
	}
	return stored
}

// aiEventSession tells the frontend which conversation it is talking to before
// the first delta arrives, so a reload mid-turn can still name the session.
const aiEventSession = "session"

// runAITurn drives the loop and streams it. It never writes a JSON error: the
// SSE response has already started, so failures travel as an `error` event.
func (s *Server) runAITurn(ctx context.Context, sse *aiSSEWriter, r *http.Request, session *store.AISession, resolved *aiResolved, req aiChatRequest) {
	budget := newAITurnBudget(time.Now())
	if err := sse.Send(aiEventSession, map[string]any{
		"session_id": session.ID, "provider_id": resolved.Provider.ID,
		"model_id": resolved.Model.ID, "protocol": resolved.Provider.Protocol,
	}); err != nil {
		return
	}

	// §12.1's "附带日志" injection died with the tail_logs tool (§12.7.6).
	system, err := s.buildAIContext(session.ID)
	if err != nil {
		_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
		return
	}
	// §12.7.5: the terminal the tools will act on is the one THIS request
	// carries. The session row's copy was frozen when the conversation was
	// created, and a page reload mints a new terminal session id — so when the
	// two differ the model must be told the old screen is gone, or it will keep
	// reasoning from the transcript's stale picture of the terminal.
	if note := aiTerminalRebindNote(session, req.TerminalSessionID); note != "" {
		system += "\n\n" + note
		// Rebinding keeps the note above a one-time event instead of a line the
		// model re-reads every turn (§12.7.5).
		if err := s.Store.SetAISessionTerminal(session.ID, req.TerminalSessionID); err != nil {
			s.Log.Warn("rebind ai terminal session", "err", err)
		}
	}
	// The tool handlers keep their §12.2 signature, so the live binding travels
	// on the request context rather than being threaded through every call.
	r = r.WithContext(withAITerminalSession(r.Context(), req.TerminalSessionID))
	if req.Message != "" {
		if err := s.Store.InsertAIMessage(session.ID, "user", req.Message); err != nil {
			_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
			return
		}
	}
	if err := s.Store.TouchAISession(session.ID); err != nil {
		s.Log.Warn("touch ai session", "err", err)
	}

	client := s.AIHTTPClient
	if client == nil {
		client = &http.Client{}
	}

	// The loop. Each iteration is ONE upstream call; the model decides whether
	// another follows by asking for tools or not (§12.6: no step cap on
	// read-only work, so repeat detection and the deadline are the brakes).
	for {
		if ok, reason := budget.Check(time.Now()); !ok {
			s.finishAITurn(sse, session.ID, reason, budget.Changes(), nil)
			return
		}
		messages, err := s.aiLoopMessages(session, system, "")
		if err != nil {
			_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
			return
		}

		var (
			text      strings.Builder
			toolCalls []aiprotocol.ToolCall
			rawBlocks json.RawMessage
		)
		streamErr := aiprotocol.Stream(ctx, client, aiprotocol.Request{
			BaseURL:         resolved.Provider.BaseURL,
			APIKey:          resolved.APIKey,
			Headers:         resolved.Headers,
			Protocol:        resolved.Provider.Protocol,
			Model:           resolved.Model.ID,
			System:          system,
			Messages:        messages,
			Tools:           aiProtocolTools(),
			Reasoning:       s.aiReasoning(resolved, req.ReasoningLevel),
			MaxOutputTokens: resolved.Model.MaxOutputTokens,
		}, func(event aiprotocol.Event) error {
			switch event.Kind {
			case aiprotocol.KindText:
				text.WriteString(event.Text)
				return sse.Send(aiEventText, map[string]string{"text": event.Text})
			case aiprotocol.KindThinking:
				return sse.Send(aiEventThinking, map[string]string{"text": event.Text})
			case aiprotocol.KindToolCall:
				if event.ToolCall != nil {
					toolCalls = append(toolCalls, *event.ToolCall)
				}
			case aiprotocol.KindDone:
				rawBlocks = event.Raw
			}
			return nil
		})

		// Whatever arrived before the failure is a matter of record: §12.6 says
		// a half turn is persisted, not discarded (the operator saw those
		// deltas; pretending they never happened makes the transcript lie).
		if err := s.persistAssistantTurn(session.ID, text.String(), rawBlocks, toolCalls); err != nil {
			_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
			return
		}
		if streamErr != nil {
			if errors.Is(streamErr, context.Canceled) {
				// The operator pressed stop. Nothing to stream (the frontend
				// initiated it) but the reason still belongs in the transcript:
				// the half turn is already stored and otherwise looks like a
				// model that simply stopped mid-sentence.
				s.finishAITurn(sse, session.ID, "stopped", budget.Changes(), nil)
				return
			}
			s.emitAIUpstreamError(sse, session.ID, streamErr)
			return
		}
		if len(toolCalls) == 0 {
			// Some OpenAI-compatible gateways cannot do native tool calling and
			// emit the call as JSON in the message body instead. The old
			// single-shot path handled that; dropping it here would silently
			// remove the assistant's ability to act on those gateways.
			if name, args, ok := parseAIContentTool(text.String()); ok {
				callID, err := security.RandomToken(8)
				if err != nil {
					_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
					return
				}
				toolCalls = []aiprotocol.ToolCall{{ID: "content-" + callID, Name: name, Arguments: args}}
				// Re-persist with the recovered call so the replay is faithful:
				// the transcript must show what the loop actually did.
				if err := s.persistAssistantTurn(session.ID, text.String(), rawBlocks, toolCalls); err != nil {
					_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
					return
				}
			} else {
				s.finishAITurn(sse, session.ID, "completed", budget.Changes(), nil)
				return
			}
		}

		// Execute this round's calls. Read-only calls may run in parallel; the
		// mutating ones are serialized because two restarts of the same service
		// racing each other is exactly the kind of damage the sequencing rule
		// in §12.6 exists to prevent.
		results := make([]aiprotocol.ToolResult, 0, len(toolCalls))
		for _, call := range toolCalls {
			// Two different brakes for two different failure modes (§12.7.4).
			// read_terminal is EXEMPT from repeat detection: its fingerprint is
			// constant (it takes no arguments), so the repeat rule would refuse
			// the second read of any turn — and re-reading a screen is what
			// reading a terminal *means*. What bounds it instead is a cap on
			// CONSECUTIVE reads, reset by any other tool call.
			if call.Name == aiToolReadTerminal {
				if ok, reason := budget.NoteRead(); !ok {
					results = append(results, aiprotocol.ToolResult{
						CallID: call.ID, IsError: true,
						Content: fmt.Sprintf("refused: %s (%d consecutive reads of the terminal with nothing in between)", reason, aiTurnReadLimit),
					})
					_ = sse.Send(aiEventToolResult, map[string]any{
						"id": call.ID, "name": call.Name, "status": "refused", "reason": reason,
					})
					if err := s.persistToolResults(session.ID, results); err != nil {
						_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
					}
					s.finishAITurn(sse, session.ID, reason, budget.Changes(), nil)
					return
				}
			} else {
				allowed, reason, _ := budget.NoteCall(call.Name, call.Arguments)
				if !allowed {
					results = append(results, aiprotocol.ToolResult{
						CallID: call.ID, IsError: true,
						Content: fmt.Sprintf("refused: %s (the same call was already made this turn)", reason),
					})
					_ = sse.Send(aiEventToolResult, map[string]any{
						"id": call.ID, "name": call.Name, "status": "refused", "reason": reason,
					})
					// Record the refusal before ending: the assistant turn that
					// asked for this call is already persisted, and Anthropic
					// rejects the NEXT request of a session whose tool_use has no
					// tool_result. Leaving it dangling only breaks the following
					// message, far from the cause.
					if err := s.persistToolResults(session.ID, results); err != nil {
						_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
					}
					s.finishAITurn(sse, session.ID, reason, budget.Changes(), nil)
					return
				}
			}
			if aiToolIsChange(call.Name) {
				if ok, reason := budget.NoteChange(); !ok {
					results = append(results, aiprotocol.ToolResult{
						CallID: call.ID, IsError: true,
						Content: fmt.Sprintf("refused: %s (per-turn budget of %d change commands reached)", reason, aiTurnChangeBudget),
					})
					_ = sse.Send(aiEventToolResult, map[string]any{
						"id": call.ID, "name": call.Name, "status": "refused", "reason": reason,
					})
					if err := s.persistToolResults(session.ID, results); err != nil {
						_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
					}
					s.finishAITurn(sse, session.ID, reason, budget.Changes(), nil)
					return
				}
			}

			outcome := s.executeAIToolCall(r, session, call)
			if outcome.needsConfirmation {
				// §12.6: the stream ENDS here and the frontend re-enters through
				// the continue endpoint once the operator decides — that is how
				// a confirmation can take minutes without holding a connection
				// open through the operator's own nginx.
				if err := sse.Send(aiEventNeedsConfirm, map[string]any{
					"id": call.ID, "name": call.Name, "action_id": outcome.actionID,
					"command": outcome.command, "reason": outcome.reason, "risk": outcome.risk,
				}); err != nil {
					return
				}
				s.finishAITurn(sse, session.ID, "needs_confirmation", budget.Changes(),
					map[string]any{"action_id": outcome.actionID})
				return
			}

			_ = sse.Send(aiEventToolResult, map[string]any{
				"id": call.ID, "name": call.Name, "status": outcome.status,
				"command": outcome.command, "exit_code": outcome.exitCode,
			})
			results = append(results, aiprotocol.ToolResult{
				CallID: call.ID, IsError: outcome.isError, Content: outcome.content,
			})
		}
		if err := s.persistToolResults(session.ID, results); err != nil {
			_ = sse.Send(aiEventError, map[string]any{"code": "internal"})
			return
		}
	}
}

// finishAITurn RECORDS and emits the end of a turn. `reason` is a machine value
// the panel can explain ("completed", "turn_budget", "repeat_call",
// "turn_timeout"); a bare "done" would make a budget stop look like a finished
// answer.
//
// The reason is persisted as a `turn_end` marker row, not just streamed: the
// reason lives in no other place, so without the row a reloaded conversation
// shows a truncated turn with no explanation — the operator sees half an answer
// and no way to learn whether the model stopped, the budget tripped or the
// upstream failed. The marker carries no conversational content and is filtered
// out of what is sent upstream (see aiLoopMessages).
func (s *Server) finishAITurn(sse *aiSSEWriter, sessionID, reason string, changes int, extra map[string]any) {
	payload := map[string]any{"changes": changes}
	for key, value := range extra {
		payload[key] = value
	}
	if err := s.persistTurnEnd(sessionID, reason, payload); err != nil {
		s.Log.Warn("persist turn end", "reason", reason, "err", err)
	}
	event := map[string]any{"session_id": sessionID, "reason": reason, "changes": changes}
	for key, value := range extra {
		event[key] = value
	}
	_ = sse.Send(aiEventTurnEnd, event)
}

// aiTurnEndRole marks the persisted reason for a finished turn. It is a row in
// ai_messages because ordering is the whole point (which turn stopped, and when
// relative to the messages around it), and a column on the last message of the
// turn could not express "the last row may be an assistant or a tool row".
const aiTurnEndRole = "turn_end"

func (s *Server) persistTurnEnd(sessionID, reason string, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.Store.InsertAIMessageBlocks(sessionID, aiTurnEndRole, reason, string(encoded))
}

// emitAIUpstreamError reports a failed upstream call. The adapter already
// redacted the API key out of the message, so the text is safe to forward — and
// forwarding it is the only way an operator learns that (say) the model name is
// wrong.
func (s *Server) emitAIUpstreamError(sse *aiSSEWriter, sessionID string, err error) {
	s.Log.Warn("ai upstream stream", "err", err)
	_ = sse.Send(aiEventError, map[string]any{"code": "ai_upstream_error", "message": err.Error()})
	s.finishAITurn(sse, sessionID, "upstream_error", 0, nil)
}

// persistAssistantTurn stores one assistant turn. A turn that produced neither
// text, nor tool calls, nor native blocks is NOT stored: it is not a message.
// That happens whenever the upstream fails before the first delta, or the
// operator stops immediately. The panel already hides such a row (it drops
// assistant rows with no text and no thinking), so the row buys nothing — while
// costing everything on the wire: `openai-completions` has no native-block
// concept to rebuild it from, so it used to go out as a bare
// {"role":"assistant"}, which gateways reject with "content or tool_calls must
// be set" — 400ing every LATER request of the session, far from the cause.
func (s *Server) persistAssistantTurn(sessionID, text string, raw json.RawMessage, calls []aiprotocol.ToolCall) error {
	if strings.TrimSpace(text) == "" && len(calls) == 0 && len(raw) == 0 {
		return nil
	}
	envelope := aiMessageEnvelope{Raw: raw, ToolCalls: calls}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return s.Store.InsertAIMessageBlocks(sessionID, "assistant", text, string(encoded))
}

func (s *Server) persistToolResults(sessionID string, results []aiprotocol.ToolResult) error {
	encoded, err := json.Marshal(aiMessageEnvelope{ToolResults: results})
	if err != nil {
		return err
	}
	summary := make([]string, 0, len(results))
	for _, result := range results {
		summary = append(summary, result.Content)
	}
	return s.Store.InsertAIMessageBlocks(sessionID, "tool", strings.Join(summary, "\n"), string(encoded))
}

// aiToolOutcome is one executed call, flattened for both the stream and the
// model's next prompt.
type aiToolOutcome struct {
	needsConfirmation bool
	actionID          string
	status            string
	command           string
	reason            string
	risk              string
	exitCode          *int
	isError           bool
	content           string
}

// executeAIToolCall runs one tool call through the existing §12.2 handlers and
// flattens the result map into something both a human and the model can read.
func (s *Server) executeAIToolCall(r *http.Request, session *store.AISession, call aiprotocol.ToolCall) aiToolOutcome {
	action := &aiToolAction{Name: call.Name, Args: call.Arguments}
	if len(action.Args) == 0 {
		action.Args = json.RawMessage("{}")
	}
	toolCall := map[string]any{"name": call.Name, "id": call.ID}
	result := s.handleAIAction(r, session, action, toolCall)

	outcome := aiToolOutcome{status: stringField(result, "status")}
	if strings.EqualFold(outcome.status, "needs_confirmation") {
		outcome.needsConfirmation = true
		outcome.actionID = stringField(result, "action_id")
		outcome.reason = stringField(result, "reason")
		outcome.risk = stringField(result, "risk")
		outcome.command = aiToolCommand(action)
		return outcome
	}

	// Since §12.7 every tool answers inline: run_shell types into the terminal
	// and reads the screen back (its exit status comes from the sentinel, not
	// from a commands-table row), send_keys reports what it typed, read_terminal
	// hands over the window. There is no longer a queued-command path here —
	// nothing the AI can call enqueues a command any more.
	outcome.isError = outcome.status != "ok" && outcome.status != "read"
	if code, ok := result["exit_code"].(int); ok {
		// Unknown statuses (a timed-out or marker-less command) simply carry no
		// exit_code, which is why this is a presence check rather than a
		// sentinel value.
		outcome.exitCode = &code
		outcome.isError = code != 0
	}
	outcome.content = truncateAIBytes(stringField(result, "result"), aiToolResultBytes)
	if outcome.command == "" {
		outcome.command = aiToolCommand(action)
	}
	if outcome.content == "" {
		encoded, _ := json.Marshal(result)
		outcome.content = truncateAIBytes(string(encoded), aiToolResultBytes)
	}
	outcome.content = fmt.Sprintf("status=%s exit_code=%s\n%s", outcome.status, formatExitCode(outcome.exitCode), outcome.content)
	return outcome
}

func stringField(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}

func formatExitCode(code *int) string {
	if code == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *code)
}

// aiToolCommand extracts the command text a confirmation dialog shows. Showing
// the raw arguments instead would bury the one line the operator must judge.
func aiToolCommand(action *aiToolAction) string {
	var parsed struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(action.Args, &parsed); err != nil {
		return ""
	}
	return parsed.Command
}

// --- resuming after a confirmation (§12.6) ---

type aiContinueRequest struct {
	SessionID string `json:"session_id"`
	ActionID  string `json:"action_id"`
	Approved  bool   `json:"approved"`
	// TerminalSessionID is accepted because the panel sends it on every request
	// that can reach a terminal tool, but for a CONFIRMED action it is
	// deliberately not authoritative: the id that matters is the one captured in
	// the pending payload when the operator was shown the command, so an
	// approval cannot silently execute against a different terminal than the one
	// it was approved for (§12.7.5).
	TerminalSessionID string `json:"terminal_session_id,omitempty"`
	ReasoningLevel    string `json:"reasoning_level,omitempty"`
}

// handleAIChatContinue re-enters the loop after the operator answered a
// confirmation prompt.
//
// This is the other half of "the stream ends at a confirmation": a decision can
// take minutes, and holding one SSE connection open across it would depend on
// the operator's own nginx being configured for long-lived responses. The turn
// is reconstructed from the transcript, so the resumed request is identical to
// the paused one except for the tool result it now carries.
func (s *Server) handleAIChatContinue(w http.ResponseWriter, r *http.Request) {
	var req aiContinueRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	action, err := s.Store.GetAIPendingAction(req.ActionID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if action.Status != "pending" {
		// Answering twice must not execute twice: the decision is already made.
		writeErr(w, http.StatusConflict, "ai_action_not_pending")
		return
	}
	session, err := s.Store.GetAISession(action.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusBadRequest, "ai_session_invalid")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if req.SessionID != "" && req.SessionID != session.ID {
		writeErr(w, http.StatusBadRequest, "ai_session_invalid")
		return
	}
	if !s.aiConfigured() {
		writeErr(w, http.StatusServiceUnavailable, "ai_not_configured")
		return
	}
	resolved, err := s.resolveAIModel(session.ProviderID, session.ModelID)
	if err != nil {
		writeAIResolveErr(w, err)
		return
	}
	// Same fallback rule as a fresh turn: an unset picker level takes the
	// stored default, validated against the session's own model.
	req.ReasoningLevel = s.effectiveAIReasoning(resolved, req.ReasoningLevel)
	if err := validateAIReasoningLevel(resolved, req.ReasoningLevel); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_reasoning_level")
		return
	}

	// The decision becomes a tool result BEFORE the stream starts: a failure to
	// record it is a plain JSON error the operator can retry, whereas failing
	// after the first delta would leave the transcript claiming a decision the
	// panel never wrote down.
	result, ok := s.resolveAIConfirmation(r, session, action, req.Approved)
	if !ok {
		writeErr(w, http.StatusConflict, "ai_execution_blocked")
		return
	}
	if err := s.persistToolResults(session.ID, []aiprotocol.ToolResult{result}); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	sse, err := newAISSEWriter(w)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// An empty message: the loop continues from the transcript, it does not
	// pretend the operator typed something.
	s.runAITurn(r.Context(), sse, r, session, resolved, aiChatRequest{ReasoningLevel: req.ReasoningLevel})
}

// resolveAIConfirmation applies the operator's decision and reports it as a tool
// result. A rejected action is a NORMAL outcome: the model is told so and gets
// to explain or propose something else, which is far more useful than an error.
func (s *Server) resolveAIConfirmation(r *http.Request, session *store.AISession, action *store.AIPendingAction, approved bool) (aiprotocol.ToolResult, bool) {
	result := aiprotocol.ToolResult{CallID: action.CallID}
	if !approved {
		if err := s.Store.RejectAIPendingAction(action.ID, nowUnix()); err != nil {
			s.Log.Warn("reject ai action", "err", err)
			return result, false
		}
		_ = s.Store.InsertAudit(&store.AuditEntry{
			Actor: "panel", NodeID: action.NodeID, Action: "ai_action_rejected",
			Command: action.Payload, Reason: action.Reason, Risk: action.Risk,
			SourceIP: s.Trust.RealIP(r), AISessionID: action.SessionID,
		})
		result.IsError = true
		result.Content = "rejected by the operator: the action was NOT executed"
		return result, true
	}

	// send_keys is NOT a command: it writes bytes into the operator's PTY
	// through the hub. Letting it fall through to the command queue would
	// enqueue a kind no agent handles, and the empty result would read to the
	// model as success.
	//
	// It carries its terminal session id in the payload because this runs on a
	// LATER request (the continue endpoint) — by then the turn's live binding is
	// gone, and a stale id must surface as "not attached" rather than type into
	// a session that no longer exists.
	if action.Kind == aiToolSendKeys {
		var payload aiSendKeysPayload
		if err := json.Unmarshal([]byte(action.Payload), &payload); err != nil || payload.Data == "" {
			result.IsError = true
			result.Content = "invalid send_keys payload"
			return result, true
		}
		if !s.Hub.SendTerminalKeys(action.NodeID, payload.TerminalSessionID, payload.Data) {
			result.IsError = true
			result.Content = "the terminal is not attached to a browser right now; nothing was sent"
			return result, true
		}
		s.Store.InsertAudit(&store.AuditEntry{
			Actor: "ai", NodeID: action.NodeID, Action: "ai_terminal_keys",
			Command: aiRenderKeys(payload.Data), Reason: action.Reason, Risk: action.Risk,
			SourceIP: s.Trust.RealIP(r), AISessionID: action.SessionID,
		})
		if err := s.Store.ConfirmAIPendingAction(action.ID, "", nowUnix()); err != nil {
			return result, false
		}
		result.Content = "sent keys: " + aiRenderKeys(payload.Data)
		return result, true
	}

	// run_shell executes in the operator's terminal now (§12.7.3), so a
	// confirmed command takes the SAME path as an unconfirmed one — only the
	// operator's approval was missing. It must not fall through to the command
	// queue, which would run it on the agent instead of on the screen the
	// operator just approved it for.
	if action.Kind == aiToolRunShell {
		var payload aiRunShellPayload
		if err := json.Unmarshal([]byte(action.Payload), &payload); err != nil || payload.Command == "" {
			result.IsError = true
			result.Content = "invalid run_shell payload"
			return result, true
		}
		if allowed, gateReason := s.aiTerminalGate(action.NodeID); !allowed {
			result.IsError = true
			result.Content = "refused: " + gateReason
			return result, true
		}
		io := newAITerminalIO(s.Hub, r.Context(), action.NodeID, payload.TerminalSessionID)
		shell, err := s.execAIShell(r.Context(), io, payload.Command)
		if err != nil {
			result.IsError = true
			result.Content = "could not run the command in the terminal: " + err.Error()
			return result, true
		}
		s.Store.InsertAudit(&store.AuditEntry{
			Actor: "ai", NodeID: action.NodeID, Action: store.AuditAITerminalRun,
			Command: truncateAIBytes(shell.typed, 2048), Reason: action.Reason, Risk: action.Risk,
			SourceIP: s.Trust.RealIP(r), AISessionID: action.SessionID,
		})
		if err := s.Store.ConfirmAIPendingAction(action.ID, "", nowUnix()); err != nil {
			return result, false
		}
		result.IsError = shell.exitCode != nil && *shell.exitCode != 0
		result.Content = aiShellToolContent(shell)
		return result, true
	}

	// Fail closed: an unrecognised pending kind used to be queued as a command,
	// which after the §12.7 rewrite means asking the agent to execute something
	// that was never designed to reach it. Refusing is the honest answer.
	result.IsError = true
	result.Content = "unsupported confirmed action: " + action.Kind
	return result, true
}

// aiSessionHistoryView is the transcript the terminal panel restores on mount.
//
// It exists because the assistant's turns are persisted (§12.6) and the panel
// has no other way to show them: without this endpoint a page reload loses the
// reasoning blocks the transcript is supposed to replay, and the "collapsed
// thinking" affordance would only ever work for the live turn.
type aiSessionHistoryView struct {
	SessionID  string                 `json:"session_id"`
	NodeID     string                 `json:"node_id"`
	ProviderID string                 `json:"provider_id,omitempty"`
	ModelID    string                 `json:"model_id,omitempty"`
	Protocol   string                 `json:"protocol,omitempty"`
	Messages   []aiSessionMessageView `json:"messages"`
}

type aiSessionMessageView struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Raw/工具块原样回传：面板只负责渲染，不解析协议细节（§12.5 keeps the
	// opaque blocks opaque outside the adapter).
	Raw json.RawMessage `json:"raw,omitempty"`
	// Thinking is the concatenated reasoning text, extracted SERVER-side so the
	// panel does not have to know which dialect put it where.
	Thinking string `json:"thinking,omitempty"`
	// TurnReason/Changes are set on the `turn_end` marker rows (§12.6). The
	// panel renders them where the live stream shows its turn-end chip, which is
	// how a reloaded conversation explains an interrupted turn.
	TurnReason string `json:"turn_reason,omitempty"`
	Changes    int    `json:"changes,omitempty"`
}

func (s *Server) handleGetAISession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, err := s.Store.GetAISession(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	messages, err := s.Store.ListAIMessages(id, aiLoopHistoryLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	view := aiSessionHistoryView{
		SessionID: session.ID, NodeID: session.NodeID,
		ProviderID: session.ProviderID, ModelID: session.ModelID, Protocol: session.Protocol,
		Messages: []aiSessionMessageView{},
	}
	for _, item := range messages {
		entry := aiSessionMessageView{Role: item.Role, Content: item.Content}
		if strings.TrimSpace(item.Blocks) != "" {
			if item.Role == aiTurnEndRole {
				var payload struct {
					Changes int `json:"changes"`
				}
				if err := json.Unmarshal([]byte(item.Blocks), &payload); err == nil {
					entry.Changes = payload.Changes
				}
				entry.TurnReason = item.Content
				entry.Content = ""
			} else {
				var envelope aiMessageEnvelope
				if err := json.Unmarshal([]byte(item.Blocks), &envelope); err == nil {
					entry.Raw = envelope.Raw
					entry.Thinking = aiThinkingText(envelope.Raw)
				}
			}
		}
		view.Messages = append(view.Messages, entry)
	}
	writeJSON(w, http.StatusOK, view)
}

// aiThinkingText pulls reasoning text out of native blocks for display. It
// accepts both dialects' shapes and returns "" for messages without any — an
// unknown shape degrades to "no thinking to show", never to an error, because
// the transcript is read-only history.
func aiThinkingText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var blocks []struct {
		Type     string `json:"type"`
		Thinking string `json:"thinking"`
		Text     string `json:"text"`
		Summary  []struct {
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	out := []string{}
	for _, block := range blocks {
		if block.Thinking != "" {
			out = append(out, block.Thinking)
			continue
		}
		for _, summary := range block.Summary {
			if summary.Text != "" {
				out = append(out, summary.Text)
			}
		}
	}
	return strings.Join(out, "\n")
}

// closeDanglingToolCalls repairs a transcript whose last assistant turn asked
// for tools that never received a result.
//
// That happens whenever a turn is interrupted before its results are recorded:
// the operator answers a confirmation prompt by typing a NEW message instead of
// using /continue, or a refusal/stop path returned early. Anthropic pairs
// tool_use/tool_result strictly, so the dangling call must be closed with an
// explicit "not executed" result before the next turn is sent — otherwise the
// operator's next message fails with a 400 naming a block they never wrote.
func (s *Server) closeDanglingToolCalls(sessionID string) error {
	messages, err := s.Store.ListAIMessages(sessionID, aiLoopHistoryLimit)
	if err != nil {
		return err
	}
	answered := map[string]bool{}
	var open []aiprotocol.ToolCall
	for _, item := range messages {
		if strings.TrimSpace(item.Blocks) == "" {
			continue
		}
		var envelope aiMessageEnvelope
		if err := json.Unmarshal([]byte(item.Blocks), &envelope); err != nil {
			continue
		}
		if item.Role == "tool" {
			for _, result := range envelope.ToolResults {
				answered[result.CallID] = true
			}
			continue
		}
		// A newer assistant turn supersedes whatever was open before it.
		open = append(open[:0], envelope.ToolCalls...)
	}
	pending := make([]aiprotocol.ToolResult, 0, len(open))
	for _, call := range open {
		if answered[call.ID] {
			continue
		}
		pending = append(pending, aiprotocol.ToolResult{
			CallID: call.ID, IsError: true,
			Content: "not executed: the turn ended before this call produced a result (the operator moved on instead of answering its confirmation)",
		})
	}
	if len(pending) == 0 {
		return nil
	}
	return s.persistToolResults(sessionID, pending)
}

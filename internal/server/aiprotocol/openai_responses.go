package aiprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// OpenAI Responses (design.md §12.5). Structured where completions is flat: the
// system prompt is the top-level `instructions`, tool definitions are flat, the
// conversation is a list of typed items, and reasoning output is an opaque
// `reasoning` item that must be sent back on the next request when the API is
// used statelessly — which is exactly how this panel uses it.

type responsesRequest struct {
	Model           string                  `json:"model"`
	Instructions    string                  `json:"instructions,omitempty"`
	Input           []json.RawMessage       `json:"input"`
	Tools           []responsesTool         `json:"tools,omitempty"`
	Stream          bool                    `json:"stream"`
	Store           bool                    `json:"store"`
	Include         []string                `json:"include,omitempty"`
	MaxOutputTokens int                     `json:"max_output_tokens,omitempty"`
	Reasoning       *responsesReasoningBody `json:"reasoning,omitempty"`
}

type responsesReasoningBody struct {
	Effort string `json:"effort,omitempty"`
	// Summary is what makes the model stream its reasoning at all
	// (response.reasoning_summary_text.delta). Without it the panel has nothing
	// to show in the thinking panel and nothing to store (§12.5).
	Summary string `json:"summary,omitempty"`
	// Enabled is the toggle spelling (OffStyle=="disabled") used by gateways
	// that expose thinking as a plain on/off switch.
	Enabled *bool `json:"enabled,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesMessageItem struct {
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Content []responsesContent `json:"content"`
}

type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesFunctionCallItem struct {
	Type string `json:"type"`
	// CallID (not the item id) is what function_call_output must quote back.
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Arguments is a JSON *string* in this dialect too.
	Arguments string `json:"arguments"`
}

type responsesFunctionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// encodeOpenAIResponses builds the body.
func encodeOpenAIResponses(req Request) ([]byte, error) {
	instructions, input, err := responsesInput(req)
	if err != nil {
		return nil, err
	}
	tools, err := responsesTools(req.Tools)
	if err != nil {
		return nil, err
	}
	reasoning, err := responsesReasoning(req.Reasoning)
	if err != nil {
		return nil, err
	}

	body := responsesRequest{
		Model:        req.Model,
		Instructions: instructions,
		Input:        input,
		Tools:        tools,
		Stream:       true,
		// store:false keeps the panel stateless: the provider retains nothing,
		// and the transcript (including reasoning items) is replayed from
		// ai_messages on every call. It also makes reasoning.encrypted_content
		// necessary — see Include below.
		Store:           false,
		MaxOutputTokens: req.MaxOutputTokens,
		Reasoning:       reasoning,
	}
	if reasoning != nil {
		// With store:false, a reasoning item is only replayable if it carries its
		// encrypted payload; without this include the item comes back without
		// encrypted_content and the NEXT request 400s. Only asked for when
		// thinking is actually on, so a non-reasoning model never sees a
		// parameter it might reject.
		body.Include = []string{"reasoning.encrypted_content"}
	}
	return json.Marshal(body)
}

// responsesInput turns the panel's turns into input items and folds every system
// message into `instructions` — that is the only place this dialect accepts a
// system prompt (§12.5).
func responsesInput(req Request) (string, []json.RawMessage, error) {
	systemParts := make([]string, 0, 1+len(req.Messages))
	if s := strings.TrimSpace(req.System); s != "" {
		systemParts = append(systemParts, s)
	}

	input := make([]json.RawMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case roleSystem:
			if t := strings.TrimSpace(m.Text); t != "" {
				systemParts = append(systemParts, t)
			}

		case roleUser:
			if len(m.Raw) > 0 {
				items, err := responsesRawItems(m.Raw, fmt.Sprintf("message #%d raw", i))
				if err != nil {
					return "", nil, err
				}
				input = append(input, items...)
				continue
			}
			item, err := marshalJSON(responsesMessageItem{
				Type:    "message",
				Role:    roleUser,
				Content: []responsesContent{{Type: "input_text", Text: m.Text}},
			})
			if err != nil {
				return "", nil, err
			}
			input = append(input, item)

		case roleAssistant:
			if len(m.Raw) > 0 {
				// The stored output items (reasoning, message, function_call) are
				// replayed byte for byte. Rebuilding them would drop
				// encrypted_content and the call ids.
				items, err := responsesRawItems(m.Raw, fmt.Sprintf("message #%d raw", i))
				if err != nil {
					return "", nil, err
				}
				input = append(input, items...)
				continue
			}
			if m.Text != "" {
				item, err := marshalJSON(responsesMessageItem{
					Type:    "message",
					Role:    roleAssistant,
					Content: []responsesContent{{Type: "output_text", Text: m.Text}},
				})
				if err != nil {
					return "", nil, err
				}
				input = append(input, item)
			}
			for j, tc := range m.ToolCalls {
				call, err := responsesFunctionCall(tc, j)
				if err != nil {
					return "", nil, err
				}
				input = append(input, call)
			}

		case roleTool:
			for _, tr := range m.ToolResults {
				// is_error has no field in this dialect; the caller folds any
				// error note into Content (see the completions encoder).
				item, err := marshalJSON(responsesFunctionCallOutputItem{
					Type:   "function_call_output",
					CallID: tr.CallID,
					Output: tr.Content,
				})
				if err != nil {
					return "", nil, err
				}
				input = append(input, item)
			}

		default:
			return "", nil, fmt.Errorf("message #%d: unsupported role %q", i, m.Role)
		}
	}
	return strings.Join(systemParts, "\n\n"), input, nil
}

// responsesRawItems decodes replayed native items and refuses ones produced by
// the other protocol.
func responsesRawItems(raw json.RawMessage, what string) ([]json.RawMessage, error) {
	items, err := rawArray(raw, what)
	if err != nil {
		return nil, err
	}
	if err := checkOpaqueProtocol(items, ProtocolOpenAIResponses, what); err != nil {
		return nil, err
	}
	return items, nil
}

// responsesFunctionCall encodes one tool call as a top-level item (in this
// dialect calls are not nested inside the assistant message).
func responsesFunctionCall(tc ToolCall, index int) (json.RawMessage, error) {
	if strings.TrimSpace(tc.ID) == "" {
		return nil, fmt.Errorf("tool call #%d (%s) has no id", index, tc.Name)
	}
	if strings.TrimSpace(tc.Name) == "" {
		return nil, fmt.Errorf("tool call #%d has no name", index)
	}
	args, err := jsonObject(tc.Arguments, fmt.Sprintf("tool call %q arguments", tc.Name))
	if err != nil {
		return nil, err
	}
	return marshalJSON(responsesFunctionCallItem{
		Type:      "function_call",
		CallID:    tc.ID,
		Name:      tc.Name,
		Arguments: string(args),
	})
}

// responsesTools encodes §12.5's flat tool shape.
func responsesTools(tools []ToolSpec) ([]responsesTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]responsesTool, 0, len(tools))
	for i, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("tool #%d has no name", i)
		}
		schema, err := jsonSchema(t.Schema, fmt.Sprintf("tool %q schema", t.Name))
		if err != nil {
			return nil, err
		}
		out = append(out, responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}
	return out, nil
}

// responsesReasoning maps the panel's Reasoning onto the `reasoning` object.
func responsesReasoning(r Reasoning) (*responsesReasoningBody, error) {
	level := normalizeLevel(r.Level)
	if level == "" {
		return nil, nil
	}
	if level == LevelOff {
		switch normalizeOffStyle(r.OffStyle) {
		case OffStyleNone:
			return &responsesReasoningBody{Effort: OffStyleNone}, nil
		case OffStyleDisabled:
			off := false
			return &responsesReasoningBody{Enabled: &off}, nil
		default:
			// omit: send no reasoning field at all (§12.5).
			return nil, nil
		}
	}
	return &responsesReasoningBody{Effort: level, Summary: "auto"}, nil
}

// responsesEvent is the envelope of every named event. Most fields are only
// populated by some event types; that is cheap and keeps one decoder.
type responsesEvent struct {
	Type        string          `json:"type"`
	ItemID      string          `json:"item_id"`
	OutputIndex *int            `json:"output_index"`
	Delta       string          `json:"delta"`
	Item        json.RawMessage `json:"item"`
	Arguments   string          `json:"arguments"`
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response *struct {
		Output []json.RawMessage `json:"output"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// responsesStreamItem is the part of an output item the parser needs.
type responsesStreamItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// streamOpenAIResponses parses the named-event stream (§12.5): text arrives as
// response.output_text.delta, thinking as response.reasoning_summary_text.delta
// (or response.reasoning_text.delta), tool arguments as
// response.function_call_arguments.delta fragments, and the turn ends at
// response.completed.
func streamOpenAIResponses(ctx context.Context, body io.Reader, key string, onEvent func(Event) error) error {
	type partialCall struct {
		callID string
		name   string
		args   strings.Builder
	}

	calls := map[string]*partialCall{}
	emitted := map[string]bool{}
	// emittedCallIDs is a second index: the same call is named by its item id on
	// the stream and by its call_id in the terminal response, so one key would
	// let a call through twice.
	emittedCallIDs := map[string]bool{}
	// items keeps the raw native items by output index so a stream that never
	// sends response.completed's full output still has something to replay.
	items := map[int]json.RawMessage{}
	var completedOutput []json.RawMessage

	emitCall := func(k string, item responsesStreamItem) error {
		if emitted[k] {
			return nil
		}
		if item.CallID != "" && emittedCallIDs[item.CallID] {
			return nil
		}
		c := calls[k]
		rawArgs := item.Arguments
		if rawArgs == "" && c != nil {
			rawArgs = c.args.String()
		}
		callID := item.CallID
		name := item.Name
		if c != nil {
			if callID == "" {
				callID = c.callID
			}
			if name == "" {
				name = c.name
			}
		}
		args, err := assembleToolArguments(rawArgs, fmt.Sprintf("tool call %q", name))
		if err != nil {
			return err
		}
		emitted[k] = true
		if callID != "" {
			emittedCallIDs[callID] = true
		}
		return onEvent(Event{Kind: KindToolCall, ToolCall: &ToolCall{ID: callID, Name: name, Arguments: args}})
	}

	err := forEachSSE(body, func(_, data string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload := strings.TrimSpace(data)
		if payload == "" {
			return nil
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return fmt.Errorf("decode event %.200q: %w", payload, err)
		}

		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				return onEvent(Event{Kind: KindText, Text: ev.Delta})
			}

		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if ev.Delta != "" {
				return onEvent(Event{Kind: KindThinking, Text: ev.Delta})
			}

		case "response.output_item.added", "response.output_item.done":
			var item responsesStreamItem
			if err := json.Unmarshal(ev.Item, &item); err != nil {
				return fmt.Errorf("decode output item: %w", err)
			}
			k := responsesKey(ev.ItemID, ev.OutputIndex)
			if item.Type == "function_call" {
				c := calls[k]
				if c == nil {
					c = &partialCall{}
					calls[k] = c
				}
				if item.CallID != "" {
					c.callID = item.CallID
				}
				if item.Name != "" {
					c.name = item.Name
				}
				if item.Arguments != "" {
					c.args.Reset()
					c.args.WriteString(item.Arguments)
				}
			}
			if ev.Type == "response.output_item.done" {
				if idx := ev.OutputIndex; idx != nil {
					items[*idx] = ev.Item
				}
				if item.Type == "function_call" {
					// The item is complete here: this is where a call is worth
					// emitting (its arguments are final).
					if err := emitCall(k, item); err != nil {
						return err
					}
				}
			}

		case "response.function_call_arguments.delta":
			k := responsesKey(ev.ItemID, ev.OutputIndex)
			c := calls[k]
			if c == nil {
				c = &partialCall{}
				calls[k] = c
			}
			c.args.WriteString(ev.Delta)

		case "response.completed":
			if ev.Response != nil {
				completedOutput = ev.Response.Output
			}
			// Any function_call that never got an output_item.done still has a
			// complete item in the final response.
			for _, raw := range completedOutput {
				var item responsesStreamItem
				if err := json.Unmarshal(raw, &item); err != nil {
					continue
				}
				if item.Type != "function_call" {
					continue
				}
				// The stream names this call by item id; the terminal response
				// carries both. Either way the same key must be derived, or the
				// call is emitted again here.
				k := item.ID
				if k == "" {
					k = item.CallID
				}
				if err := emitCall(k, item); err != nil {
					return err
				}
			}
			raw, err := responsesDoneRaw(completedOutput, items)
			if err != nil {
				return err
			}
			if err := onEvent(Event{Kind: KindDone, Raw: raw}); err != nil {
				return err
			}
			return errStreamDone

		case "response.failed":
			return responsesFailure("response failed", ev, key)

		case "response.incomplete":
			// Not an upstream error, but a truncated turn: the caller must not
			// treat what it just received as a complete assistant message.
			if ev.Response != nil && ev.Response.IncompleteDetails != nil && ev.Response.IncompleteDetails.Reason != "" {
				return fmt.Errorf("response incomplete: %s", ev.Response.IncompleteDetails.Reason)
			}
			return errors.New("response incomplete")

		case "error":
			return responsesFailure("upstream error", ev, key)
		}
		return nil
	})
	if errors.Is(err, errStreamDone) {
		return nil
	}
	if err != nil {
		return err
	}
	// Unlike completions there is no lenient EOF path: response.completed is the
	// only place the final output items (with their encrypted reasoning) are
	// guaranteed to be present, so a stream without it is truncated.
	return errors.New("stream ended before response.completed")
}

// responsesFailure builds the error for response.failed / error events. Both
// carry free-form upstream text, so it goes through the redactor — with the
// provider's own key, not just the shape patterns: the usual echo
// ("Incorrect API key provided: sk-…") matches no field pattern at all.
func responsesFailure(prefix string, ev responsesEvent, key string) error {
	var code, message string
	switch {
	case ev.Error != nil:
		code, message = ev.Error.Code, ev.Error.Message
	case ev.Response != nil && ev.Response.Error != nil:
		code, message = ev.Response.Error.Code, ev.Response.Error.Message
	}
	message = redact(strings.TrimSpace(message), key)
	if code == "" && message == "" {
		return errors.New(prefix)
	}
	if message == "" {
		return fmt.Errorf("%s: %s", prefix, code)
	}
	return fmt.Errorf("%s: %s: %s", prefix, code, message)
}

// responsesDoneRaw picks the block array handed to the caller for replay: the
// final response's output when present (authoritative and in order), otherwise
// the items seen on the stream, ordered by output index.
func responsesDoneRaw(completed []json.RawMessage, items map[int]json.RawMessage) (json.RawMessage, error) {
	if len(completed) > 0 {
		return json.Marshal(completed)
	}
	if len(items) > 0 {
		ordered := make([]int, 0, len(items))
		for idx := range items {
			ordered = append(ordered, idx)
		}
		sort.Ints(ordered)
		out := make([]json.RawMessage, 0, len(ordered))
		for _, idx := range ordered {
			out = append(out, items[idx])
		}
		return json.Marshal(out)
	}
	return nil, nil
}

// responsesKey identifies a function call across the events that mention it.
// item_id is present on every function-call event; output_index is the fallback
// for gateways that omit it (and index 0 is a real index, hence the pointer).
func responsesKey(itemID string, outputIndex *int) string {
	if itemID != "" {
		return itemID
	}
	if outputIndex != nil {
		return fmt.Sprintf("index:%d", *outputIndex)
	}
	return ""
}

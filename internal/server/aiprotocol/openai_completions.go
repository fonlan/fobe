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

// OpenAI chat completions (design.md §12.5). The least opinionated dialect:
// the system prompt is just the first message, tools nest under "function",
// tool arguments travel as a JSON *string*, and there is no concept of an
// opaque reasoning block — so Message.Raw is ignored here and the turn is
// rebuilt from Text/ToolCalls. That is safe because a completions transcript
// carries nothing that cannot be rebuilt; §12.1 prevents cross-protocol reuse
// in the first place.

type completionsRequest struct {
	Model           string               `json:"model"`
	Messages        []json.RawMessage    `json:"messages"`
	Tools           []completionsTool    `json:"tools,omitempty"`
	Stream          bool                 `json:"stream"`
	MaxTokens       int                  `json:"max_tokens,omitempty"`
	ReasoningEffort string               `json:"reasoning_effort,omitempty"`
	Reasoning       *reasoningToggleBody `json:"reasoning,omitempty"`
}

type completionsMessage struct {
	Role       string                `json:"role"`
	Content    *string               `json:"content,omitempty"`
	ToolCalls  []completionsToolCall `json:"tool_calls,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
}

type completionsToolCall struct {
	ID       string                      `json:"id"`
	Type     string                      `json:"type"`
	Function completionsToolCallFunction `json:"function"`
}

type completionsToolCallFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON *string* here, not an object — the one place the
	// completions dialect differs structurally from the other two.
	Arguments string `json:"arguments"`
}

type completionsTool struct {
	Type     string                      `json:"type"`
	Function completionsToolFunctionBody `json:"function"`
}

type completionsToolFunctionBody struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// encodeOpenAICompletions builds the body.
func encodeOpenAICompletions(req Request) ([]byte, error) {
	messages := make([]json.RawMessage, 0, len(req.Messages)+1)
	if system := strings.TrimSpace(req.System); system != "" {
		// §12.5: the system prompt is messages[0] with role "system" here, not a
		// top-level field.
		msg, err := marshalJSON(completionsMessage{Role: roleSystem, Content: &system})
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	for i, m := range req.Messages {
		built, err := completionsMessages(m)
		if err != nil {
			return nil, fmt.Errorf("message #%d: %w", i, err)
		}
		messages = append(messages, built...)
	}

	tools, err := completionsTools(req.Tools)
	if err != nil {
		return nil, err
	}

	body := completionsRequest{
		Model:           req.Model,
		Messages:        messages,
		Tools:           tools,
		Stream:          true,
		MaxTokens:       req.MaxOutputTokens, // optional in this dialect: 0 omits it
		ReasoningEffort: openAIReasoningEffort(req.Reasoning),
		Reasoning:       openAIToggleBody(req.Reasoning),
	}
	return json.Marshal(body)
}

// completionsMessages expands one panel turn into the dialect's messages. A
// tool turn becomes one message per result, because every result needs its own
// tool_call_id here.
func completionsMessages(m Message) ([]json.RawMessage, error) {
	switch m.Role {
	case roleSystem, roleUser:
		content := m.Text
		msg, err := marshalJSON(completionsMessage{Role: m.Role, Content: &content})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{msg}, nil

	case roleAssistant:
		msg := completionsMessage{Role: roleAssistant}
		if m.Text != "" {
			text := m.Text
			msg.Content = &text
		}
		for i, tc := range m.ToolCalls {
			call, err := completionsToolCallOf(tc, i)
			if err != nil {
				return nil, err
			}
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
		out, err := marshalJSON(msg)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{out}, nil

	case roleTool:
		out := make([]json.RawMessage, 0, len(m.ToolResults))
		for _, tr := range m.ToolResults {
			content := tr.Content
			// This dialect has no is_error field; the caller folds any error
			// note into Content. Nothing is invented here.
			msg, err := marshalJSON(completionsMessage{
				Role:       roleTool,
				Content:    &content,
				ToolCallID: tr.CallID,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, msg)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unsupported role %q", m.Role)
	}
}

// completionsToolCallOf validates one tool call. The id and name are both
// mandatory: without them the tool result cannot be paired on the next turn.
func completionsToolCallOf(tc ToolCall, index int) (completionsToolCall, error) {
	if strings.TrimSpace(tc.ID) == "" {
		return completionsToolCall{}, fmt.Errorf("tool call #%d (%s) has no id", index, tc.Name)
	}
	if strings.TrimSpace(tc.Name) == "" {
		return completionsToolCall{}, fmt.Errorf("tool call #%d has no name", index)
	}
	args, err := jsonObject(tc.Arguments, fmt.Sprintf("tool call %q arguments", tc.Name))
	if err != nil {
		return completionsToolCall{}, err
	}
	return completionsToolCall{
		ID:       tc.ID,
		Type:     "function",
		Function: completionsToolCallFunction{Name: tc.Name, Arguments: string(args)},
	}, nil
}

// completionsTools encodes the tool definitions: §12.5's nested
// {type:"function",function:{…}} shape.
func completionsTools(tools []ToolSpec) ([]completionsTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]completionsTool, 0, len(tools))
	for i, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("tool #%d has no name", i)
		}
		schema, err := jsonSchema(t.Schema, fmt.Sprintf("tool %q schema", t.Name))
		if err != nil {
			return nil, err
		}
		out = append(out, completionsTool{
			Type: "function",
			Function: completionsToolFunctionBody{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	return out, nil
}

// looseString accepts a JSON string and quietly ignores any other shape.
//
// It exists for the thinking fields only: gateways that publish reasoning as an
// object (OpenRouter's reasoning_details, or a {"text": …} wrapper) must not make
// the whole chunk undecodable — a decode error aborts the turn and throws away
// every delta that already arrived.
type looseString string

func (s *looseString) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = looseString(str)
	}
	return nil
}

// completionsChunk is one `data:` payload of the stream.
type completionsChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Gateways that expose thinking on this dialect put it here:
			// reasoning_content is the DeepSeek/vLLM spelling, reasoning the
			// OpenRouter one.
			ReasoningContent looseString `json:"reasoning_content"`
			Reasoning        looseString `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	// Some gateways report a failure as a data line instead of an HTTP status.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// streamOpenAICompletions parses `data:` lines with choices[].delta. Tool
// arguments arrive as fragments keyed by `index`, so they can only be emitted
// once the stream says the turn is over (finish_reason or [DONE]) — there is no
// per-call "done" event in this dialect.
func streamOpenAICompletions(ctx context.Context, body io.Reader, key string, onEvent func(Event) error) error {
	type partialCall struct {
		id   string
		name string
		args strings.Builder
	}

	calls := map[int]*partialCall{}
	indices := make([]int, 0, 2)
	finishSeen := false
	gotChunk := false
	doneSent := false

	emitDone := func() error {
		if doneSent {
			return nil
		}
		doneSent = true
		// No native blocks exist in this dialect, so Raw stays empty.
		return onEvent(Event{Kind: KindDone})
	}
	flush := func() error {
		sort.Ints(indices)
		for _, idx := range indices {
			c := calls[idx]
			args, err := assembleToolArguments(c.args.String(), fmt.Sprintf("tool call %q (index %d)", c.name, idx))
			if err != nil {
				return err
			}
			call := &ToolCall{ID: c.id, Name: c.name, Arguments: args}
			if err := onEvent(Event{Kind: KindToolCall, ToolCall: call}); err != nil {
				return err
			}
		}
		return emitDone()
	}

	err := forEachSSE(body, func(_, data string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload := strings.TrimSpace(data)
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			if err := flush(); err != nil {
				return err
			}
			return errStreamDone
		}

		var chunk completionsChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("decode chunk %.200q: %w", payload, err)
		}
		if chunk.Error != nil {
			// A failure delivered as a data line: reporting it here beats letting
			// the caller see "stream ended without a finish_reason".
			message := redact(strings.TrimSpace(chunk.Error.Message), key)
			if chunk.Error.Type != "" {
				return fmt.Errorf("upstream error: %s: %s", chunk.Error.Type, message)
			}
			return fmt.Errorf("upstream error: %s", message)
		}
		gotChunk = true
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := onEvent(Event{Kind: KindText, Text: choice.Delta.Content}); err != nil {
					return err
				}
			}
			thinking := string(choice.Delta.ReasoningContent)
			if thinking == "" {
				thinking = string(choice.Delta.Reasoning)
			}
			if thinking != "" {
				if err := onEvent(Event{Kind: KindThinking, Text: thinking}); err != nil {
					return err
				}
			}
			for _, frag := range choice.Delta.ToolCalls {
				c := calls[frag.Index]
				if c == nil {
					c = &partialCall{}
					calls[frag.Index] = c
					indices = append(indices, frag.Index)
				}
				// id and name only arrive on the first fragment of a call.
				if frag.ID != "" {
					c.id = frag.ID
				}
				if frag.Function.Name != "" {
					c.name = frag.Function.Name
				}
				c.args.WriteString(frag.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishSeen = true
			}
		}
		return nil
	})
	if errors.Is(err, errStreamDone) {
		return nil
	}
	if err != nil {
		return err
	}

	if !gotChunk {
		return errors.New("upstream sent an empty event stream")
	}
	// EOF without [DONE] happens with gateways that close right after
	// finish_reason; a stream that ends WITHOUT either marker is truncated and
	// its half-assembled tool arguments must not be presented as a complete turn.
	if !finishSeen && !doneSent {
		return errors.New("stream ended without a finish_reason or [DONE] marker (truncated?)")
	}
	return flush()
}

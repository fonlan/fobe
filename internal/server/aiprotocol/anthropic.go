package aiprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Anthropic messages (design.md §12.5). The dialect with the most traps:
//
//   - max_tokens is REQUIRED (the other two make it optional).
//   - the system prompt is a top-level field, never a message.
//   - thinking is opt-in, so switching it off means omitting the field —
//     {"type":"disabled"} is a 400.
//   - a tool_result block must sit in the user turn that IMMEDIATELY follows the
//     assistant turn carrying the matching tool_use; a tool_result one turn late
//     (or with no tool_use at all) is a 400 with a confusing message.
//   - a replayed thinking block must keep its signature, which the stream
//     delivers as a separate signature_delta.

type anthropicRequest struct {
	Model     string                 `json:"model"`
	System    string                 `json:"system,omitempty"`
	Messages  []json.RawMessage      `json:"messages"`
	MaxTokens int                    `json:"max_tokens"`
	Stream    bool                   `json:"stream"`
	Thinking  *anthropicThinkingBody `json:"thinking,omitempty"`
	Tools     []anthropicTool        `json:"tools,omitempty"`
}

type anthropicThinkingBody struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicTurn is one entry of the messages array. Content stays raw JSON so a
// replayed opaque block keeps its exact bytes.
type anthropicTurn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// encodeAnthropicMessages builds the body.
func encodeAnthropicMessages(req Request) ([]byte, error) {
	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		// Required field: omitting it is not "use the default", it is a 400.
		maxTokens = anthropicDefaultMaxTokens
	}
	thinking, err := anthropicThinking(req.Reasoning, maxTokens)
	if err != nil {
		return nil, err
	}
	messages, system, err := anthropicMessages(req.Messages, req.System)
	if err != nil {
		return nil, err
	}
	tools, err := anthropicTools(req.Tools)
	if err != nil {
		return nil, err
	}
	body := anthropicRequest{
		Model:     req.Model,
		System:    system,
		Messages:  messages,
		MaxTokens: maxTokens,
		Stream:    true,
		Thinking:  thinking,
		Tools:     tools,
	}
	return json.Marshal(body)
}

// anthropicMessages converts the panel's flat turns into Anthropic's alternating
// turns and enforces the tool_use/tool_result pairing.
//
// The pairing rule is not cosmetic: §12.5's manual-thinking tool loop only works
// when each tool_result follows its tool_use immediately. Two corrections are
// applied here rather than letting the upstream 400:
//
//   - a tool turn that the caller placed after an intervening message is moved
//     up so it directly follows its assistant turn;
//   - the tool_result blocks of one assistant turn are collected into a single
//     user turn (Anthropic wants them together, and separate ones would also
//     have to keep the exact order).
//
// An orphan tool_result (no matching tool_use anywhere) or one placed BEFORE its
// tool_use cannot be corrected without rewriting the transcript, so it is an
// explicit error.
func anthropicMessages(msgs []Message, system string) ([]json.RawMessage, string, error) {
	systemParts := make([]string, 0, 1)
	if s := strings.TrimSpace(system); s != "" {
		systemParts = append(systemParts, s)
	}

	// Pass 1: which assistant turn owns each tool_use id. Only the field that
	// will actually be sent counts — when Raw is present the content is replayed
	// verbatim and ToolCalls is ignored, so its ids must not be paired either.
	owner := map[string]int{}
	for i, m := range msgs {
		if m.Role != roleAssistant {
			continue
		}
		ids, err := anthropicToolUseIDs(m)
		if err != nil {
			return nil, "", fmt.Errorf("message #%d: %w", i, err)
		}
		for _, id := range ids {
			owner[id] = i
		}
	}

	// Pass 2: attach every tool_result to the assistant turn it answers.
	resultsFor := map[int][]json.RawMessage{}
	for i, m := range msgs {
		if m.Role != roleTool {
			continue
		}
		for j, tr := range m.ToolResults {
			own, ok := owner[tr.CallID]
			if !ok {
				return nil, "", fmt.Errorf(
					"message #%d tool_result #%d: no tool_use with id %q to answer (Anthropic requires every tool_result to follow its tool_use)",
					i, j, tr.CallID,
				)
			}
			if i < own {
				return nil, "", fmt.Errorf(
					"message #%d: tool_result for %q appears before its tool_use (message #%d)",
					i, tr.CallID, own,
				)
			}
			block, err := marshalJSON(anthropicToolResultBlock{
				Type:      "tool_result",
				ToolUseID: tr.CallID,
				Content:   tr.Content,
				IsError:   tr.IsError,
			})
			if err != nil {
				return nil, "", err
			}
			resultsFor[own] = append(resultsFor[own], block)
		}
	}

	// Pass 3: emit, inserting each assistant turn's tool_results immediately
	// after it.
	turns := make([]json.RawMessage, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case roleSystem:
			// Folded into the top-level system field: "system" is not a legal
			// role inside Anthropic's messages array.
			if t := strings.TrimSpace(m.Text); t != "" {
				systemParts = append(systemParts, t)
			}

		case roleTool:
			// Emitted with its assistant turn (pass 2). Nothing to do here.
			continue

		case roleUser:
			blocks, err := anthropicUserBlocks(m)
			if err != nil {
				return nil, "", fmt.Errorf("message #%d: %w", i, err)
			}
			if len(blocks) == 0 {
				continue
			}
			turn, err := anthropicTurnOf(roleUser, blocks)
			if err != nil {
				return nil, "", err
			}
			turns = append(turns, turn)

		case roleAssistant:
			blocks, err := anthropicAssistantBlocks(m)
			if err != nil {
				return nil, "", fmt.Errorf("message #%d: %w", i, err)
			}
			if len(blocks) > 0 {
				turn, err := anthropicTurnOf(roleAssistant, blocks)
				if err != nil {
					return nil, "", err
				}
				turns = append(turns, turn)
			}
			if results := resultsFor[i]; len(results) > 0 {
				turn, err := anthropicTurnOf(roleUser, results)
				if err != nil {
					return nil, "", err
				}
				turns = append(turns, turn)
			}

		default:
			return nil, "", fmt.Errorf("message #%d: unsupported role %q", i, m.Role)
		}
	}
	return turns, strings.Join(systemParts, "\n\n"), nil
}

// anthropicTurnOf wraps blocks into a turn whose content keeps the blocks' exact
// bytes.
func anthropicTurnOf(role string, blocks []json.RawMessage) (json.RawMessage, error) {
	content, err := json.Marshal(blocks)
	if err != nil {
		return nil, fmt.Errorf("encode %s content: %w", role, err)
	}
	return marshalJSON(anthropicTurn{Role: role, Content: content})
}

// anthropicUserBlocks builds a user turn's content. A non-empty Raw is replayed
// verbatim (image blocks, document blocks, a stored tool_result turn…).
func anthropicUserBlocks(m Message) ([]json.RawMessage, error) {
	if len(m.Raw) > 0 {
		blocks, err := anthropicRawBlocks(m.Raw, "user raw")
		if err != nil {
			return nil, err
		}
		return blocks, nil
	}
	if strings.TrimSpace(m.Text) == "" {
		return nil, nil
	}
	block, err := marshalJSON(anthropicTextBlock{Type: "text", Text: m.Text})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{block}, nil
}

// anthropicAssistantBlocks builds an assistant turn's content: the replayed
// native blocks when there are any, otherwise text + tool_use rebuilt from the
// structured fields.
func anthropicAssistantBlocks(m Message) ([]json.RawMessage, error) {
	if len(m.Raw) > 0 {
		return anthropicRawBlocks(m.Raw, "assistant raw")
	}
	blocks := make([]json.RawMessage, 0, len(m.ToolCalls)+1)
	if m.Text != "" {
		block, err := marshalJSON(anthropicTextBlock{Type: "text", Text: m.Text})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	for i, tc := range m.ToolCalls {
		if strings.TrimSpace(tc.ID) == "" {
			return nil, fmt.Errorf("tool call #%d (%s) has no id", i, tc.Name)
		}
		if strings.TrimSpace(tc.Name) == "" {
			return nil, fmt.Errorf("tool call #%d has no name", i)
		}
		input, err := jsonObject(tc.Arguments, fmt.Sprintf("tool call %q arguments", tc.Name))
		if err != nil {
			return nil, err
		}
		block, err := marshalJSON(anthropicToolUseBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: input,
		})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// anthropicRawBlocks decodes replayed blocks and refuses the other protocol's.
func anthropicRawBlocks(raw json.RawMessage, what string) ([]json.RawMessage, error) {
	blocks, err := rawArray(raw, what)
	if err != nil {
		return nil, err
	}
	if err := checkOpaqueProtocol(blocks, ProtocolAnthropicMessages, what); err != nil {
		return nil, err
	}
	return blocks, nil
}

// anthropicToolUseIDs lists the tool_use ids a turn will actually send.
func anthropicToolUseIDs(m Message) ([]string, error) {
	if len(m.Raw) > 0 {
		blocks, err := rawArray(m.Raw, "assistant raw")
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(blocks))
		for _, b := range blocks {
			var head struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(b, &head); err != nil {
				continue
			}
			if head.Type == "tool_use" && head.ID != "" {
				ids = append(ids, head.ID)
			}
		}
		return ids, nil
	}
	ids := make([]string, 0, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		if tc.ID != "" {
			ids = append(ids, tc.ID)
		}
	}
	return ids, nil
}

// anthropicTools encodes §12.5's {name,description,input_schema} shape.
func anthropicTools(tools []ToolSpec) ([]anthropicTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for i, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("tool #%d has no name", i)
		}
		schema, err := jsonSchema(t.Schema, fmt.Sprintf("tool %q schema", t.Name))
		if err != nil {
			return nil, err
		}
		out = append(out, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

// anthropicStreamBlock is the assembled state of one content block. The start
// event's fields are kept as a map so unknown block types (redacted_thinking,
// server_tool_use, web_search_tool_result…) survive the round trip unchanged.
type anthropicStreamBlock struct {
	start     map[string]any
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	partial   strings.Builder
}

// finalize renders the block back to JSON. json.RawMessage values inside the map
// marshal byte for byte, which is what keeps a tool_use input exact.
func (b *anthropicStreamBlock) finalize() (json.RawMessage, json.RawMessage, error) {
	block := make(map[string]any, len(b.start)+3)
	for k, v := range b.start {
		block[k] = v
	}
	blockType, _ := b.start["type"].(string)

	var input json.RawMessage
	switch blockType {
	case "text":
		block["text"] = b.text.String()
	case "thinking":
		block["thinking"] = b.thinking.String()
		if b.signature.Len() > 0 {
			// Dropping this is the classic failure: the next turn 400s with
			// "Expected `thinking` or `redacted_thinking`, but found `tool_use`".
			block["signature"] = b.signature.String()
		}
	case "tool_use":
		name, _ := b.start["name"].(string)
		var err error
		input, err = assembleToolArguments(b.partial.String(), fmt.Sprintf("tool_use %q", name))
		if err != nil {
			return nil, nil, err
		}
		block["input"] = input
	default:
		// Other block types carry their payload in the start event.
	}
	raw, err := json.Marshal(block)
	if err != nil {
		return nil, nil, fmt.Errorf("encode %s block: %w", blockType, err)
	}
	return raw, input, nil
}

// streamAnthropicMessages parses Anthropic's content_block_* stream. Unlike the
// OpenAI dialects it has an explicit content_block_stop, so a tool call can be
// emitted the moment its arguments are complete.
func streamAnthropicMessages(ctx context.Context, body io.Reader, key string, onEvent func(Event) error) error {
	blocks := map[int]*anthropicStreamBlock{}
	order := make([]int, 0, 4)
	finished := map[int]json.RawMessage{}
	stopped := false

	err := forEachSSE(body, func(_, data string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload := strings.TrimSpace(data)
		if payload == "" {
			return nil
		}
		var ev struct {
			Type         string          `json:"type"`
			Index        int             `json:"index"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        json.RawMessage `json:"delta"`
			Error        *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return fmt.Errorf("decode event %.200q: %w", payload, err)
		}

		switch ev.Type {
		case "content_block_start":
			var start map[string]any
			if err := json.Unmarshal(ev.ContentBlock, &start); err != nil {
				return fmt.Errorf("decode content_block_start: %w", err)
			}
			if _, seen := blocks[ev.Index]; !seen {
				order = append(order, ev.Index)
			}
			blocks[ev.Index] = &anthropicStreamBlock{start: start}

		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				return fmt.Errorf("delta for block %d before content_block_start", ev.Index)
			}
			var delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				Signature   string `json:"signature"`
			}
			if err := json.Unmarshal(ev.Delta, &delta); err != nil {
				return fmt.Errorf("decode content_block_delta: %w", err)
			}
			switch delta.Type {
			case "text_delta":
				b.text.WriteString(delta.Text)
				if delta.Text != "" {
					if err := onEvent(Event{Kind: KindText, Text: delta.Text}); err != nil {
						return err
					}
				}
			case "thinking_delta":
				b.thinking.WriteString(delta.Thinking)
				if delta.Thinking != "" {
					if err := onEvent(Event{Kind: KindThinking, Text: delta.Thinking}); err != nil {
						return err
					}
				}
			case "signature_delta":
				b.signature.WriteString(delta.Signature)
			case "input_json_delta":
				b.partial.WriteString(delta.PartialJSON)
			default:
				// citations_delta and future delta types are not needed to
				// replay the block; the start event's fields are preserved.
			}

		case "content_block_stop":
			b := blocks[ev.Index]
			if b == nil {
				return fmt.Errorf("content_block_stop for unknown block %d", ev.Index)
			}
			raw, input, err := b.finalize()
			if err != nil {
				return err
			}
			finished[ev.Index] = raw
			blockType, _ := b.start["type"].(string)
			if blockType == "tool_use" {
				name, _ := b.start["name"].(string)
				id, _ := b.start["id"].(string)
				if err := onEvent(Event{Kind: KindToolCall, ToolCall: &ToolCall{ID: id, Name: name, Arguments: input}}); err != nil {
					return err
				}
			}

		case "message_stop":
			stopped = true
			raw, err := anthropicDoneRaw(finished, order)
			if err != nil {
				return err
			}
			if err := onEvent(Event{Kind: KindDone, Raw: raw}); err != nil {
				return err
			}
			return errStreamDone

		case "error":
			if ev.Error != nil {
				message := redact(strings.TrimSpace(ev.Error.Message), key)
				return fmt.Errorf("upstream error: %s: %s", ev.Error.Type, message)
			}
			return errors.New("upstream error")
		}
		// message_start / message_delta / ping / unknown event types are not
		// needed: everything they carry is either already streamed or a usage
		// number the caller does not persist.
		return nil
	})
	if errors.Is(err, errStreamDone) {
		return nil
	}
	if err != nil {
		return err
	}
	if !stopped {
		// A truncated stream means at least one block (possibly a thinking block
		// with its signature) is missing; emitting a "done" as if the turn were
		// whole would poison the next request.
		return errors.New("stream ended before message_stop")
	}
	return nil
}

// anthropicDoneRaw assembles the turn's native blocks in stream order.
func anthropicDoneRaw(finished map[int]json.RawMessage, order []int) (json.RawMessage, error) {
	if len(finished) == 0 {
		return nil, nil
	}
	out := make([]json.RawMessage, 0, len(finished))
	for _, idx := range order {
		if raw, ok := finished[idx]; ok {
			out = append(out, raw)
		}
	}
	return json.Marshal(out)
}

package aiprotocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Shared request-building helpers. Everything here is a pure function; the
// per-protocol files only decide the wire shape, while these helpers keep the
// validation rules identical across the three dialects (an object really is an
// object, a tool really has a name, a replayed block really came from a
// compatible protocol).

// Message roles, as they appear in Message.Role.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// emptyJSONObject is what an absent tool schema, absent tool arguments or an
// absent Anthropic `input` becomes. Every tool payload in all three dialects
// must be a JSON object: sending an empty string, or omitting the key, is what
// turns "the model called a tool with no arguments" into a 400 on the next turn.
var emptyJSONObject = json.RawMessage(`{}`)

// defaultReasoningBudgets is §12.5's fixed low 2k / medium 8k / high 16k mapping
// for Anthropic's budget_tokens. It is only a fallback: the operator may edit the
// numbers, and Request.Reasoning.Budgets carries the edited values.
var defaultReasoningBudgets = map[string]int{
	LevelLow:    2048,
	LevelMedium: 8192,
	LevelHigh:   16384,
}

// emptyToolSchema is the schema sent for a tool the caller declared without one.
// Anthropic and OpenAI both require input_schema/parameters to be an object.
var emptyToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// jsonObject validates raw as a JSON object and returns it unchanged, so the
// caller's bytes (and their key order) survive.
func jsonObject(raw json.RawMessage, what string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return emptyJSONObject, nil
	}
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("%s is not valid JSON: %.200q", what, trimmed)
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("%s must be a JSON object, got %.200q", what, trimmed)
	}
	return json.RawMessage(trimmed), nil
}

// jsonSchema is jsonObject with the "a tool without a schema" default.
func jsonSchema(raw json.RawMessage, what string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return emptyToolSchema, nil
	}
	return jsonObject(raw, what)
}

// rawArray decodes a stored array of native blocks. The bytes of each element
// are preserved, which is what "replay Raw verbatim" means in practice.
func rawArray(raw json.RawMessage, what string) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		return items, nil
	}
	// A single block where an array was expected: wrapping it is friendlier than
	// sending something the upstream will reject.
	var one json.RawMessage
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return []json.RawMessage{one}, nil
}

// marshalJSON encodes one wire fragment.
func marshalJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", v, err)
	}
	return b, nil
}

// assembleToolArguments validates the argument fragments a stream assembled.
//
// An empty result is `{}` — gateways that stream no fragments at all mean "no
// arguments", and an empty string is not valid JSON. Anything non-empty must be
// a valid JSON object; returning it silently as-is would push the failure into
// the *next* request, one round trip away from the cause.
func assembleToolArguments(raw, what string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return emptyJSONObject, nil
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("%s: assembled arguments are not valid JSON: %.200q", what, trimmed)
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("%s: assembled arguments must be a JSON object, got %.200q", what, trimmed)
	}
	return json.RawMessage(trimmed), nil
}

// normalizeLevel lower-cases a level and trims it; the panel writes lowercase,
// but a hand-typed settings value should not silently become a wire value.
func normalizeLevel(level string) string {
	return strings.ToLower(strings.TrimSpace(level))
}

// normalizeOffStyle maps an unknown or empty OffStyle to OffStyleOmit: 上层没表态时
// 最保守的解释就是"省略字段"，绝不擅自发一个等于最低档的值冒充关闭。
func normalizeOffStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case OffStyleNone:
		return OffStyleNone
	case OffStyleDisabled:
		return OffStyleDisabled
	default:
		return OffStyleOmit
	}
}

// reasoningToggleBody is the {"enabled": false} toggle spelling used by gateways
// that expose thinking as a plain on/off switch (OffStyle=="disabled").
type reasoningToggleBody struct {
	Enabled bool `json:"enabled"`
}

// openAIReasoningEffort translates Reasoning for either OpenAI dialect (the
// effort value is the same string in both). "" means "send no reasoning field
// at all".
func openAIReasoningEffort(r Reasoning) string {
	level := normalizeLevel(r.Level)
	if level == "" {
		// Level=="" is explicitly "do not intervene" (§12.5).
		return ""
	}
	if level == LevelOff {
		if normalizeOffStyle(r.OffStyle) == OffStyleNone {
			// Not a lower tier pretending to be "off": these models publish
			// "none" among their effort values, and omitting the field leaves
			// thinking ON on those gateways.
			return OffStyleNone
		}
		return ""
	}
	// minimal/low/medium/high pass straight through, minimal included: it is a
	// real tier (gpt-5 and friends), not a synonym for low.
	return level
}

// openAIToggleBody returns the {"enabled": false} object for
// OffStyle=="disabled", and nil for every other style.
func openAIToggleBody(r Reasoning) *reasoningToggleBody {
	if normalizeLevel(r.Level) != LevelOff {
		return nil
	}
	if normalizeOffStyle(r.OffStyle) != OffStyleDisabled {
		return nil
	}
	return &reasoningToggleBody{Enabled: false}
}

// anthropicThinking builds Anthropic's thinking object, or nil to omit it.
//
// Omission is the ONLY way to switch thinking off there: §12.5 records that
// {"type":"disabled"} is answered with a 400, so OffStyle is ignored entirely
// for this dialect.
func anthropicThinking(r Reasoning, maxTokens int) (*anthropicThinkingBody, error) {
	level := normalizeLevel(r.Level)
	if level == "" || level == LevelOff {
		return nil, nil
	}

	budget := r.Budgets[level]
	if budget <= 0 {
		if level == LevelMinimal {
			// budget_tokens has no "minimal" tier; use the caller's low budget
			// when they gave one, otherwise low's default. This only matters if
			// the panel ever offers minimal for a budget_tokens model.
			budget = r.Budgets[LevelLow]
		}
		if budget <= 0 {
			budget = defaultReasoningBudgets[level]
		}
	}
	if budget <= 0 {
		budget = defaultReasoningBudgets[LevelLow]
	}
	if budget < anthropicMinThinkingBudget {
		return nil, fmt.Errorf(
			"thinking budget %d for level %q is below the %d Anthropic accepts",
			budget, level, anthropicMinThinkingBudget,
		)
	}
	if budget >= maxTokens {
		// budget_tokens must be strictly smaller than max_tokens or the request
		// 400s. Clamp in place instead of failing the turn; comment in §12.5.
		budget = maxTokens / 2
		if budget < anthropicMinThinkingBudget {
			return nil, fmt.Errorf(
				"max_tokens %d leaves no room for extended thinking: the clamped budget would be %d, below the %d minimum (raise the model's max output)",
				maxTokens, budget, anthropicMinThinkingBudget,
			)
		}
	}
	return &anthropicThinkingBody{Type: "enabled", BudgetTokens: budget}, nil
}

// opaqueProtocol sniffs which protocol produced a stored block array. Only
// distinctive block/item types count, so an array of plain {"type":"text"}
// blocks stays inconclusive ("") and is accepted for whichever dialect is in
// use.
func opaqueProtocol(blocks []json.RawMessage) string {
	var anthropic, responses bool
	for _, b := range blocks {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			continue
		}
		switch head.Type {
		case "thinking", "redacted_thinking", "tool_use", "server_tool_use",
			"web_search_tool_result", "code_execution_tool_result", "mcp_tool_use",
			"mcp_tool_result", "container_upload", "search_result":
			anthropic = true
		case "reasoning", "function_call", "function_call_output", "computer_call",
			"web_search_call", "file_search_call", "image_generation_call",
			"code_interpreter_call", "mcp_call", "mcp_list_tools",
			"mcp_approval_request", "local_shell_call":
			responses = true
		}
	}
	switch {
	case anthropic && !responses:
		return ProtocolAnthropicMessages
	case responses && !anthropic:
		return ProtocolOpenAIResponses
	default:
		return ""
	}
}

// checkOpaqueProtocol refuses to inline blocks produced by the other protocol.
//
// §12.1 answers this at the source ("换模型即开新会话"), and this check is the
// cheap backstop: replaying Anthropic `tool_use`/`thinking` blocks as Responses
// input items (or the reverse) fails upstream with a message that says nothing
// about the real cause, one request later.
func checkOpaqueProtocol(blocks []json.RawMessage, target string, what string) error {
	got := opaqueProtocol(blocks)
	if got == "" || got == target {
		return nil
	}
	return fmt.Errorf(
		"%s: opaque blocks look like %s but this request speaks %s (the panel opens a new session when the protocol changes, §12.1)",
		what, got, target,
	)
}

// Package aiprotocol is the panel's codec for the three upstream chat protocols
// the AI assistant speaks: openai-completions, openai-responses and
// anthropic-messages (design.md §12.5).
//
// The package is transport-only: it builds one HTTP request, parses one event
// stream, and never touches the database — §12.5 keeps the opaque native blocks
// in ai_messages and this layer only moves them. It deliberately does not
// import internal/server/store, so the protocol vocabulary is repeated here; if
// the strings ever change, both packages and the adapters change together.
//
// Three traps drive most of the code below, all of them observed in practice:
//
//  1. Anthropic's manual extended thinking must be replayed verbatim. In a tool
//     loop the assistant turn that requested a tool has to come back with its
//     `thinking` block AND its `signature` (streamed as signature_delta),
//     otherwise the next request dies with "Expected `thinking` or
//     `redacted_thinking`, but found `tool_use`". Message.Raw exists exactly for
//     this: a non-empty Raw is inlined byte for byte, and the stream's `done`
//     event hands the caller the assembled native blocks to persist.
//  2. "Turn thinking off" is not one wire spelling. Anthropic is opt-in:
//     omitting the field is the only way, and {"type":"disabled"} is a 400.
//     The OpenAI dialects may need an explicit "none" (many gateways think by
//     default) or a toggle shape, so Reasoning.OffStyle carries the caller's
//     choice — 只有上层知道该模型的 reasoning_options（§12.5）。
//  3. Tool arguments arrive as fragments that are each invalid JSON on their
//     own (choices[].delta.tool_calls[].function.arguments,
//     input_json_delta, function_call_arguments.delta). They are concatenated
//     and validated here, and an empty result becomes `{}` — never an empty
//     string, which the following turn would reject.
//
// Headers are assembled so a provider's extra_headers can never override the
// protocol's own auth (that is an error, not a silent ignore), and no error
// string ever contains the API key: upstream error bodies are redacted before
// they are wrapped.
package aiprotocol

import (
	"encoding/json"
)

// Protocol names. Same values as the store layer's protocol column; duplicated
// on purpose so this package stays free of storage dependencies.
const (
	ProtocolOpenAICompletions = "openai-completions"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
)

// The panel's unified thinking levels (§12.5): off / minimal / low / medium /
// high. Reasoning.Level carries one of these, or "" for "do not intervene".
const (
	LevelOff     = "off"
	LevelMinimal = "minimal"
	LevelLow     = "low"
	LevelMedium  = "medium"
	LevelHigh    = "high"
)

// Reasoning.OffStyle values: how Level=="off" is spelled on the wire.
const (
	// OffStyleOmit sends nothing at all.
	OffStyleOmit = "omit"
	// OffStyleNone sends an explicit reasoning_effort / effort of "none".
	// Many effort models list "none" among their values precisely because
	// omitting the field does NOT switch thinking off on those gateways.
	OffStyleNone = "none"
	// OffStyleDisabled sends the toggle shape {"enabled": false}. Never sent to
	// Anthropic, which answers 400 to {"type":"disabled"}.
	OffStyleDisabled = "disabled"
)

// Event kinds emitted by Stream.
const (
	KindText     = "text"
	KindThinking = "thinking"
	KindToolCall = "tool_call"
	KindDone     = "done"
)

// anthropicVersion is the only API version this adapter speaks; Anthropic
// requires the header on every request.
const anthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens is sent when the caller left MaxOutputTokens at 0.
// Anthropic rejects a request without max_tokens outright (unlike both OpenAI
// dialects, where the field is optional). 4096 is the smallest published output
// cap among current Claude models, so it can never overshoot.
const anthropicDefaultMaxTokens = 4096

// anthropicMinThinkingBudget is the smallest budget_tokens Anthropic accepts.
const anthropicMinThinkingBudget = 1024

// Message is one turn of the panel's unified conversation shape.
//
// Raw holds the **native content blocks exactly as the upstream returned them**
// (Anthropic's [{type:"thinking",...},{type:"tool_use",...}], Responses'
// reasoning/function_call items). When Raw is non-empty and the target protocol
// is the one that produced it, the adapter MUST inline Raw verbatim: dropping it
// makes Anthropic fail the next step of a tool loop with 400 ("Expected
// `thinking` or `redacted_thinking`, but found `tool_use`").
type Message struct {
	Role        string // "system" | "user" | "assistant" | "tool"
	Text        string
	Raw         json.RawMessage // protocol-native blocks; empty = rebuild from Text/ToolCalls
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ToolCall is one tool invocation requested by the model. Arguments is a JSON
// object; it is validated (and defaulted to {}) on the way out.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage // JSON object
}

// ToolResult is one tool output being fed back to the model.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// ToolSpec is one tool the model may call. Schema is a JSON Schema object;
// an empty Schema becomes {"type":"object","properties":{}}.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema object
}

// Reasoning is §12.5's unified thinking request.
//
// OffStyle exists because "off" is not universally expressible by omission:
// 实测 3415 个带 values 的 effort 模型里有 1273 个 values 含 "none" —— 对这些模型
// 省略字段并不会真的关闭思考（网关默认开），必须显式发 none；而 Anthropic 传
// disabled 会 400。上层按模型能力决定 OffStyle，适配器只负责翻译。
type Reasoning struct {
	Level   string         // "off"|"minimal"|"low"|"medium"|"high"; "" = 不干预
	Budgets map[string]int // low/medium/high → thinking token 数（Anthropic 用）
	// OffStyle 说明 Level=="off" 时怎么关，取值：
	//   "omit"     — 不发送任何推理字段（Anthropic 恒用这个：它的 thinking 是 opt-in）
	//   "none"     — OpenAI 两协议发送 reasoning_effort:"none" / reasoning:{"effort":"none"}
	//   "disabled" — OpenAI 两协议发送推理开关的关闭形态 {"enabled":false}
	//   ""         — 视同 "omit"
	// 对 anthropic-messages 一律忽略：它只能省略，传关闭形态会 400。
	OffStyle string
}

// Request is one upstream call. BaseURL keeps the panel's ROOT semantics — the
// protocol's path (/chat/completions, /responses, /messages) is appended here.
type Request struct {
	BaseURL         string // ROOT semantics (no /chat/completions and friends)
	APIKey          string
	Headers         map[string]string // extra headers; may not override the protocol's auth headers
	Protocol        string
	Model           string
	System          string
	Messages        []Message
	Tools           []ToolSpec
	Reasoning       Reasoning
	MaxOutputTokens int
}

// Event is one streaming increment. ToolCall is only ever set on a KindToolCall
// event, and only once its arguments are fully assembled. Raw carries the
// turn's native blocks on KindDone (nil for OpenAI completions, which has no
// such concept) so the caller can persist them for replay.
type Event struct {
	Kind     string // "text" | "thinking" | "tool_call" | "done"
	Text     string
	ToolCall *ToolCall       // fully assembled call
	Raw      json.RawMessage // this assistant turn's native blocks (for replay)
}

// FetchedModel is one entry of a provider's model list.
type FetchedModel struct {
	ID          string
	DisplayName string
}

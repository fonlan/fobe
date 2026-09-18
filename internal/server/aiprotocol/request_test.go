package aiprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Request shape per protocol (design.md §12.5's table): where the system prompt
// goes, what the output cap is called, how tools are spelled, how auth is sent.

func TestRequestShapeOpenAICompletions(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolOpenAICompletions))
	req := baseRequest(f, ProtocolOpenAICompletions)
	req.System = "be brief"
	req.MaxOutputTokens = 512
	req.Tools = []ToolSpec{{
		Name:        "get_time",
		Description: "clock",
		Schema:      json.RawMessage(`{"type":"object","properties":{"tz":{"type":"string"}}}`),
	}}
	req.Messages = []Message{
		{Role: roleUser, Text: "hi"},
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "get_time", Arguments: json.RawMessage(`{"tz":"UTC"}`)}}},
		{Role: roleTool, ToolResults: []ToolResult{{CallID: "call_1", Content: "12:00"}}},
	}
	runStream(t, f, req)

	got := f.last(t)
	if got.Path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", got.Path)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer "+testAPIKey {
		t.Fatalf("Authorization = %q, want Bearer <key>", auth)
	}

	body := decodeObject(t, got.Body)
	if !hasKey(body, "messages") || hasKey(body, "system") || hasKey(body, "instructions") {
		t.Fatalf("system prompt must live in messages[0], body = %v", body)
	}
	messages := arrayField(t, body, "messages")
	first := object(t, messages[0], "messages[0]")
	if first["role"] != roleSystem || first["content"] != "be brief" {
		t.Fatalf("messages[0] = %v, want the system turn", first)
	}

	// The assistant turn's tool arguments are a JSON *string* in this dialect.
	assistant := object(t, messages[2], "messages[2]")
	calls := arrayField(t, assistant, "tool_calls")
	call := object(t, calls[0], "tool_calls[0]")
	if call["type"] != "function" || call["id"] != "call_1" {
		t.Fatalf("tool_calls[0] = %v", call)
	}
	fn := objectField(t, call, "function")
	if fn["name"] != "get_time" {
		t.Fatalf("tool call name = %v", fn["name"])
	}
	if args := stringField(t, fn, "arguments"); args != `{"tz":"UTC"}` {
		t.Fatalf("function.arguments = %q, want the JSON text of the object", args)
	}

	toolMsg := object(t, messages[3], "messages[3]")
	if toolMsg["role"] != roleTool || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "12:00" {
		t.Fatalf("tool result message = %v", toolMsg)
	}

	tools := arrayField(t, body, "tools")
	tool := object(t, tools[0], "tools[0]")
	if tool["type"] != "function" {
		t.Fatalf("tools[0].type = %v, want function", tool["type"])
	}
	toolFn := objectField(t, tool, "function")
	if toolFn["name"] != "get_time" || !hasKey(toolFn, "parameters") {
		t.Fatalf("tools[0].function = %v", toolFn)
	}

	if n := countField(t, body, "max_tokens"); n != 512 {
		t.Fatalf("max_tokens = %d, want 512", n)
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
}

// TestOpenAICompletionsDropsATurnWithNothingToSay pins the transcript defect a
// failed or immediately-stopped turn leaves behind: an assistant row whose text
// is empty and which asked for no tools. This dialect ignores Message.Raw (see
// the package comment), so such a row used to be encoded as a bare
// {"role":"assistant"} — which a gateway rejects with 400 "Invalid assistant
// message: content or tool_calls must be set", on EVERY later request of the
// session. The operator therefore sees the failure far from its cause: a
// message they never wrote, in a conversation that used to work.
func TestOpenAICompletionsDropsATurnWithNothingToSay(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolOpenAICompletions))
	req := baseRequest(f, ProtocolOpenAICompletions)
	req.Messages = []Message{
		{Role: roleUser, Text: "hello"},
		// Exactly what the loop stores when the upstream died before the first
		// delta, or the operator pressed stop straight away.
		{Role: roleAssistant},
		// A thinking-only turn is the same thing on this dialect: there is no
		// native block to replay, so there is nothing it could contribute.
		{Role: roleAssistant, Raw: json.RawMessage(`[{"type":"thinking","thinking":"hmm"}]`)},
		{Role: roleUser, Text: "are you there?"},
	}
	runStream(t, f, req)

	messages := arrayField(t, decodeObject(t, f.last(t).Body), "messages")
	roles := make([]any, 0, len(messages))
	for i, raw := range messages {
		m := object(t, raw, "message")
		roles = append(roles, m["role"])
		// Nothing that IS sent may be a bare assistant turn either: content and
		// tool_calls are the only two things this dialect accepts there.
		if m["role"] == roleAssistant && m["content"] == nil && !hasKey(m, "tool_calls") {
			t.Fatalf("messages[%d] is a bare assistant turn: %v", i, m)
		}
	}
	if want := []any{roleUser, roleUser}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
}

func TestRequestShapeOpenAIResponses(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolOpenAIResponses))
	req := baseRequest(f, ProtocolOpenAIResponses)
	req.System = "be brief"
	req.MaxOutputTokens = 512
	req.Tools = []ToolSpec{{Name: "get_time", Schema: json.RawMessage(`{"type":"object"}`)}}
	req.Messages = []Message{
		{Role: roleUser, Text: "hi"},
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "get_time"}}},
		{Role: roleTool, ToolResults: []ToolResult{{CallID: "call_1", Content: "12:00"}}},
	}
	runStream(t, f, req)

	got := f.last(t)
	if got.Path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", got.Path)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer "+testAPIKey {
		t.Fatalf("Authorization = %q, want Bearer <key>", auth)
	}

	body := decodeObject(t, got.Body)
	if stringField(t, body, "instructions") != "be brief" {
		t.Fatalf("instructions = %v", body["instructions"])
	}
	if hasKey(body, "system") || hasKey(body, "messages") || hasKey(body, "max_tokens") {
		t.Fatalf("responses body must use instructions/input/max_output_tokens, got %v", body)
	}
	if n := countField(t, body, "max_output_tokens"); n != 512 {
		t.Fatalf("max_output_tokens = %d, want 512", n)
	}
	if store, ok := body["store"]; !ok || store != false {
		t.Fatalf("store = %v, want an explicit false (stateless replay)", body["store"])
	}

	input := arrayField(t, body, "input")
	user := object(t, input[0], "input[0]")
	if user["type"] != "message" || user["role"] != roleUser {
		t.Fatalf("input[0] = %v", user)
	}
	part := object(t, arrayField(t, user, "content")[0], "input[0].content[0]")
	if part["type"] != "input_text" || part["text"] != "hi" {
		t.Fatalf("input[0].content[0] = %v", part)
	}

	callItem := object(t, input[1], "input[1]")
	if callItem["type"] != "function_call" || callItem["call_id"] != "call_1" || callItem["name"] != "get_time" {
		t.Fatalf("function_call item = %v", callItem)
	}
	if args := stringField(t, callItem, "arguments"); args != "{}" {
		t.Fatalf("function_call.arguments = %q, want {} (this dialect sends a JSON string)", args)
	}
	outputItem := object(t, input[2], "input[2]")
	if outputItem["type"] != "function_call_output" || outputItem["call_id"] != "call_1" || outputItem["output"] != "12:00" {
		t.Fatalf("function_call_output item = %v", outputItem)
	}

	tool := object(t, arrayField(t, body, "tools")[0], "tools[0]")
	if tool["type"] != "function" || tool["name"] != "get_time" {
		t.Fatalf("tools[0] = %v, want the flat shape", tool)
	}
	if hasKey(tool, "function") {
		t.Fatalf("tools[0] must not nest under function, got %v", tool)
	}
}

func TestRequestShapeAnthropicMessages(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolAnthropicMessages))
	req := baseRequest(f, ProtocolAnthropicMessages)
	req.System = "be brief"
	req.MaxOutputTokens = 4096
	req.Tools = []ToolSpec{{Name: "get_time", Schema: json.RawMessage(`{"type":"object"}`)}}
	req.Messages = []Message{
		{Role: roleUser, Text: "hi"},
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "toolu_1", Name: "get_time", Arguments: json.RawMessage(`{"tz":"UTC"}`)}}},
		{Role: roleTool, ToolResults: []ToolResult{{CallID: "toolu_1", Content: "12:00", IsError: true}}},
	}
	runStream(t, f, req)

	got := f.last(t)
	if got.Path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", got.Path)
	}
	if key := got.Header.Get("x-api-key"); key != testAPIKey {
		t.Fatalf("x-api-key = %q, want the provider key", key)
	}
	if v := got.Header.Get("anthropic-version"); v != anthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", v, anthropicVersion)
	}
	if auth := got.Header.Get("Authorization"); auth != "" {
		t.Fatalf("Anthropic must not receive Authorization, got %q", auth)
	}

	body := decodeObject(t, got.Body)
	if stringField(t, body, "system") != "be brief" {
		t.Fatalf("system = %v, want the top-level field", body["system"])
	}
	if hasKey(body, "instructions") || hasKey(body, "max_output_tokens") {
		t.Fatalf("unexpected OpenAI field in the Anthropic body: %v", body)
	}
	if n := countField(t, body, "max_tokens"); n != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", n)
	}

	messages := arrayField(t, body, "messages")
	user := object(t, messages[0], "messages[0]")
	if user["role"] != roleUser {
		t.Fatalf("messages[0] = %v", user)
	}
	text := object(t, arrayField(t, user, "content")[0], "messages[0].content[0]")
	if text["type"] != "text" || text["text"] != "hi" {
		t.Fatalf("user text block = %v", text)
	}

	assistant := object(t, messages[1], "messages[1]")
	use := object(t, arrayField(t, assistant, "content")[0], "messages[1].content[0]")
	if use["type"] != "tool_use" || use["id"] != "toolu_1" || use["name"] != "get_time" {
		t.Fatalf("tool_use block = %v", use)
	}
	if input := objectField(t, use, "input"); input["tz"] != "UTC" {
		t.Fatalf("tool_use input = %v", input)
	}

	result := object(t, messages[2], "messages[2]")
	if result["role"] != roleUser {
		t.Fatalf("tool results must travel in a user turn, got %v", result["role"])
	}
	block := object(t, arrayField(t, result, "content")[0], "messages[2].content[0]")
	if block["type"] != "tool_result" || block["tool_use_id"] != "toolu_1" || block["content"] != "12:00" {
		t.Fatalf("tool_result block = %v", block)
	}
	if block["is_error"] != true {
		t.Fatalf("tool_result.is_error = %v, want true", block["is_error"])
	}

	tool := object(t, arrayField(t, body, "tools")[0], "tools[0]")
	if tool["name"] != "get_time" || !hasKey(tool, "input_schema") {
		t.Fatalf("tools[0] = %v, want {name,description,input_schema}", tool)
	}
	if hasKey(tool, "function") || hasKey(tool, "type") {
		t.Fatalf("Anthropic tools carry no type/function wrapper, got %v", tool)
	}
}

// --- reasoning translation ------------------------------------------------

// TestAnthropicThinkingOffOmitsField is the trap §12.5 records: Anthropic's
// thinking is opt-in, {"type":"disabled"} is a 400, so the ONLY correct "off" is
// the absent key — OffStyle must not be able to reintroduce it.
func TestAnthropicThinkingOffOmitsField(t *testing.T) {
	cases := []struct {
		name  string
		level string
		style string
	}{
		{"level off, omit", LevelOff, OffStyleOmit},
		{"level off, empty style", LevelOff, ""},
		{"level off, disabled would 400", LevelOff, OffStyleDisabled},
		{"level off, none is not expressible", LevelOff, OffStyleNone},
		{"level empty (no intervention)", "", OffStyleDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSEFixture(t, stopStream(ProtocolAnthropicMessages))
			req := baseRequest(f, ProtocolAnthropicMessages)
			req.MaxOutputTokens = 4096
			req.Reasoning = Reasoning{Level: tc.level, OffStyle: tc.style}
			runStream(t, f, req)

			body := decodeObject(t, f.last(t).Body)
			if hasKey(body, "thinking") {
				t.Fatalf("thinking must be absent, body = %v", body)
			}
		})
	}
}

func TestAnthropicThinkingBudgetStaysUnderMaxTokens(t *testing.T) {
	cases := []struct {
		name      string
		level     string
		maxTokens int
		budgets   map[string]int
		want      int
	}{
		{"high default 16k", LevelHigh, 32000, nil, 16384},
		{"medium default 8k", LevelMedium, 32000, nil, 8192},
		{"low default 2k", LevelLow, 32000, nil, 2048},
		{"operator override", LevelHigh, 32000, map[string]int{LevelHigh: 4096}, 4096},
		// budget_tokens must be strictly smaller than max_tokens: the adapter
		// clamps to max_tokens/2 instead of sending a request that 400s.
		{"clamped to half the cap", LevelHigh, 4096, nil, 2048},
		{"clamped to half the default cap", LevelHigh, 0, nil, 2048},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSEFixture(t, stopStream(ProtocolAnthropicMessages))
			req := baseRequest(f, ProtocolAnthropicMessages)
			req.MaxOutputTokens = tc.maxTokens
			req.Reasoning = Reasoning{Level: tc.level, Budgets: tc.budgets, OffStyle: OffStyleDisabled}
			runStream(t, f, req)

			body := decodeObject(t, f.last(t).Body)
			thinking := objectField(t, body, "thinking")
			if thinking["type"] != "enabled" {
				t.Fatalf("thinking.type = %v, want enabled (OffStyle is ignored here)", thinking["type"])
			}
			budget := countField(t, thinking, "budget_tokens")
			if budget != tc.want {
				t.Fatalf("budget_tokens = %d, want %d", budget, tc.want)
			}
			if budget >= countField(t, body, "max_tokens") {
				t.Fatalf("budget_tokens %d must be < max_tokens %v", budget, body["max_tokens"])
			}
		})
	}
}

func TestAnthropicThinkingBudgetRejectedWhenImpossible(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		budgets   map[string]int
	}{
		// 2048 default >= 2000, clamp = 1000 < the 1024 Anthropic minimum.
		{"cap too small to clamp into", 2000, nil},
		{"operator budget below the minimum", 32000, map[string]int{LevelLow: 512}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSEFixture(t, stopStream(ProtocolAnthropicMessages))
			req := baseRequest(f, ProtocolAnthropicMessages)
			req.MaxOutputTokens = tc.maxTokens
			req.Reasoning = Reasoning{Level: LevelLow, Budgets: tc.budgets}
			err := Stream(context.Background(), f.Client(), req, func(Event) error { return nil })
			if err == nil {
				t.Fatal("want an error rather than a request Anthropic would 400")
			}
			if len(f.requests()) != 0 {
				t.Fatal("nothing may be sent when the budget cannot be satisfied")
			}
		})
	}
}

// TestOpenAIOffStyle covers the second half of the parent task's correction: for
// the OpenAI dialects, "off" may have to be said explicitly ("none"), and the
// request must NOT simply omit the field in that case.
func TestOpenAIOffStyle(t *testing.T) {
	for _, protocol := range []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses} {
		t.Run(protocol, func(t *testing.T) {
			cases := []struct {
				name  string
				style string
				// wantEffort is "" when no reasoning field may be sent at all.
				wantEffort  string
				wantToggle  bool
				wantEnabled bool
			}{
				{"none is explicit", OffStyleNone, "none", false, false},
				{"disabled is the toggle shape", OffStyleDisabled, "", true, false},
				{"omit sends nothing", OffStyleOmit, "", false, false},
				{"empty style means omit", "", "", false, false},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					f := newSSEFixture(t, stopStream(protocol))
					req := baseRequest(f, protocol)
					req.Reasoning = Reasoning{Level: LevelOff, OffStyle: tc.style}
					runStream(t, f, req)

					body := decodeObject(t, f.last(t).Body)
					var effort string
					var reasoning map[string]any
					if protocol == ProtocolOpenAICompletions {
						if hasKey(body, "reasoning_effort") {
							effort = stringField(t, body, "reasoning_effort")
						}
						if hasKey(body, "reasoning") {
							reasoning = objectField(t, body, "reasoning")
						}
					} else {
						if hasKey(body, "reasoning") {
							reasoning = objectField(t, body, "reasoning")
							if hasKey(reasoning, "effort") {
								effort = stringField(t, reasoning, "effort")
							}
						}
					}

					if effort != tc.wantEffort {
						t.Fatalf("effort = %q, want %q (OffStyle %q, body %v)", effort, tc.wantEffort, tc.style, body)
					}
					if tc.wantToggle {
						if reasoning == nil {
							t.Fatalf("OffStyle=disabled must send the toggle object, body = %v", body)
						}
						if reasoning["enabled"] != false {
							t.Fatalf("toggle = %v, want enabled:false", reasoning)
						}
					} else if reasoning != nil && hasKey(reasoning, "enabled") {
						t.Fatalf("unexpected toggle object: %v", reasoning)
					}
				})
			}
		})
	}
}

// TestReasoningLevelsPassThrough: minimal is a real level (232 models publish
// it), so it must reach the wire untouched — never dropped, never downgraded.
func TestReasoningLevelsPassThrough(t *testing.T) {
	for _, protocol := range []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses} {
		for _, level := range []string{LevelMinimal, LevelLow, LevelMedium, LevelHigh} {
			t.Run(protocol+"/"+level, func(t *testing.T) {
				f := newSSEFixture(t, stopStream(protocol))
				req := baseRequest(f, protocol)
				// OffStyle is irrelevant while thinking is on.
				req.Reasoning = Reasoning{Level: level, OffStyle: OffStyleDisabled}
				runStream(t, f, req)

				body := decodeObject(t, f.last(t).Body)
				if protocol == ProtocolOpenAICompletions {
					if got := stringField(t, body, "reasoning_effort"); got != level {
						t.Fatalf("reasoning_effort = %q, want %q", got, level)
					}
					if hasKey(body, "reasoning") {
						t.Fatalf("toggle object must not accompany an effort level: %v", body)
					}
					return
				}
				reasoning := objectField(t, body, "reasoning")
				if got := stringField(t, reasoning, "effort"); got != level {
					t.Fatalf("reasoning.effort = %q, want %q", got, level)
				}
			})
		}
	}
}

// TestResponsesReasoningRequestsEncryptedContent: with store:false a reasoning
// item is only replayable when it carries encrypted_content, so the include must
// be there exactly when thinking is on.
func TestResponsesReasoningRequestsEncryptedContent(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolOpenAIResponses))
	req := baseRequest(f, ProtocolOpenAIResponses)
	req.Reasoning = Reasoning{Level: LevelMedium}
	runStream(t, f, req)

	include := arrayField(t, decodeObject(t, f.last(t).Body), "include")
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v, want [reasoning.encrypted_content]", include)
	}
}

// TestReasoningLevelEmptySendsNothing: "" is "do not intervene" — not "send the
// lowest tier".
func TestReasoningLevelEmptySendsNothing(t *testing.T) {
	for _, protocol := range []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages} {
		t.Run(protocol, func(t *testing.T) {
			f := newSSEFixture(t, stopStream(protocol))
			req := baseRequest(f, protocol)
			req.Reasoning = Reasoning{}
			runStream(t, f, req)

			body := decodeObject(t, f.last(t).Body)
			for _, key := range []string{"reasoning", "reasoning_effort", "thinking"} {
				if hasKey(body, key) {
					t.Fatalf("%s must be absent when Level is empty, body = %v", key, body)
				}
			}
		})
	}
}

// --- opaque block replay --------------------------------------------------

func TestAnthropicRawReplayIsVerbatim(t *testing.T) {
	raw := json.RawMessage(`[{"type":"thinking","thinking":"weigh the options","signature":"sig-abc"},{"type":"tool_use","id":"toolu_1","name":"get_time","input":{"tz":"UTC"}}]`)
	f := newSSEFixture(t, stopStream(ProtocolAnthropicMessages))
	req := baseRequest(f, ProtocolAnthropicMessages)
	req.MaxOutputTokens = 4096
	req.Reasoning = Reasoning{Level: LevelHigh}
	req.Messages = []Message{
		{Role: roleUser, Text: "hi"},
		{Role: roleAssistant, Raw: raw},
		{Role: roleTool, ToolResults: []ToolResult{{CallID: "toolu_1", Content: "12:00"}}},
	}
	runStream(t, f, req)

	got := f.last(t)
	// Byte level: the stored block's own bytes survive into the request body.
	for _, fragment := range []string{
		`"signature":"sig-abc"`,
		`"thinking":"weigh the options"`,
		`{"type":"tool_use","id":"toolu_1","name":"get_time","input":{"tz":"UTC"}}`,
	} {
		if !bytes.Contains(got.Body, []byte(fragment)) {
			t.Fatalf("request body must contain %s verbatim, body = %s", fragment, got.Body)
		}
	}

	// Structural level: the blocks are the request's own content array.
	body := decodeObject(t, got.Body)
	messages := arrayField(t, body, "messages")
	assistant := object(t, messages[1], "messages[1]")
	var want []any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode fixture raw: %v", err)
	}
	if !reflect.DeepEqual(arrayField(t, assistant, "content"), want) {
		t.Fatalf("assistant content = %v, want %v", assistant["content"], want)
	}

	// And the tool_result for that turn sits in the user turn right after it.
	result := object(t, messages[2], "messages[2]")
	if result["role"] != roleUser {
		t.Fatalf("messages[2] = %v, want the tool_result user turn", result)
	}
	block := object(t, arrayField(t, result, "content")[0], "messages[2].content[0]")
	if block["type"] != "tool_result" || block["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool_result block = %v", block)
	}
}

// TestAnthropicToolResultHoistedNextToItsToolUse: the upstream contract is
// "tool_result immediately after the assistant turn that asked for it". A caller
// that interleaved another message must not produce a request that 400s.
func TestAnthropicToolResultHoistedNextToItsToolUse(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolAnthropicMessages))
	req := baseRequest(f, ProtocolAnthropicMessages)
	req.MaxOutputTokens = 4096
	req.Messages = []Message{
		{Role: roleUser, Text: "hi"},
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "toolu_1", Name: "get_time"}}},
		{Role: roleUser, Text: "meanwhile"},
		{Role: roleTool, ToolResults: []ToolResult{{CallID: "toolu_1", Content: "12:00"}}},
	}
	runStream(t, f, req)

	messages := arrayField(t, decodeObject(t, f.last(t).Body), "messages")
	if len(messages) != 4 {
		t.Fatalf("messages = %v, want 4 turns", messages)
	}
	// [user hi] [assistant tool_use] [user tool_result] [user meanwhile]
	resultTurn := object(t, messages[2], "messages[2]")
	if resultTurn["role"] != roleUser {
		t.Fatalf("messages[2] must be the tool_result turn, got %v", resultTurn["role"])
	}
	block := object(t, arrayField(t, resultTurn, "content")[0], "messages[2].content[0]")
	if block["type"] != "tool_result" {
		t.Fatalf("messages[2] must carry the tool_result first, got %v", block)
	}
	later := object(t, messages[3], "messages[3]")
	laterBlock := object(t, arrayField(t, later, "content")[0], "messages[3].content[0]")
	if laterBlock["type"] != "text" || laterBlock["text"] != "meanwhile" {
		t.Fatalf("messages[3] = %v, want the displaced user text", laterBlock)
	}
}

func TestAnthropicOrphanToolResultIsRefused(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolAnthropicMessages))
	req := baseRequest(f, ProtocolAnthropicMessages)
	req.MaxOutputTokens = 4096
	req.Messages = []Message{
		{Role: roleUser, Text: "hi"},
		{Role: roleTool, ToolResults: []ToolResult{{CallID: "toolu_missing", Content: "12:00"}}},
	}
	err := Stream(context.Background(), f.Client(), req, func(Event) error { return nil })
	if err == nil {
		t.Fatal("a tool_result with no tool_use must be an error, not a request")
	}
	if !strings.Contains(err.Error(), "toolu_missing") {
		t.Fatalf("error should name the orphan call, got %v", err)
	}
	if len(f.requests()) != 0 {
		t.Fatal("nothing may be sent")
	}
}

func TestResponsesRawReplay(t *testing.T) {
	raw := json.RawMessage(`[{"type":"reasoning","id":"rs_1","encrypted_content":"enc-payload"},{"type":"function_call","call_id":"call_1","name":"get_time","arguments":"{}"}]`)
	f := newSSEFixture(t, stopStream(ProtocolOpenAIResponses))
	req := baseRequest(f, ProtocolOpenAIResponses)
	req.Reasoning = Reasoning{Level: LevelMedium}
	req.Messages = []Message{
		{Role: roleUser, Text: "hi"},
		{Role: roleAssistant, Raw: raw},
		{Role: roleTool, ToolResults: []ToolResult{{CallID: "call_1", Content: "12:00"}}},
	}
	runStream(t, f, req)

	body := decodeObject(t, f.last(t).Body)
	input := arrayField(t, body, "input")
	if len(input) != 4 {
		t.Fatalf("input = %v, want the user turn + 2 replayed items + the output item", input)
	}
	var want []any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode fixture raw: %v", err)
	}
	if !reflect.DeepEqual(input[1], want[0]) {
		t.Fatalf("input[1] = %v, want %v", input[1], want[0])
	}
	if !reflect.DeepEqual(input[2], want[1]) {
		t.Fatalf("input[2] = %v, want %v", input[2], want[1])
	}
}

// TestOpaqueBlocksFromAnotherProtocolAreRefused: §12.1 prevents this at the
// source (a new session per protocol); this is the backstop that fails loudly
// here instead of one request later upstream.
func TestOpaqueBlocksFromAnotherProtocolAreRefused(t *testing.T) {
	anthropicRaw := json.RawMessage(`[{"type":"thinking","thinking":"hmm","signature":"sig"}]`)
	responsesRaw := json.RawMessage(`[{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}]`)

	cases := []struct {
		name     string
		protocol string
		raw      json.RawMessage
	}{
		{"anthropic blocks into responses", ProtocolOpenAIResponses, anthropicRaw},
		{"responses blocks into anthropic", ProtocolAnthropicMessages, responsesRaw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSEFixture(t, stopStream(tc.protocol))
			req := baseRequest(f, tc.protocol)
			req.MaxOutputTokens = 4096
			req.Messages = []Message{
				{Role: roleUser, Text: "hi"},
				{Role: roleAssistant, Raw: tc.raw},
			}
			err := Stream(context.Background(), f.Client(), req, func(Event) error { return nil })
			if err == nil {
				t.Fatal("want an explicit error about the foreign block shape")
			}
			if len(f.requests()) != 0 {
				t.Fatal("nothing may be sent")
			}
		})
	}
}

// TestToolArgumentsDefaultToEmptyObject: "no arguments" must reach the wire as
// {}, in every dialect. An empty string (or a missing key) breaks the next turn.
func TestToolArgumentsDefaultToEmptyObject(t *testing.T) {
	for _, protocol := range []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages} {
		t.Run(protocol, func(t *testing.T) {
			f := newSSEFixture(t, stopStream(protocol))
			req := baseRequest(f, protocol)
			req.MaxOutputTokens = 4096
			req.Messages = []Message{
				{Role: roleUser, Text: "hi"},
				{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "noop"}}},
			}
			runStream(t, f, req)

			body := decodeObject(t, f.last(t).Body)
			switch protocol {
			case ProtocolOpenAICompletions:
				call := object(t, arrayField(t, object(t, arrayField(t, body, "messages")[1], "messages[1]"), "tool_calls")[0], "tool_calls[0]")
				if args := stringField(t, objectField(t, call, "function"), "arguments"); args != "{}" {
					t.Fatalf("arguments = %q, want {}", args)
				}
			case ProtocolOpenAIResponses:
				item := object(t, arrayField(t, body, "input")[1], "input[1]")
				if args := stringField(t, item, "arguments"); args != "{}" {
					t.Fatalf("arguments = %q, want {}", args)
				}
			default:
				content := arrayField(t, object(t, arrayField(t, body, "messages")[1], "messages[1]"), "content")
				input := objectField(t, object(t, content[0], "content[0]"), "input")
				if len(input) != 0 {
					t.Fatalf("tool_use input = %v, want {}", input)
				}
			}
		})
	}
}

// TestToolSchemaDefaultsToEmptyObject: a tool declared without a schema still
// needs a JSON Schema object, or the provider rejects the whole request.
func TestToolSchemaDefaultsToEmptyObject(t *testing.T) {
	want := map[string]any{"type": "object", "properties": map[string]any{}}
	for _, protocol := range []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages} {
		t.Run(protocol, func(t *testing.T) {
			f := newSSEFixture(t, stopStream(protocol))
			req := baseRequest(f, protocol)
			req.Tools = []ToolSpec{{Name: "noop"}}
			runStream(t, f, req)

			body := decodeObject(t, f.last(t).Body)
			tool := object(t, arrayField(t, body, "tools")[0], "tools[0]")
			key := "parameters"
			if protocol == ProtocolAnthropicMessages {
				key = "input_schema"
			}
			// The empty schema never carries a "description" key at this level:
			// check the whole object to catch a wrong nesting.
			var got map[string]any
			if protocol == ProtocolOpenAICompletions {
				got = objectField(t, objectField(t, tool, "function"), key)
			} else {
				got = objectField(t, tool, key)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s = %v, want %v", key, got, want)
			}
		})
	}
}

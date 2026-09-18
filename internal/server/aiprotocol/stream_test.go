package aiprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Streaming shape per protocol (design.md §12.5): text deltas, thinking deltas,
// fragmented tool arguments, and the turn's terminal event.

func TestStreamOpenAICompletions(t *testing.T) {
	f := newSSEFixture(t, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":"weigh "},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":"options"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`)
	req := baseRequest(f, ProtocolOpenAICompletions)
	events := runStream(t, f, req)

	want := []string{KindText, KindThinking, KindThinking, KindToolCall, KindDone}
	if got := (&eventRecorder{events: events}).kinds(); !equalStrings(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	rec := &eventRecorder{events: events}
	if got := rec.text(); got != "Hello" {
		t.Fatalf("text = %q", got)
	}
	if got := rec.thinking(); got != "weigh options" {
		t.Fatalf("thinking = %q", got)
	}
	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want 1", calls)
	}
	if calls[0].ID != "call_1" || calls[0].Name != "get_time" {
		t.Fatalf("call = %+v", calls[0])
	}
	assertJSONEqual(t, calls[0].Arguments, `{"tz":"UTC"}`)

	if done := rec.first(KindDone); done == nil {
		t.Fatal("no done event")
	} else if len(done.Raw) != 0 {
		t.Fatalf("completions has no native block to replay, done.Raw = %s", done.Raw)
	}
}

// TestStreamOpenAICompletionsToolCallWithoutArguments: a call whose fragments
// never arrive still has to reach the caller as {}.
func TestStreamOpenAICompletionsToolCallWithoutArguments(t *testing.T) {
	f := newSSEFixture(t, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"noop"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`)
	events := runStream(t, f, baseRequest(f, ProtocolOpenAICompletions))
	calls := (&eventRecorder{events: events}).calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
	assertJSONEqual(t, calls[0].Arguments, `{}`)
}

// TestStreamOpenAICompletionsTruncated: no finish_reason and no [DONE] means the
// turn was cut off; half-assembled arguments must not be presented as complete.
func TestStreamOpenAICompletionsTruncated(t *testing.T) {
	f := newSSEFixture(t, `data: {"choices":[{"index":0,"delta":{"content":"half"},"finish_reason":null}]}

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolOpenAICompletions), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("want a truncation error, got %v", err)
	}
}

// TestStreamOpenAICompletionsInvalidToolArguments: the fragments never form JSON.
// The error must name the call, and must not be swallowed into an empty object.
func TestStreamOpenAICompletionsInvalidToolArguments(t *testing.T) {
	f := newSSEFixture(t, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolOpenAICompletions), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("want a JSON error, got %v", err)
	}
	if !strings.Contains(err.Error(), "get_time") {
		t.Fatalf("error should name the tool, got %v", err)
	}
}

func TestStreamAnthropic(t *testing.T) {
	f := newSSEFixture(t, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weigh "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"options"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-xyz"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me check."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_time","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"UTC\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`)
	req := baseRequest(f, ProtocolAnthropicMessages)
	events := runStream(t, f, req)
	rec := &eventRecorder{events: events}

	want := []string{KindThinking, KindThinking, KindText, KindToolCall, KindDone}
	if got := rec.kinds(); !equalStrings(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	if got := rec.thinking(); got != "weigh options" {
		t.Fatalf("thinking = %q", got)
	}
	if got := rec.text(); got != "Let me check." {
		t.Fatalf("text = %q", got)
	}
	calls := rec.calls()
	if len(calls) != 1 || calls[0].ID != "toolu_1" || calls[0].Name != "get_time" {
		t.Fatalf("calls = %+v", calls)
	}
	assertJSONEqual(t, calls[0].Arguments, `{"tz":"UTC"}`)

	// The turn's native blocks must come back complete — signature included,
	// because the next tool-loop request replays them verbatim.
	done := rec.first(KindDone)
	if done == nil || len(done.Raw) == 0 {
		t.Fatal("done must carry the assembled native blocks")
	}
	blocks := decodeObjectArray(t, done.Raw)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %s, want 3", done.Raw)
	}
	if blocks[0]["type"] != "thinking" || blocks[0]["thinking"] != "weigh options" || blocks[0]["signature"] != "sig-xyz" {
		t.Fatalf("thinking block = %v (signature_delta must survive)", blocks[0])
	}
	if blocks[1]["type"] != "text" || blocks[1]["text"] != "Let me check." {
		t.Fatalf("text block = %v", blocks[1])
	}
	if blocks[2]["type"] != "tool_use" || blocks[2]["id"] != "toolu_1" || blocks[2]["name"] != "get_time" {
		t.Fatalf("tool_use block = %v", blocks[2])
	}
	if input, ok := blocks[2]["input"].(map[string]any); !ok || input["tz"] != "UTC" {
		t.Fatalf("tool_use input = %v", blocks[2]["input"])
	}
}

// TestStreamAnthropicToolUseWithoutInput: no input_json_delta at all.
func TestStreamAnthropicToolUseWithoutInput(t *testing.T) {
	f := newSSEFixture(t, `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"noop","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`)
	events := runStream(t, f, baseRequest(f, ProtocolAnthropicMessages))
	calls := (&eventRecorder{events: events}).calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
	assertJSONEqual(t, calls[0].Arguments, `{}`)
}

// TestStreamAnthropicInvalidToolJSON: the input fragments never form a JSON
// object, so the turn must fail here rather than upstream on the next request.
func TestStreamAnthropicInvalidToolJSON(t *testing.T) {
	f := newSSEFixture(t, `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_time","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolAnthropicMessages), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("want a JSON error, got %v", err)
	}
	if !strings.Contains(err.Error(), "get_time") {
		t.Fatalf("error should name the tool, got %v", err)
	}
}

func TestStreamAnthropicTruncated(t *testing.T) {
	f := newSSEFixture(t, `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half"}}

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolAnthropicMessages), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("want a truncation error, got %v", err)
	}
}

// TestStreamAnthropicErrorEvent: upstream errors arrive on the stream, and their
// text may echo the credential.
func TestStreamAnthropicErrorEventRedactsKey(t *testing.T) {
	f := newSSEFixture(t, `event: error
data: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key: `+testAPIKey+`"}}

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolAnthropicMessages), func(Event) error { return nil })
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoSecret(t, err.Error())
}

func TestStreamOpenAIResponses(t *testing.T) {
	f := newSSEFixture(t, `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"weigh "}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"options"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-1","summary":[{"type":"summary_text","text":"weigh options"}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"Hello"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_time","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"tz\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"\"UTC\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":2,"arguments":"{\"tz\":\"UTC\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}

event: response.completed
data: {"type":"response.completed","response":{"output":[{"id":"rs_1","type":"reasoning","encrypted_content":"enc-1"},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_time","arguments":"{\"tz\":\"UTC\"}"}]}}

`)
	events := runStream(t, f, baseRequest(f, ProtocolOpenAIResponses))
	rec := &eventRecorder{events: events}

	if got := rec.thinking(); got != "weigh options" {
		t.Fatalf("thinking = %q", got)
	}
	if got := rec.text(); got != "Hello" {
		t.Fatalf("text = %q", got)
	}
	calls := rec.calls()
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Name != "get_time" {
		t.Fatalf("calls = %+v", calls)
	}
	assertJSONEqual(t, calls[0].Arguments, `{"tz":"UTC"}`)

	// The replayed item must keep encrypted_content: with store:false that is
	// the only thing that makes the reasoning item reusable.
	done := rec.first(KindDone)
	if done == nil || len(done.Raw) == 0 {
		t.Fatal("done must carry the output items")
	}
	items := decodeObjectArray(t, done.Raw)
	if len(items) != 3 {
		t.Fatalf("items = %s, want 3", done.Raw)
	}
	if items[0]["encrypted_content"] != "enc-1" {
		t.Fatalf("reasoning item = %v", items[0])
	}
}

// TestStreamResponsesReasoningTextDelta covers the other spelling of streamed
// reasoning.
func TestStreamResponsesReasoningTextDelta(t *testing.T) {
	f := newSSEFixture(t, `event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","item_id":"rs_1","output_index":0,"delta":"hmm"}

event: response.completed
data: {"type":"response.completed","response":{"output":[]}}

`)
	events := runStream(t, f, baseRequest(f, ProtocolOpenAIResponses))
	if got := (&eventRecorder{events: events}).thinking(); got != "hmm" {
		t.Fatalf("thinking = %q", got)
	}
}

// TestStreamResponsesCallOnlyInCompleted: a gateway that never sends
// output_item.done still has the finished call in the terminal event.
func TestStreamResponsesCallOnlyInCompleted(t *testing.T) {
	f := newSSEFixture(t, `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_time","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"tz\":\"UTC\"}"}

event: response.completed
data: {"type":"response.completed","response":{"output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_time","arguments":"{\"tz\":\"UTC\"}"}]}}

`)
	events := runStream(t, f, baseRequest(f, ProtocolOpenAIResponses))
	calls := (&eventRecorder{events: events}).calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	assertJSONEqual(t, calls[0].Arguments, `{"tz":"UTC"}`)
}

func TestStreamResponsesTruncated(t *testing.T) {
	f := newSSEFixture(t, `event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"half"}

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolOpenAIResponses), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("want a truncation error, got %v", err)
	}
}

func TestStreamResponsesFailedRedactsKey(t *testing.T) {
	f := newSSEFixture(t, `event: response.failed
data: {"type":"response.failed","response":{"error":{"code":"invalid_api_key","message":"Incorrect API key provided: `+testAPIKey+`"}}}

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolOpenAIResponses), func(Event) error { return nil })
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoSecret(t, err.Error())
}

// TestStreamRejectsNonEventStream: a 2xx that ignored stream:true must say so,
// not be parsed as an empty event stream.
func TestStreamRejectsNonEventStream(t *testing.T) {
	f := newJSONFixture(t, http.StatusOK, `{"choices":[{"message":{"content":"hi"}}]}`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolOpenAICompletions), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "text/event-stream") {
		t.Fatalf("want a content-type error, got %v", err)
	}
}

// TestStreamRespectsContextCancellation: the loop stops by cancelling ctx, and
// the error must still satisfy errors.Is(err, context.Canceled).
func TestStreamRespectsContextCancellation(t *testing.T) {
	reached := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		close(reached)
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Stream(ctx, f.Client(), baseRequest(f, ProtocolOpenAICompletions), func(Event) error { return nil })
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the server")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled in the chain, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after cancellation")
	}
}

// --- helpers --------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotAny, wantAny any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatalf("got %s is not valid JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantAny); err != nil {
		t.Fatalf("want %s is not valid JSON: %v", want, err)
	}
	if !jsonEqual(gotAny, wantAny) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func decodeObjectArray(t *testing.T, raw json.RawMessage) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// assertNoSecret is the redaction assertion used by every leak test.
func assertNoSecret(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, testAPIKey) {
		t.Fatalf("credential leaked into %q", text)
	}
	// The key is also recognized in its stripped form (the fixture never strips
	// it, so an unredacted body trips this).
	if strings.Contains(text, "0123456789") {
		t.Fatalf("credential fragment leaked into %q", text)
	}
}

// TestStreamOpenAICompletionsTolerantReasoning: a gateway that publishes
// reasoning as an object (OpenRouter's reasoning_details shape) must not make
// the chunk undecodable — that would throw away every delta of the turn.
func TestStreamOpenAICompletionsTolerantReasoning(t *testing.T) {
	f := newSSEFixture(t, `data: {"choices":[{"index":0,"delta":{"reasoning":{"text":"wrapped"}},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`)
	events := runStream(t, f, baseRequest(f, ProtocolOpenAICompletions))
	rec := &eventRecorder{events: events}
	if got := rec.thinking(); got != "" {
		t.Fatalf("thinking = %q, want the non-string shape ignored", got)
	}
	if got := rec.text(); got != "Hello" {
		t.Fatalf("text = %q, want the turn to continue", got)
	}
}

// TestStreamOpenAICompletionsErrorLine: a failure delivered as a data line, with
// the credential echoed in it.
func TestStreamOpenAICompletionsErrorLine(t *testing.T) {
	f := newSSEFixture(t, `data: {"error":{"type":"invalid_request_error","message":"bad key `+testAPIKey+`"}}

`)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolOpenAICompletions), func(Event) error { return nil })
	if err == nil {
		t.Fatal("want the upstream error")
	}
	assertNoSecret(t, err.Error())
	if !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("error should carry the upstream type, got %v", err)
	}
}

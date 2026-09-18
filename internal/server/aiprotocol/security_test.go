package aiprotocol

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Request-header safety and credential hygiene (hard requirements 1 and 5).

// TestExtraHeadersCannotOverrideAuthHeaders: a collision is refused outright —
// ignoring the extra header would leave the operator believing it applied.
func TestExtraHeadersCannotOverrideAuthHeaders(t *testing.T) {
	names := []string{"Authorization", "authorization", "AUTHORIZATION", "x-api-key", "X-Api-Key", "anthropic-version"}
	for _, protocol := range []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages} {
		for _, name := range names {
			t.Run(protocol+"/"+name, func(t *testing.T) {
				f := newSSEFixture(t, stopStream(protocol))
				req := baseRequest(f, protocol)
				req.Headers = map[string]string{name: "Bearer attacker-controlled"}
				err := Stream(context.Background(), f.Client(), req, func(Event) error { return nil })
				if err == nil {
					t.Fatalf("extra header %q must be refused", name)
				}
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("error should name the offending header, got %v", err)
				}
				if got := len(f.requests()); got != 0 {
					t.Fatalf("%d requests were sent despite the refusal", got)
				}
			})
		}
	}
}

// TestExtraHeadersAreAppliedOnTop: everything outside the reserved set is passed
// through unchanged (extra_headers is how gateways with their own auth (e.g. an
// organisation header) are configured).
func TestExtraHeadersAreAppliedOnTop(t *testing.T) {
	f := newSSEFixture(t, stopStream(ProtocolOpenAICompletions))
	req := baseRequest(f, ProtocolOpenAICompletions)
	req.Headers = map[string]string{"X-Trace-Id": "abc123", "OpenAI-Organization": "org-1"}
	runStream(t, f, req)

	got := f.last(t)
	if v := got.Header.Get("X-Trace-Id"); v != "abc123" {
		t.Fatalf("X-Trace-Id = %q", v)
	}
	if v := got.Header.Get("OpenAI-Organization"); v != "org-1" {
		t.Fatalf("OpenAI-Organization = %q", v)
	}
	if v := got.Header.Get("Authorization"); v != "Bearer "+testAPIKey {
		t.Fatalf("Authorization = %q, the protocol's own header must win", v)
	}
	if v := got.Header.Get("Content-Type"); v != "application/json" {
		t.Fatalf("Content-Type = %q", v)
	}
	if v := got.Header.Get("Accept"); v != "text/event-stream" {
		t.Fatalf("Accept = %q", v)
	}
}

// TestAPIKeyNeverAppearsInErrors: the two leak paths that matter — a non-2xx
// body that echoes the credential, and a transport-level failure.
func TestAPIKeyNeverAppearsInErrors(t *testing.T) {
	protocols := []string{ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages}
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			body := `{"error":{"message":"Incorrect API key provided: ` + testAPIKey + `","type":"invalid_request_error"}}`
			f := newJSONFixture(t, http.StatusUnauthorized, body)
			req := baseRequest(f, protocol)
			err := Stream(context.Background(), f.Client(), req, func(Event) error { return nil })
			if err == nil {
				t.Fatal("want an error for http 401")
			}
			assertNoSecret(t, err.Error())
			if !strings.Contains(err.Error(), redactedMarker) {
				t.Fatalf("expected the redaction marker in %q", err.Error())
			}
		})
	}

	t.Run("models endpoint", func(t *testing.T) {
		f := newJSONFixture(t, http.StatusUnauthorized, `{"error":{"message":"bad key `+testAPIKey+`"}}`)
		req := baseRequest(f, ProtocolOpenAICompletions)
		if _, err := FetchModels(context.Background(), f.Client(), req); err == nil {
			t.Fatal("want an error")
		} else {
			assertNoSecret(t, err.Error())
		}
	})

	t.Run("transport error", func(t *testing.T) {
		// A URL that cannot connect: the transport error is an arbitrary string.
		req := Request{
			BaseURL:  "http://127.0.0.1:1",
			APIKey:   testAPIKey,
			Protocol: ProtocolOpenAICompletions,
			Model:    "m",
		}
		err := Stream(context.Background(), &http.Client{}, req, func(Event) error { return nil })
		if err == nil {
			t.Fatal("want a transport error")
		}
		assertNoSecret(t, err.Error())
	})
}

// TestErrorBodyIsRedactedBeforeTruncation: truncating first can split the key
// and leave its readable prefix in the message.
func TestErrorBodyIsRedactedBeforeTruncation(t *testing.T) {
	// The key sits beyond the truncation point; redaction must still have run.
	padding := strings.Repeat("x", maxErrorMessageBytes+64)
	body := `{"error":{"message":"` + padding + ` key=` + testAPIKey + `"}}`
	f := newJSONFixture(t, http.StatusBadRequest, body)
	err := Stream(context.Background(), f.Client(), baseRequest(f, ProtocolAnthropicMessages), func(Event) error { return nil })
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoSecret(t, err.Error())
	if len(err.Error()) > maxErrorMessageBytes+512 {
		t.Fatalf("error message was not truncated: %d bytes", len(err.Error()))
	}
}

func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		text string
		key  string
	}{
		{"exact key", `{"message":"Incorrect API key provided: ` + testAPIKey + `"}`, testAPIKey},
		{"bearer echo", `Authorization: Bearer sk-abcdefghijklmnop`, ""},
		{"json field echo", `{"api_key":"sk-abcdefghijklmnop"}`, ""},
		{"x-api-key echo", `x-api-key: sk-abcdefghijklmnop`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(tc.text, tc.key)
			if strings.Contains(got, "sk-") {
				t.Fatalf("redact(%q) = %q, still contains a credential", tc.text, got)
			}
			if !strings.Contains(got, redactedMarker) {
				t.Fatalf("redact(%q) = %q, want the marker", tc.text, got)
			}
		})
	}

	// A message that merely mentions "API key" without a value must not be
	// mangled: net 1 is what removes real keys from sentences.
	plain := "the API key provided is wrong"
	if got := redact(plain, ""); got != plain {
		t.Fatalf("redact(%q) = %q, want it untouched", plain, got)
	}
}

// TestRedactedErrorKeepsTheChain: the loop detects "user pressed stop" with
// errors.Is, so sanitizing an error must not break Unwrap.
func TestRedactedErrorKeepsTheChain(t *testing.T) {
	wrapped := redactError(errors.New("boom: "+testAPIKey), testAPIKey)
	if !strings.Contains(wrapped.Error(), redactedMarker) {
		t.Fatalf("message not redacted: %v", wrapped)
	}
	if !errors.Is(wrapped, wrapped) {
		t.Fatal("Unwrap chain broken")
	}
	ctxErr := redactError(context.Canceled, testAPIKey)
	if !errors.Is(ctxErr, context.Canceled) {
		t.Fatalf("errors.Is(%v, context.Canceled) = false", ctxErr)
	}
}

// TestStreamRejectsMissingInputs: no silent fallbacks — a nil client would mean
// "no timeout" on the one call that can hang, and a nil callback would discard
// the whole stream.
func TestStreamRejectsMissingInputs(t *testing.T) {
	req := Request{BaseURL: "http://127.0.0.1:1/v1", Protocol: ProtocolOpenAICompletions, Model: "m"}
	if err := Stream(context.Background(), nil, req, func(Event) error { return nil }); err == nil {
		t.Fatal("want an error for a nil http client")
	}
	if err := Stream(context.Background(), &http.Client{}, req, nil); err == nil {
		t.Fatal("want an error for a nil callback")
	}
	if _, err := FetchModels(context.Background(), nil, req); err == nil {
		t.Fatal("want an error for a nil http client")
	}
}

func TestUnknownProtocolIsRejected(t *testing.T) {
	f := newSSEFixture(t, "")
	req := baseRequest(f, "gemini-generate-content")
	err := Stream(context.Background(), f.Client(), req, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("Stream: want an unknown-protocol error, got %v", err)
	}
	if _, err := FetchModels(context.Background(), f.Client(), req); err == nil || !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("FetchModels: want an unknown-protocol error, got %v", err)
	}
}

func TestEmptyBaseURLIsRejected(t *testing.T) {
	req := Request{Protocol: ProtocolOpenAICompletions, Model: "m"}
	err := Stream(context.Background(), &http.Client{}, req, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("want a base_url error, got %v", err)
	}
}

package aiprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Stream is the package's entry point: one upstream call, one callback per
// increment, until the model stops (returns nil) or something goes wrong.
//
// The caller owns the http.Client, on purpose (§12.6): the timeout policy is an
// idle timeout per upstream call plus a per-turn wall clock, both of which live
// in the assistant loop. Falling back to http.DefaultClient here would turn
// "no timeout" into the default behaviour of the one call that can hang for
// minutes, so a nil client is an error rather than a convenience.
func Stream(ctx context.Context, client *http.Client, req Request, onEvent func(Event) error) error {
	if onEvent == nil {
		return errors.New("aiprotocol: nil onEvent callback")
	}
	d, err := dialectFor(req.Protocol)
	if err != nil {
		return err
	}
	endpoint, err := endpointURL(req.BaseURL, d.path)
	if err != nil {
		return err
	}
	body, err := d.encode(req)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", req.Protocol, err)
	}
	if client == nil {
		return errors.New("aiprotocol: nil http client (the caller supplies the timeout policy)")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request for %s: %w", req.Protocol, endpoint, err)
	}
	if err := applyAuthHeaders(httpReq.Header, req, d.auth); err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Accept must not be settable by extra_headers: a wrong one is what makes a
	// gateway buffer or refuse the stream.
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(httpReq)
	if err != nil {
		// The transport error embeds the URL, never our headers, but it is an
		// arbitrary string built from whatever the network returned: redact it
		// while keeping the chain (errors.Is(err, context.Canceled) is how the
		// loop notices the user pressed stop).
		return fmt.Errorf("%s %s: %w", req.Protocol, endpoint, redactError(err, req.APIKey))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return upstreamError(req.Protocol, endpoint, resp, req.APIKey)
	}
	// A 2xx that is not an event stream means the gateway ignored stream:true.
	// Saying so explicitly beats feeding a JSON body to the SSE reader and
	// reporting "empty stream" — the operator can act on this.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf(
			"%s %s: expected text/event-stream, got %q; the upstream ignored stream:true (body starts %.200q)",
			req.Protocol, endpoint, ct, redact(string(snippet), req.APIKey),
		)
	}

	if err := d.stream(ctx, resp.Body, req.APIKey, onEvent); err != nil {
		return fmt.Errorf("%s %s: %w", req.Protocol, endpoint, err)
	}
	return nil
}

// encodeFunc builds one request body.
type encodeFunc func(req Request) ([]byte, error)

// streamFunc parses one event stream.
type streamFunc func(ctx context.Context, body io.Reader, key string, onEvent func(Event) error) error

// dialect is one protocol's wire facts: where it lives, how it authenticates,
// and the two codecs.
type dialect struct {
	path   string
	auth   authStyle
	encode encodeFunc
	stream streamFunc
}

// dialectFor is the single place the three protocols diverge (§12.5's table).
func dialectFor(protocol string) (dialect, error) {
	switch protocol {
	case ProtocolOpenAICompletions:
		return dialect{
			path:   "/chat/completions",
			auth:   authBearer,
			encode: encodeOpenAICompletions,
			stream: streamOpenAICompletions,
		}, nil
	case ProtocolOpenAIResponses:
		return dialect{
			path:   "/responses",
			auth:   authBearer,
			encode: encodeOpenAIResponses,
			stream: streamOpenAIResponses,
		}, nil
	case ProtocolAnthropicMessages:
		return dialect{
			path:   "/messages",
			auth:   authAnthropic,
			encode: encodeAnthropicMessages,
			stream: streamAnthropicMessages,
		}, nil
	default:
		return dialect{}, fmt.Errorf(
			"aiprotocol: unknown protocol %q (want %s, %s or %s)",
			protocol, ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages,
		)
	}
}

// endpointURL joins the panel's ROOT base_url with one protocol path. Providers
// store the root ("https://api.openai.com/v1"), never a full endpoint — the form
// shows the resolved URL so the operator can see this join happen (§12.1).
func endpointURL(base, path string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("aiprotocol: empty base_url (the provider row stores the root, e.g. https://api.openai.com/v1)")
	}
	return strings.TrimRight(base, "/") + path, nil
}

// maxErrorBodyBytes / maxErrorMessageBytes bound how much of a failing upstream
// answer is quoted back. A wrong base_url often answers with a full HTML page.
const (
	maxErrorBodyBytes    = 64 << 10
	maxErrorMessageBytes = 2048
)

// UpstreamError is a failing (or unreadable) answer from a provider endpoint.
//
// Error() is deliberately BODY-FREE. It is forwarded to the panel verbatim as
// ai_upstream_error, so interpolating the response body meant "point a provider
// at an internal address and press the test button" displayed that service's
// answer in the panel — an SSRF read channel rather than a debugging aid
// (§12.5 实现修订 2026-09-20). The redacted, truncated body travels separately in
// Snippet, which only ever goes to the server log.
type UpstreamError struct {
	Protocol string
	Endpoint string
	Status   int
	Reason   string // short, panel-safe
	Snippet  string // log only: redacted body excerpt
}

func (e *UpstreamError) Error() string {
	msg := fmt.Sprintf("%s %s: upstream returned http %d", e.Protocol, e.Endpoint, e.Status)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// Snippet returns the log-only detail of an UpstreamError ("" for anything
// else). It must never be sent to the panel.
func Snippet(err error) string {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Snippet
	}
	return ""
}

// upstreamError turns a non-2xx answer into an error whose body is redacted
// first and truncated second (never the other way round: truncation can split a
// key and leave its prefix readable).
func upstreamError(protocol, endpoint string, resp *http.Response, key string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return &UpstreamError{
		Protocol: protocol, Endpoint: endpoint, Status: resp.StatusCode,
		Snippet: truncate(redact(strings.TrimSpace(string(raw)), key), maxErrorMessageBytes),
	}
}

// decodeJSONBody reads a small non-streaming body (the model list) with the
// same redaction discipline.
func decodeJSONBody(protocol, endpoint string, resp *http.Response, key string, out any) error {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return upstreamError(protocol, endpoint, resp, key)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", protocol, endpoint, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// The body excerpt is log-only here too: a "not JSON" answer is exactly
		// what an internal service replies with (§12.5 实现修订 2026-09-20).
		return &UpstreamError{
			Protocol: protocol, Endpoint: endpoint, Status: resp.StatusCode,
			Reason:  "the response is not valid JSON",
			Snippet: fmt.Sprintf("decode: %v (body starts %.200q)", err, redact(string(raw), key)),
		}
	}
	return nil
}

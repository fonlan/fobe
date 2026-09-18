package aiprotocol

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Header assembly. One rule matters: the protocol's own auth headers are not
// negotiable.
//
// Request.Headers is a provider's extra_headers (§12.1 — values encrypted at
// rest precisely because they are credentials). A collision with this adapter's
// auth header is REJECTED rather than ignored: silently dropping the extra
// header would leave the operator believing it applied, and the visible symptom
// of the other choice (letting it win) is "the key is filled in but the provider
// still answers 401" with nothing pointing at the cause.
//
// The framing headers (Content-Type / Accept) are not in the reserved set —
// they are simply written after the extras, so extras cannot break the codec
// either, without an honest mistake there costing an error.

// authStyle is how this adapter authenticates to one dialect.
type authStyle int

const (
	// authBearer is Authorization: Bearer <key> (both OpenAI dialects).
	authBearer authStyle = iota
	// authAnthropic is x-api-key + anthropic-version. The version header is sent
	// even with no key: it selects the dialect, and Anthropic requires it on
	// every request.
	authAnthropic
)

// isReservedAuthHeader reports whether name belongs to the adapter. The check
// is case-insensitive because HTTP header names are, and it is applied
// regardless of the protocol in use: a provider's protocol can be switched in
// the form later, and a stale Authorization entry must not silently start
// taking effect at that point.
func isReservedAuthHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "x-api-key", "anthropic-version":
		return true
	default:
		return false
	}
}

// applyAuthHeaders copies the extras and then writes the protocol's own auth.
//
// An empty APIKey omits the credential header entirely instead of sending
// "Bearer " / "x-api-key: ": local OpenAI-compatible servers (and the panel's
// own tests) are happy without one, while an empty-but-present header is
// rejected by some gateways.
func applyAuthHeaders(h http.Header, req Request, style authStyle) error {
	reserved := make([]string, 0, 1)
	for name := range req.Headers {
		if isReservedAuthHeader(name) {
			reserved = append(reserved, name)
		}
	}
	if len(reserved) > 0 {
		// Sorted so the message is deterministic (Go randomizes map order).
		sort.Strings(reserved)
		return fmt.Errorf(
			"aiprotocol: extra header(s) %s would replace the protocol's own auth header; remove them from extra_headers",
			strings.Join(reserved, ", "),
		)
	}

	for name, value := range req.Headers {
		h.Set(name, value)
	}

	switch style {
	case authBearer:
		if req.APIKey != "" {
			h.Set("Authorization", "Bearer "+req.APIKey)
		}
	case authAnthropic:
		h.Set("anthropic-version", anthropicVersion)
		if req.APIKey != "" {
			h.Set("x-api-key", req.APIKey)
		}
	}
	return nil
}

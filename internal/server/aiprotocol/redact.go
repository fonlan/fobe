package aiprotocol

import (
	"regexp"
	"strings"
)

// Redaction. Nothing this package returns may carry the provider's API key: a
// gateway that echoes the credential in its error body ("Incorrect API key
// provided: sk-…") would otherwise put it into the panel's logs and into the
// model's context through the assistant's error text.
//
// Two nets, in this order:
//
//  1. the exact key, replaced wherever it appears. This is the one that catches
//     an arbitrary credential shape and the reason redaction happens BEFORE any
//     truncation — truncating first can cut a key in half and leave the prefix
//     visible.
//  2. the shapes a key echo usually takes (Bearer <token>, "api_key":"…"), which
//     catch a *different* credential that happens to travel in the same error
//     body.

// redactedMarker replaces anything that looks like a credential.
const redactedMarker = "[redacted]"

var (
	// bearerPattern catches "Authorization: Bearer <token>" style echoes.
	bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	// keyFieldPattern catches `"api_key":"…"` / `x-api-key: …` style echoes.
	// It deliberately requires the ':' or '=' right after the field name: a
	// message like "Incorrect API key provided: sk-…" must not be mangled here
	// (net 1 already removed the real key from it).
	keyFieldPattern = regexp.MustCompile(`(?i)\b(x-api-key|api[-_]?key)("?\s*[:=]\s*"?)[A-Za-z0-9._~+/=-]{8,}`)
)

// redact removes the key and credential-shaped text from s.
func redact(s, key string) string {
	if s == "" {
		return s
	}
	if key != "" {
		s = strings.ReplaceAll(s, key, redactedMarker)
	}
	s = bearerPattern.ReplaceAllString(s, "Bearer "+redactedMarker)
	s = keyFieldPattern.ReplaceAllString(s, "${1}${2}"+redactedMarker)
	return s
}

// redactedError wraps an error so its message is sanitized while the chain
// survives: errors.Is(err, context.Canceled) is how the assistant loop detects
// "the user pressed stop", and a plain fmt.Errorf("%s", …) would break it.
type redactedError struct {
	err error
	key string
}

func (e redactedError) Error() string { return redact(e.err.Error(), e.key) }

func (e redactedError) Unwrap() error { return e.err }

// redactError returns err with a sanitized message, or err unchanged when there
// is nothing to redact.
func redactError(err error, key string) error {
	if err == nil || key == "" {
		return err
	}
	return redactedError{err: err, key: key}
}

// truncate caps s at n bytes, appending an ellipsis so a cut body is visibly
// cut. Callers redact first (see the file comment).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

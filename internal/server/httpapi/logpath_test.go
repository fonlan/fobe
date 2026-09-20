package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The access log must never carry a subscription token: /sub/<token> IS that
// subscription's credential (design §16). Both log branches — the debug line and
// the 5xx error — go through logPath, so neither request outcome can leak it.
func TestAccessLogRedactsSubscriptionToken(t *testing.T) {
	const token = "supersecrettoken"

	for _, tc := range []struct {
		name   string
		status int
	}{
		{"debug line (2xx)", http.StatusOK},
		{"error line (5xx)", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			req := httptest.NewRequest(http.MethodGet, "/sub/"+token, nil)
			logRequests(log, next).ServeHTTP(httptest.NewRecorder(), req)

			out := buf.String()
			if strings.Contains(out, token) {
				t.Fatalf("the subscription token leaked into the access log: %s", out)
			}
			if !strings.Contains(out, "/sub/[redacted]") {
				t.Fatalf("want the redacted path in the log, got: %s", out)
			}
		})
	}
}

// A path that carries no credential is still logged as-is, so the log stays
// useful for debugging.
func TestAccessLogKeepsOrdinaryPaths(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	logRequests(log, next).ServeHTTP(httptest.NewRecorder(), req)

	if out := buf.String(); !strings.Contains(out, "/api/nodes") {
		t.Fatalf("ordinary path missing from the log: %s", out)
	}
}

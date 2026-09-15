// Tests for the last-resort handler: the SPA / API-only page must never answer
// an /api request (design §19.6: the API returns structured data and snake_case
// codes, always).
package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// TestUnknownAPIPathIsJSON pins the fallout that motivated the guard: a server
// whose route table predates the caller (a stale dev process, an image that was
// not rebuilt) used to answer an unknown /api path with the API-only HTML page
// and HTTP 200. The panel then failed to parse it and reported "cannot reach
// the server", which sent everyone looking for a network problem that did not
// exist.
func TestUnknownAPIPathIsJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, tc := range []struct {
		method, path string
	}{
		{"POST", "/api/geoip/refresh"},     // never existed
		{"GET", "/api/geoip/status/extra"}, // deeper than any route
		{"POST", "/api/"},                  // bare, trailing slash
		{"PUT", "/api/nodes/1/reboot"},     // a plausible-but-absent action route
	} {
		resp, raw := doAuthed(t, tc.method, srv.URL+tc.path, "", nil)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s %s: content-type = %q, want JSON (body: %s)", tc.method, tc.path, ct, raw)
		}
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(raw), "unknown_endpoint") {
			t.Fatalf("%s %s: got %d %s, want 404 unknown_endpoint", tc.method, tc.path, resp.StatusCode, raw)
		}
	}

	// A wrong method on an existing route also lands here rather than on the
	// mux's automatic 405: the catch-all "/" pattern matches the path, so the
	// mux never reaches its "path matched, method did not" branch. The point of
	// the assertion is the shape of the answer — a status with a JSON code,
	// never the HTML page.
	resp, raw := doAuthed(t, "GET", srv.URL+"/api/geoip/update", "", nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(raw), "unknown_endpoint") {
		t.Fatalf("GET on a POST route: got %d %s, want 404 unknown_endpoint", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("GET on a POST route answered HTML (%q)", ct)
	}
}

// TestStaticPageStillServesRoot keeps the guard narrow: the panel itself (and
// the API-only hint) must keep working.
func TestStaticPageStillServesRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, raw := doAuthed(t, "GET", srv.URL+"/", "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /: got %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(raw), "fobe") {
		t.Fatalf("GET / body = %s, want the panel/API-only page", raw)
	}
}

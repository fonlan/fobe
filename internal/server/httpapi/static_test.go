// Tests for the last-resort handler: the SPA / API-only page must never answer
// an /api request (design §19.6: the API returns structured data and snake_case
// codes, always).
package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
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

// TestStaticCachePolicy pins the 2026-09-21 policy. The shell must revalidate:
// with nothing but Last-Modified the browser applies heuristic freshness (≈10%
// of the time since that stamp) and keeps rendering the bundle from before a
// rebuild — the panel looks healthy while showing the old UI, and the person
// who just rebuilt it is told "nothing changed". Content-hashed bundles are the
// opposite case: their name changes with their bytes, so they may be immutable.
func TestStaticCachePolicy(t *testing.T) {
	srv, api := newTestServer(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"index.html":             "<!doctype html><title>fobe</title>",
		"logo.svg":               "<svg xmlns=\"http://www.w3.org/2000/svg\"/>",
		"assets/index-abc123.js": "export default 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api.WebDir = dir

	for _, tc := range []struct{ path, why string }{
		{"/", "the shell itself"},
		{"/nodes/3TFhf4xe2AA", "a deep link falls back to the shell"},
		{"/logo.svg", "an unhashed root asset"},
	} {
		resp, _ := doAuthed(t, "GET", srv.URL+tc.path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (%s): got %d, want 200", tc.path, tc.why, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("GET %s (%s): Cache-Control = %q, want no-cache", tc.path, tc.why, cc)
		}
	}

	resp, _ := doAuthed(t, "GET", srv.URL+"/assets/index-abc123.js", "", nil)
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed bundle: Cache-Control = %q, want immutable", cc)
	}

	// The case that motivated the split: a shell from an older build asks for a
	// bundle this build no longer ships. index.html here would be executed as a
	// module (the old fallback did exactly that, with a 200); a 404 says what
	// happened.
	resp, raw := doAuthed(t, "GET", srv.URL+"/assets/index-gone.js", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing bundle: got %d, want 404 (body %s)", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("missing bundle answered HTML (%q): %s", ct, raw)
	}
}

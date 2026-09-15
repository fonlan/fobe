package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/server/singboxdl"
)

// Tests for the settings-page version picker and the explicit per-version
// download behind it (design §9.5): GET /api/singbox/releases and
// POST /api/singbox/versions/{version}/download.

type releasesBody struct {
	Releases []struct {
		Version      string `json:"version"`
		Tag          string `json:"tag"`
		Prerelease   bool   `json:"prerelease"`
		Cached       bool   `json:"cached"`
		LatestStable bool   `json:"latest_stable"`
	} `json:"releases"`
	FetchedAt int64  `json:"fetched_at"`
	Stale     bool   `json:"stale"`
	Error     string `json:"error"`
}

func decodeReleases(t *testing.T, raw []byte) releasesBody {
	t.Helper()
	var body releasesBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode releases body: %v (%s)", err, raw)
	}
	return body
}

func TestSingboxReleasesEndpoint(t *testing.T) {
	rel := newE2ERelease(t, "", "1.12.0", "1.11.0", "1.12.0-beta.2")
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	api.SingboxAPIBase = rel.srv.URL
	api.SingboxDownloadBase = rel.srv.URL
	writeCachedVersion(t, api.DLDir, "1.11.0", []byte("bin-1.11.0"), 1700000000)

	cookie := loginSession(t, srv)

	resp, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/releases", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("releases status = %d: %s", resp.StatusCode, raw)
	}
	body := decodeReleases(t, raw)
	if body.FetchedAt == 0 || body.Stale || body.Error != "" {
		t.Fatalf("release metadata = %+v", body)
	}
	if len(body.Releases) != 3 {
		t.Fatalf("releases = %+v", body.Releases)
	}
	byVersion := map[string]int{}
	for i, r := range body.Releases {
		byVersion[r.Version] = i
	}
	// the cached version is flagged, the others are offered for download
	if i, ok := byVersion["1.11.0"]; !ok || !body.Releases[i].Cached {
		t.Fatalf("1.11.0 should be marked cached: %+v", body.Releases)
	}
	if i, ok := byVersion["1.12.0"]; !ok || body.Releases[i].Cached {
		t.Fatalf("1.12.0 should not be marked cached: %+v", body.Releases)
	}
	// a beta is downloadable but never the "latest stable" anchor
	if i, ok := byVersion["1.12.0-beta.2"]; !ok || !body.Releases[i].Prerelease {
		t.Fatalf("beta not flagged: %+v", body.Releases)
	}
	if i, ok := byVersion["1.12.0"]; !ok || !body.Releases[i].LatestStable {
		t.Fatalf("1.12.0 should be latest_stable: %+v", body.Releases)
	}
	if got := rel.lists.Load(); got != 1 {
		t.Fatalf("listing fetches = %d, want 1", got)
	}

	// A second read is served from the TTL cache: flipping through the page
	// must not refetch a ~10 MB listing.
	if _, raw = doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/releases", cookie, nil); len(raw) == 0 {
		t.Fatal("empty second listing")
	}
	if got := rel.lists.Load(); got != 1 {
		t.Fatalf("listing fetches after cache hit = %d, want 1", got)
	}

	// An explicit refresh goes upstream again.
	if _, raw = doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/releases?refresh=1", cookie, nil); len(raw) == 0 {
		t.Fatal("empty refreshed listing")
	}
	if got := rel.lists.Load(); got != 2 {
		t.Fatalf("listing fetches after refresh = %d, want 2", got)
	}

	// A failed refresh must serve the last good listing rather than an empty
	// picker, and say so.
	rel.failListings.Store(true)
	resp, raw = doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/releases?refresh=1", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale status = %d: %s", resp.StatusCode, raw)
	}
	stale := decodeReleases(t, raw)
	if !stale.Stale || stale.Error != "release_unavailable" || len(stale.Releases) != 3 {
		t.Fatalf("stale body = %+v", stale)
	}

	// With nothing cached yet the failure is honest: 502, not an empty list.
	api.sbRel.mu.Lock()
	api.sbRel.items = nil
	api.sbRel.mu.Unlock()
	resp, raw = doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/releases?refresh=1", cookie, nil)
	if resp.StatusCode != http.StatusBadGateway || !hasCode(raw, "release_unavailable") {
		t.Fatalf("empty+failed status = %d: %s", resp.StatusCode, raw)
	}

	// no session → 401
	anon, err := http.Get(srv.URL + "/api/singbox/releases")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", anon.StatusCode)
	}
}

// waitCached polls until <dlDir>/singbox/<version>/linux-amd64 exists.
func waitCached(t *testing.T, dlDir, version string) {
	t.Helper()
	path := filepath.Join(dlDir, singboxdl.DirName, version, singboxdl.BinaryName)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was not published in time", path)
}

func TestSingboxVersionDownload(t *testing.T) {
	rel := newE2ERelease(t, "", "1.12.0", "1.13.0")
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	api.SingboxAPIBase = rel.srv.URL
	api.SingboxDownloadBase = rel.srv.URL
	cookie := loginSession(t, srv)

	// An invalid version never reaches the network.
	resp, raw := doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/versions/not-a-version/download", cookie, nil)
	if resp.StatusCode != http.StatusBadRequest || !hasCode(raw, "bad_version") {
		t.Fatalf("invalid version status = %d: %s", resp.StatusCode, raw)
	}

	// A download that needs a moment to finish: hold the asset body so the
	// in-flight state is observable, then let it complete.
	rel.hold = make(chan struct{})
	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/versions/1.12.0/download", cookie, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("download status = %d: %s", resp.StatusCode, raw)
	}
	var accepted struct {
		OK       bool            `json:"ok"`
		Version  string          `json:"version"`
		Cached   bool            `json:"cached"`
		Download SingboxDownload `json:"download"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatalf("decode accepted: %v", err)
	}
	if !accepted.OK || accepted.Version != "1.12.0" || accepted.Cached {
		t.Fatalf("accepted = %+v", accepted)
	}
	// The row exists from the click: resolving is reported before the (slow)
	// release lookup, so the panel can add it immediately.
	if !accepted.Download.Active || accepted.Download.Version != "1.12.0" {
		t.Fatalf("immediate download snapshot = %+v", accepted.Download)
	}

	// Wait until the body is actually streaming, then a second, *different*
	// version is refused: the panel has one progress row, not a queue.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && api.sbProg.snapshot().Phase != singboxdl.PhaseDownloading {
		time.Sleep(10 * time.Millisecond)
	}
	if got := api.sbProg.snapshot().Phase; got != singboxdl.PhaseDownloading {
		t.Fatalf("phase = %q, want downloading", got)
	}
	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/versions/1.13.0/download", cookie, nil)
	if resp.StatusCode != http.StatusConflict || !hasCode(raw, "download_in_progress") {
		t.Fatalf("second download status = %d: %s", resp.StatusCode, raw)
	}
	close(rel.hold)
	rel.hold = nil

	waitCached(t, api.DLDir, "1.12.0")

	// The version is cached now: asking again is a no-op, not an error.
	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/versions/1.12.0/download", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat download status = %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatalf("decode repeat: %v", err)
	}
	if !accepted.Cached {
		t.Fatalf("repeat download = %+v, want cached", accepted)
	}

	// An unknown version fails in the background (the response is already
	// sent) — the row must not stay at "resolving" forever.
	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/versions/9.9.9/download", cookie, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("unknown version status = %d: %s", resp.StatusCode, raw)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && api.sbProg.snapshot().Phase != singboxdl.PhaseFailed {
		time.Sleep(10 * time.Millisecond)
	}
	failed := api.sbProg.snapshot()
	if failed.Phase != singboxdl.PhaseFailed || failed.Version != "9.9.9" || failed.Error == "" {
		t.Fatalf("failed snapshot = %+v", failed)
	}
	if _, err := os.Stat(filepath.Join(api.DLDir, singboxdl.DirName, "9.9.9")); !os.IsNotExist(err) {
		t.Fatalf("a failed lookup must not create %s (err=%v)", "9.9.9", err)
	}

	// The download is audited like every other operator action.
	audits, err := api.Store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, a := range audits {
		if a.Action == "singbox_version_download" && a.Command == "1.12.0" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("no singbox_version_download audit entry: %+v", audits)
	}

	// no session → 401
	anon, err := http.Post(srv.URL+"/api/singbox/versions/1.12.0/download", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", anon.StatusCode)
	}
}

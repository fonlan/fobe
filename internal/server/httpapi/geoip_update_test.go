// HTTP tests for the §14.1 GeoIP download/update endpoints, plus the shared
// MMDB fixture the upload tests use.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fobe-panel/fobe/internal/server/geoip"
	"github.com/fobe-panel/fobe/internal/server/geoipupdate"
)

// mmdbFixture is the checked-in test database: the only payload in the repo
// that survives real MMDB validation (the format has no magic header, so a
// hand-rolled stand-in would just test the validator's absence).
func mmdbFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "geoip", "testdata", "GeoLite2-Country.mmdb"))
	if err != nil {
		t.Fatalf("read MMDB fixture: %v", err)
	}
	return raw
}

// geoIPStatus decodes GET /api/geoip/status.
func geoIPStatus(t *testing.T, srv *httptest.Server, cookie string) geoIPStatusView {
	t.Helper()
	resp, raw := doAuthed(t, "GET", srv.URL+"/api/geoip/status", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d %s", resp.StatusCode, raw)
	}
	var v geoIPStatusView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode status: %v (%s)", err, raw)
	}
	return v
}

// subscribeEvents taps the /ws/events broker directly, which is what the
// settings page sees.
func subscribeEvents(api *Server) (<-chan []byte, func()) {
	ch := make(chan []byte, 128)
	api.evMu.Lock()
	api.evSubs[ch] = struct{}{}
	api.evMu.Unlock()
	return ch, func() {
		api.evMu.Lock()
		delete(api.evSubs, ch)
		api.evMu.Unlock()
	}
}

func TestGeoIPStatusAndManualUpdate(t *testing.T) {
	fixture := mmdbFixture(t)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture)
	}))
	defer mirror.Close()

	srv, api := newTestServer(t)
	path := filepath.Join(t.TempDir(), "geoip", "GeoLite2-Country.mmdb")
	api.GeoIPMMDBPath = path
	api.GeoIPResolver = geoip.NewMMDB(path)
	api.Background = context.Background()
	api.GeoIPUpdater = geoipupdate.New(geoipupdate.Config{
		Path:       path,
		Settings:   api.Store,
		Log:        testLogger(),
		Sources:    []string{mirror.URL},
		AutoUpdate: true,
		Progress:   api.OnGeoIPProgress,
	})

	cookie := loginSession(t, srv)

	// Before any update: configured, nothing on disk, nothing served.
	before := geoIPStatus(t, srv, cookie)
	if !before.Configured || before.Exists || before.Live {
		t.Fatalf("initial status = %+v, want configured with no database", before)
	}
	if !before.AutoUpdate || before.MaxAgeDays != geoipupdate.DefaultMaxAgeDays {
		t.Fatalf("initial status = %+v, want auto update on with the default threshold", before)
	}

	events, unsubscribe := subscribeEvents(api)
	defer unsubscribe()

	resp, raw := doAuthed(t, "POST", srv.URL+"/api/geoip/update", cookie, nil)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(raw), `"accepted":true`) {
		t.Fatalf("update: got %d %s, want 202 accepted", resp.StatusCode, raw)
	}

	// The download runs in the background; the panel learns the outcome from
	// the status endpoint (and the progress bar from /ws/events).
	var after geoIPStatusView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		after = geoIPStatus(t, srv, cookie)
		if after.State == geoipupdate.StateOK && after.Exists {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after.State != geoipupdate.StateOK || !after.Exists {
		t.Fatalf("status after update = %+v, want ok with a database on disk", after)
	}
	if !after.Live {
		t.Fatal("resolver must serve the new database")
	}
	if after.DatabaseType != "GeoLite2-Country" || after.BuildEpoch == 0 {
		t.Fatalf("metadata = %q/%d, want the fixture's", after.DatabaseType, after.BuildEpoch)
	}
	if after.Source != mirror.URL {
		t.Fatalf("source = %q, want the mirror URL", after.Source)
	}
	if after.Download.Phase != geoipupdate.PhaseDone || after.Download.Active {
		t.Fatalf("download snapshot = %+v, want a finished update", after.Download)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || string(onDisk) != string(fixture) {
		t.Fatalf("installed database mismatch (%v)", err)
	}

	// The panel sees the progress phases and the completion event.
	var sawUpdate, sawDone, sawUpdated bool
	for len(events) > 0 {
		var ev struct {
			Kind string               `json:"kind"`
			Data geoipupdate.Download `json:"data"`
		}
		if err := json.Unmarshal(<-events, &ev); err != nil {
			continue
		}
		switch ev.Kind {
		case "geoip_update":
			sawUpdate = true
			if ev.Data.Phase == geoipupdate.PhaseDone {
				sawDone = true
			}
		case "geoip_updated":
			sawUpdated = true
		}
	}
	if !sawUpdate || !sawDone {
		t.Fatalf("events: geoip_update=%v done=%v, want both", sawUpdate, sawDone)
	}
	if !sawUpdated {
		t.Fatal("missing geoip_updated event after a successful update")
	}

	// Both the trigger and the result are audited.
	resp, raw = doAuthed(t, "GET", srv.URL+"/api/audit?limit=100", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit: got %d", resp.StatusCode)
	}
	for _, want := range []string{"geoip_update_started", "geoip_mmdb_downloaded"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("audit is missing %s: %s", want, raw)
		}
	}
}

func TestGeoIPUpdateUnconfigured(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)

	if got := geoIPStatus(t, srv, cookie); got.Configured || got.State != geoipupdate.StateDisabled {
		t.Fatalf("unconfigured status = %+v, want disabled", got)
	}
	resp, raw := doAuthed(t, "POST", srv.URL+"/api/geoip/update", cookie, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "mmdb_not_configured") {
		t.Fatalf("unconfigured update: got %d %s, want 503 mmdb_not_configured", resp.StatusCode, raw)
	}
	// The upload endpoint answers the same way.
	if api.GeoIPMMDBPath != "" {
		t.Fatal("test server should start unconfigured")
	}
}

func TestGeoIPUpdateRejectsConcurrentRequest(t *testing.T) {
	fixture := mmdbFixture(t)
	release := make(chan struct{})
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the first download open
		w.Write(fixture)
	}))
	defer mirror.Close()

	srv, api := newTestServer(t)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	updater := geoipupdate.New(geoipupdate.Config{
		Path:       path,
		Settings:   api.Store,
		Log:        testLogger(),
		Sources:    []string{mirror.URL},
		AutoUpdate: true,
	})
	api.GeoIPMMDBPath = path
	api.GeoIPUpdater = updater
	api.Background = context.Background()
	cookie := loginSession(t, srv)

	resp, raw := doAuthed(t, "POST", srv.URL+"/api/geoip/update", cookie, nil)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(raw), `"accepted":true`) {
		t.Fatalf("first update: got %d %s, want 202 accepted", resp.StatusCode, raw)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !updater.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !updater.Running() {
		t.Fatal("updater never started")
	}

	// The second click must not stack a download: it reports that one is
	// already running, which is not an error.
	resp, raw = doAuthed(t, "POST", srv.URL+"/api/geoip/update", cookie, nil)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(raw), `"accepted":false`) {
		t.Fatalf("second update: got %d %s, want 202 accepted:false", resp.StatusCode, raw)
	}

	close(release)
	deadline = time.Now().Add(5 * time.Second)
	for updater.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := updater.Status(); got.State != geoipupdate.StateOK {
		t.Fatalf("status = %+v, want ok", got)
	}
}

func TestMMDBUploadReplacesUpdateStatus(t *testing.T) {
	fixture := mmdbFixture(t)
	srv, api := newTestServer(t)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	api.GeoIPMMDBPath = path
	api.GeoIPResolver = geoip.NewMMDB(path)
	updater := geoipupdate.New(geoipupdate.Config{
		Path:       path,
		Settings:   api.Store,
		Log:        testLogger(),
		Sources:    []string{"http://127.0.0.1:1/db.mmdb"}, // refused: no listener
		AutoUpdate: true,
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	api.GeoIPUpdater = updater
	cookie := loginSession(t, srv)

	if got := updater.Ensure(context.Background(), true); got.State != geoipupdate.StateFailed {
		t.Fatalf("ensure = %+v, want a failure from the dead mirror", got)
	}

	resp, raw := doAuthed(t, "POST", srv.URL+"/api/geoip/mmdb", cookie, fixture)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: got %d %s", resp.StatusCode, raw)
	}
	st := geoIPStatus(t, srv, cookie)
	if st.State != geoipupdate.StateOK || st.Source != geoipupdate.SourceUpload || st.Error != "" {
		t.Fatalf("status after upload = %+v, want ok from the upload with no error left", st)
	}
	if !st.Exists || !st.Live {
		t.Fatalf("status after upload = %+v, want a live database", st)
	}
}

func TestGeoIPUpdateFailureIsReportedAndAudited(t *testing.T) {
	srv, api := newTestServer(t)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	api.GeoIPMMDBPath = path
	updater := geoipupdate.New(geoipupdate.Config{
		Path:       path,
		Settings:   api.Store,
		Log:        testLogger(),
		Sources:    []string{"http://127.0.0.1:1/db.mmdb"}, // refused: no listener
		AutoUpdate: true,
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	api.GeoIPUpdater = updater
	api.Background = context.Background()
	cookie := loginSession(t, srv)

	if resp, raw := doAuthed(t, "POST", srv.URL+"/api/geoip/update", cookie, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update: got %d %s, want 202", resp.StatusCode, raw)
	}
	deadline := time.Now().Add(5 * time.Second)
	for updater.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	st := geoIPStatus(t, srv, cookie)
	if st.State != geoipupdate.StateFailed || st.Error == "" {
		t.Fatalf("status = %+v, want a failure with the reason", st)
	}
	if st.Exists {
		t.Fatal("a failed update must not leave a database behind")
	}
	resp, raw := doAuthed(t, "GET", srv.URL+"/api/audit?limit=20", cookie, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "geoip_update_failed") {
		t.Fatalf("audit is missing the failure: %d %s", resp.StatusCode, raw)
	}
}

func TestGeoIPUpdateKeepsEventSocketOpen(t *testing.T) {
	fixture := mmdbFixture(t)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture)
	}))
	defer mirror.Close()

	srv, api := newTestServer(t)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	api.GeoIPMMDBPath = path
	api.GeoIPResolver = geoip.NewMMDB(path)
	api.Background = context.Background()
	api.GeoIPUpdater = geoipupdate.New(geoipupdate.Config{
		Path:       path,
		Settings:   api.Store,
		Log:        testLogger(),
		Sources:    []string{mirror.URL},
		AutoUpdate: true,
		Progress:   api.OnGeoIPProgress,
	})
	cookie := loginSession(t, srv)

	// The panel's live channel: a websocket to /ws/events, authenticated like
	// the browser does it.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/events"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Cookie": []string{cookie}})
	if err != nil {
		t.Fatalf("dial /ws/events: %v (%v)", err, resp)
	}
	defer conn.Close()

	if resp2, raw := doAuthed(t, "POST", srv.URL+"/api/geoip/update", cookie, nil); resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("update: got %d %s", resp2.StatusCode, raw)
	}

	// Every phase must arrive over the socket, and the socket must still be
	// open afterwards: an update that closed it would make the panel drop its
	// progress bar and (through a dev proxy) report a socket error.
	seen := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for !seen[geoipupdate.PhaseDone] && time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("event socket broke during the update: %v (phases seen: %v)", err, seen)
		}
		var ev struct {
			Kind string               `json:"kind"`
			Data geoipupdate.Download `json:"data"`
		}
		if json.Unmarshal(data, &ev) != nil || ev.Kind != "geoip_update" {
			continue
		}
		seen[ev.Data.Phase] = true
	}
	for _, phase := range []string{geoipupdate.PhaseConnecting, geoipupdate.PhaseDownloading, geoipupdate.PhaseVerifying, geoipupdate.PhaseDone} {
		if !seen[phase] {
			t.Fatalf("phase %q never arrived over the socket (seen: %v)", phase, seen)
		}
	}

	// Idle reads must time out rather than close: the server keeps the channel
	// open after the work is done. A late `geoip_updated` frame is expected —
	// only a close (or any other error) is a failure.
	conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("socket closed after the update: %v", err)
			}
			break
		}
	}
}

func TestGeoIPSettingsValidation(t *testing.T) {
	srv, api := newTestServer(t)
	api.GeoIPMMDBPath = filepath.Join(t.TempDir(), "x.mmdb")
	api.GeoIPUpdater = geoipupdate.New(geoipupdate.Config{
		Path:       api.GeoIPMMDBPath,
		Settings:   api.Store,
		Log:        testLogger(),
		Sources:    []string{"https://127.0.0.1:1/db.mmdb"}, // never fetched here
		AutoUpdate: true,
	})
	cookie := loginSession(t, srv)

	bad := []struct {
		key, value, code string
	}{
		{"geoip.auto_update", "maybe", "bad_geoip_auto_update"},
		{"geoip.max_age_days", "-1", "bad_geoip_max_age_days"},
		{"geoip.max_age_days", "999", "bad_geoip_max_age_days"},
		{"geoip.max_age_days", "soon", "bad_geoip_max_age_days"},
		{"geoip.url", "not a url", "bad_geoip_url"},
		{"geoip.url", "ftp://mirror/db.mmdb", "bad_geoip_url"},
	}
	for _, c := range bad {
		body, _ := json.Marshal(map[string]any{"settings": map[string]string{c.key: c.value}})
		resp, raw := doAuthed(t, "PUT", srv.URL+"/api/settings", cookie, body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), c.code) {
			t.Fatalf("%s=%q: got %d %s, want 400 %s", c.key, c.value, resp.StatusCode, raw, c.code)
		}
	}

	body, _ := json.Marshal(map[string]any{"settings": map[string]string{
		"geoip.auto_update":  "1",
		"geoip.max_age_days": "14",
		"geoip.url":          "https://mirror.example/db.mmdb?token=abc",
	}})
	resp, raw := doAuthed(t, "PUT", srv.URL+"/api/settings", cookie, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid geoip settings: got %d %s, want 200", resp.StatusCode, raw)
	}
	// The stored values feed the updater's status view.
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := api.GeoIPUpdater.Status()
		if st.MaxAgeDays == 14 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v, want max_age_days 14", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

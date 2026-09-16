// Settings-page download progress for the sing-box artifact (design §9.5.3).
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/server/singboxdl"

	"github.com/gorilla/websocket"
)

// TestSingboxCacheReportsDownloadProgress drives one install through the
// server's own artifact client and checks both ways the settings page learns
// about it: the `download` snapshot in GET /api/singbox/cache (a page that
// loads mid-download) and the singbox_download event on /ws/events (a page
// that is already open).
func TestSingboxCacheReportsDownloadProgress(t *testing.T) {
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	rel := newE2ERelease(t, "", "1.12.0")
	rel.hold = make(chan struct{})
	api.SingboxAPIBase = rel.srv.URL
	api.SingboxDownloadBase = rel.srv.URL

	cookie := loginSession(t, srv)

	// The asset handler parks until this is called; a failing assertion must
	// not leave it parked, or the fixture's Close would hang the test binary.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(rel.hold) }) }
	t.Cleanup(release)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/events"
	hdr := http.Header{}
	hdr.Set("Cookie", cookie)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("dial /ws/events: %v", err)
	}
	defer conn.Close()
	// The broker drops events for a subscriber that is not registered yet.
	time.Sleep(150 * time.Millisecond)

	installErr := make(chan error, 1)
	go func() {
		r, err := api.SingboxDL().ReleaseByVersion(context.Background(), "1.12.0")
		if err != nil {
			installErr <- err
			return
		}
		_, err = api.SingboxDL().Install(context.Background(), r, false)
		installErr <- err
	}()

	// The download is held open inside the asset handler, so polling must see
	// an active snapshot with real byte counts.
	var got SingboxDownload
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/cache", cookie, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cache status = %d: %s", resp.StatusCode, raw)
		}
		got = decodeCache(t, raw).Download
		if got.Active && got.Downloaded > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("download never became visible: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Version != "1.12.0" || got.Phase != "downloading" {
		t.Fatalf("snapshot = %+v, want a 1.12.0 download", got)
	}
	if got.Total == 0 || got.Percent <= 0 || got.Percent >= 100 || got.StartedAt == 0 {
		t.Fatalf("snapshot lacks byte accounting: %+v", got)
	}

	// The same snapshot rides /ws/events while the page is open. The early
	// events carry the phases before any byte is read, so keep reading until a
	// tick with real byte counts shows up.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	sawEvent := false
	for !sawEvent {
		var ev struct {
			Kind string          `json:"kind"`
			Ref  string          `json:"ref"`
			Data json.RawMessage `json:"data"`
		}
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("read event: %v", err)
		}
		if ev.Kind != eventSingboxDownload {
			continue
		}
		var snap SingboxDownload
		if err := json.Unmarshal(ev.Data, &snap); err != nil {
			t.Fatalf("decode download event: %v", err)
		}
		if ev.Ref != "1.12.0" || snap.Version != "1.12.0" || !snap.Active {
			t.Fatalf("download event = %+v (ref %q)", snap, ev.Ref)
		}
		if snap.Downloaded > 0 {
			sawEvent = true
		}
	}

	release()
	if err := <-installErr; err != nil {
		t.Fatalf("install: %v", err)
	}

	// Once it is over the bar must disappear; the outcome lives in cache_status.
	_, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/cache", cookie, nil)
	final := decodeCache(t, raw)
	if final.Download.Active || final.Download.Phase != "done" {
		t.Fatalf("final snapshot = %+v, want an inactive done state", final.Download)
	}
	if final.LatestCached != "1.12.0" {
		t.Fatalf("latest_cached = %q, want 1.12.0", final.LatestCached)
	}
}

// TestSingboxDLIsOneClientPerProcess guards the invariant the duplicate
// download fix rests on: the cache manager (wired in package main) and every
// handler must talk to the same client, because singboxdl serializes Install
// per client (§9.5.3). A per-request client silently restores double downloads.
func TestSingboxDLIsOneClientPerProcess(t *testing.T) {
	_, api := newTestServer(t)
	api.DLDir = t.TempDir()
	if api.SingboxDL() != api.SingboxDL() {
		t.Fatal("SingboxDL must return one process-wide client")
	}
	if api.SingboxUpdater() == nil {
		t.Fatal("update manager missing")
	}
	if api.sbDL == nil {
		t.Fatal("the updater must not build a second artifact client")
	}
}

// TestSingboxProgressSnapshotRules pins the snapshot's UX rules: a queued
// caller must not reset a running bar, the post-download phases must keep the
// byte count visible, and the first bytes must be pushed even inside the
// throttle window (a stalled download would otherwise sit at 0 B).
func TestSingboxProgressSnapshotRules(t *testing.T) {
	// The first bytes always publish, even in the same second as the phase.
	var p singboxProgress
	if _, publish := p.observe(singboxdl.Progress{
		Version: "1.10.0", Phase: singboxdl.PhaseDownloading, Total: 1000,
	}, 100); !publish {
		t.Fatal("a phase change must publish")
	}
	got, publish := p.observe(singboxdl.Progress{
		Version: "1.10.0", Phase: singboxdl.PhaseDownloading, Downloaded: 100, Total: 1000,
	}, 100)
	if !publish || got.Percent != 10 {
		t.Fatalf("first bytes = %+v (publish %v), want 10%% and a push", got, publish)
	}

	// A caller queued behind the running download must not blank the bar.
	got, publish = p.observe(singboxdl.Progress{Version: "1.10.0", Phase: singboxdl.PhaseWaiting}, 101)
	if publish || got.Downloaded != 100 || !got.Active {
		t.Fatalf("queued caller changed the bar: %+v (publish %v)", got, publish)
	}

	// Verifying/extracting carry no byte count but keep the last one.
	got, _ = p.observe(singboxdl.Progress{Version: "1.10.0", Phase: singboxdl.PhasePublishing}, 102)
	if got.Downloaded != 100 || got.Percent != 10 || !got.Active {
		t.Fatalf("publishing snapshot = %+v, want the byte count kept", got)
	}

	// Terminal states end the activity and keep the failure reason.
	got, _ = p.observe(singboxdl.Progress{Version: "1.10.0", Phase: singboxdl.PhaseFailed, Error: "boom"}, 103)
	if got.Active || got.Error != "boom" {
		t.Fatalf("failed snapshot = %+v, want an inactive error state", got)
	}
}

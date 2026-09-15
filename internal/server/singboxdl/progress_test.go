package singboxdl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- download progress + single-flight install (design §9.5.3) ---

// progressLog collects Config.Progress observations from any goroutine.
type progressLog struct {
	mu     sync.Mutex
	events []Progress
}

func (l *progressLog) add(p Progress) {
	l.mu.Lock()
	l.events = append(l.events, p)
	l.mu.Unlock()
}

func (l *progressLog) all() []Progress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Progress(nil), l.events...)
}

// waitFor blocks until an event matches, so the test never races the download.
func (l *progressLog) waitFor(t *testing.T, what string, pred func(Progress) bool) Progress {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range l.all() {
			if pred(p) {
				return p
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; events so far: %+v", what, l.all())
	return Progress{}
}

// phasesInOrder returns the distinct phase names, collapsing the repeated
// downloading ticks.
func (l *progressLog) phasesInOrder() []string {
	out := []string{}
	for _, p := range l.all() {
		if len(out) == 0 || out[len(out)-1] != p.Phase {
			out = append(out, p.Phase)
		}
	}
	return out
}

// slowRelease serves one version whose asset download is held open in the
// middle, so a second Install provably arrives while the first is in flight.
type slowRelease struct {
	srv     *httptest.Server
	asset   string
	digest  string
	body    []byte
	release chan struct{}
	fetches atomic.Int64
}

func newSlowRelease(t *testing.T, version string) *slowRelease {
	t.Helper()
	body := makeTarGz(t, map[string][]byte{
		"sing-box-" + version + "-linux-amd64-musl/sing-box": []byte("SLOW-SINGBOX-" + version),
	})
	sum := sha256.Sum256(body)
	f := &slowRelease{
		asset:   AssetName(version),
		digest:  "sha256:" + hex.EncodeToString(sum[:]),
		body:    body,
		release: make(chan struct{}),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+testOwner+"/"+testRepo+"/releases":
			type assetJSON struct {
				Name               string `json:"name"`
				BrowserDownloadURL string `json:"browser_download_url"`
				Digest             string `json:"digest"`
				Size               int64  `json:"size"`
			}
			type releaseJSON struct {
				TagName string      `json:"tag_name"`
				Assets  []assetJSON `json:"assets"`
			}
			_ = json.NewEncoder(w).Encode([]releaseJSON{{
				TagName: "v" + version,
				Assets: []assetJSON{{
					Name:               f.asset,
					BrowserDownloadURL: f.srv.URL + "/dl/" + f.asset,
					Digest:             f.digest,
					Size:               int64(len(f.body)),
				}},
			}})
		case r.URL.Path == "/dl/"+f.asset:
			f.fetches.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
			w.WriteHeader(http.StatusOK)
			half := len(f.body) / 2
			_, _ = w.Write(f.body[:half])
			w.(http.Flusher).Flush()
			<-f.release
			_, _ = w.Write(f.body[half:])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestInstallReportsProgressPhases pins the phase sequence the settings page
// renders: resolving → downloading (with byte counts) → verifying →
// extracting → publishing → done.
func TestInstallReportsProgressPhases(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.10.0"})
	log := &progressLog{}
	c := New(Config{
		DLDir: f.dlDir, APIBase: f.srv.URL, DownloadBase: f.srv.URL,
		Owner: testOwner, Repo: testRepo, HTTPClient: f.srv.Client(),
		Progress: log.add,
	})
	ctx := context.Background()
	rel, err := c.ReleaseByVersion(ctx, "1.10.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := c.Install(ctx, rel, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	got := log.phasesInOrder()
	want := []string{PhaseResolving, PhaseDownloading, PhaseVerifying, PhaseExtracting, PhasePublishing, PhaseDone}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phases = %v, want %v", got, want)
		}
	}

	size := int64(len(f.tarballs[AssetName("1.10.0")]))
	last := Progress{}
	for _, p := range log.all() {
		if p.Phase == PhaseDownloading && p.Downloaded > 0 {
			last = p
		}
		if p.Version != "1.10.0" {
			t.Fatalf("event without the version: %+v", p)
		}
		if p.StartedAt == 0 {
			t.Fatalf("event without started_at: %+v", p)
		}
	}
	if last.Downloaded != size {
		t.Fatalf("final downloaded = %d, want the whole archive (%d)", last.Downloaded, size)
	}
	if last.Total != 0 && last.Total != size {
		t.Fatalf("total = %d, want 0 (unknown) or %d", last.Total, size)
	}
}

// TestInstallFailureReportsFailedPhase: the panel must never leave a bar stuck
// at "downloading" when the install is over.
func TestInstallFailureReportsFailedPhase(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.10.0", Mode: "baddigest"})
	log := &progressLog{}
	c := New(Config{
		DLDir: f.dlDir, APIBase: f.srv.URL, DownloadBase: f.srv.URL,
		Owner: testOwner, Repo: testRepo, HTTPClient: f.srv.Client(),
		Progress: log.add,
	})
	ctx := context.Background()
	rel, err := c.ReleaseByVersion(ctx, "1.10.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := c.Install(ctx, rel, false); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("install err = %v, want ErrChecksumMismatch", err)
	}
	events := log.all()
	if len(events) == 0 {
		t.Fatal("no progress events")
	}
	last := events[len(events)-1]
	if last.Phase != PhaseFailed || last.Error == "" || last.Version != "1.10.0" {
		t.Fatalf("last event = %+v, want a failure carrying the reason", last)
	}
}

// TestInstallOfCachedVersionIsSilent: the queued caller of a download that has
// just finished (and any "update sing-box" click on an already cached version)
// gets ErrVersionExists without a second download and without a failure flash.
func TestInstallOfCachedVersionIsSilent(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.10.0"})
	log := &progressLog{}
	c := New(Config{
		DLDir: f.dlDir, APIBase: f.srv.URL, DownloadBase: f.srv.URL,
		Owner: testOwner, Repo: testRepo, HTTPClient: f.srv.Client(),
		Progress: log.add,
	})
	ctx := context.Background()
	rel, err := c.ReleaseByVersion(ctx, "1.10.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := c.Install(ctx, rel, false); err != nil {
		t.Fatalf("first install: %v", err)
	}

	log.mu.Lock()
	log.events = nil
	log.mu.Unlock()

	if _, err := c.Install(ctx, rel, false); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("second install err = %v, want ErrVersionExists", err)
	}
	if got := log.all(); len(got) != 0 {
		t.Fatalf("a cached version must not report progress: %+v", got)
	}
}

// TestInstallSingleFlightFetchesOnce is the regression test for the duplicate
// download: a caller that arrives while the same tarball is being downloaded
// queues (reporting "waiting") and then finds the version published, so the
// release host is hit exactly once.
func TestInstallSingleFlightFetchesOnce(t *testing.T) {
	f := newSlowRelease(t, "1.11.0")
	log := &progressLog{}
	c := New(Config{
		DLDir: t.TempDir(), APIBase: f.srv.URL, DownloadBase: f.srv.URL,
		Owner: testOwner, Repo: testRepo, HTTPClient: f.srv.Client(),
		Progress: log.add,
	})
	ctx := context.Background()
	rel, err := c.ReleaseByVersion(ctx, "1.11.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	type result struct {
		cv  *CachedVersion
		err error
	}
	first := make(chan result, 1)
	go func() {
		cv, err := c.Install(ctx, rel, false)
		first <- result{cv, err}
	}()
	log.waitFor(t, "a partial download", func(p Progress) bool {
		return p.Phase == PhaseDownloading && p.Downloaded > 0
	})

	second := make(chan result, 1)
	go func() {
		cv, err := c.Install(ctx, rel, false)
		second <- result{cv, err}
	}()
	log.waitFor(t, "the queued phase", func(p Progress) bool { return p.Phase == PhaseWaiting })

	close(f.release)
	if r := <-first; r.err != nil {
		t.Fatalf("first install: %v", r.err)
	}
	r := <-second
	if !errors.Is(r.err, ErrVersionExists) {
		t.Fatalf("queued install err = %v, want ErrVersionExists", r.err)
	}
	if got := f.fetches.Load(); got != 1 {
		t.Fatalf("the asset was fetched %d times, want exactly 1", got)
	}
}

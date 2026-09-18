package modelsdev

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureServer serves the checked-in fixture and counts requests.
type fixtureServer struct {
	*httptest.Server

	mu     sync.Mutex
	body   []byte
	status int
	hits   int
}

func newFixtureServer(t *testing.T) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{body: fixtureBytes(t), status: http.StatusOK}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.hits++
		body, status := fs.body, fs.status
		fs.mu.Unlock()

		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (f *fixtureServer) set(body []byte, status int) {
	f.mu.Lock()
	f.body, f.status = body, status
	f.mu.Unlock()
}

func (f *fixtureServer) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// manager builds a Manager pointed at the fixture server. Auto-update stays on
// (the zero value), so tests exercise the production default.
func manager(t *testing.T, dir, url string, tweak ...func(*Config)) *Manager {
	t.Helper()
	cfg := Config{Dir: dir, URL: url}
	for _, fn := range tweak {
		fn(&cfg)
	}
	return New(cfg)
}

// waitFor polls until cond holds or the deadline passes. Background refreshes
// are goroutines, so the tests wait on the observable state instead of sleeping
// a fixed amount.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEnsureDownloadsAndCaches(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	m := manager(t, dir, srv.URL)

	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	path := CachePath(dir)
	if want := filepath.Join(dir, "models", "api.json"); path != want {
		t.Errorf("CachePath(%q) = %q, want %q", dir, path, want)
	}
	if m.Path() != path {
		t.Errorf("Path() = %q, want %q", m.Path(), path)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if string(onDisk) != string(fixtureBytes(t)) {
		t.Error("cache file differs from the downloaded document")
	}

	meta, ok := m.Lookup("anthropic", "claude-sonnet-4-6")
	if !ok {
		t.Fatal("Lookup after Ensure = miss, want hit")
	}
	if meta.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d, want 1000000", meta.ContextWindow)
	}
	if p, ok := m.Provider("anthropic"); !ok || p.NPM != "@ai-sdk/anthropic" {
		t.Errorf("Provider(anthropic) = %+v, %v", p, ok)
	}

	// A second Ensure finds a fresh cache: one stat, no request.
	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if hits := srv.Hits(); hits != 1 {
		t.Errorf("server hits = %d, want 1 (a fresh cache must not refetch)", hits)
	}
}

func TestEnsureForceRefreshesFreshCache(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	m := manager(t, dir, srv.URL)

	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, ok := m.Lookup("tokengo", "qwen/qwen3.5-397b-a17b"); !ok {
		t.Fatal("fixture model missing before refresh")
	}

	// The upstream document gains a model; force must pick it up even though the
	// cached file is not stale yet (the panel's "refresh now").
	added := `"freshly-added": {"id": "freshly-added", "name": "New",` +
		` "limit": {"context": 4096, "output": 1024}}, "no-id-model": {`
	updated := strings.Replace(string(fixtureBytes(t)), `"no-id-model": {`, added, 1)
	if updated == string(fixtureBytes(t)) {
		t.Fatal("fixture patch did not apply")
	}
	srv.set([]byte(updated), http.StatusOK)

	if err := m.Ensure(context.Background(), true); err != nil {
		t.Fatalf("force Ensure: %v", err)
	}
	if _, ok := m.Lookup("tokengo", "freshly-added"); !ok {
		t.Error("forced refresh did not pick up the new model")
	}
	if srv.Hits() != 2 {
		t.Errorf("server hits = %d, want 2", srv.Hits())
	}
}

func TestLoadCacheReadsTheFileBack(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, fixtureBytes(t), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	// No server and no network: reading the cache must be enough.
	m := manager(t, dir, "http://127.0.0.1:1/never-called")
	if err := m.LoadCache(); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if _, ok := m.Lookup("openai", "gpt-5"); !ok {
		t.Error("Lookup after LoadCache = miss, want hit")
	}

	st := m.Status()
	if !st.Loaded || st.Providers != 6 || st.Models != 9 {
		t.Errorf("Status() = %+v, want loaded with 6 providers / 9 models", st)
	}
	if st.UpdatedAt.IsZero() {
		t.Error("Status().UpdatedAt = zero, want the cache file's mtime")
	}
	if st.LastError != "" {
		t.Errorf("Status().LastError = %q, want empty after a good load", st.LastError)
	}
}

func TestLoadCacheMissingFile(t *testing.T) {
	m := manager(t, t.TempDir(), DefaultURL)
	err := m.LoadCache()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadCache with no file = %v, want os.ErrNotExist", err)
	}
	if m.Loaded() {
		t.Error("Loaded() = true after a failed load, want false")
	}
}

func TestEnsureKeepsPreviousMetadataWhenFetchFails(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	m := manager(t, dir, srv.URL)

	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	before, err := os.ReadFile(CachePath(dir))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}

	// 1) HTTP failure, 2) a 200 that is an HTML error page, 3) a 200 that is
	// valid JSON but the wrong shape. None of them may touch the live state.
	failures := []struct {
		name string
		body []byte
		code int
	}{
		{"http 500", []byte("boom"), http.StatusInternalServerError},
		{"html error page", []byte("<!doctype html><html><body>502 Bad Gateway</body></html>"), http.StatusOK},
		{"wrong shape", []byte(`{"anthropic":[]}`), http.StatusOK},
		{"empty document", []byte(`{}`), http.StatusOK},
		{"truncated document", []byte(`{"anthropic":{"id":"anthropic","models":`), http.StatusOK},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			srv.set(tc.body, tc.code)

			err := m.Ensure(context.Background(), true)
			if err == nil {
				t.Fatal("Ensure = nil error, want a non-fatal failure")
			}

			// The index in memory still answers.
			if _, ok := m.Lookup("anthropic", "claude-sonnet-4-6"); !ok {
				t.Error("Lookup after a failed refresh = miss, want the previous index")
			}
			// The cache on disk is byte-for-byte the previous one.
			after, err := os.ReadFile(CachePath(dir))
			if err != nil {
				t.Fatalf("read cache: %v", err)
			}
			if string(after) != string(before) {
				t.Error("failed refresh replaced the cache file")
			}
			// The failure is reported, not swallowed.
			if st := m.Status(); st.LastError == "" || !st.Loaded {
				t.Errorf("Status() = %+v, want loaded with LastError set", st)
			}
			// And no temp file is left behind.
			leftovers, err := filepath.Glob(filepath.Join(dir, "models", "*.tmp-*"))
			if err != nil {
				t.Fatalf("glob: %v", err)
			}
			if len(leftovers) != 0 {
				t.Errorf("temp files left behind: %v", leftovers)
			}
		})
	}

	// A later success clears the recorded failure.
	srv.set(fixtureBytes(t), http.StatusOK)
	if err := m.Ensure(context.Background(), true); err != nil {
		t.Fatalf("recovery Ensure: %v", err)
	}
	if st := m.Status(); st.LastError != "" {
		t.Errorf("Status().LastError = %q after recovery, want empty", st.LastError)
	}
}

func TestEnsureRejectsOversizedDocument(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	m := manager(t, dir, srv.URL, func(c *Config) { c.MaxBytes = 128 })

	err := m.Ensure(context.Background(), false)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Ensure = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(CachePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache file exists after a rejected download (stat err = %v)", err)
	}
	if m.Loaded() {
		t.Error("Loaded() = true after a rejected download, want false")
	}
}

func TestStartFetchesMissingCacheInBackground(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := manager(t, dir, srv.URL)
	m.Start(ctx)

	// Start returns immediately: the fetch is a background goroutine.
	waitFor(t, "the background fetch", func() bool { return m.Loaded() })
	if _, ok := m.Lookup("anthropic", "claude-sonnet-4-6"); !ok {
		t.Error("Lookup after the background fetch = miss, want hit")
	}
	if _, err := os.Stat(CachePath(dir)); err != nil {
		t.Errorf("cache file was not written: %v", err)
	}
}

func TestStartServesExistingCacheWithoutFetching(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, fixtureBytes(t), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := manager(t, dir, srv.URL)
	m.Start(ctx)

	// The cache is parsed synchronously, so it is already usable.
	if _, ok := m.Lookup("openai", "gpt-5"); !ok {
		t.Error("Lookup right after Start = miss, want the cached index")
	}
	time.Sleep(100 * time.Millisecond)
	if hits := srv.Hits(); hits != 0 {
		t.Errorf("server hits = %d, want 0 (a fresh cache must not be fetched)", hits)
	}
}

func TestStartRefreshesStaleCache(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, fixtureBytes(t), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	// Older than the refresh period: a server restarted every day would never let
	// the daily ticker fire, so startup has to notice staleness itself.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := manager(t, dir, srv.URL)
	m.Start(ctx)

	if _, ok := m.Lookup("openai", "gpt-5"); !ok {
		t.Error("stale cache was not served while refreshing")
	}
	waitFor(t, "the stale-cache refresh", func() bool { return srv.Hits() > 0 })
}

func TestDailyLoopRefreshes(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A very short period stands in for the 24h round; the file is fresh at
	// startup, so the first fetch can only come from the ticker.
	m := manager(t, dir, srv.URL, func(c *Config) { c.CheckEvery = 20 * time.Millisecond })
	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	waitFor(t, "the initial fetch", func() bool { return srv.Hits() == 1 })

	m.Start(ctx)
	waitFor(t, "the ticker refresh", func() bool { return srv.Hits() >= 2 })
}

func TestStartHonoursAutoUpdateEnv(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	t.Setenv(EnvAutoUpdate, "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := manager(t, dir, srv.URL)
	if m.AutoUpdate() {
		t.Fatal("AutoUpdate() = true with FOBE_MODELS_AUTO_UPDATE=0")
	}
	m.Start(ctx)

	time.Sleep(100 * time.Millisecond)
	if hits := srv.Hits(); hits != 0 {
		t.Errorf("server hits = %d, want 0 with automatic refresh off", hits)
	}
	if m.Loaded() {
		t.Error("Loaded() = true with no cache and auto-update off, want false")
	}

	// An explicit Ensure is an operator action and still works.
	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("manual Ensure with auto-update off: %v", err)
	}
	if _, ok := m.Lookup("openai", "gpt-5"); !ok {
		t.Error("manual Ensure did not load the metadata")
	}
}

func TestURLPrecedence(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()

	// Default.
	if got := manager(t, dir, "").URL(); got != DefaultURL {
		t.Errorf("URL() = %q, want DefaultURL", got)
	}
	// Environment override (the operator's private mirror).
	t.Setenv(EnvURL, srv.URL)
	if got := manager(t, dir, "").URL(); got != srv.URL {
		t.Errorf("URL() = %q, want the FOBE_MODELS_URL value", got)
	}
	// An explicit Config.URL wins over the environment.
	if got := manager(t, dir, "https://mirror.example/api.json").URL(); got != "https://mirror.example/api.json" {
		t.Errorf("URL() = %q, want the explicit Config.URL", got)
	}
}

func TestEnsureUsesEnvURL(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	t.Setenv(EnvURL, srv.URL)

	m := New(Config{Dir: dir})
	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if srv.Hits() != 1 {
		t.Errorf("server hits = %d, want 1 from FOBE_MODELS_URL", srv.Hits())
	}
}

func TestDisabledWithoutDataDir(t *testing.T) {
	m := New(Config{})
	if m.Path() != "" {
		t.Errorf("Path() = %q, want empty", m.Path())
	}
	if err := m.Ensure(context.Background(), true); !errors.Is(err, ErrDisabled) {
		t.Errorf("Ensure = %v, want ErrDisabled", err)
	}
	if err := m.LoadCache(); !errors.Is(err, ErrDisabled) {
		t.Errorf("LoadCache = %v, want ErrDisabled", err)
	}
	// Start must be a no-op, not a panic or a stray goroutine.
	m.Start(context.Background())
	if _, ok := m.Lookup("anthropic", "claude-sonnet-4-6"); ok {
		t.Error("Lookup on a disabled manager = hit, want miss")
	}
	if st := m.Status(); st.Loaded || st.Path != "" {
		t.Errorf("Status() = %+v, want an unloaded, disabled snapshot", st)
	}
}

func TestAutoUpdateEnvIgnoresNonFalseyValues(t *testing.T) {
	// A value that is not a falsey token must not silently disable the refresh.
	t.Setenv(EnvAutoUpdate, "please")
	if !New(Config{Dir: t.TempDir()}).AutoUpdate() {
		t.Error("AutoUpdate() = false, want true for a non-falsey value")
	}
}

func TestConcurrentLookupDuringRefresh(t *testing.T) {
	srv := newFixtureServer(t)
	dir := t.TempDir()
	m := manager(t, dir, srv.URL)

	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, ok := m.Lookup("anthropic", "claude-sonnet-4-6"); !ok {
						t.Error("Lookup = miss during a refresh")
						return
					}
					if _, ok := m.Provider("anthropic"); !ok {
						t.Error("Provider = miss during a refresh")
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		if err := m.Ensure(context.Background(), true); err != nil {
			t.Errorf("concurrent Ensure: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestIndexSurvivesFailedRefreshAfterFirstLoad(t *testing.T) {
	// Sequence: empty cache -> broken mirror -> a later good document. The first
	// failure must leave an empty (but usable) state, not a poisoned one.
	srv := newFixtureServer(t)
	srv.set([]byte("<html>nope</html>"), http.StatusOK)
	dir := t.TempDir()
	m := manager(t, dir, srv.URL)

	if err := m.Ensure(context.Background(), true); err == nil {
		t.Fatal("Ensure with a broken mirror = nil error, want failure")
	}
	if m.Loaded() {
		t.Error("Loaded() = true after a failed first fetch, want false")
	}
	if _, ok := m.Lookup("openai", "gpt-5"); ok {
		t.Error("Lookup on an unloaded manager = hit, want miss")
	}

	srv.set(fixtureBytes(t), http.StatusOK)
	if err := m.Ensure(context.Background(), false); err != nil {
		t.Fatalf("recovery Ensure: %v", err)
	}
	if _, ok := m.Lookup("openai", "gpt-5"); !ok {
		t.Error("Lookup after recovery = miss, want hit")
	}
}

func TestEnsureSingleFlight(t *testing.T) {
	// A slow mirror plus concurrent callers must produce one request, not one per
	// caller (the daily round and a panel click can easily overlap).
	var mu sync.Mutex
	hits := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		<-release
		_, _ = w.Write(fixtureBytes(t))
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := manager(t, dir, srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Ensure(context.Background(), true); err != nil {
				t.Errorf("Ensure: %v", err)
			}
		}()
	}
	// Give the goroutines time to pile up on the in-flight refresh.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (refreshes must not stack)", hits)
	}
}

func TestStatusBeforeAnyRefresh(t *testing.T) {
	dir := t.TempDir()
	m := manager(t, dir, "https://mirror.example/api.json")

	st := m.Status()
	if st.Loaded {
		t.Error("Loaded = true before anything was fetched")
	}
	if st.Path != CachePath(dir) || st.URL != "https://mirror.example/api.json" || !st.AutoUpdate {
		t.Errorf("Status() = %+v, want path/url/auto-update filled", st)
	}
	if st.Providers != 0 || st.Models != 0 || !st.UpdatedAt.IsZero() || st.LastError != "" {
		t.Errorf("Status() = %+v, want zero counts and no error", st)
	}
}

func TestContextCancellationAbortsFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := manager(t, t.TempDir(), srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := m.Ensure(ctx, true); err == nil {
		t.Fatal("Ensure with a cancelled context = nil error, want failure")
	}
	if m.Loaded() {
		t.Error("Loaded() = true after an aborted fetch, want false")
	}
}

func TestParseRealDocumentShape(t *testing.T) {
	// Guard against the fixture drifting away from the real document's shape.
	// Skipped unless the developer kept a copy around (the repo must not carry
	// 4.7 MB of third-party metadata):
	//
	//	curl -o internal/server/modelsdev/testdata/api.real.json https://models.dev/api.json
	raw, err := os.ReadFile(filepath.Join("testdata", "api.real.json"))
	if err != nil {
		t.Skip("no testdata/api.real.json kept locally")
	}
	start := time.Now()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	ix, err := Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("Parse(real document): %v", err)
	}

	// §12.5 claims a typed parse keeps ~3 MB resident where a map[string]any one
	// holds ~22-27 MB. This opt-in run is where that claim can be re-checked.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("parsed %d providers / %d models in %s; heap %d KiB -> %d KiB",
		ix.ProviderCount(), ix.ModelCount(), time.Since(start).Round(time.Millisecond),
		before.HeapAlloc/1024, after.HeapAlloc/1024)

	if ix.ProviderCount() < 100 {
		t.Errorf("ProviderCount = %d, want the real document's hundreds", ix.ProviderCount())
	}
	if _, ok := ix.Lookup("anthropic", "claude-sonnet-4-6"); !ok {
		t.Error("Lookup(anthropic/claude-sonnet-4-6) = miss in the real document")
	}
	if p, ok := ix.Provider("openrouter"); !ok || p.API == "" {
		t.Errorf("Provider(openrouter) = %+v, %v; want an api field", p, ok)
	}

	// Aggregate the level sets over the whole document, using each provider's
	// own protocol hint: this is the sanity check that the vocabulary mapping
	// never leaves a reasoning model with nothing to choose.
	var withOff, withMinimal, empty, emptyReasoning int
	for _, p := range ix.Providers() {
		protocol := ProtocolForNPM(p.NPM)
		if protocol == "" {
			continue
		}
		for _, m := range ix.Models(p.ID) {
			levels := ReasoningLevels(m, protocol)
			switch {
			case slices.Contains(levels, LevelOff):
				withOff++
			case slices.Contains(levels, LevelMinimal):
				withMinimal++
			}
			if len(levels) == 0 {
				empty++
				if m.Reasoning {
					emptyReasoning++
				}
			}
		}
	}
	t.Logf("levels by protocol hint: off=%d minimal=%d empty=%d (of which reasoning=true: %d)",
		withOff, withMinimal, empty, emptyReasoning)

	// Values are what make "minimal" reachable at all; without them the panel
	// could only ever offer low/medium/high (the pre-values behaviour).
	if withMinimal == 0 {
		t.Error("no model in the real document offers minimal; the values field is probably not being parsed")
	}
}

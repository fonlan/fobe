package geoipupdate

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixtureBytes is the checked-in MMDB the geoip package ships for tests: it is
// the only payload in the repo that passes real MMDB validation.
func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "geoip", "testdata", "GeoLite2-Country.mmdb"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

// memSettings is a minimal in-memory store.Settings.
type memSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemSettings() *memSettings { return &memSettings{m: map[string]string{}} }

func (s *memSettings) GetSetting(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (s *memSettings) SetSetting(key, value string, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

func (s *memSettings) set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
}

// mirror serves the given bodies and counts requests per path.
type mirror struct {
	*httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	bytes []byte
}

func newMirror(t *testing.T, fixture []byte) *mirror {
	t.Helper()
	m := &mirror{hits: map[string]int{}, bytes: fixture}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits[r.URL.Path]++
		m.mu.Unlock()
		switch r.URL.Path {
		case "/db.mmdb":
			w.Write(m.bytes)
		case "/junk":
			w.Write([]byte("<html>404 not found</html>"))
		case "/empty":
			w.Write(nil)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mirror) url(path string) string { return m.URL + path }

func (m *mirror) count(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[path]
}

func newManager(t *testing.T, path string, st Settings, cfg Config) *Manager {
	t.Helper()
	cfg.Path = path
	cfg.Settings = st
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	return New(cfg)
}

func TestEnsureDownloadsAndInstalls(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	path := filepath.Join(t.TempDir(), "geoip", "GeoLite2-Country.mmdb")
	st := newMemSettings()

	var phases []string
	var reloads int
	m := newManager(t, path, st, Config{
		Sources:  []string{mir.url("/db.mmdb")},
		Reload:   func() bool { reloads++; return true },
		Progress: func(d Download) { phases = append(phases, d.Phase) },
	})

	got := m.Ensure(context.Background(), true)
	if got.State != StateOK {
		t.Fatalf("state = %q (%s), want ok", got.State, got.Error)
	}
	if got.Source != mir.url("/db.mmdb") {
		t.Fatalf("source = %q, want the mirror URL", got.Source)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("installed file: %v", err)
	}
	if string(onDisk) != string(fixture) {
		t.Fatal("installed bytes differ from the served database")
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1 (the live resolver must pick the file up now)", reloads)
	}

	// The live snapshot is terminal and carries the size, so a panel opening
	// after the update still sees the outcome.
	dl := m.Download()
	if dl.Active || dl.Phase != PhaseDone || dl.Downloaded != int64(len(fixture)) {
		t.Fatalf("download snapshot = %+v, want a finished %d-byte install", dl, len(fixture))
	}
	want := []string{PhaseConnecting, PhaseDownloading, PhaseVerifying, PhaseDone}
	if strings.Join(phases, ",") != strings.Join(want, ",") {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
	// No temp files left behind next to the database.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mmdb-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}

	// The status is persisted for the next boot.
	persisted, err := st.GetSetting(SettingStatus)
	if err != nil || !strings.Contains(persisted, `"state":"ok"`) {
		t.Fatalf("persisted status = %q (%v), want state ok", persisted, err)
	}
}

func TestEnsureFallsBackThroughMirrors(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")

	m := newManager(t, path, newMemSettings(), Config{
		Sources: []string{mir.url("/missing"), mir.url("/junk"), mir.url("/db.mmdb")},
	})
	got := m.Ensure(context.Background(), true)
	if got.State != StateOK || got.Source != mir.url("/db.mmdb") {
		t.Fatalf("status = %+v, want ok from /db.mmdb", got)
	}
	for _, p := range []string{"/missing", "/junk", "/db.mmdb"} {
		if mir.count(p) != 1 {
			t.Fatalf("%s hit %d times, want exactly 1", p, mir.count(p))
		}
	}
}

func TestEnsureKeepsWorkingDatabaseWhenEveryMirrorFails(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	dir := t.TempDir()
	path := filepath.Join(dir, "GeoLite2-Country.mmdb")
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	m := newManager(t, path, newMemSettings(), Config{
		Sources: []string{mir.url("/junk"), mir.url("/empty"), mir.url("/missing")},
	})
	got := m.Ensure(context.Background(), true)
	if got.State != StateFailed || got.Error == "" {
		t.Fatalf("status = %+v, want a failure with a reason", got)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the previous database must survive a failed update: %v", err)
	}
	if string(onDisk) != string(fixture) {
		t.Fatal("a failed update replaced the live database")
	}
	if dl := m.Download(); dl.Active || dl.Phase != PhaseFailed {
		t.Fatalf("download snapshot = %+v, want a terminal failure", dl)
	}
}

func TestEnsureRejectsOversizedBody(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")

	m := newManager(t, path, newMemSettings(), Config{
		Sources:  []string{mir.url("/db.mmdb")},
		MaxBytes: 32, // smaller than any database
	})
	got := m.Ensure(context.Background(), true)
	if got.State != StateFailed || !strings.Contains(got.Error, "size limit") {
		t.Fatalf("status = %+v, want a size-limit failure", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized body must not be installed: %v", err)
	}
}

func TestEnsureSkipsFreshDatabase(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	m := newManager(t, path, newMemSettings(), Config{Sources: []string{mir.url("/db.mmdb")}})
	if got := m.Ensure(context.Background(), false); got.State != StateOK {
		t.Fatalf("status = %+v, want ok", got)
	}
	if mir.count("/db.mmdb") != 0 {
		t.Fatal("a fresh database must not cause a download")
	}
	if got := m.Ensure(context.Background(), true); got.State != StateOK {
		t.Fatalf("forced status = %+v, want ok", got)
	}
	if mir.count("/db.mmdb") != 1 {
		t.Fatal("force=true must bypass the freshness check")
	}
}

func TestEnsureRefreshesPerMaxAgeSetting(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	// The file was last written ten days ago.
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	st := newMemSettings()
	m := newManager(t, path, st, Config{Sources: []string{mir.url("/db.mmdb")}})

	// Default threshold is 7 days: ten days old is stale.
	if got := m.Ensure(context.Background(), false); got.State != StateOK {
		t.Fatalf("status = %+v, want ok", got)
	}
	if mir.count("/db.mmdb") != 1 {
		t.Fatal("a database older than the default threshold must be refreshed")
	}

	// Raise the threshold to 30 days and age the file again: the next check
	// must leave it alone.
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	st.set(SettingMaxAgeDays, "30")
	if got := m.Ensure(context.Background(), false); got.State != StateOK {
		t.Fatalf("status = %+v, want ok", got)
	}
	if mir.count("/db.mmdb") != 1 {
		t.Fatal("a database inside the configured threshold must not be refetched")
	}
	if got := m.Status(); got.MaxAgeDays != 30 {
		t.Fatalf("status max_age_days = %d, want 30", got.MaxAgeDays)
	}
}

func TestCheckHonoursSwitches(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	old := time.Now().Add(-30 * 24 * time.Hour)

	st := newMemSettings()
	st.set(SettingAutoUpdate, "0")
	m := newManager(t, path, st, Config{Sources: []string{mir.url("/db.mmdb")}, AutoUpdate: true})
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	m.Check(context.Background())
	if mir.count("/db.mmdb") != 0 {
		t.Fatal("the panel switch off must stop the periodic refresh")
	}

	st.set(SettingAutoUpdate, "1")
	m.Check(context.Background())
	if mir.count("/db.mmdb") != 1 {
		t.Fatal("the periodic check must refresh a stale database once enabled")
	}
}

func TestEnvSwitchDisablesAutoUpdate(t *testing.T) {
	fixture := fixtureBytes(t)
	mir := newMirror(t, fixture)
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newManager(t, path, newMemSettings(), Config{
		Sources:    []string{mir.url("/db.mmdb")},
		AutoUpdate: false,
	})
	m.Start(ctx)
	if !m.EnvLocked() {
		t.Fatal("env_locked must be reported so the panel can explain the dead switch")
	}
	if m.Status().State != StateDisabled {
		t.Fatalf("state = %q, want disabled", m.Status().State)
	}
	time.Sleep(50 * time.Millisecond)
	if mir.count("/db.mmdb") != 0 {
		t.Fatal("FOBE_GEOIP_AUTO_UPDATE=0 must stop even the startup download")
	}
	if got := m.Ensure(context.Background(), true); got.State != StateOK {
		t.Fatalf("the manual path must still work: %+v", got)
	}
}

func TestStartDownloadsMissingDatabaseInBackground(t *testing.T) {
	fixture := fixtureBytes(t)
	path := filepath.Join(t.TempDir(), "geoip", "GeoLite2-Country.mmdb")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var hits int64
	progress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Write(fixture)
	}))
	defer progress.Close()

	m := newManager(t, path, newMemSettings(), Config{Sources: []string{progress.URL}, AutoUpdate: true})
	m.Start(ctx)
	if m.Status().State != StatePending {
		t.Fatalf("startup state = %q, want pending while the download runs", m.Status().State)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil && m.Status().State == StateOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("startup download did not finish: status = %+v", m.Status())
}

func TestSourcesPrecedence(t *testing.T) {
	st := newMemSettings()
	st.set(SettingSourceURL, "https://internal.example/db.mmdb")
	m := newManager(t, "/tmp/x.mmdb", st, Config{Sources: DefaultSourceURLs})

	// No env pin: the setting replaces the chain entirely.
	if got := m.sources(); len(got) != 1 || got[0] != "https://internal.example/db.mmdb" {
		t.Fatalf("sources = %v, want the pinned setting URL only", got)
	}
	// The env pin wins over the setting.
	m = newManager(t, "/tmp/x.mmdb", st, Config{Sources: DefaultSourceURLs, SourceURL: "https://env.example/db.mmdb"})
	if got := m.sources(); len(got) != 1 || got[0] != "https://env.example/db.mmdb" {
		t.Fatalf("sources = %v, want the env URL only", got)
	}
	// Nothing pinned: the default chain.
	m = newManager(t, "/tmp/x.mmdb", newMemSettings(), Config{})
	if got := m.sources(); len(got) != len(DefaultSourceURLs) {
		t.Fatalf("sources = %v, want the default mirror chain", got)
	}
}

func TestStatusDefaultsAndLoad(t *testing.T) {
	st := newMemSettings()
	m := newManager(t, "/tmp/x.mmdb", st, Config{AutoUpdate: true})
	if got := m.Status(); got.State != "" {
		t.Fatalf("status before Start = %+v, want zero", got)
	}
	if st, ok := LoadStatus(newMemSettings()); ok || st.State != "" {
		t.Fatal("LoadStatus must report nothing for a fresh install")
	}
	if st, ok := LoadStatus(nil); ok {
		t.Fatalf("LoadStatus(nil) = %+v, want false", st)
	}

	now := time.Now()
	m.cfg.Now = func() time.Time { return now }
	m.setStatus(Status{State: StateOK, Source: "https://mirror/db.mmdb"})
	if got, ok := LoadStatus(st); !ok || got.State != StateOK || got.Source != "https://mirror/db.mmdb" {
		t.Fatalf("persisted status = %+v (%v)", got, ok)
	}
	// The switch state is stamped from the manager, not from the caller.
	if got := m.Status(); !got.AutoUpdate || got.MaxAgeDays != DefaultMaxAgeDays || got.CheckedAt != now.Unix() {
		t.Fatalf("status = %+v, want the defaults stamped", got)
	}
}

func TestDownloadSpeedAndPercent(t *testing.T) {
	// 200 KiB of *invalid* bytes: the download reports progress before the
	// verification fails, which is exactly the path the throttle covers.
	blob := make([]byte, 200<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "204800")
		w.Write(blob)
	}))
	defer srv.Close()

	var last Download
	m := newManager(t, filepath.Join(t.TempDir(), "x.mmdb"), newMemSettings(), Config{
		Sources:  []string{srv.URL},
		Progress: func(d Download) { last = d },
		Now:      func() time.Time { return time.Now() },
	})
	if got := m.Ensure(context.Background(), true); got.State != StateFailed {
		t.Fatalf("status = %+v, want failed (the blob is not an MMDB)", got)
	}
	if last.Phase != PhaseFailed {
		t.Fatalf("last observation = %+v, want the failure", last)
	}
}

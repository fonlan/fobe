package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/server/singboxcache"
	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "fobe.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testDLClient builds the artifact client the way runServer does — the server
// owns the one process-wide instance (§9.5.3), so these tests keep exercising
// the same upstream overrides the environment provides.
func testDLClient(dlDir string) *singboxdl.Client {
	return singboxdl.New(singboxdl.Config{
		DLDir:        dlDir,
		APIBase:      os.Getenv("FOBE_SINGBOX_API_BASE"),
		DownloadBase: os.Getenv("FOBE_SINGBOX_DOWNLOAD_BASE"),
		Log:          testLogger(),
	})
}

// writeCachedVersion lays out a valid cached release (<DLDir>/singbox/<ver>/).
func writeCachedVersion(t *testing.T, dlDir, version string) {
	t.Helper()
	dir := filepath.Join(dlDir, singboxdl.DirName, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, singboxdl.BinaryName), []byte("bin-"+version), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
}

// loadCacheStatus reads the status the panel will show (singbox.cache_status).
func loadCacheStatus(t *testing.T, st *store.Store) singboxcache.Status {
	t.Helper()
	s, ok := singboxcache.LoadStatus(st)
	if !ok {
		t.Fatalf("%s was not written to settings", singboxcache.SettingCacheStatus)
	}
	return s
}

// waitForCacheState polls the persisted status until it reports want.
func waitForCacheState(t *testing.T, st *store.Store, want string) singboxcache.Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := singboxcache.LoadStatus(st); ok && s.State == want {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	raw, err := st.GetSetting(singboxcache.SettingCacheStatus)
	t.Fatalf("cache status never became %q (last: %s err=%v)", want, raw, err)
	return singboxcache.Status{}
}

func TestSingboxAutoDownloadEnabled(t *testing.T) {
	cases := map[string]bool{
		"":         true, // unset: enabled by default
		"1":        true,
		"true":     true,
		"on":       true,
		"yes":      true,
		"0":        false,
		"false":    false,
		"off":      false,
		"no":       false,
		" OFF ":    false, // trimmed and case-insensitive
		"False":    false,
		"whatever": true, // only the documented off-values disable it
	}
	for value, want := range cases {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("FOBE_SINGBOX_AUTO_DOWNLOAD", value)
			if got := singboxAutoDownloadEnabled(); got != want {
				t.Fatalf("FOBE_SINGBOX_AUTO_DOWNLOAD=%q -> %v, want %v", value, got, want)
			}
		})
	}
}

func TestStartSingboxCacheDisabledWritesStatus(t *testing.T) {
	t.Setenv("FOBE_SINGBOX_AUTO_DOWNLOAD", "0")
	st := newTestStore(t)
	dlDir := t.TempDir()

	mgr := startSingboxCache(context.Background(), st, testLogger(), testDLClient(dlDir))

	s := loadCacheStatus(t, st)
	if s.State != singboxcache.StateDisabled {
		t.Fatalf("state = %q, want disabled", s.State)
	}
	if !strings.Contains(s.Error, "FOBE_SINGBOX_AUTO_DOWNLOAD") {
		t.Fatalf("error = %q, want the switch named", s.Error)
	}
	if s.AutoDownload {
		t.Fatalf("auto_download = true, want false: %+v", s)
	}
	if s.UpdatedAt == 0 {
		t.Fatalf("updated_at not stamped: %+v", s)
	}
	if mgr.Status().State != singboxcache.StateDisabled {
		t.Fatalf("manager status = %+v", mgr.Status())
	}
	if _, err := os.Stat(filepath.Join(dlDir, singboxdl.DirName)); !os.IsNotExist(err) {
		t.Fatalf("a disabled auto-download must not create the cache dir: %v", err)
	}
}

func TestStartSingboxCacheWithoutDLDirWritesDisabledStatus(t *testing.T) {
	t.Setenv("FOBE_SINGBOX_AUTO_DOWNLOAD", "1")
	st := newTestStore(t)

	startSingboxCache(context.Background(), st, testLogger(), testDLClient(""))

	s := loadCacheStatus(t, st)
	if s.State != singboxcache.StateDisabled || !strings.Contains(s.Error, "FOBE_DL_DIR") {
		t.Fatalf("status = %+v", s)
	}
}

func TestStartSingboxCacheWarmCacheNeverCallsTheAPI(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "the network must not be touched", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("FOBE_SINGBOX_AUTO_DOWNLOAD", "1")
	t.Setenv("FOBE_SINGBOX_API_BASE", srv.URL)
	t.Setenv("FOBE_SINGBOX_DOWNLOAD_BASE", srv.URL)

	st := newTestStore(t)
	dlDir := t.TempDir()
	writeCachedVersion(t, dlDir, "1.9.0")

	startSingboxCache(context.Background(), st, testLogger(), testDLClient(dlDir))

	s := loadCacheStatus(t, st)
	if s.State != singboxcache.StateOK || s.Version != "1.9.0" {
		t.Fatalf("status = %+v, want the cached 1.9.0", s)
	}
	if !s.AutoDownload {
		t.Fatalf("auto_download not mirrored: %+v", s)
	}
	time.Sleep(50 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("a warm cache must stay offline, got %d API requests", n)
	}
}

// TestStartSingboxCacheUsesTheInjectedClient pins the §9.5.3 wiring: runServer
// hands the cache manager api.SingboxDL(), the process-wide client. A manager
// that built its own client would hold a second install mutex, and the startup
// download plus an "update sing-box" job would fetch the same tarball twice.
func TestStartSingboxCacheUsesTheInjectedClient(t *testing.T) {
	t.Setenv("FOBE_SINGBOX_AUTO_DOWNLOAD", "1")
	st := newTestStore(t)
	dlDir := t.TempDir()
	writeCachedVersion(t, dlDir, "1.9.0")

	// Refs is only reachable through the injected client (the warm-cache probe
	// scans the cache with it), so it doubles as a "was this client used?" flag.
	used := false
	dl := singboxdl.New(singboxdl.Config{
		DLDir: dlDir,
		Refs:  func(string) int { used = true; return 0 },
	})
	mgr := startSingboxCache(context.Background(), st, testLogger(), dl)

	if !used {
		t.Fatal("startSingboxCache did not use the client it was given")
	}
	if s := loadCacheStatus(t, st); s.State != singboxcache.StateOK || s.Version != "1.9.0" {
		t.Fatalf("status = %+v, want the cached 1.9.0", s)
	}
	if mgr.Status().State != singboxcache.StateOK {
		t.Fatalf("manager status = %+v", mgr.Status())
	}
}

func TestStartSingboxCacheFailureIsRecordedAndNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "release host down", http.StatusBadGateway)
	}))
	defer srv.Close()
	t.Setenv("FOBE_SINGBOX_AUTO_DOWNLOAD", "1")
	t.Setenv("FOBE_SINGBOX_API_BASE", srv.URL)
	t.Setenv("FOBE_SINGBOX_DOWNLOAD_BASE", srv.URL)

	st := newTestStore(t)
	dlDir := t.TempDir()

	mgr := startSingboxCache(context.Background(), st, testLogger(), testDLClient(dlDir))

	s := waitForCacheState(t, st, singboxcache.StateFailed)
	if s.Error == "" {
		t.Fatalf("a failed download must record the reason: %+v", s)
	}
	if s.Version != "" {
		t.Fatalf("no version was cached, yet status claims %q", s.Version)
	}
	if mgr.Status().State != singboxcache.StateFailed {
		t.Fatalf("manager status = %+v", mgr.Status())
	}
	// The failure is non-fatal: the store stays usable and nothing was cached.
	if _, err := st.GetUser(); err != nil && err != store.ErrNotFound {
		t.Fatalf("store unusable after a failed download: %v", err)
	}
	dl := singboxdl.New(singboxdl.Config{DLDir: dlDir})
	empty, err := dl.CacheEmpty()
	if err != nil {
		t.Fatalf("scan cache: %v", err)
	}
	if !empty {
		t.Fatalf("a failed download must not pollute the cache")
	}
}

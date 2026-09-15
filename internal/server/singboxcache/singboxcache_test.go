package singboxcache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/server/singboxdl"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- settings fake ---

type fakeSettings struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeSettings() *fakeSettings { return &fakeSettings{values: map[string]string{}} }

var errUnset = errors.New("settings: unset")

func (f *fakeSettings) GetSetting(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[key]
	if !ok {
		return "", errUnset
	}
	return v, nil
}

func (f *fakeSettings) SetSetting(key, value string, encrypted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
	return nil
}

func (f *fakeSettings) get(t *testing.T, key string) string {
	t.Helper()
	v, err := f.GetSetting(key)
	if err != nil {
		t.Fatalf("setting %s unset: %v", key, err)
	}
	return v
}

// --- release fixture (fake GitHub API + asset download) ---

type releaseFixture struct {
	t        *testing.T
	srv      *httptest.Server
	versions []string
	status   int
	bodies   map[string][]byte
	digests  map[string]string

	mu        sync.Mutex
	apiHits   int
	assetHits int
}

// newReleaseFixture serves releases for the given versions (newest first is
// irrelevant: sing-box picks the highest stable). status != 0 makes the API
// fail with that status.
func newReleaseFixture(t *testing.T, status int, versions ...string) *releaseFixture {
	t.Helper()
	f := &releaseFixture{t: t, versions: versions, status: status, bodies: map[string][]byte{}, digests: map[string]string{}}
	for _, v := range versions {
		name := singboxdl.AssetName(v)
		body := makeTarGz(t, "sing-box-"+v+"-linux-amd64-musl/sing-box", []byte("FAKE-SINGBOX-"+v))
		sum := sha256.Sum256(body)
		f.bodies[name] = body
		f.digests[name] = hex.EncodeToString(sum[:])
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func makeTarGz(t *testing.T, member string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: member, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func (f *releaseFixture) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/releases"):
		f.mu.Lock()
		f.apiHits++
		f.mu.Unlock()
		if f.status != 0 {
			http.Error(w, "boom", f.status)
			return
		}
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
		out := []releaseJSON{}
		for _, v := range f.versions {
			name := singboxdl.AssetName(v)
			out = append(out, releaseJSON{TagName: "v" + v, Assets: []assetJSON{{
				Name:               name,
				BrowserDownloadURL: f.srv.URL + "/dl/" + name,
				Digest:             "sha256:" + f.digests[name],
				Size:               int64(len(f.bodies[name])),
			}}})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			f.t.Errorf("encode releases: %v", err)
		}
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		f.mu.Lock()
		f.assetHits++
		f.mu.Unlock()
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		body, ok := f.bodies[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

func (f *releaseFixture) counts() (api, asset int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiHits, f.assetHits
}

func (f *releaseFixture) client(dlDir string) *singboxdl.Client {
	return singboxdl.New(singboxdl.Config{
		DLDir:        dlDir,
		APIBase:      f.srv.URL,
		DownloadBase: f.srv.URL,
		HTTPClient:   f.srv.Client(),
		Log:          testLogger(),
	})
}

// writeCachedVersion lays out a valid cached release the way singboxdl.Install
// would (a regular linux-amd64 file is all ScanCache needs).
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

// waitForState polls the manager until it reports want, or fails the test.
func waitForState(t *testing.T, m *Manager, want string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := m.Status(); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status never became %q (last %+v)", want, m.Status())
	return Status{}
}

func TestStartDownloadsLatestStableWhenCacheEmpty(t *testing.T) {
	fx := newReleaseFixture(t, 0, "1.9.0", "1.10.0", "1.11.0-beta.1")
	dlDir := t.TempDir()
	settings := newFakeSettings()
	m := New(Config{DL: fx.client(dlDir), Settings: settings, Log: testLogger(), AutoDownload: true})

	m.Start(context.Background()) // must return without waiting for the network

	st := waitForState(t, m, StateOK)
	if st.Version != "1.10.0" {
		t.Fatalf("version = %q, want the newest *stable* 1.10.0", st.Version)
	}
	if !st.AutoDownload {
		t.Fatalf("status must mirror the auto-download switch: %+v", st)
	}
	if st.UpdatedAt == 0 {
		t.Fatalf("updated_at not stamped: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dlDir, singboxdl.DirName, "1.10.0", singboxdl.BinaryName)); err != nil {
		t.Fatalf("binary not cached: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dlDir, singboxdl.DirName, "1.11.0-beta.1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prerelease must not be cached: %v", err)
	}

	persisted, ok := LoadStatus(settings)
	if !ok || persisted.State != StateOK || persisted.Version != "1.10.0" {
		t.Fatalf("persisted status = %+v ok=%v", persisted, ok)
	}
	if raw := settings.get(t, SettingCacheStatus); !json.Valid([]byte(raw)) {
		t.Fatalf("settings value is not JSON: %s", raw)
	}
}

func TestStartWarmCacheNeverTouchesTheNetwork(t *testing.T) {
	fx := newReleaseFixture(t, 0, "1.10.0")
	dlDir := t.TempDir()
	writeCachedVersion(t, dlDir, "1.9.0")
	m := New(Config{DL: fx.client(dlDir), Settings: newFakeSettings(), Log: testLogger(), AutoDownload: true})

	m.Start(context.Background())

	st := m.Status() // resolved synchronously: no download was needed
	if st.State != StateOK || st.Version != "1.9.0" {
		t.Fatalf("status = %+v", st)
	}
	time.Sleep(50 * time.Millisecond)
	if api, asset := fx.counts(); api != 0 || asset != 0 {
		t.Fatalf("warm cache must not call the network: api=%d asset=%d", api, asset)
	}
}

func TestStartDisabledRecordsReason(t *testing.T) {
	fx := newReleaseFixture(t, 0, "1.10.0")
	dlDir := t.TempDir()
	settings := newFakeSettings()
	m := New(Config{DL: fx.client(dlDir), Settings: settings, Log: testLogger(), AutoDownload: false})

	m.Start(context.Background())

	st := m.Status()
	if st.State != StateDisabled || !strings.Contains(st.Error, "FOBE_SINGBOX_AUTO_DOWNLOAD") {
		t.Fatalf("status = %+v", st)
	}
	if st.AutoDownload {
		t.Fatalf("auto_download must be false: %+v", st)
	}
	if api, _ := fx.counts(); api != 0 {
		t.Fatalf("disabled auto-download must not call the network")
	}
	if persisted, ok := LoadStatus(settings); !ok || persisted.State != StateDisabled {
		t.Fatalf("persisted = %+v ok=%v", persisted, ok)
	}
}

func TestStartWithoutDLDirIsDisabled(t *testing.T) {
	settings := newFakeSettings()
	m := New(Config{DL: singboxdl.New(singboxdl.Config{}), Settings: settings, Log: testLogger(), AutoDownload: true})

	m.Start(context.Background())

	st := m.Status()
	if st.State != StateDisabled || !strings.Contains(st.Error, "FOBE_DL_DIR") {
		t.Fatalf("status = %+v", st)
	}
	if persisted, ok := LoadStatus(settings); !ok || persisted.State != StateDisabled {
		t.Fatalf("persisted = %+v ok=%v", persisted, ok)
	}
}

func TestEnsureFailureRecordsReasonAndLeavesCacheClean(t *testing.T) {
	fx := newReleaseFixture(t, http.StatusInternalServerError, "1.10.0")
	dlDir := t.TempDir()
	settings := newFakeSettings()
	dl := fx.client(dlDir)
	m := New(Config{DL: dl, Settings: settings, Log: testLogger(), AutoDownload: true})

	st := m.Ensure(context.Background())
	if st.State != StateFailed || st.Error == "" {
		t.Fatalf("status = %+v", st)
	}
	empty, err := dl.CacheEmpty()
	if err != nil {
		t.Fatalf("scan cache: %v", err)
	}
	if !empty {
		t.Fatalf("a failed download must not pollute the cache")
	}
	if entries, err := os.ReadDir(filepath.Join(dlDir, singboxdl.DirName)); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Fatalf("temp dir left behind: %s", e.Name())
			}
		}
	}
	persisted, ok := LoadStatus(settings)
	if !ok || persisted.State != StateFailed || persisted.Error == "" {
		t.Fatalf("persisted = %+v ok=%v", persisted, ok)
	}
}

func TestEnsureIsIdempotentOnceWarm(t *testing.T) {
	fx := newReleaseFixture(t, 0, "1.10.0")
	dlDir := t.TempDir()
	m := New(Config{DL: fx.client(dlDir), Settings: newFakeSettings(), Log: testLogger(), AutoDownload: true})
	ctx := context.Background()

	if st := m.Ensure(ctx); st.State != StateOK || st.Version != "1.10.0" {
		t.Fatalf("first ensure = %+v", st)
	}
	if st := m.Ensure(ctx); st.State != StateOK || st.Version != "1.10.0" {
		t.Fatalf("second ensure = %+v", st)
	}
	if api, _ := fx.counts(); api != 1 {
		t.Fatalf("a warm cache must be answered locally, api hits = %d", api)
	}
}

func TestEnsureSkipsWhenADownloadIsAlreadyRunning(t *testing.T) {
	fx := newReleaseFixture(t, 0, "1.10.0")
	dlDir := t.TempDir()
	dl := fx.client(dlDir)
	m := New(Config{DL: dl, Settings: newFakeSettings(), Log: testLogger(), AutoDownload: true})

	m.mu.Lock()
	m.running = true
	m.mu.Unlock()
	st := m.Ensure(context.Background())
	if api, _ := fx.counts(); api != 0 {
		t.Fatalf("a concurrent ensure must not start a second download")
	}
	if st.State == "" {
		// the running attempt owns the status; the caller only gets it back
		t.Logf("status while running: %+v", st)
	}
}

func TestLoadStatusAndMountOKDefaults(t *testing.T) {
	settings := newFakeSettings()
	if _, ok := LoadStatus(settings); ok {
		t.Fatalf("unset status must not report ok")
	}
	if _, ok := LoadMountOK(settings); ok {
		t.Fatalf("unset mount verdict must not report ok")
	}
	if _, ok := LoadStatus(nil); ok {
		t.Fatalf("nil settings must not report ok")
	}
	if err := settings.SetSetting(SettingCacheStatus, "{not json", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadStatus(settings); ok {
		t.Fatalf("corrupt status must not report ok")
	}
	if err := settings.SetSetting(SettingDLMountOK, "true", false); err != nil {
		t.Fatal(err)
	}
	mounted, ok := LoadMountOK(settings)
	if !ok || !mounted {
		t.Fatalf("mount verdict = %v ok=%v", mounted, ok)
	}
}

func TestStatusSurvivesWithoutSettings(t *testing.T) {
	fx := newReleaseFixture(t, 0, "1.10.0")
	m := New(Config{DL: fx.client(t.TempDir()), Log: testLogger(), AutoDownload: true})
	if st := m.Ensure(context.Background()); st.State != StateOK {
		t.Fatalf("status = %+v", st)
	}
}

func TestCheckMountIsPersistedInsideAContainer(t *testing.T) {
	stubMountEnv(t, true, mountinfoRootOnly)
	dlDir := t.TempDir()
	settings := newFakeSettings()
	m := New(Config{DL: singboxdl.New(singboxdl.Config{DLDir: dlDir}), Settings: settings, Log: testLogger(), AutoDownload: false})

	m.Start(context.Background())

	mounted, ok := LoadMountOK(settings)
	if !ok {
		t.Fatalf("a container run must record the mount verdict")
	}
	if mounted {
		t.Fatalf("a directory on the container layer must be reported as not mounted")
	}
	if mc := m.MountCheck(); mc.OK() {
		t.Fatalf("mount check = %+v", mc)
	}
	if mc := m.MountCheck(); mc.MountPoint != "/" {
		t.Fatalf("mount point = %q", mc.MountPoint)
	}
}

func TestCheckMountIsNotPersistedOutsideAContainer(t *testing.T) {
	stubMountEnv(t, false, mountinfoRootOnly)
	settings := newFakeSettings()
	m := New(Config{DL: singboxdl.New(singboxdl.Config{DLDir: t.TempDir()}), Settings: settings, Log: testLogger(), AutoDownload: false})

	m.Start(context.Background())

	if _, err := settings.GetSetting(SettingDLMountOK); err == nil {
		t.Fatalf("outside a container the mount verdict must stay unwritten (no dev noise)")
	}
	if mc := m.MountCheck(); !mc.OK() {
		t.Fatalf("mount check = %+v", mc)
	}
}

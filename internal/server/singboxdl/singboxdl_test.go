package singboxdl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	testOwner = "SagerNet"
	testRepo  = "sing-box"
	zeroSum   = "0000000000000000000000000000000000000000000000000000000000000000"
)

// makeTarGz builds an in-memory tar.gz from name -> body.
func makeTarGz(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range members {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// fakeRelease describes one release served by the fixture. Mode selects the
// checksum/archive shape:
//
//	""          API digest is correct (primary path)
//	"sidecar"   no API digest, correct <asset>.sha256 sibling
//	"none"      no checksum anywhere (fail-closed)
//	"baddigest" API digest is wrong
//	"badsidecar" no API digest, wrong sibling
//	"badfile"   sibling exists but is not a digest
//	"nobinary"  archive has no sing-box member
//	"corrupt"   archive is not gzip
type fakeRelease struct {
	Version    string
	Prerelease bool
	Draft      bool
	Mode       string
}

type fixture struct {
	t        *testing.T
	releases []fakeRelease
	tarballs map[string][]byte
	goodSum  map[string]string
	srv      *httptest.Server
	dlDir    string
}

func newFixture(t *testing.T, releases ...fakeRelease) *fixture {
	t.Helper()
	f := &fixture{t: t, releases: releases, tarballs: map[string][]byte{}, goodSum: map[string]string{}}
	for _, rl := range releases {
		name := AssetName(rl.Version)
		var body []byte
		switch rl.Mode {
		case "corrupt":
			body = []byte("definitely not a gzip stream")
		case "nobinary":
			body = makeTarGz(t, map[string][]byte{"README.md": []byte("no binary here")})
		default:
			member := "sing-box-" + rl.Version + "-linux-amd64-musl/sing-box"
			body = makeTarGz(t, map[string][]byte{member: []byte("FAKE-SINGBOX-" + rl.Version)})
		}
		sum := sha256.Sum256(body)
		f.tarballs[name] = body
		f.goodSum[name] = hex.EncodeToString(sum[:])
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.dlDir = t.TempDir()
	return f
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/repos/"+testOwner+"/"+testRepo+"/releases" {
		type assetJSON struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Digest             string `json:"digest"`
			Size               int64  `json:"size"`
		}
		type releaseJSON struct {
			TagName    string      `json:"tag_name"`
			Draft      bool        `json:"draft"`
			Prerelease bool        `json:"prerelease"`
			Assets     []assetJSON `json:"assets"`
		}
		out := []releaseJSON{}
		for _, rl := range f.releases {
			name := AssetName(rl.Version)
			a := assetJSON{Name: name, BrowserDownloadURL: f.srv.URL + "/dl/" + name, Size: int64(len(f.tarballs[name]))}
			switch rl.Mode {
			case "baddigest":
				a.Digest = "sha256:" + zeroSum
			case "sidecar", "none", "badsidecar", "badfile", "nobinary", "corrupt":
				a.Digest = ""
			default:
				a.Digest = "sha256:" + f.goodSum[name]
			}
			out = append(out, releaseJSON{TagName: "v" + rl.Version, Draft: rl.Draft, Prerelease: rl.Prerelease, Assets: []assetJSON{a}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/dl/") {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		if strings.HasSuffix(name, ".sha256") {
			if body, ok := f.sidecar(strings.TrimSuffix(name, ".sha256")); ok {
				_, _ = w.Write([]byte(body))
				return
			}
			http.NotFound(w, r)
			return
		}
		if body, ok := f.tarballs[name]; ok {
			_, _ = w.Write(body)
			return
		}
	}
	http.NotFound(w, r)
}

func (f *fixture) sidecar(asset string) (string, bool) {
	for _, rl := range f.releases {
		if AssetName(rl.Version) != asset {
			continue
		}
		switch rl.Mode {
		case "none", "baddigest":
			return "", false
		case "badsidecar":
			return zeroSum + "  " + asset, true
		case "badfile":
			return "not a checksum", true
		default:
			return f.goodSum[asset] + "  " + asset, true
		}
	}
	return "", false
}

func (f *fixture) client(refs func(string) int) *Client {
	return New(Config{
		DLDir:        f.dlDir,
		APIBase:      f.srv.URL,
		DownloadBase: f.srv.URL,
		Owner:        testOwner,
		Repo:         testRepo,
		HTTPClient:   f.srv.Client(),
		Refs:         refs,
	})
}

func (f *fixture) cacheRoot() string { return filepath.Join(f.dlDir, DirName) }

func (f *fixture) install(t *testing.T, version string) *CachedVersion {
	t.Helper()
	c := f.client(nil)
	rel, err := c.ReleaseByVersion(context.Background(), version)
	if err != nil {
		t.Fatalf("resolve %s: %v", version, err)
	}
	got, err := c.Install(context.Background(), rel, false)
	if err != nil {
		t.Fatalf("install %s: %v", version, err)
	}
	return got
}

func (f *fixture) assertNoTempDirs(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.cacheRoot())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("read cache: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			t.Fatalf("leftover temp dir %s", e.Name())
		}
	}
}

func (f *fixture) assertNotCached(t *testing.T, version string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.cacheRoot(), version)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version %s should not be cached, stat err = %v", version, err)
	}
}

func TestInstallSuccessPublishesAgentLayout(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.10.0"})
	got := f.install(t, "1.10.0")
	if got.Version != "1.10.0" || got.Size == 0 || got.DownloadedAt == 0 || len(got.SHA256) != 64 {
		t.Fatalf("unexpected result: %+v", got)
	}

	dir := filepath.Join(f.cacheRoot(), "1.10.0")
	binPath := filepath.Join(dir, BinaryName)
	st, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	if !st.Mode().IsRegular() {
		t.Fatalf("binary is not a regular file")
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode = %v, want 0755", st.Mode().Perm())
	}
	bin, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if string(bin) != "FAKE-SINGBOX-1.10.0" {
		t.Fatalf("binary body = %q", string(bin))
	}
	if int64(len(bin)) != got.Size {
		t.Fatalf("size = %d, file = %d", got.Size, len(bin))
	}

	sidecar, err := os.ReadFile(filepath.Join(dir, ChecksumName))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(sidecar) != got.SHA256+"  "+BinaryName {
		t.Fatalf("sidecar = %q", string(sidecar))
	}

	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if m.Version != "1.10.0" || m.SHA256 != got.SHA256 || m.Asset != AssetName("1.10.0") || m.Size != got.Size {
		t.Fatalf("manifest = %+v", m)
	}
	if _, err := os.Stat(filepath.Join(dir, AssetName("1.10.0"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive must not be published, stat err = %v", err)
	}
	f.assertNoTempDirs(t)
}

func TestInstallTwiceWithoutForce(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.10.0"})
	f.install(t, "1.10.0")
	c := f.client(nil)
	rel, err := c.ReleaseByVersion(context.Background(), "1.10.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := c.Install(context.Background(), rel, false); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("want ErrVersionExists, got %v", err)
	}
}

func TestInstallFallbackToSidecar(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.9.0", Mode: "sidecar"})
	got := f.install(t, "1.9.0")
	if len(got.SHA256) != 64 || got.Version != "1.9.0" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestChecksumMismatchAbortsAndKeepsCacheClean(t *testing.T) {
	for _, mode := range []string{"baddigest", "badsidecar"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, fakeRelease{Version: "1.10.0", Mode: mode})
			c := f.client(nil)
			rel, err := c.ReleaseByVersion(context.Background(), "1.10.0")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if _, err := c.Install(context.Background(), rel, false); !errors.Is(err, ErrChecksumMismatch) {
				t.Fatalf("want ErrChecksumMismatch, got %v", err)
			}
			f.assertNotCached(t, "1.10.0")
			f.assertNoTempDirs(t)
		})
	}
}

func TestNoChecksumFailsClosed(t *testing.T) {
	for _, mode := range []string{"none", "badfile"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, fakeRelease{Version: "1.10.0", Mode: mode})
			c := f.client(nil)
			rel, err := c.ReleaseByVersion(context.Background(), "1.10.0")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if _, err := c.Install(context.Background(), rel, false); !errors.Is(err, ErrChecksumUnavailable) {
				t.Fatalf("want ErrChecksumUnavailable, got %v", err)
			}
			f.assertNotCached(t, "1.10.0")
			f.assertNoTempDirs(t)
		})
	}
}

func TestExtractFailureKeepsCacheClean(t *testing.T) {
	t.Run("no binary member", func(t *testing.T) {
		f := newFixture(t, fakeRelease{Version: "1.10.0", Mode: "nobinary"})
		c := f.client(nil)
		rel, err := c.ReleaseByVersion(context.Background(), "1.10.0")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if _, err := c.Install(context.Background(), rel, false); !errors.Is(err, ErrBinaryNotFound) {
			t.Fatalf("want ErrBinaryNotFound, got %v", err)
		}
		f.assertNotCached(t, "1.10.0")
		f.assertNoTempDirs(t)
	})
	t.Run("corrupt archive", func(t *testing.T) {
		f := newFixture(t, fakeRelease{Version: "1.10.0", Mode: "corrupt"})
		c := f.client(nil)
		rel, err := c.ReleaseByVersion(context.Background(), "1.10.0")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		_, err = c.Install(context.Background(), rel, false)
		if err == nil || errors.Is(err, ErrBinaryNotFound) {
			t.Fatalf("want archive error, got %v", err)
		}
		f.assertNotCached(t, "1.10.0")
		f.assertNoTempDirs(t)
	})
}

func TestInstallMissingAsset(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.10.0"})
	c := f.client(nil)
	rel := Release{Version: "9.9.9", Tag: "v9.9.9"}
	if _, err := c.Install(context.Background(), rel, false); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("want ErrAssetNotFound, got %v", err)
	}
}

func TestLatestStableSkipsDraftsAndPrereleases(t *testing.T) {
	f := newFixture(t,
		fakeRelease{Version: "1.10.0"},
		fakeRelease{Version: "1.11.0-beta.5"},
		fakeRelease{Version: "1.12.0", Prerelease: true},
		fakeRelease{Version: "1.13.0", Draft: true},
	)
	rel, err := f.client(nil).LatestStable(context.Background())
	if err != nil {
		t.Fatalf("latest stable: %v", err)
	}
	if rel.Version != "1.10.0" {
		t.Fatalf("latest = %s, want 1.10.0", rel.Version)
	}
}

func TestScanCacheOrderRefsAndDelete(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.9.0"}, fakeRelease{Version: "1.10.0"})
	f.install(t, "1.9.0")
	f.install(t, "1.10.0")

	c := f.client(func(v string) int {
		if v == "1.10.0" {
			return 3
		}
		return 0
	})
	got, err := c.ScanCache()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d versions, want 2", len(got))
	}
	if got[0].Version != "1.10.0" || got[1].Version != "1.9.0" {
		t.Fatalf("order = %s, %s", got[0].Version, got[1].Version)
	}
	if got[0].Refs != 3 || got[1].Refs != 0 {
		t.Fatalf("refs = %d, %d", got[0].Refs, got[1].Refs)
	}
	if got[0].Size == 0 || got[0].DownloadedAt == 0 || len(got[0].SHA256) != 64 {
		t.Fatalf("entry = %+v", got[0])
	}

	latest, err := c.LatestCached()
	if err != nil || latest.Version != "1.10.0" {
		t.Fatalf("latest cached = %+v, err = %v", latest, err)
	}

	if err := c.DeleteVersion("1.10.0"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	f.assertNotCached(t, "1.10.0")
	if err := c.DeleteVersion("1.10.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if left, err := c.ScanCache(); err != nil || len(left) != 1 {
		t.Fatalf("scan after delete = %+v, err = %v", left, err)
	}
}

func TestScanSkipsPartialAndCleanTemps(t *testing.T) {
	f := newFixture(t, fakeRelease{Version: "1.10.0"})
	f.install(t, "1.10.0")
	if err := os.MkdirAll(filepath.Join(f.cacheRoot(), "1.11.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.cacheRoot(), "not-a-version"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(f.cacheRoot(), tempPrefix+"1.12.0-abcd")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, BinaryName), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := f.client(nil)
	got, err := c.ScanCache()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Version != "1.10.0" {
		t.Fatalf("scan = %+v", got)
	}

	n, err := c.CleanTemps()
	if err != nil {
		t.Fatalf("clean temps: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleaned %d temp dirs, want 1", n)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp dir still present, stat err = %v", err)
	}
	if left, err := c.ScanCache(); err != nil || len(left) != 1 {
		t.Fatalf("scan after clean = %+v, err = %v", left, err)
	}
}

func TestNoDLDirErrors(t *testing.T) {
	c := New(Config{})
	if _, err := c.ScanCache(); !errors.Is(err, ErrNoDLDir) {
		t.Fatalf("scan: want ErrNoDLDir, got %v", err)
	}
	if _, err := c.Install(context.Background(), Release{Version: "1.10.0"}, false); !errors.Is(err, ErrNoDLDir) {
		t.Fatalf("install: want ErrNoDLDir, got %v", err)
	}
	if err := c.DeleteVersion("1.10.0"); !errors.Is(err, ErrNoDLDir) {
		t.Fatalf("delete: want ErrNoDLDir, got %v", err)
	}
}

func TestVersionParsingAndOrdering(t *testing.T) {
	parsed := []struct{ in, want string }{
		{"1.10.0", "1.10.0"},
		{"v1.10.0", "1.10.0"},
		{"1.10", "1.10.0"},
		{"1.11.0-beta.5", "1.11.0-beta.5"},
		{"1.10.0+build.7", "1.10.0"},
	}
	for _, c := range parsed {
		v, err := ParseVersion(c.in)
		if err != nil {
			t.Fatalf("parse %s: %v", c.in, err)
		}
		if v.String() != c.want {
			t.Fatalf("parse %s = %s, want %s", c.in, v.String(), c.want)
		}
	}
	if _, err := ParseVersion("not-a-version"); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("want ErrInvalidVersion, got %v", err)
	}

	order := []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1},
		{"v1.10.0", "1.10.0", 0},
		{"1.11.0-beta.5", "1.11.0", -1},
		{"1.11.0-beta.5", "1.11.0-beta.4", 1},
		{"1.11.0-rc.1", "1.11.0-beta.5", 1},
		{"1.11.0", "1.10.9", 1},
		{"1.10.0+meta", "1.10.0", 0},
	}
	for _, c := range order {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Fatalf("CompareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}

	in := []string{"1.9.0", "1.10.0", "1.10.0-rc.1", "bogus"}
	SortVersions(in)
	want := []string{"1.10.0", "1.10.0-rc.1", "1.9.0", "bogus"}
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", in, want)
		}
	}

	if !IsStableVersion("1.10.0") || IsStableVersion("1.11.0-beta.1") {
		t.Fatalf("IsStableVersion is wrong")
	}
	if AssetName("1.10.0") != "sing-box-1.10.0-linux-amd64-musl.tar.gz" {
		t.Fatalf("AssetName = %s", AssetName("1.10.0"))
	}
}

// legacyMaxJSONBytes is the response cap that used to truncate the real
// listing (8 MiB). Kept here so the regression guard below stays meaningful if
// the cap is ever lowered again.
const legacyMaxJSONBytes = 8 << 20

// TestListReleasesHandlesPageLargerThanLegacyCap is the regression guard for
// the reported "unexpected EOF": SagerNet/sing-box answers
// /releases?per_page=100 with ~33 MB uncompressed (every release repeats
// ~170 assets), so any cap under ~30 MB truncates the array mid-element.
func TestListReleasesHandlesPageLargerThanLegacyCap(t *testing.T) {
	const releases = 100
	// A release body large enough to push the page past the old 8 MiB cap;
	// GitHub really sends one (unmapped here, so the decoder skips it).
	body := strings.Repeat("x", 120<<10)

	var buf bytes.Buffer
	buf.WriteByte('[')
	// Newest first, the order the API returns (ListReleases preserves it).
	for i := releases - 1; i >= 0; i-- {
		if i != releases-1 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf,
			`{"tag_name":"v1.%d.0","draft":false,"prerelease":false,"body":%q,`+
				`"assets":[{"name":%q,"browser_download_url":"https://example.test/x","digest":"sha256:%s","size":1}]}`,
			i, body, AssetName(fmt.Sprintf("1.%d.0", i)), zeroSum)
	}
	buf.WriteByte(']')
	if buf.Len() <= legacyMaxJSONBytes {
		t.Fatalf("fixture is %d bytes, must exceed the old %d-byte cap", buf.Len(), legacyMaxJSONBytes)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c := New(Config{APIBase: srv.URL, DownloadBase: srv.URL, Owner: testOwner, Repo: testRepo, HTTPClient: srv.Client()})
	got, err := c.ListReleases(context.Background())
	if err != nil {
		t.Fatalf("ListReleases on a %d-byte page: %v", buf.Len(), err)
	}
	if len(got) != releases {
		t.Fatalf("ListReleases = %d releases, want %d", len(got), releases)
	}
	if got[0].Version != "1.99.0" || got[0].Prerelease {
		t.Fatalf("first release = %+v, want newest 1.99.0 stable", got[0])
	}
}

// TestDecodeJSONBodyOverflowIsNotReportedAsEOF pins the error text: hitting the
// size cap must not masquerade as a truncated download (io.ErrUnexpectedEOF),
// while a genuinely short stream must keep its real cause.
func TestDecodeJSONBodyOverflowIsNotReportedAsEOF(t *testing.T) {
	full := []byte(`[{"tag_name":"v1.10.0"}]`)

	var out []githubRelease
	err := decodeJSONBody(bytes.NewReader(full), 8, &out)
	if err == nil || !strings.Contains(err.Error(), "response exceeds 8 bytes") {
		t.Fatalf("overflow error = %v, want a size report", err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("overflow must not be reported as EOF: %v", err)
	}

	var truncated []githubRelease
	err = decodeJSONBody(bytes.NewReader(full[:10]), 4096, &truncated)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated stream = %v, want io.ErrUnexpectedEOF", err)
	}
}

// pagedAPI serves a multi-page releases listing the way GitHub does (Link
// header with rel="next") and counts the pages actually fetched, so the tests
// below can assert that single-target lookups stop early.
type pagedAPI struct {
	pages [][]fakeRelease
	srv   *httptest.Server
	mu    sync.Mutex
	hits  int
	// perPage records the per_page each request asked for, so the tests can
	// pin the "latest" fast path to a small page.
	perPage []string
}

func newPagedAPI(t *testing.T, pages ...[]fakeRelease) *pagedAPI {
	t.Helper()
	p := &pagedAPI{pages: pages}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pagedAPI) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits++
	p.perPage = append(p.perPage, r.URL.Query().Get("per_page"))

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > len(p.pages) {
		http.NotFound(w, r)
		return
	}
	if page < len(p.pages) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/%s/%s/releases?per_page=100&page=%d>; rel="next", `+
			`<%s/repos/%s/%s/releases?per_page=100&page=%d>; rel="last"`,
			p.srv.URL, testOwner, testRepo, page+1, p.srv.URL, testOwner, testRepo, len(p.pages)))
	}
	out := []map[string]any{}
	for _, rl := range p.pages[page-1] {
		out = append(out, map[string]any{
			"tag_name":   "v" + rl.Version,
			"draft":      rl.Draft,
			"prerelease": rl.Prerelease,
			"assets": []map[string]any{{
				"name": AssetName(rl.Version), "browser_download_url": p.srv.URL + "/dl/" + AssetName(rl.Version),
				"digest": "sha256:" + zeroSum, "size": 1,
			}},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (p *pagedAPI) client() *Client {
	return New(Config{
		APIBase: p.srv.URL, DownloadBase: p.srv.URL,
		Owner: testOwner, Repo: testRepo, HTTPClient: p.srv.Client(),
	})
}

func (p *pagedAPI) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

func (p *pagedAPI) pageSizes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.perPage...)
}

func TestListReleasesFollowsLinkPagination(t *testing.T) {
	p := newPagedAPI(t,
		[]fakeRelease{{Version: "1.12.0-beta.1", Prerelease: true}, {Version: "1.11.0"}},
		[]fakeRelease{{Version: "1.10.0"}, {Version: "not-a-version"}, {Version: "1.9.0", Draft: true}},
	)
	got, err := p.client().ListReleases(context.Background())
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	want := []string{"1.12.0-beta.1", "1.11.0", "1.10.0"}
	if len(got) != len(want) {
		t.Fatalf("ListReleases = %+v, want %v", got, want)
	}
	for i := range want {
		if got[i].Version != want[i] {
			t.Fatalf("ListReleases[%d] = %s, want %s", i, got[i].Version, want[i])
		}
	}
	if !got[0].Prerelease || got[1].Prerelease {
		t.Fatalf("prerelease flags wrong: %+v", got)
	}
	if n := p.requests(); n != 2 {
		t.Fatalf("fetched %d pages, want 2", n)
	}
	if got := p.pageSizes(); len(got) != 2 || got[0] != "100" {
		t.Fatalf("per_page = %v, want 100", got)
	}
}

func TestLatestStableStopsAtFirstStablePage(t *testing.T) {
	p := newPagedAPI(t,
		[]fakeRelease{{Version: "1.13.0-alpha.1", Prerelease: true}, {Version: "1.12.0"}},
		[]fakeRelease{{Version: "1.11.0"}},
	)
	rel, err := p.client().LatestStable(context.Background())
	if err != nil {
		t.Fatalf("LatestStable: %v", err)
	}
	if rel.Version != "1.12.0" {
		t.Fatalf("latest = %s, want 1.12.0", rel.Version)
	}
	if n := p.requests(); n != 1 {
		t.Fatalf("fetched %d pages, want 1 (the rest of the history is older)", n)
	}
	// The fast path must keep asking for the small page: at 100 the single
	// request alone is ~33 MB and eats most of the panel's resolve budget.
	if got := p.pageSizes(); len(got) != 1 || got[0] != "30" {
		t.Fatalf("per_page = %v, want 30", got)
	}
}

func TestLatestStableWalksPastPrereleaseOnlyPages(t *testing.T) {
	p := newPagedAPI(t,
		[]fakeRelease{{Version: "1.13.0-beta.2", Prerelease: true}, {Version: "1.13.0-beta.1", Prerelease: true}},
		[]fakeRelease{{Version: "1.12.0"}},
	)
	rel, err := p.client().LatestStable(context.Background())
	if err != nil {
		t.Fatalf("LatestStable: %v", err)
	}
	if rel.Version != "1.12.0" {
		t.Fatalf("latest = %s, want 1.12.0", rel.Version)
	}
	if n := p.requests(); n != 2 {
		t.Fatalf("fetched %d pages, want 2", n)
	}
}

func TestReleaseByVersionStopsAtMatchingPage(t *testing.T) {
	p := newPagedAPI(t,
		[]fakeRelease{{Version: "1.11.0"}, {Version: "1.10.0"}},
		[]fakeRelease{{Version: "1.9.0"}},
	)
	c := p.client()
	rel, err := c.ReleaseByVersion(context.Background(), "v1.10.0")
	if err != nil {
		t.Fatalf("ReleaseByVersion: %v", err)
	}
	if rel.Version != "1.10.0" || rel.Tag != "v1.10.0" {
		t.Fatalf("resolved %+v, want 1.10.0 / v1.10.0", rel)
	}
	if n := p.requests(); n != 1 {
		t.Fatalf("fetched %d pages for a page-1 hit, want 1", n)
	}

	if _, err := c.ReleaseByVersion(context.Background(), "1.8.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version = %v, want ErrNotFound", err)
	}
	if n := p.requests(); n != 3 {
		t.Fatalf("fetched %d pages total, want 3 (miss walks the whole history)", n)
	}
}

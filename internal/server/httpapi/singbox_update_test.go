package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/server/singboxcache"
	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

// seedSingboxNode registers a node that already runs sing-box with a desired
// state (§9): the batch update only ever touches those.
func seedSingboxNode(t *testing.T, api *Server, id, name, version, desired string, port int) {
	t.Helper()
	if err := api.Store.CreateNode(&store.Node{ID: id, Name: name, MachineID: "m-" + id}, "hash"); err != nil {
		t.Fatalf("create node %s: %v", id, err)
	}
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: version, DesiredVersion: desired, Port: port,
		Status: "running", ConfigHash: "cfg-" + id,
	}); err != nil {
		t.Fatalf("upsert node_singbox %s: %v", id, err)
	}
}

type cacheBody struct {
	DLDir           string `json:"dl_dir"`
	LatestCached    string `json:"latest_cached"`
	AutoDownload    bool   `json:"auto_download"`
	MountOK         bool   `json:"mount_ok"`
	MountApplicable bool   `json:"mount_applicable"`
	Versions        []struct {
		Version      string `json:"version"`
		Size         int64  `json:"size"`
		DownloadedAt int64  `json:"downloaded_at"`
		Refs         int    `json:"refs"`
		IsLatest     bool   `json:"is_latest"`
	} `json:"versions"`
	CacheStatus json.RawMessage `json:"cache_status"`
	LastUpdate  *jobBody        `json:"last_update"`
	Download    SingboxDownload `json:"download"`
}

type jobBody struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	TargetVersion string `json:"target_version"`
	Deadline      int64  `json:"deadline"`
	Error         string `json:"error"`
	Counts        struct {
		Total          int `json:"total"`
		AlreadyCurrent int `json:"already_current"`
		Pushed         int `json:"pushed"`
		OfflinePending int `json:"offline_pending"`
		Failed         int `json:"failed"`
	} `json:"counts"`
	Nodes []struct {
		NodeID         string `json:"node_id"`
		Outcome        string `json:"outcome"`
		Reason         string `json:"reason"`
		DesiredVersion string `json:"desired_version"`
	} `json:"nodes"`
}

func decodeCache(t *testing.T, raw []byte) cacheBody {
	t.Helper()
	var body cacheBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode cache body: %v (%s)", err, raw)
	}
	return body
}

func TestSingboxCacheEndpoint(t *testing.T) {
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	api.SingboxAutoDownload = true
	writeCachedVersion(t, api.DLDir, "1.9.0", []byte("bin-1.9.0"), 1700000000)
	writeCachedVersion(t, api.DLDir, "1.10.0", []byte("bin-1.10.0-longer"), 1700000100)
	seedSingboxNode(t, api, "n1", "tokyo", "1.9.0", "1.10.0", 23456)

	cookie := loginSession(t, srv)
	resp, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/cache", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	body := decodeCache(t, raw)
	if body.DLDir != api.DLDir {
		t.Fatalf("dl_dir = %q", body.DLDir)
	}
	if len(body.Versions) != 2 || body.Versions[0].Version != "1.10.0" || !body.Versions[0].IsLatest {
		t.Fatalf("versions = %+v", body.Versions)
	}
	if body.Versions[1].IsLatest {
		t.Fatalf("only the newest cached version carries is_latest: %+v", body.Versions)
	}
	if body.LatestCached != "1.10.0" {
		t.Fatalf("latest_cached = %q", body.LatestCached)
	}
	if body.Versions[0].Refs != 1 || body.Versions[1].Refs != 0 {
		t.Fatalf("refs = %d/%d", body.Versions[0].Refs, body.Versions[1].Refs)
	}
	if body.Versions[0].Size != int64(len("bin-1.10.0-longer")) || body.Versions[0].DownloadedAt != 1700000100 {
		t.Fatalf("size/downloaded_at = %+v", body.Versions[0])
	}
	if !body.AutoDownload {
		t.Fatal("auto_download = false")
	}
	if body.MountApplicable {
		t.Fatal("mount checks are not applicable in a plain test process")
	}
	if body.LastUpdate != nil {
		t.Fatalf("last_update = %+v, want null before any batch", body.LastUpdate)
	}

	// no session → 401
	anon, err := http.Get(srv.URL + "/api/singbox/cache")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", anon.StatusCode)
	}
}

// TestSingboxUpdateImpactEndpoint covers the three upstream shapes behind the
// confirmation dialog. Each scenario gets its own server: the artifact client
// is built once per server process and bound to SingboxAPIBase at that moment
// (§9.5.3), so a test that swaps the base mid-flight would silently talk to
// the previous host.
func TestSingboxUpdateImpactEndpoint(t *testing.T) {
	seed := func(t *testing.T) (*httptest.Server, *Server, string) {
		t.Helper()
		srv, api := newTestServer(t)
		api.DLDir = t.TempDir()
		writeCachedVersion(t, api.DLDir, "1.10.0", []byte("bin"), 1700000000)
		seedSingboxNode(t, api, "n1", "old", "1.9.0", "1.9.0", 1000)
		seedSingboxNode(t, api, "n2", "current", "1.10.0", "1.10.0", 1001)
		// no desired state → not part of any distribution
		if err := api.Store.CreateNode(&store.Node{ID: "n3", Name: "unmanaged", MachineID: "m-n3"}, "hash"); err != nil {
			t.Fatal(err)
		}
		return srv, api, loginSession(t, srv)
	}
	impact := func(t *testing.T, srv *httptest.Server, cookie, query string) (int, []byte) {
		t.Helper()
		resp, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/update/impact?version="+query, cookie, nil)
		return resp.StatusCode, raw
	}

	t.Run("explicit version stays local", func(t *testing.T) {
		srv, _, cookie := seed(t)
		status, raw := impact(t, srv, cookie, "1.10.0")
		if status != http.StatusOK {
			t.Fatalf("status = %d: %s", status, raw)
		}
		var imp struct {
			TargetVersion  string `json:"target_version"`
			Cached         bool   `json:"cached"`
			DownloadNeeded bool   `json:"download_needed"`
			Count          int    `json:"count"`
			Offline        int    `json:"offline"`
			AlreadyCurrent int    `json:"already_current"`
		}
		if err := json.Unmarshal(raw, &imp); err != nil {
			t.Fatalf("decode impact: %v", err)
		}
		if imp.TargetVersion != "1.10.0" || imp.Count != 2 || imp.AlreadyCurrent != 1 || imp.Offline != 2 {
			t.Fatalf("impact = %+v", imp)
		}
		if !imp.Cached || imp.DownloadNeeded {
			t.Fatalf("cache flags = %+v", imp)
		}
	})

	// "latest" is the one case that talks upstream; v-prefixed tags normalize
	t.Run("latest resolves upstream", func(t *testing.T) {
		srv, api, cookie := seed(t)
		apiBase := fakeReleaseServer(t, "v1.12.0")
		api.SingboxAPIBase = apiBase
		api.SingboxDownloadBase = apiBase
		status, raw := impact(t, srv, cookie, "latest")
		if status != http.StatusOK {
			t.Fatalf("latest impact status = %d: %s", status, raw)
		}
		var imp struct {
			TargetVersion  string `json:"target_version"`
			Requested      string `json:"requested"`
			DownloadNeeded bool   `json:"download_needed"`
		}
		if err := json.Unmarshal(raw, &imp); err != nil {
			t.Fatalf("decode latest impact: %v", err)
		}
		if imp.TargetVersion != "1.12.0" || imp.Requested != "latest" || !imp.DownloadNeeded {
			t.Fatalf("latest impact = %+v", imp)
		}
	})

	t.Run("unreachable host is a 502", func(t *testing.T) {
		srv, api, cookie := seed(t)
		api.SingboxAPIBase = "http://127.0.0.1:1"
		api.SingboxDownloadBase = "http://127.0.0.1:1"
		if status, raw := impact(t, srv, cookie, "latest"); status != http.StatusBadGateway {
			t.Fatalf("unreachable host status = %d: %s", status, raw)
		}
	})
}

// fakeReleaseServer serves just enough of the GitHub releases API to resolve
// the latest stable tag.
func fakeReleaseServer(t *testing.T, tag string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/SagerNet/sing-box/releases" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q,"digest":"sha256:%s","size":1}]}]`,
			tag, singboxdl.AssetName("1.12.0"), "http://127.0.0.1:1/x", "00")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSingboxUpdateFlowOverHTTP(t *testing.T) {
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	writeCachedVersion(t, api.DLDir, "1.10.0", []byte("bin"), 1700000000)
	seedSingboxNode(t, api, "n1", "tokyo", "1.9.0", "1.9.0", 23456)
	seedSingboxNode(t, api, "n2", "osaka", "1.9.0", "1.9.0", 23457)
	cookie := loginSession(t, srv)

	// the confirmation flag is mandatory
	resp, raw := doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/update", cookie,
		[]byte(`{"version":"1.10.0"}`))
	if resp.StatusCode != http.StatusBadRequest || !hasCode(raw, "confirm_required") {
		t.Fatalf("unconfirmed update: %d %s", resp.StatusCode, raw)
	}

	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/update", cookie,
		[]byte(`{"version":"1.10.0","confirm":true}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update status = %d: %s", resp.StatusCode, raw)
	}
	var accepted struct {
		Job jobBody `json:"job"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatalf("decode accepted job: %v", err)
	}
	if accepted.Job.ID == "" || accepted.Job.TargetVersion != "1.10.0" {
		t.Fatalf("accepted job = %+v", accepted.Job)
	}

	done := waitJobOverHTTP(t, srv, cookie, accepted.Job.ID, "done")
	// the hub has no live agent in this test: every node is offline_pending
	if done.Counts.Total != 2 || done.Counts.OfflinePending != 2 || done.Counts.Failed != 0 {
		t.Fatalf("counts = %+v", done.Counts)
	}
	if done.Deadline == 0 {
		t.Fatalf("deadline not stamped: %+v", done)
	}

	// desired_version moved and the port survived
	for _, id := range []string{"n1", "n2"} {
		sb, err := api.Store.GetNodeSingbox(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if sb.DesiredVersion != "1.10.0" {
			t.Fatalf("%s desired = %q", id, sb.DesiredVersion)
		}
	}
	if sb, _ := api.Store.GetNodeSingbox("n1"); sb.Port != 23456 {
		t.Fatalf("port changed: %d", sb.Port)
	}

	// an unknown job id is a 404, not the current job
	resp, _ = doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/update/sbupd-nope", cookie, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown job status = %d", resp.StatusCode)
	}

	// the cache view now carries the finished job (a refresh sees progress)
	resp, raw = doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/cache", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache status = %d", resp.StatusCode)
	}
	body := decodeCache(t, raw)
	if body.LastUpdate == nil || body.LastUpdate.ID != accepted.Job.ID || body.LastUpdate.State != "done" {
		t.Fatalf("last_update = %+v", body.LastUpdate)
	}

	// the batch is audited
	audits, err := api.Store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, a := range audits {
		if a.Action == "singbox_update" && a.Command == "1.10.0" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("no singbox_update audit entry: %+v", audits)
	}
}

func TestSingboxUpdatePublishesProgressOnEventsWS(t *testing.T) {
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	writeCachedVersion(t, api.DLDir, "1.10.0", []byte("bin"), 1700000000)
	seedSingboxNode(t, api, "n1", "tokyo", "1.9.0", "1.9.0", 23456)
	cookie := loginSession(t, srv)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/events"
	hdr := http.Header{}
	hdr.Set("Cookie", cookie)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("dial /ws/events: %v", err)
	}
	defer conn.Close()
	// give the server a moment to register the subscriber: events published
	// before that are dropped (slow-client policy) and the read would block.
	time.Sleep(150 * time.Millisecond)

	resp, raw := doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/update", cookie,
		[]byte(`{"version":"1.10.0","confirm":true}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update status = %d: %s", resp.StatusCode, raw)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	sawProgress := false
	for i := 0; i < 20 && !sawProgress; i++ {
		var ev struct {
			Kind string `json:"kind"`
			Ref  string `json:"ref"`
			Data struct {
				State         string `json:"state"`
				TargetVersion string `json:"target_version"`
				Counts        struct {
					Total int `json:"total"`
				} `json:"counts"`
			} `json:"data"`
		}
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("read event: %v", err)
		}
		if ev.Kind == "singbox_update" && ev.Ref != "" && ev.Data.TargetVersion == "1.10.0" {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Fatal("no singbox_update progress event on /ws/events")
	}
}

func TestSingboxCacheRetryEndpoint(t *testing.T) {
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	// an unreachable release host makes the retry record a failure instead of
	// hanging the test
	api.SingboxAPIBase = "http://127.0.0.1:1"
	api.SingboxDownloadBase = "http://127.0.0.1:1"
	mgr := singboxcache.New(singboxcache.Config{
		DL: singboxdl.New(singboxdl.Config{
			DLDir: api.DLDir, APIBase: api.SingboxAPIBase, DownloadBase: api.SingboxDownloadBase,
		}),
		Settings: api.Store, Log: testLogger(), AutoDownload: true,
	})
	api.SingboxCache = mgr
	cookie := loginSession(t, srv)

	resp, raw := doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/cache/retry", cookie, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry status = %d: %s", resp.StatusCode, raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && mgr.Status().State != singboxcache.StateFailed {
		time.Sleep(5 * time.Millisecond)
	}
	if got := mgr.Status(); got.State != singboxcache.StateFailed || got.Error == "" {
		t.Fatalf("retry outcome = %+v, want a recorded failure", got)
	}

	audits, err := api.Store.ListAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, a := range audits {
		if a.Action == "singbox_cache_retry" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("retry was not audited: %+v", audits)
	}
}

func TestSingboxCacheRetryWithoutManagerIsUnavailable(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginSession(t, srv)
	resp, raw := doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/cache/retry", cookie, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !hasCode(raw, "cache_unavailable") {
		t.Fatalf("retry without a manager: %d %s", resp.StatusCode, raw)
	}
}

func TestSingboxUpdateNeedsTargetsAndSession(t *testing.T) {
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	writeCachedVersion(t, api.DLDir, "1.10.0", []byte("bin"), 1700000000)
	cookie := loginSession(t, srv)

	resp, raw := doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/update", cookie,
		[]byte(`{"version":"1.10.0","confirm":true}`))
	if resp.StatusCode != http.StatusBadRequest || !hasCode(raw, "no_targets") {
		t.Fatalf("no-targets update: %d %s", resp.StatusCode, raw)
	}

	anon, err := http.Post(srv.URL+"/api/singbox/update", "application/json",
		nil)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous update = %d", anon.StatusCode)
	}
}

func TestSingboxDeleteVersionEndpoint(t *testing.T) {
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	writeCachedVersion(t, api.DLDir, "1.9.0", []byte("bin-1.9.0"), 1700000000)
	writeCachedVersion(t, api.DLDir, "1.10.0", []byte("bin-1.10.0"), 1700000100)
	seedSingboxNode(t, api, "n1", "tokyo", "1.10.0", "1.10.0", 23456)
	cookie := loginSession(t, srv)
	dirOf := func(v string) string { return filepath.Join(api.DLDir, singboxdl.DirName, v) }

	// referenced version: refused without force, with the reference count
	resp, raw := doAuthed(t, http.MethodDelete, srv.URL+"/api/singbox/versions/1.10.0", cookie, nil)
	if resp.StatusCode != http.StatusConflict || !hasCode(raw, "version_in_use") {
		t.Fatalf("referenced delete: %d %s", resp.StatusCode, raw)
	}
	var conflict struct {
		Refs int `json:"refs"`
	}
	if err := json.Unmarshal(raw, &conflict); err != nil || conflict.Refs != 1 {
		t.Fatalf("conflict body = %s (%v)", raw, err)
	}
	if _, err := os.Stat(dirOf("1.10.0")); err != nil {
		t.Fatalf("version vanished without force: %v", err)
	}

	// an unreferenced version deletes straight away
	resp, raw = doAuthed(t, http.MethodDelete, srv.URL+"/api/singbox/versions/1.9.0", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unreferenced delete: %d %s", resp.StatusCode, raw)
	}
	if _, err := os.Stat(dirOf("1.9.0")); !os.IsNotExist(err) {
		t.Fatalf("version survived: %v", err)
	}

	// force deletes a referenced version
	resp, raw = doAuthed(t, http.MethodDelete, srv.URL+"/api/singbox/versions/1.10.0?force=1", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forced delete: %d %s", resp.StatusCode, raw)
	}
	if _, err := os.Stat(dirOf("1.10.0")); !os.IsNotExist(err) {
		t.Fatalf("referenced version survived force: %v", err)
	}

	// unknown version → 404, junk version → 400
	resp, _ = doAuthed(t, http.MethodDelete, srv.URL+"/api/singbox/versions/9.9.9", cookie, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown delete status = %d", resp.StatusCode)
	}
	resp, _ = doAuthed(t, http.MethodDelete, srv.URL+"/api/singbox/versions/not-a-version", cookie, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("junk delete status = %d", resp.StatusCode)
	}
}

// waitJobOverHTTP polls the job endpoint until it reaches want.
func waitJobOverHTTP(t *testing.T, srv *httptest.Server, cookie, jobID, want string) jobBody {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last jobBody
	for time.Now().Before(deadline) {
		resp, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/update/"+jobID, cookie, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("job status = %d: %s", resp.StatusCode, raw)
		}
		var body struct {
			Job jobBody `json:"job"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode job: %v", err)
		}
		last = body.Job
		if last.State == want {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job never reached %q: %+v", want, last)
	return last
}

func hasCode(raw []byte, code string) bool {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return false
	}
	return body.Error.Code == code
}

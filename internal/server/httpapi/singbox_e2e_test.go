// Server-side sing-box acceptance tests (design §9.2 实现修订 / §9.5).
//
// These two tests drive the whole chain against a fake GitHub-compatible
// release host, so they never touch the network:
//
//	TestSingboxEndToEndAcceptance  空缓存 → 启动自动下载 → 面板一键更新（latest 固化为
//	                               具体版本）→ desired_version 下发到在线 agent 并
//	                               排队给离线节点 → 双节点收敛 → 无未收敛告警
//	TestSingboxEndToEndChecksumFailureKeepsCacheClean
//	                               校验失败必须中止：缓存不被污染、期望版本不动
//
// The startup glue that reads FOBE_SINGBOX_* and calls the cache manager lives
// in package main; its wiring is covered by cmd/server/main_test.go, while this
// file starts at the cache policy itself.
package httpapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/singboxcache"
	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

const (
	e2eOwner = "SagerNet"
	e2eRepo  = "sing-box"
	e2eZero  = "0000000000000000000000000000000000000000000000000000000000000000"
)

// e2eRelease is a GitHub-compatible release host for the acceptance tests: the
// release list endpoint singboxdl reads and the asset download it follows.
// mode selects the checksum shape — "" serves the correct API digest,
// "baddigest" serves a wrong one and no sibling .sha256, so installation must
// fail closed.
type e2eRelease struct {
	t        *testing.T
	srv      *httptest.Server
	versions []string
	mode     string
	bodies   map[string][]byte
	digests  map[string]string
	assets   atomic.Int64
	// lists counts release-listing fetches, so a test can prove the picker
	// answered from its TTL cache instead of refetching the upstream listing.
	lists atomic.Int64
	// failListings makes the releases endpoint answer 500 (the "stale picker"
	// path). It is read per request, so a test can flip it after a good fetch.
	failListings atomic.Bool
	// hold, when non-nil, makes the asset handler write half the body, flush
	// and wait for the channel before finishing: a download that is provably
	// still in flight. Nil keeps the plain "serve it all at once" behaviour.
	hold chan struct{}
}

func newE2ERelease(t *testing.T, mode string, versions ...string) *e2eRelease {
	t.Helper()
	f := &e2eRelease{t: t, mode: mode, versions: versions, bodies: map[string][]byte{}, digests: map[string]string{}}
	for _, v := range versions {
		name := singboxdl.AssetName(v)
		body := e2eTarGz(t, "sing-box-"+v+"-linux-amd64-musl/sing-box", []byte("E2E-SINGBOX-"+v))
		sum := sha256.Sum256(body)
		f.bodies[name] = body
		f.digests[name] = hex.EncodeToString(sum[:])
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func e2eTarGz(t *testing.T, member string, body []byte) []byte {
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

func (f *e2eRelease) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/repos/"+e2eOwner+"/"+e2eRepo+"/releases" {
		f.lists.Add(1)
		if f.failListings.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
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
		for _, v := range f.versions {
			name := singboxdl.AssetName(v)
			a := assetJSON{
				Name:               name,
				BrowserDownloadURL: f.srv.URL + "/dl/" + name,
				Size:               int64(len(f.bodies[name])),
			}
			if f.mode == "baddigest" {
				a.Digest = "sha256:" + e2eZero
			} else {
				a.Digest = "sha256:" + f.digests[name]
			}
			out = append(out, releaseJSON{TagName: "v" + v, Assets: []assetJSON{a}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/dl/") {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		if body, ok := f.bodies[name]; ok {
			f.assets.Add(1)
			if f.hold != nil {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				half := len(body) / 2
				_, _ = w.Write(body[:half])
				w.(http.Flusher).Flush()
				<-f.hold
				_, _ = w.Write(body[half:])
				return
			}
			_, _ = w.Write(body)
			return
		}
	}
	http.NotFound(w, r)
}

// client builds the downloader the server itself would build, pointed at this
// fixture.
func (f *e2eRelease) client(dlDir string, refs func(string) int) *singboxdl.Client {
	return singboxdl.New(singboxdl.Config{
		DLDir:        dlDir,
		APIBase:      f.srv.URL,
		DownloadBase: f.srv.URL,
		Owner:        e2eOwner,
		Repo:         e2eRepo,
		HTTPClient:   f.srv.Client(),
		Refs:         refs,
	})
}

// --- end-to-end acceptance (§9.5) ------------------------------------------

func TestSingboxEndToEndAcceptance(t *testing.T) {
	rel := newE2ERelease(t, "", "1.10.0")
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	api.SingboxAPIBase = rel.srv.URL
	api.SingboxDownloadBase = rel.srv.URL
	api.SingboxAutoDownload = true

	// 1. 启动自动下载：缓存是空的，服务端自己把最新稳定版取回来（§9.5.3）。
	cache := singboxcache.New(singboxcache.Config{
		DL:           rel.client(api.DLDir, nil),
		Settings:     api.Store,
		Log:          testLogger(),
		AutoDownload: true,
	})
	cache.Start(context.Background())
	api.SingboxCache = cache

	status := e2eWaitCacheState(t, api.Store, singboxcache.StateOK)
	if status.Version != "1.10.0" || !status.AutoDownload {
		t.Fatalf("cache status = %+v", status)
	}
	e2eAssertCachedLayout(t, api.DLDir, "1.10.0")
	if got := rel.assets.Load(); got != 1 {
		t.Fatalf("startup fetched %d assets, want exactly 1", got)
	}

	cookie := loginSession(t, srv)

	// 面板读到的就是刚下载的版本（设置页的缓存列表）
	resp, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/cache", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache endpoint = %d: %s", resp.StatusCode, raw)
	}
	body := decodeCache(t, raw)
	if len(body.Versions) != 1 || body.Versions[0].Version != "1.10.0" || !body.Versions[0].IsLatest {
		t.Fatalf("cached versions = %+v", body.Versions)
	}
	if body.LatestCached != "1.10.0" || body.CacheStatus == nil {
		t.Fatalf("cache body = %+v", body)
	}

	// 2. 两个已启用 sing-box 的节点：n1 有活跃 agent，n2 离线。都还停在 1.9.0。
	const (
		n2     = "n2"
		portN1 = 23456
		portN2 = 23457
	)
	n1, secret := e2eRegisterAgent(t, srv, cookie, "tokyo", "m-e2e-1")
	seedSingboxNode(t, api, n2, "osaka", "1.9.0", "1.9.0", portN2)
	e2eSeedSingbox(t, api, n1, "1.9.0", "1.9.0", portN1)
	ws := e2eDialAgent(t, srv, n1, secret, "m-e2e-1")

	// 3. 确认弹窗的预检：latest 在点击那一刻解析并固化为具体版本号。
	resp, raw = doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/update/impact?version=latest", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("impact = %d: %s", resp.StatusCode, raw)
	}
	var impact struct {
		TargetVersion  string `json:"target_version"`
		Requested      string `json:"requested"`
		Cached         bool   `json:"cached"`
		DownloadNeeded bool   `json:"download_needed"`
		Count          int    `json:"count"`
	}
	if err := json.Unmarshal(raw, &impact); err != nil {
		t.Fatalf("decode impact: %v", err)
	}
	if impact.TargetVersion != "1.10.0" || impact.Requested != "latest" || !impact.Cached || impact.DownloadNeeded {
		t.Fatalf("impact = %+v", impact)
	}
	if impact.Count != 2 {
		t.Fatalf("impact count = %d, want the two sing-box nodes", impact.Count)
	}

	// 4. 二次确认是硬闸门。
	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/update", cookie, []byte(`{"latest":true}`))
	if resp.StatusCode != http.StatusBadRequest || !hasCode(raw, "confirm_required") {
		t.Fatalf("unconfirmed update = %d %s", resp.StatusCode, raw)
	}
	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/update", cookie,
		[]byte(`{"latest":true,"confirm":true}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update = %d: %s", resp.StatusCode, raw)
	}
	var accepted struct {
		Job jobBody `json:"job"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatalf("decode accepted job: %v", err)
	}
	if accepted.Job.ID == "" || accepted.Job.TargetVersion != "1.10.0" {
		t.Fatalf("accepted job = %+v (latest must be frozen to a concrete version)", accepted.Job)
	}

	// 5. desired_version 真的下发到了在线 agent（端口保持不变）。
	desired := e2eAwaitDesiredVersion(t, ws, "1.10.0")
	if desired.Port != portN1 {
		t.Fatalf("desired port = %d, want the untouched %d", desired.Port, portN1)
	}
	// agent 应用后回报实际版本（真实的收敛路径）。
	e2eAgentReport(t, ws, "1.10.0", portN1)

	done := waitJobOverHTTP(t, srv, cookie, accepted.Job.ID, "done")
	if done.Counts.Total != 2 || done.Counts.Pushed != 1 || done.Counts.OfflinePending != 1 || done.Counts.Failed != 0 {
		t.Fatalf("counts = %+v", done.Counts)
	}
	// the batch reused the version the startup download had already cached
	if got := rel.assets.Load(); got != 1 {
		t.Fatalf("the batch re-downloaded the artifact: %d asset fetches", got)
	}
	for _, r := range done.Nodes {
		if r.DesiredVersion != "1.10.0" {
			t.Fatalf("node result = %+v", r)
		}
	}

	// 离线节点重连后由 hello_ack 收敛（§7 声明式期望状态）——这里直接写入
	// agent 会回报的内容，声明式通道本身由 TestFullAgentPath 覆盖。
	e2eConvergeNode(t, api, n2, "1.10.0", portN2)
	e2eWaitNodeVersion(t, api, n1, "1.10.0")

	// 6. 15 分钟收敛复查：两个节点都已到位，因此不产生未收敛告警（§9.5.4）。
	api.SingboxUpdater().CheckConvergence(accepted.Job.ID)
	job := api.SingboxUpdater().Current()
	if job == nil || job.ID != accepted.Job.ID {
		t.Fatalf("current job = %+v", job)
	}
	if !job.ConvergenceChecked || len(job.Stale) != 0 || job.StaleAlerted {
		t.Fatalf("convergence check = %+v (stale=%v)", job, job.Stale)
	}
	if alerts := e2eAlertsOfKind(t, api.Store, "singbox_update_stale"); len(alerts) != 0 {
		t.Fatalf("converged nodes must not raise a stale alert: %+v", alerts)
	}

	// 期望版本与端口都保住了
	for id, port := range map[string]int{n1: portN1, n2: portN2} {
		sb, err := api.Store.GetNodeSingbox(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if sb.DesiredVersion != "1.10.0" || sb.Version != "1.10.0" || sb.Port != port {
			t.Fatalf("%s = %+v", id, sb)
		}
	}
}

// TestSingboxEndToEndChecksumFailureKeepsCacheClean is the fail-closed
// regression: a tampered artifact must abort the download and the distribution,
// leave no half-written version behind, and never move a node's desired state.
func TestSingboxEndToEndChecksumFailureKeepsCacheClean(t *testing.T) {
	rel := newE2ERelease(t, "baddigest", "1.10.0")
	srv, api := newTestServer(t)
	api.DLDir = t.TempDir()
	api.SingboxAPIBase = rel.srv.URL
	api.SingboxDownloadBase = rel.srv.URL
	api.SingboxAutoDownload = true

	cache := singboxcache.New(singboxcache.Config{
		DL:           rel.client(api.DLDir, nil),
		Settings:     api.Store,
		Log:          testLogger(),
		AutoDownload: true,
	})
	cache.Start(context.Background())
	api.SingboxCache = cache

	status := e2eWaitCacheState(t, api.Store, singboxcache.StateFailed)
	if !strings.Contains(status.Error, "sha256") {
		t.Fatalf("failure reason = %q, want the checksum mismatch", status.Error)
	}
	if status.Version != "" {
		t.Fatalf("nothing was installed, yet the status claims %q", status.Version)
	}
	e2eAssertCacheClean(t, api.DLDir)

	cookie := loginSession(t, srv)
	seedSingboxNode(t, api, "n1", "tokyo", "1.9.0", "1.9.0", 23456)

	// 面板没有可选版本可卖，也没有半成品冒出来
	resp, raw := doAuthed(t, http.MethodGet, srv.URL+"/api/singbox/cache", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache endpoint = %d: %s", resp.StatusCode, raw)
	}
	if body := decodeCache(t, raw); len(body.Versions) != 0 {
		t.Fatalf("a rejected download must not publish a version: %+v", body.Versions)
	}

	// 需要下载该版本的批量更新必须整体失败，且不动期望状态
	resp, raw = doAuthed(t, http.MethodPost, srv.URL+"/api/singbox/update", cookie,
		[]byte(`{"latest":true,"confirm":true}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update = %d: %s", resp.StatusCode, raw)
	}
	var accepted struct {
		Job jobBody `json:"job"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatalf("decode accepted job: %v", err)
	}
	failed := waitJobOverHTTP(t, srv, cookie, accepted.Job.ID, "failed")
	if !strings.Contains(failed.Error, "download_failed") || !strings.Contains(failed.Error, "sha256") {
		t.Fatalf("job error = %q", failed.Error)
	}
	if failed.Counts.Total != 0 {
		t.Fatalf("a failed download must distribute nothing: %+v", failed.Counts)
	}
	sb, err := api.Store.GetNodeSingbox("n1")
	if err != nil {
		t.Fatalf("get n1: %v", err)
	}
	if sb.DesiredVersion != "1.9.0" || sb.Version != "1.9.0" {
		t.Fatalf("desired state moved after a rejected artifact: %+v", sb)
	}
	e2eAssertCacheClean(t, api.DLDir)

	// 手动重试同样失败，同样不留残渣
	retry := cache.Ensure(context.Background())
	if retry.State != singboxcache.StateFailed {
		t.Fatalf("manual retry = %+v", retry)
	}
	e2eAssertCacheClean(t, api.DLDir)
}

// --- helpers ---------------------------------------------------------------

// e2eWaitCacheState polls the persisted startup status (singbox.cache_status).
func e2eWaitCacheState(t *testing.T, st *store.Store, want string) singboxcache.Status {
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

// e2eAssertCachedLayout checks the on-disk contract the agent installs from
// (§9.5.1): binary + sidecar checksum + manifest, and no leftover temp dir.
func e2eAssertCachedLayout(t *testing.T, dlDir, version string) {
	t.Helper()
	dir := filepath.Join(dlDir, singboxdl.DirName, version)
	for _, name := range []string{singboxdl.BinaryName, singboxdl.ChecksumName, singboxdl.ManifestName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("cached %s is missing %s: %v", version, name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dlDir, singboxdl.DirName))
	if err != nil {
		t.Fatalf("read cache root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != version {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("cache root holds %v, want only %s", names, version)
	}
}

// e2eAssertCacheClean asserts nothing was published and no half-written install
// survives under <DLDir>/singbox.
func e2eAssertCacheClean(t *testing.T, dlDir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dlDir, singboxdl.DirName))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read cache root: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("a rejected download left %q behind", e.Name())
	}
}

// e2eRegisterAgent runs the install-script registration path and returns the
// node credentials.
func e2eRegisterAgent(t *testing.T, srv *httptest.Server, cookie, name, machineID string) (string, string) {
	t.Helper()
	token := freshToken(t, srv, cookie, name)
	_, reg := postJSON(t, &http.Client{}, srv.URL+"/api/agent/register", map[string]any{
		"token": token, "machine_id": machineID, "hostname": name + "-host",
		"os": "linux", "arch": "amd64", "version": "dev", "tz": "UTC", "cpu_cores": 2,
	})
	id, _ := reg["node_id"].(string)
	secret, _ := reg["node_secret"].(string)
	if id == "" || secret == "" {
		t.Fatalf("register %s failed: %v", name, reg)
	}
	return id, secret
}

// e2eSeedSingbox gives an already-registered node its sing-box state (§9.1).
func e2eSeedSingbox(t *testing.T, api *Server, id, version, desired string, port int) {
	t.Helper()
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: version, DesiredVersion: desired, Port: port,
		Status: "running", ConfigHash: "cfg-" + id,
	}); err != nil {
		t.Fatalf("upsert node_singbox %s: %v", id, err)
	}
}

// e2eDialAgent connects the agent WSS and completes hello → hello_ack.
func e2eDialAgent(t *testing.T, srv *httptest.Server, nodeID, secret, machineID string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, hsResp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
	})
	if err != nil {
		code := 0
		if hsResp != nil {
			code = hsResp.StatusCode
		}
		t.Fatalf("agent dial: %v (status %d)", err, code)
	}
	t.Cleanup(func() { ws.Close() })

	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{
		MachineID: machineID, Hostname: nodeID, Version: "dev",
		OS: "linux", Arch: "amd64", CPUCores: 2, TZ: "UTC",
		IPs: []protocol.IPInfo{{IP: "203.0.113.7", Family: 4, Scope: "public", IsPrimary: true}},
	})); err != nil {
		t.Fatalf("agent hello: %v", err)
	}
	e2eReadEnvelope(t, ws, protocol.TypeHelloAck)
	return ws
}

func e2eReadEnvelope(t *testing.T, ws *websocket.Conn, want ...string) protocol.Envelope {
	t.Helper()
	for {
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var env protocol.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatalf("read agent frame (want %v): %v", want, err)
		}
		if len(want) == 0 {
			return env
		}
		for _, w := range want {
			if env.Type == w {
				return env
			}
		}
	}
}

// e2eAwaitDesiredVersion waits for the desired frame that carries the version
// the batch wrote (§9.5.4: 在线节点 pushDesired).
func e2eAwaitDesiredVersion(t *testing.T, ws *websocket.Conn, want string) protocol.SingboxDesired {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		env := e2eReadEnvelope(t, ws)
		if env.Type != protocol.TypeDesired {
			continue
		}
		var d protocol.DesiredState
		if err := json.Unmarshal(env.Payload, &d); err != nil || d.Singbox == nil {
			continue
		}
		if d.Singbox.Version == want {
			return *d.Singbox
		}
	}
	t.Fatalf("the agent never received a desired frame for %s", want)
	return protocol.SingboxDesired{}
}

// e2eAgentReport sends what a converged agent reports after applying the update.
func e2eAgentReport(t *testing.T, ws *websocket.Conn, version string, port int) {
	t.Helper()
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{
		Singbox: &protocol.SingboxState{Running: true, Version: version, Port: port},
	})); err != nil {
		t.Fatalf("agent state: %v", err)
	}
}

// e2eConvergeNode writes what an offline agent reports after reconnecting and
// converging from hello_ack.
func e2eConvergeNode(t *testing.T, api *Server, id, version string, port int) {
	t.Helper()
	sb, err := api.Store.GetNodeSingbox(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	sb.Version = version
	sb.Port = port
	if err := api.Store.UpsertNodeSingbox(sb); err != nil {
		t.Fatalf("converge %s: %v", id, err)
	}
}

// e2eWaitNodeVersion waits until the store reflects the version an agent
// reported (the hub ingests state frames asynchronously).
func e2eWaitNodeVersion(t *testing.T, api *Server, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sb, err := api.Store.GetNodeSingbox(id); err == nil && sb.Version == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	sb, _ := api.Store.GetNodeSingbox(id)
	t.Fatalf("node %s never reported %s (last %+v)", id, want, sb)
}

func e2eAlertsOfKind(t *testing.T, st *store.Store, kind string) []store.Alert {
	t.Helper()
	alerts, err := st.ListAlerts(50)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	out := []store.Alert{}
	for _, a := range alerts {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

package singboxupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/server/singboxdl"
	"github.com/fonlan/fobe/internal/server/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// writeCached lays out a valid cached release the way singboxdl.Install would.
func writeCached(t *testing.T, dlDir, version string) {
	t.Helper()
	dir := filepath.Join(dlDir, singboxdl.DirName, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("bin-" + version)
	if err := os.WriteFile(filepath.Join(dir, singboxdl.BinaryName), body, 0o755); err != nil {
		t.Fatal(err)
	}
	m := singboxdl.Manifest{
		Version: version, Asset: singboxdl.AssetName(version),
		SHA256: strings.Repeat("a", 64), Size: int64(len(body)), DownloadedAt: 1700000000,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, singboxdl.ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

type fakeOnline map[string]bool

func (f fakeOnline) IsOnline(nodeID string) bool { return f[nodeID] }

// seedNode registers a node that already runs sing-box.
func seedNode(t *testing.T, st *store.Store, id, name, version, desired string, port int) {
	t.Helper()
	if err := st.CreateNode(&store.Node{ID: id, Name: name, MachineID: "m-" + id}, "hash"); err != nil {
		t.Fatalf("create node %s: %v", id, err)
	}
	if err := st.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: id, Version: version, DesiredVersion: desired, Port: port,
		Status: "running", ConfigHash: "cfg-" + id,
	}); err != nil {
		t.Fatalf("upsert node_singbox %s: %v", id, err)
	}
}

// waitJob polls until the job reaches want.
func waitJob(t *testing.T, m *Manager, want string) *Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j := m.Current(); j != nil && j.State == want {
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job never reached %q: %+v", want, m.Current())
	return nil
}

// waitAlert polls until an alert of kind exists.
func waitAlert(t *testing.T, st *store.Store, kind string) store.Alert {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alerts, err := st.ListAlerts(50)
		if err != nil {
			t.Fatalf("list alerts: %v", err)
		}
		for _, a := range alerts {
			if a.Kind == kind {
				return a
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("alert %q was never written", kind)
	return store.Alert{}
}

func TestBatchBucketsOnlineOfflineAndAlreadyCurrent(t *testing.T) {
	st := newStore(t)
	dlDir := t.TempDir()
	writeCached(t, dlDir, "1.10.0")

	seedNode(t, st, "n1", "online-old", "1.9.0", "1.9.0", 23456)
	seedNode(t, st, "n2", "offline-old", "1.9.0", "1.9.0", 23457)
	seedNode(t, st, "n3", "already-current", "1.10.0", "1.10.0", 23458)

	var pushed []string
	m := New(Config{
		Store:  st,
		DL:     singboxdl.New(singboxdl.Config{DLDir: dlDir}),
		Online: fakeOnline{"n1": true, "n3": true},
		Push:   func(id string) bool { pushed = append(pushed, id); return true },
	})
	job, err := m.StartUpdate(UpdateRequest{Version: "1.10.0", Requested: "1.10.0", Actor: "panel"})
	if err != nil {
		t.Fatalf("start update: %v", err)
	}
	if job.State != StatePending || job.ID == "" {
		t.Fatalf("initial job = %+v", job)
	}

	done := waitJob(t, m, StateDone)
	want := Counts{Total: 3, AlreadyCurrent: 1, Pushed: 1, OfflinePending: 1}
	if done.Counts != want {
		t.Fatalf("counts = %+v, want %+v", done.Counts, want)
	}
	if len(pushed) != 1 || pushed[0] != "n1" {
		t.Fatalf("pushed = %v, want [n1]", pushed)
	}
	if done.Deadline == 0 {
		t.Fatalf("deadline was not stamped: %+v", done)
	}

	// the desired version moved, the port and the generated config did not
	for _, id := range []string{"n1", "n2"} {
		sb, err := st.GetNodeSingbox(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if sb.DesiredVersion != "1.10.0" {
			t.Fatalf("%s desired_version = %q", id, sb.DesiredVersion)
		}
		if sb.ConfigHash != "cfg-"+id {
			t.Fatalf("%s config hash changed: %q", id, sb.ConfigHash)
		}
	}
	if sb, _ := st.GetNodeSingbox("n1"); sb.Port != 23456 {
		t.Fatalf("n1 port changed: %d", sb.Port)
	}
	if sb, _ := st.GetNodeSingbox("n2"); sb.Port != 23457 || sb.Status != "installing" {
		t.Fatalf("n2 = %+v", sb)
	}
	if sb, _ := st.GetNodeSingbox("n3"); sb.Status != "running" {
		t.Fatalf("already-current node was touched: %+v", sb)
	}

	// a refreshed page reads the same job out of settings
	raw, err := st.GetSetting(SettingLastUpdate)
	if err != nil {
		t.Fatalf("persisted job missing: %v", err)
	}
	var persisted Job
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		t.Fatalf("decode persisted job: %v", err)
	}
	if persisted.ID != done.ID || persisted.State != StateDone || persisted.Counts != want {
		t.Fatalf("persisted = %+v", persisted)
	}
}

func makeTarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
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

func TestUpdateDownloadsVersionItDoesNotHaveYet(t *testing.T) {
	const version = "1.11.0"
	bin := []byte("FAKE-SINGBOX-" + version)
	archive := makeTarGz(t, "sing-box-"+version+"-linux-amd64-musl/sing-box", bin)
	sum := sha256.Sum256(archive)
	assetName := singboxdl.AssetName(version)

	type assetJSON struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	type releaseJSON struct {
		TagName string      `json:"tag_name"`
		Assets  []assetJSON `json:"assets"`
	}

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/SagerNet/sing-box/releases":
			rel := []releaseJSON{{
				TagName: "v" + version,
				Assets: []assetJSON{{
					Name: assetName, URL: srvURL + "/dl/" + assetName,
					Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(archive)),
				}},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rel)
		case "/dl/" + assetName:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	srvURL = srv.URL
	t.Cleanup(srv.Close)

	st := newStore(t)
	dlDir := t.TempDir()
	seedNode(t, st, "n1", "n1", "1.9.0", "1.9.0", 10000)
	dl := singboxdl.New(singboxdl.Config{DLDir: dlDir, APIBase: srv.URL, DownloadBase: srv.URL})
	m := New(Config{Store: st, DL: dl, Online: fakeOnline{"n1": true}, Push: func(string) bool { return true }})

	rel, err := dl.LatestStable(context.Background())
	if err != nil {
		t.Fatalf("latest stable: %v", err)
	}
	if _, err := m.StartUpdate(UpdateRequest{Version: rel.Version, Requested: "latest", Release: &rel}); err != nil {
		t.Fatalf("start update: %v", err)
	}
	done := waitJob(t, m, StateDone)
	if done.TargetVersion != version {
		t.Fatalf("target = %q, want %q", done.TargetVersion, version)
	}
	cached, err := dl.ScanCache()
	if err != nil {
		t.Fatalf("scan cache: %v", err)
	}
	if len(cached) != 1 || cached[0].Version != version {
		t.Fatalf("cache = %+v", cached)
	}
}

func TestUpdateRefusesSecondJobWhileRunning(t *testing.T) {
	st := newStore(t)
	dlDir := t.TempDir()
	writeCached(t, dlDir, "1.10.0")
	seedNode(t, st, "n1", "n1", "1.9.0", "1.9.0", 10000)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	m := New(Config{
		Store: st, DL: singboxdl.New(singboxdl.Config{DLDir: dlDir}),
		Online: fakeOnline{"n1": true},
		Push: func(string) bool {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return true
		},
	})
	if _, err := m.StartUpdate(UpdateRequest{Version: "1.10.0"}); err != nil {
		t.Fatalf("start update: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never reached the push phase")
	}
	if _, err := m.StartUpdate(UpdateRequest{Version: "1.10.0"}); !errors.Is(err, ErrInProgress) {
		t.Fatalf("second update = %v, want ErrInProgress", err)
	}
	close(release)
	waitJob(t, m, StateDone)
}

func TestFailedPushIsBucketedWithAReason(t *testing.T) {
	st := newStore(t)
	dlDir := t.TempDir()
	writeCached(t, dlDir, "1.10.0")
	seedNode(t, st, "n1", "flaky", "1.9.0", "1.9.0", 1000)

	m := New(Config{
		Store: st, DL: singboxdl.New(singboxdl.Config{DLDir: dlDir}),
		Online: fakeOnline{"n1": true},
		Push:   func(string) bool { return false },
	})
	if _, err := m.StartUpdate(UpdateRequest{Version: "1.10.0"}); err != nil {
		t.Fatalf("start update: %v", err)
	}
	done := waitJob(t, m, StateDone)
	if done.Counts.Failed != 1 || done.Counts.Pushed != 0 {
		t.Fatalf("counts = %+v", done.Counts)
	}
	if len(done.Nodes) != 1 || done.Nodes[0].Outcome != OutcomeFailed || done.Nodes[0].Reason == "" {
		t.Fatalf("nodes = %+v", done.Nodes)
	}
	// the desired state is persisted anyway: the agent converges on reconnect
	sb, err := st.GetNodeSingbox("n1")
	if err != nil {
		t.Fatal(err)
	}
	if sb.DesiredVersion != "1.10.0" {
		t.Fatalf("desired = %q", sb.DesiredVersion)
	}
}

func TestConvergenceRaisesOneAggregateStaleAlert(t *testing.T) {
	st := newStore(t)
	dlDir := t.TempDir()
	writeCached(t, dlDir, "1.10.0")
	seedNode(t, st, "n1", "converged", "1.9.0", "1.9.0", 1000)
	seedNode(t, st, "n2", "stuck", "1.9.0", "1.9.0", 1001)

	m := New(Config{
		Store: st, DL: singboxdl.New(singboxdl.Config{DLDir: dlDir}),
		Online: fakeOnline{"n1": true, "n2": true}, Push: func(string) bool { return true },
	})
	if _, err := m.StartUpdate(UpdateRequest{Version: "1.10.0"}); err != nil {
		t.Fatalf("start update: %v", err)
	}
	done := waitJob(t, m, StateDone)

	// n1 converged while n2 stayed behind
	sb, err := st.GetNodeSingbox("n1")
	if err != nil {
		t.Fatal(err)
	}
	sb.Version = "1.10.0"
	if err := st.UpsertNodeSingbox(sb); err != nil {
		t.Fatal(err)
	}

	m.CheckConvergence(done.ID)
	checked := m.Current()
	if !checked.ConvergenceChecked || checked.CheckedAt == 0 {
		t.Fatalf("check not recorded: %+v", checked)
	}
	if len(checked.Stale) != 1 || checked.Stale[0] != "n2" {
		t.Fatalf("stale = %v, want [n2]", checked.Stale)
	}
	for _, n := range checked.Nodes {
		if n.Converged == nil {
			t.Fatalf("node %s was never checked", n.NodeID)
		}
		switch n.NodeID {
		case "n1":
			if !*n.Converged {
				t.Fatal("n1 reports the target version and must count as converged")
			}
		case "n2":
			if *n.Converged {
				t.Fatal("n2 still reports 1.9.0 and must count as stale")
			}
		}
	}

	alert := waitAlert(t, st, AlertSingboxUpdateStale)
	if alert.NodeID != "" {
		t.Fatalf("alert node = %q, want an aggregate alert", alert.NodeID)
	}
	if !strings.Contains(alert.Payload, "n2") || !strings.Contains(alert.Payload, "1.10.0") {
		t.Fatalf("payload = %s", alert.Payload)
	}
	if !m.Current().StaleAlerted {
		t.Fatal("StaleAlerted was not recorded")
	}

	// a repeated check (restart + timer race) must not write a second alert
	m.CheckConvergence(done.ID)
	alerts, err := st.ListAlerts(50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range alerts {
		if a.Kind == AlertSingboxUpdateStale {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
}

func TestStartRearmsPersistedConvergenceCheck(t *testing.T) {
	st := newStore(t)
	seedNode(t, st, "n1", "stuck", "1.9.0", "1.10.0", 1000)

	// a job written by an earlier process whose deadline has already passed
	persisted := Job{
		ID: "sbupd-restart", State: StateDone, Requested: "latest", TargetVersion: "1.10.0",
		StartedAt: 1, FinishedAt: 2, Deadline: time.Now().Add(-time.Minute).Unix(),
		Nodes: []NodeResult{{
			NodeID: "n1", Name: "stuck", Online: true, Outcome: OutcomePushed,
			Version: "1.9.0", DesiredVersion: "1.10.0",
		}},
		Counts: Counts{Total: 1, Pushed: 1},
	}
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(SettingLastUpdate, string(raw), false); err != nil {
		t.Fatal(err)
	}

	m := New(Config{
		Store: st, DL: singboxdl.New(singboxdl.Config{DLDir: t.TempDir()}),
		Online: fakeOnline{"n1": true},
	})
	if got := m.Current(); got == nil || got.ID != "sbupd-restart" {
		t.Fatalf("persisted job not loaded: %+v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	alert := waitAlert(t, st, AlertSingboxUpdateStale)
	if !strings.Contains(alert.Payload, "n1") {
		t.Fatalf("payload = %s", alert.Payload)
	}
	if got := m.Current(); !got.ConvergenceChecked {
		t.Fatalf("the re-armed check never ran: %+v", got)
	}
}

func TestStartMarksAnInterruptedJobFailed(t *testing.T) {
	st := newStore(t)
	seedNode(t, st, "n1", "n1", "1.9.0", "1.10.0", 1000)
	persisted := Job{
		ID: "sbupd-crash", State: StatePushing, TargetVersion: "1.10.0", StartedAt: 1,
		Nodes: []NodeResult{{NodeID: "n1", Outcome: OutcomePushed}},
	}
	raw, _ := json.Marshal(persisted)
	if err := st.SetSetting(SettingLastUpdate, string(raw), false); err != nil {
		t.Fatal(err)
	}

	m := New(Config{Store: st})
	m.Start(context.Background())

	got := m.Current()
	if got.State != StateFailed {
		t.Fatalf("state = %q, want failed", got.State)
	}
	if !strings.Contains(got.Error, "restart") {
		t.Fatalf("error = %q", got.Error)
	}
	if got.FinishedAt == 0 {
		t.Fatalf("finished_at not stamped: %+v", got)
	}
}

func TestDeleteVersionNeedsForceWhenReferenced(t *testing.T) {
	st := newStore(t)
	dlDir := t.TempDir()
	writeCached(t, dlDir, "1.9.0")
	writeCached(t, dlDir, "1.10.0")
	seedNode(t, st, "n1", "n1", "1.10.0", "1.10.0", 1000)

	m := New(Config{Store: st, DL: singboxdl.New(singboxdl.Config{DLDir: dlDir})})
	var inUse *InUseError
	err := m.DeleteVersion("1.10.0", false)
	if !errors.As(err, &inUse) {
		t.Fatalf("delete referenced version = %v, want *InUseError", err)
	}
	if inUse.Refs != 1 {
		t.Fatalf("refs = %d, want 1", inUse.Refs)
	}
	if _, statErr := os.Stat(filepath.Join(dlDir, singboxdl.DirName, "1.10.0")); statErr != nil {
		t.Fatalf("the version was removed without force: %v", statErr)
	}
	if err := m.DeleteVersion("1.10.0", true); err != nil {
		t.Fatalf("forced delete: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dlDir, singboxdl.DirName, "1.10.0")); !os.IsNotExist(statErr) {
		t.Fatalf("the version survived a forced delete: %v", statErr)
	}
	if err := m.DeleteVersion("9.9.9", false); !errors.Is(err, singboxdl.ErrNotFound) {
		t.Fatalf("unknown version = %v, want ErrNotFound", err)
	}
}

func TestImpactListsAffectedNodesOnly(t *testing.T) {
	st := newStore(t)
	dlDir := t.TempDir()
	writeCached(t, dlDir, "1.10.0")
	seedNode(t, st, "n1", "old", "1.9.0", "1.9.0", 1000)
	seedNode(t, st, "n2", "current", "1.10.0", "1.10.0", 1001)
	// a node without a desired state is not part of a distribution
	if err := st.CreateNode(&store.Node{ID: "n3", Name: "unmanaged", MachineID: "m-n3"}, "hash"); err != nil {
		t.Fatal(err)
	}

	m := New(Config{
		Store: st, DL: singboxdl.New(singboxdl.Config{DLDir: dlDir}),
		Online: fakeOnline{"n1": true},
	})
	imp, err := m.Impact("1.10.0")
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if imp.Count != 2 || imp.Online != 1 || imp.Offline != 1 || imp.AlreadyCurrent != 1 {
		t.Fatalf("impact = %+v", imp)
	}
	if !imp.Cached || imp.DownloadNeeded {
		t.Fatalf("cache flags = %+v", imp)
	}
	imp2, err := m.Impact("1.11.0")
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if imp2.Cached || !imp2.DownloadNeeded {
		t.Fatalf("uncached flags = %+v", imp2)
	}
}

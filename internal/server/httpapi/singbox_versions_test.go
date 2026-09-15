package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// writeCachedVersion lays out a minimal but valid cached release under
// <DLDir>/singbox/<version>/ the way singboxdl.Install would.
func writeCachedVersion(t *testing.T, dlDir, version string, body []byte, downloadedAt int64) {
	t.Helper()
	dir := filepath.Join(dlDir, singboxdl.DirName, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, singboxdl.BinaryName), body, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	m := singboxdl.Manifest{
		Version:      version,
		Asset:        singboxdl.AssetName(version),
		SHA256:       strings.Repeat("a", 64),
		Size:         int64(len(body)),
		DownloadedAt: downloadedAt,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, singboxdl.ManifestName), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestSingboxVersionsSemverOrderAndFields(t *testing.T) {
	_, api := newTestServer(t)
	api.DLDir = t.TempDir()

	writeCachedVersion(t, api.DLDir, "1.9.0", []byte("bin-1.9.0"), 1700000000)
	writeCachedVersion(t, api.DLDir, "1.10.0", []byte("bin-1.10.0-longer"), 1700000100)
	// invalid / half-written entries must be ignored
	if err := os.MkdirAll(filepath.Join(api.DLDir, singboxdl.DirName, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(api.DLDir, singboxdl.DirName, "1.11.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	// one node still points at 1.10.0 -> Refs
	if err := api.Store.CreateNode(&store.Node{ID: "n1", Name: "n1", MachineID: "m1"}, "hash"); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: "n1", DesiredVersion: "1.10.0", Status: "running",
	}); err != nil {
		t.Fatalf("upsert node_singbox: %v", err)
	}

	got := api.singboxVersions()
	if len(got) != 2 {
		t.Fatalf("got %d versions, want 2: %+v", len(got), got)
	}
	if got[0].Version != "1.10.0" || got[1].Version != "1.9.0" {
		t.Fatalf("semver order = %s, %s", got[0].Version, got[1].Version)
	}
	if got[0].Size != int64(len("bin-1.10.0-longer")) {
		t.Fatalf("size = %d", got[0].Size)
	}
	if got[0].DownloadedAt != 1700000100 {
		t.Fatalf("downloaded_at = %d", got[0].DownloadedAt)
	}
	if got[0].Refs != 1 || got[1].Refs != 0 {
		t.Fatalf("refs = %d, %d", got[0].Refs, got[1].Refs)
	}
	if len(got[0].SHA256) != 64 {
		t.Fatalf("sha256 = %q", got[0].SHA256)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/singbox/versions", nil)
	api.handleSingboxVersions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Versions []SingboxVersion `json:"versions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Versions) != 2 || body.Versions[0].Version != "1.10.0" {
		t.Fatalf("body = %+v", body)
	}
}

func TestSingboxVersionsEmptyWithoutDLDir(t *testing.T) {
	_, api := newTestServer(t)
	if got := api.singboxVersions(); len(got) != 0 {
		t.Fatalf("want empty list, got %+v", got)
	}
}

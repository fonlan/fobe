package agentupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSeed plants a build inside the image's seed layout.
func writeSeed(t *testing.T, seedDir, version, body string) {
	t.Helper()
	dir := filepath.Join(seedDir, "agent", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"linux-amd64":        body,
		"linux-amd64.sha256": "checksum-" + body,
		"manifest.json":      `{"version":"` + version + `"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeedCopiesArtifactsAndPointsLatest(t *testing.T) {
	seed, dl := t.TempDir(), t.TempDir()
	writeSeed(t, seed, "20260915.000000", "agent-binary")

	if err := Seed(seed, dl, "20260915.000000", testLog()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dl, "agent", "20260915.000000", "linux-amd64"))
	if err != nil || string(body) != "agent-binary" {
		t.Fatalf("artifact not copied: %q %v", body, err)
	}
	if !ArtifactPresent(dl, "20260915.000000") {
		t.Fatal("seeded build must satisfy the publish gate")
	}

	// latest must point at the running server's build, or /install.sh would
	// install whatever an earlier image left behind.
	link := filepath.Join(dl, "agent", "latest", "linux-amd64")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("latest is not a symlink: %v", err)
	}
	want := filepath.Join("..", "20260915.000000", "linux-amd64")
	if got != want {
		t.Fatalf("latest → %q, want %q", got, want)
	}
	if body, err := os.ReadFile(link); err != nil || string(body) != "agent-binary" {
		t.Fatalf("latest does not resolve to the artifact: %q %v", body, err)
	}
}

func TestSeedIsAdditiveAndIdempotent(t *testing.T) {
	seed, dl := t.TempDir(), t.TempDir()
	writeSeed(t, seed, "20260915.000000", "new-image-build")

	// The volume already holds a different build of the same version (a manual
	// drop, or an older image). Seeding must not clobber it.
	old := filepath.Join(dl, "agent", "20260915.000000")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "linux-amd64"), []byte("volume-build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "linux-amd64.sha256"), []byte("volume-sha"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := Seed(seed, dl, "20260915.000000", testLog()); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	body, _ := os.ReadFile(filepath.Join(old, "linux-amd64"))
	if string(body) != "volume-build" {
		t.Fatalf("existing volume artifact was overwritten: %q", body)
	}
	// A version the volume does not have is still added.
	writeSeed(t, seed, "20260916.000000", "next-build")
	if err := Seed(seed, dl, "20260916.000000", testLog()); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(dl, "agent", "20260916.000000", "linux-amd64")); string(body) != "next-build" {
		t.Fatalf("new seed version not copied: %q", body)
	}
	if got, _ := os.Readlink(filepath.Join(dl, "agent", "latest", "linux-amd64")); got != filepath.Join("..", "20260916.000000", "linux-amd64") {
		t.Fatalf("latest was not repointed at the newer build: %q", got)
	}
}

// A real directory named latest belongs to local tooling (scripts/build.sh
// stages one); deleting it to create a symlink would destroy work.
func TestSeedLeavesRealLatestDirectoryAlone(t *testing.T) {
	seed, dl := t.TempDir(), t.TempDir()
	writeSeed(t, seed, "20260915.000000", "agent-binary")
	realLatest := filepath.Join(dl, "agent", "latest")
	if err := os.MkdirAll(realLatest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realLatest, "linux-amd64"), []byte("staged"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Seed(seed, dl, "20260915.000000", testLog()); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(realLatest, "linux-amd64"))
	if string(body) != "staged" {
		t.Fatalf("real latest directory was modified: %q", body)
	}
}

func TestSeedWithoutSeedDirIsANoOp(t *testing.T) {
	dl := t.TempDir()
	if err := Seed(filepath.Join(t.TempDir(), "missing"), dl, "20260915.000000", testLog()); err != nil {
		t.Fatalf("a missing seed directory must not fail startup: %v", err)
	}
	if err := Seed("", dl, "20260915.000000", testLog()); err != nil {
		t.Fatalf("an unconfigured seed dir must not fail startup: %v", err)
	}
	if err := Seed(t.TempDir(), "", "20260915.000000", testLog()); err != nil {
		t.Fatalf("an unconfigured artifact dir must not fail startup: %v", err)
	}
}

// Without the version's artifact on the volume there is nothing to point at, so
// "latest" must not be created at all: a dangling symlink is worse than 404.
func TestSeedSkipsLatestWhenArtifactMissing(t *testing.T) {
	seed, dl := t.TempDir(), t.TempDir()
	writeSeed(t, seed, "1.0.0", "some-build")
	if err := Seed(seed, dl, "2.0.0", testLog()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dl, "agent", "latest")); !os.IsNotExist(err) {
		t.Fatalf("latest must not exist when the server version has no artifact (err=%v)", err)
	}
}

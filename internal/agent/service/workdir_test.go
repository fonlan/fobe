package service

import (
	"path/filepath"
	"testing"
)

func TestSetWorkDirMovesAndRestoresLayout(t *testing.T) {
	dir := t.TempDir()
	SetWorkDir(dir)
	t.Cleanup(func() { SetWorkDir(SingboxWorkDir) })

	if got := EffectiveWorkDir(); got != dir {
		t.Fatalf("EffectiveWorkDir() = %q, want %q", got, dir)
	}
	bin, config, certDir := SingboxPaths()
	if bin != filepath.Join(dir, "sing-box") ||
		config != filepath.Join(dir, "config.json") ||
		certDir != filepath.Join(dir, "cert") {
		t.Fatalf("SingboxPaths() = (%q, %q, %q), want paths below %q", bin, config, certDir, dir)
	}
}

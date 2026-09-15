package collect

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeTailLogsLines(t *testing.T) {
	tests := []struct {
		name  string
		lines int
		want  int
	}{
		{name: "zero becomes default", lines: 0, want: DefaultTailLogsLines},
		{name: "negative becomes default", lines: -5, want: DefaultTailLogsLines},
		{name: "in range passes through", lines: 42, want: 42},
		{name: "cap passes through", lines: MaxTailLogsLines, want: MaxTailLogsLines},
		{name: "above cap is capped", lines: 100000, want: MaxTailLogsLines},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := NormalizeTailLogsLines(test.lines); got != test.want {
				t.Fatalf("NormalizeTailLogsLines(%d) = %d, want %d", test.lines, got, test.want)
			}
		})
	}
}

func TestSingboxLogCommands(t *testing.T) {
	primary, fallback := singboxLogCommands("systemd", 100)
	if len(primary) != 1 || primary[0] != "journalctl -u fobe-singbox --no-pager -n 100" {
		t.Fatalf("systemd primary = %v", primary)
	}
	if len(fallback) != 2 {
		t.Fatalf("systemd fallback = %v", fallback)
	}

	primary, fallback = singboxLogCommands("procd", 100)
	if len(primary) != 1 || primary[0] != "logread | tail -n 100" {
		t.Fatalf("procd primary = %v", primary)
	}
	if len(fallback) != 2 {
		t.Fatalf("procd fallback = %v", fallback)
	}

	primary, fallback = singboxLogCommands("fallback", 100)
	if len(primary) != 0 {
		t.Fatalf("fallback primary = %v, want none", primary)
	}
	if len(fallback) != 2 ||
		!strings.Contains(fallback[0], "/tmp/fobe-singbox.log") ||
		!strings.Contains(fallback[1], "/var/log/sing-box.log") {
		t.Fatalf("fallback commands = %v", fallback)
	}
}

func TestTailSingboxLogsMergesFallbackFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("log pipelines run through /bin/sh")
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.log")
	existing := filepath.Join(dir, "exists.log")
	if err := os.WriteFile(existing, []byte("line-one\nline-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := tailLogFallbackFiles
	tailLogFallbackFiles = []string{missing, existing}
	defer func() { tailLogFallbackFiles = prev }()

	// fallback platform: no primary source, both files tried, missing one skipped
	got := TailSingboxLogs("fallback", 100)
	if !strings.Contains(got, "line-one") || !strings.Contains(got, "line-two") {
		t.Fatalf("merged output = %q", got)
	}
}

func TestTailSingboxLogsCapsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("log pipelines run through /bin/sh")
	}
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", tailLogsMaxBytes*2)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := tailLogFallbackFiles
	tailLogFallbackFiles = []string{big}
	defer func() { tailLogFallbackFiles = prev }()

	got := TailSingboxLogs("fallback", 10)
	if len(got) > tailLogsMaxBytes {
		t.Fatalf("output length = %d, want <= %d", len(got), tailLogsMaxBytes)
	}
}

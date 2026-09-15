// collect_logs.go gathers recent sing-box service logs for the AI tail_logs
// command (design §12.1): journalctl under systemd, logread under
// procd/OpenWrt, and a tail of the well-known fallback files elsewhere.
// Sources are best-effort — a missing binary or log file never fails the
// command, it just contributes nothing.
package collect

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

const (
	// DefaultTailLogsLines applies when the tail_logs payload omits lines.
	DefaultTailLogsLines = 100
	// MaxTailLogsLines caps the request regardless of the payload (§12.2).
	MaxTailLogsLines = 500
	// tailLogCmdTimeout bounds each log pipeline (journalctl can be slow).
	tailLogCmdTimeout = 5 * time.Second
	// tailLogsMaxBytes caps the merged output before it reaches the
	// commands table (the server caps its own context slice at 8KB).
	tailLogsMaxBytes = 16 << 10
)

// tailLogFallbackFiles are the best-effort log locations when no service
// manager source answers. A var so tests can point them at temp files.
var tailLogFallbackFiles = []string{"/tmp/fobe-singbox.log", "/var/log/sing-box.log"}

// NormalizeTailLogsLines clamps the requested line count: non-positive
// counts become the default and everything above the cap is capped
// (§12.2: 读行数上限 500).
func NormalizeTailLogsLines(lines int) int {
	if lines <= 0 {
		return DefaultTailLogsLines
	}
	if lines > MaxTailLogsLines {
		return MaxTailLogsLines
	}
	return lines
}

// singboxLogCommands returns the shell pipelines that may hold sing-box
// logs for one service-manager kind: the platform source first (empty when
// there is none), then the two file fallbacks.
func singboxLogCommands(kind string, lines int) (primary []string, fallback []string) {
	for _, file := range tailLogFallbackFiles {
		fallback = append(fallback, fmt.Sprintf("tail -n %d %s 2>/dev/null", lines, file))
	}
	switch kind {
	case "systemd":
		return []string{fmt.Sprintf("journalctl -u fobe-singbox --no-pager -n %d", lines)}, fallback
	case "procd":
		return []string{fmt.Sprintf("logread | tail -n %d", lines)}, fallback
	default:
		return nil, fallback
	}
}

// TailSingboxLogs returns up to lines recent sing-box log lines for the
// platform's service manager. The platform source wins; when it answers
// nothing the file fallbacks are both tried and merged. Missing sources
// are skipped, so the result is possibly empty but never an error
// (§12.1: exit 0 即使部分来源缺失).
func TailSingboxLogs(kind string, lines int) string {
	lines = NormalizeTailLogsLines(lines)
	primary, fallback := singboxLogCommands(kind, lines)

	var merged bytes.Buffer
	for _, command := range primary {
		if out, ok := runLogCommand(command); ok {
			merged.WriteString(out)
		}
	}
	if merged.Len() == 0 {
		for _, command := range fallback {
			if out, ok := runLogCommand(command); ok {
				merged.WriteString(out)
			}
		}
	}

	out := merged.String()
	if len(out) > tailLogsMaxBytes {
		out = out[len(out)-tailLogsMaxBytes:] // logs are chronological: keep the newest
	}
	return out
}

// runLogCommand runs one log pipeline and returns its stdout. Failures
// (missing binary, missing file, timeout) yield ok=false — sources are
// optional by design.
func runLogCommand(command string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), tailLogCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil && out.Len() == 0 {
		return "", false
	}
	if out.Len() == 0 {
		return "", false
	}
	return out.String(), true
}

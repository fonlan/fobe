//go:build linux

package agent

import (
	"os"
	"strconv"
	"strings"
)

// findProcByExe scans /proc for a process whose executable (or argv[0]) is
// bin and returns its pid, 0 when absent. Works under systemd, procd and the
// fallback alike; only reached on linux.
func findProcByExe(bin string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if link, err := os.Readlink("/proc/" + e.Name() + "/exe"); err == nil {
			if link == bin {
				return pid
			}
			continue // exe resolved: no need for the cmdline fallback
		}
		if raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil {
			argv0, _, _ := strings.Cut(string(raw), "\x00")
			if argv0 == bin {
				return pid
			}
		}
	}
	return 0
}

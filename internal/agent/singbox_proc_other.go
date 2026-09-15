//go:build !linux

package agent

// findProcByExe has no /proc to scan off-linux, so process discovery is
// unavailable; convergence gates treat sing-box as not running there. The
// agent targets linux (design §5.1) — this only keeps the build portable.
func findProcByExe(string) int { return 0 }

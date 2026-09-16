//go:build !windows

package agent

import "golang.org/x/sys/unix"

// execSelfDefault replaces the process image in place. The two privileged
// touchpoints of §5.5 live behind these shims so the rest of the file stays
// platform-neutral: syscall.Exec has no Windows definition, and raw
// syscall.W_OK is not uniform across unix flavors — x/sys/unix is.
func execSelfDefault(self string, argv, env []string) error {
	return unix.Exec(self, argv, env)
}

// dirWritable answers "may the current user rename within dir". rename-onto
// needs write permission on the directory, not on the file — a root-owned
// 0755 binary inside a user-owned directory is replaceable, which is exactly
// what the unprivileged install (design §5.3 实现修订 2026-09-16) relies on.
func dirWritable(dir string) bool {
	return unix.Access(dir, unix.W_OK) == nil
}

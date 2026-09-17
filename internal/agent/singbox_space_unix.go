//go:build linux || darwin

package agent

import "syscall"

// statfsFree reports free bytes and the inode budget/free counts on the
// filesystem holding path (§5.4 overlay space gate). totalInodes is reported
// separately because some filesystems have none: btrfs allocates inodes on
// demand and statfs on it returns f_files = f_ffree = 0 — "untracked", not
// "full" (see checkInstallSpaceFor for why that must not refuse the install).
// The stdlib syscall.Statfs_t names the total-inode field `Files` on both
// Linux and Darwin (only golang.org/x/sys/unix calls it Ffiles), so one file
// serves both; other platforms take the fail-open stub in
// singbox_space_other.go (fobe agents only ever target linux, darwin exists
// so `go test` runs on a Mac).
func statfsFree(path string) (freeBytes, freeInodes, totalInodes uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Ffree), uint64(st.Files), nil
}

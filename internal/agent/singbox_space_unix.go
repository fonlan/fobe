//go:build unix

package agent

import "syscall"

// statfsFree reports the free bytes and inodes on the filesystem holding
// path (§5.4 overlay space gate). Unix only — sing-box targets linux.
func statfsFree(path string) (freeBytes, freeInodes uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Ffree), nil
}

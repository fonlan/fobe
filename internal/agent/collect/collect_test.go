package collect

import (
	"strings"
	"testing"
)

// /proc/mounts lines are "<source> <mountpoint> <fstype> <options> ...". A
// container/OpenWrt overlay root has source "overlay" *and* fstype "overlay",
// so mixing up the two fields made readDisks statfs the fstype as a relative
// path — an empty disk list (and a permanent 0% in the panel) everywhere.
const containerMounts = `overlay / overlay rw,relatime,lowerdir=/l,upperdir=/u,workdir=/w 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /dev tmpfs rw,nosuid,size=65536k,mode=755 0 0
devpts /dev/pts devpts rw,nosuid,noexec,relatime,gid=5,mode=620,ptmxmode=666 0 0
shm /dev/shm tmpfs rw,nosuid,nodev,noexec,relatime,size=8212480k 0 0
/dev/vdb1 /etc/resolv.conf btrfs rw,noatime 0 0
/dev/vdb1 /etc/hosts btrfs rw,noatime 0 0`

// OpenWrt: the read-only squashfs image is the lower half of the overlay root,
// so only the overlay entry carries the writable partition's capacity.
const openwrtMounts = `/dev/root / squashfs ro,relatime 0 0
overlayfs:/overlay / overlay rw,noatime,lowerdir=/,upperdir=/overlay/upper 0 0
proc /proc proc rw,relatime 0 0
tmpfs /tmp tmpfs rw,relatime 0 0
/dev/sda1 /mnt/data ext4 rw,relatime 0 0`

func TestParseMountsReadsMountpointAndFstype(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"container", containerMounts, []string{"/", "/etc/resolv.conf", "/etc/hosts"}},
		{"openwrt", openwrtMounts, []string{"/", "/mnt/data"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMounts(strings.NewReader(tc.in))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("parseMounts = %v, want %v", got, tc.want)
			}
		})
	}
}

// A container rootfs and an OpenWrt root ("overlayfs:/overlay /") are overlay
// mounts; squashfs is the read-only lower half of that same root.
func TestSkipFSTypeKeepsOverlayRoot(t *testing.T) {
	for _, fstype := range []string{"overlay", "ext4", "btrfs", "xfs", "f2fs", "vfat", "zfs"} {
		if skipFSType(fstype) {
			t.Errorf("skipFSType(%q) = true, want false", fstype)
		}
	}
	for _, fstype := range []string{"squashfs", "tmpfs", "proc", "sysfs", "devpts", "devtmpfs", "cgroup2"} {
		if !skipFSType(fstype) {
			t.Errorf("skipFSType(%q) = false, want true", fstype)
		}
	}
}

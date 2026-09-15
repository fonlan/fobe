package singboxcache

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Paths probed to decide whether the process runs inside a container, plus the
// mount table. They are variables so tests can point them at fixtures.
var (
	dockerEnvPath    = "/.dockerenv"
	containerEnvPath = "/run/.containerenv"
	cgroupPath       = "/proc/self/cgroup"
	mountinfoPath    = "/proc/self/mountinfo"
)

// containerCgroupMarkers are the runtime names that show up in cgroup paths.
var containerCgroupMarkers = []string{
	"docker", "containerd", "kubepods", "libpod", "podman", "lxc", "garden", "systemd-nspawn",
}

// MountCheck answers "does the artifact directory survive a container upgrade?".
type MountCheck struct {
	// Container reports whether the process runs inside a container.
	Container bool
	// DLDir is the absolute artifact directory that was checked.
	DLDir string
	// MountPoint is the mount whose root covers DLDir. "/" is the container's
	// writable layer: a download that lands there is gone after an image
	// upgrade.
	MountPoint string
	// Mounted reports whether DLDir is served by a mount other than "/".
	Mounted bool
}

// OK reports whether the check should stay quiet. Outside a container there is
// nothing to warn about.
func (c MountCheck) OK() bool { return !c.Container || c.Mounted }

// InContainer reports whether the process runs inside a container: Docker and
// Podman leave a marker file, and the cgroup path names the runtime.
func InContainer() bool {
	if _, err := os.Stat(dockerEnvPath); err == nil {
		return true
	}
	if _, err := os.Stat(containerEnvPath); err == nil {
		return true
	}
	if raw, err := os.ReadFile(cgroupPath); err == nil && cgroupNamesContainer(string(raw)) {
		return true
	}
	return false
}

func cgroupNamesContainer(s string) bool {
	for _, m := range containerCgroupMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// CheckMount inspects /proc/self/mountinfo for DLDir.
//
// Inside a container the directory must live on a mount of its own — a bind
// mount or a volume (Docker's VOLUME declaration included). A path that
// resolves to the container's writable layer is lost on the next image upgrade,
// which is the failure this check exists to warn about.
//
// Outside a container, with an empty directory or without a mount table
// (non-Linux), the check is skipped and Mounted stays true, so local
// development stays quiet.
func CheckMount(dlDir string) MountCheck {
	mc := MountCheck{Container: InContainer(), Mounted: true}
	if strings.TrimSpace(dlDir) == "" {
		return mc // the server has no artifact directory: nothing to mount
	}
	abs, err := filepath.Abs(dlDir)
	if err != nil {
		return mc
	}
	abs = filepath.Clean(abs)
	mc.DLDir = abs
	raw, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return mc // no mount table to judge from
	}
	mp, ok := coveringMount(string(raw), abs)
	if !ok {
		return mc
	}
	mc.MountPoint = mp
	mc.Mounted = mp != "/"
	return mc
}

// coveringMount returns the mount point covering dir: the longest mount point
// that is dir itself or a parent of it. Return ok is false only for an empty
// mount table.
func coveringMount(mountinfo, dir string) (string, bool) {
	best := ""
	for _, line := range strings.Split(mountinfo, "\n") {
		// mountinfo(5): id parent major:minor root mountpoint options …
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountField(fields[4])
		if mp == "" || !pathWithin(dir, mp) {
			continue
		}
		if len(mp) > len(best) {
			best = mp
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// pathWithin reports whether dir is mp itself or lives below it.
func pathWithin(dir, mp string) bool {
	if dir == mp {
		return true
	}
	if mp == "/" {
		return strings.HasPrefix(dir, "/")
	}
	return strings.HasPrefix(dir, strings.TrimSuffix(mp, "/")+"/")
}

// unescapeMountField undoes the octal escapes mountinfo uses for whitespace
// (mountinfo(5): \040 space, \011 tab, \012 newline, \134 backslash).
func unescapeMountField(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

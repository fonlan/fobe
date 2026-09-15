package singboxcache

import (
	"os"
	"path/filepath"
	"testing"
)

// mountinfoRootOnly is the minimum a container mount table always has: the
// writable layer mounted at "/" (mountinfo(5) layout).
const mountinfoRootOnly = "21 0 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n"

// stubMountEnv points the container/mount probes at fixtures for one test.
func stubMountEnv(t *testing.T, container bool, mountinfo string) {
	t.Helper()
	dir := t.TempDir()
	oldDocker, oldContainer, oldCgroup, oldMount := dockerEnvPath, containerEnvPath, cgroupPath, mountinfoPath
	t.Cleanup(func() {
		dockerEnvPath, containerEnvPath, cgroupPath, mountinfoPath = oldDocker, oldContainer, oldCgroup, oldMount
	})

	dockerEnvPath = filepath.Join(dir, "dockerenv")
	containerEnvPath = filepath.Join(dir, "containerenv")
	cgroupPath = filepath.Join(dir, "cgroup")
	if container {
		if err := os.WriteFile(dockerEnvPath, nil, 0o644); err != nil {
			t.Fatalf("write dockerenv: %v", err)
		}
	}
	mountinfoPath = filepath.Join(dir, "mountinfo")
	if mountinfo != "" {
		if err := os.WriteFile(mountinfoPath, []byte(mountinfo), 0o644); err != nil {
			t.Fatalf("write mountinfo: %v", err)
		}
	}
}

// stubContainerPaths gives a test its own marker/cgroup files and returns the
// directory they live in.
func stubContainerPaths(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldDocker, oldContainer, oldCgroup := dockerEnvPath, containerEnvPath, cgroupPath
	t.Cleanup(func() { dockerEnvPath, containerEnvPath, cgroupPath = oldDocker, oldContainer, oldCgroup })
	dockerEnvPath = filepath.Join(dir, "dockerenv")
	containerEnvPath = filepath.Join(dir, "containerenv")
	cgroupPath = filepath.Join(dir, "cgroup")
	return dir
}

func TestInContainerMarkers(t *testing.T) {
	t.Run("bare host", func(t *testing.T) {
		stubContainerPaths(t)
		if InContainer() {
			t.Fatalf("no marker files must mean: not a container")
		}
	})
	t.Run("dockerenv", func(t *testing.T) {
		dir := stubContainerPaths(t)
		if err := os.WriteFile(filepath.Join(dir, "dockerenv"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if !InContainer() {
			t.Fatalf("/.dockerenv must mean: container")
		}
	})
	t.Run("containerenv", func(t *testing.T) {
		dir := stubContainerPaths(t)
		if err := os.WriteFile(filepath.Join(dir, "containerenv"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if !InContainer() {
			t.Fatalf("podman's /run/.containerenv must mean: container")
		}
	})
	t.Run("docker cgroup", func(t *testing.T) {
		dir := stubContainerPaths(t)
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::/docker/8f3a1b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !InContainer() {
			t.Fatalf("a docker cgroup path must mean: container")
		}
	})
	t.Run("plain cgroup", func(t *testing.T) {
		dir := stubContainerPaths(t)
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::/user.slice/session-3.scope\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if InContainer() {
			t.Fatalf("a plain session cgroup must mean: not a container")
		}
	})
}

func TestCheckMountTable(t *testing.T) {
	root := mountinfoRootOnly
	withVolume := root + "30 21 8:2 /var/lib/docker/volumes/abc/_data /srv/dl rw,relatime - ext4 /dev/sda2 rw\n"
	withParentMount := root + "31 21 8:3 / /data rw,relatime - ext4 /dev/sda3 rw\n"
	withEscaped := root + "32 21 8:4 / /mnt/my\\040dl rw,relatime - ext4 /dev/sda4 rw\n"

	cases := []struct {
		name       string
		mountinfo  string
		dlDir      string
		mounted    bool
		mountPoint string
	}{
		{"container layer", root, "/srv/dl", false, "/"},
		{"own volume", withVolume, "/srv/dl", true, "/srv/dl"},
		{"mounted parent", withParentMount, "/data/dl", true, "/data"},
		{"escaped space", withEscaped, "/mnt/my dl", true, "/mnt/my dl"},
		{"trailing slash", withVolume, "/srv/dl/", true, "/srv/dl"},
		{"sibling directory", withVolume, "/srv/other", false, "/"},
		{"deepest match wins", withParentMount + "33 21 8:5 / /data/dl rw,relatime - ext4 /dev/sda5 rw\n", "/data/dl", true, "/data/dl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubMountEnv(t, true, tc.mountinfo)
			mc := CheckMount(tc.dlDir)
			if !mc.Container {
				t.Fatalf("container = false, want true")
			}
			if mc.Mounted != tc.mounted || mc.MountPoint != tc.mountPoint {
				t.Fatalf("CheckMount(%q) = %+v, want mounted=%v mountPoint=%q", tc.dlDir, mc, tc.mounted, tc.mountPoint)
			}
			if mc.OK() != tc.mounted {
				t.Fatalf("OK() = %v, want %v", mc.OK(), tc.mounted)
			}
			if mc.DLDir == "" {
				t.Fatalf("DLDir not recorded: %+v", mc)
			}
		})
	}
}

func TestCheckMountSkipsQuietly(t *testing.T) {
	t.Run("no mount table", func(t *testing.T) {
		stubMountEnv(t, true, "")
		mc := CheckMount("/srv/dl")
		if !mc.Mounted || !mc.OK() {
			t.Fatalf("without /proc/self/mountinfo the check must stay quiet: %+v", mc)
		}
	})
	t.Run("outside a container", func(t *testing.T) {
		stubMountEnv(t, false, mountinfoRootOnly)
		mc := CheckMount("/srv/dl")
		if mc.Container {
			t.Fatalf("container = true on a bare host")
		}
		if !mc.OK() {
			t.Fatalf("a bare host must stay quiet: %+v", mc)
		}
	})
	t.Run("no artifact directory", func(t *testing.T) {
		stubMountEnv(t, true, mountinfoRootOnly)
		mc := CheckMount("")
		if !mc.Mounted || !mc.OK() {
			t.Fatalf("an unset FOBE_DL_DIR must stay quiet: %+v", mc)
		}
	})
}

func TestUnescapeMountField(t *testing.T) {
	cases := map[string]string{
		"/plain":         "/plain",
		`/with\040space`: "/with space",
		`/with\011tab`:   "/with	tab",
		`/bad\04`:        `/bad\04`,
		`/literal\x`:     `/literal\x`,
	}
	for in, want := range cases {
		if got := unescapeMountField(in); got != want {
			t.Fatalf("unescapeMountField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathWithin(t *testing.T) {
	cases := []struct {
		dir, mp string
		want    bool
	}{
		{"/srv/dl", "/srv/dl", true},
		{"/srv/dl", "/srv", true},
		{"/srv/dl", "/", true},
		{"/srvdl", "/srv", false},
		{"/srv", "/srv/dl", false},
	}
	for _, tc := range cases {
		if got := pathWithin(tc.dir, tc.mp); got != tc.want {
			t.Fatalf("pathWithin(%q, %q) = %v, want %v", tc.dir, tc.mp, got, tc.want)
		}
	}
}

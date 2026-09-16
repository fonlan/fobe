package collect

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Distro reports the running Linux distribution (design §8.1): ID is the
// lower-case distro identifier ("debian", "ubuntu", "openwrt") and version is
// its release ("13", "24.04"). Either may come back empty — rolling releases
// like Arch ship no VERSION_ID, and unusual systems may have no release files
// at all. Only file reads are used, per the no-external-commands invariant
// that keeps busybox OpenWrt working.
func Distro() (id, version string) {
	return distroAt("/")
}

// distroAt is Distro against a fake filesystem root (the service.Detect /
// detectAt precedent), so the OpenWrt-family fallbacks are unit-testable.
//
// Two sources beyond a plain os-release ID:
//   - ID_LIKE: fork distros (iStoreOS, ImmortalWrt, ...) rewrite ID but keep
//     "openwrt" in ID_LIKE; the panel expects the family, so they normalize
//     to "openwrt" instead of falling through to the unknown-letter badge.
//   - /etc/openwrt_release (DISTRIB_ID/DISTRIB_RELEASE): shipped by every
//     OpenWrt-family build and the only source left when a vendor stripped
//     os-release; also fills in the version for snapshot-style builds whose
//     os-release carries no VERSION_ID.
func distroAt(root string) (id, version string) {
	id, version, idLike := readOSReleaseAt(root)
	if hasToken(idLike, "openwrt") {
		id = "openwrt"
	}
	if id == "" || id == "openwrt" {
		if oid, over := openwrtReleaseAt(root); oid != "" {
			if id == "" {
				id = oid
			}
			if version == "" {
				version = over
			}
		}
	}
	return id, version
}

func readOSReleaseAt(root string) (id, version, idLike string) {
	// /usr/lib/os-release is the spec's fallback location; systemd distros
	// normally only have /etc/os-release (itself a symlink there), OpenWrt
	// ships both as plain files.
	for _, path := range []string{"etc/os-release", "usr/lib/os-release"} {
		f, err := os.Open(filepath.Join(root, path))
		if err != nil {
			continue
		}
		id, version, idLike = parseOSRelease(f)
		_ = f.Close()
		if id != "" || idLike != "" {
			return id, version, idLike
		}
	}
	return "", "", ""
}

// parseOSRelease extracts ID, VERSION_ID and ID_LIKE from os-release content.
// The format is KEY=value lines with optionally single/double-quoted values
// (freedesktop.org os-release spec); everything else (PRETTY_NAME, codenames
// inside VERSION) is deliberately ignored so the panel renders one stable
// "Debian 13"-style label instead of marketing strings.
func parseOSRelease(r io.Reader) (id, version, idLike string) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = trimReleaseQuotes(strings.TrimSpace(value))
		switch key {
		case "ID":
			id = strings.ToLower(value)
		case "VERSION_ID":
			version = value
		case "ID_LIKE":
			idLike = strings.ToLower(value)
		}
	}
	return id, version, idLike
}

// openwrtReleaseAt reads /etc/openwrt_release (DISTRIB_ID='OpenWrt',
// DISTRIB_RELEASE='24.10.1'; single quotes, always present on the family).
func openwrtReleaseAt(root string) (id, version string) {
	f, err := os.Open(filepath.Join(root, "etc/openwrt_release"))
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = trimReleaseQuotes(strings.TrimSpace(value))
		switch key {
		case "DISTRIB_ID":
			id = strings.ToLower(value)
		case "DISTRIB_RELEASE":
			version = value
		}
	}
	return id, version
}

// hasToken matches a space-separated ID_LIKE member exactly ("lede" must not
// match a hypothetical "ledesmith").
func hasToken(list, want string) bool {
	for _, tok := range strings.Fields(list) {
		if tok == want {
			return true
		}
	}
	return false
}

func trimReleaseQuotes(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

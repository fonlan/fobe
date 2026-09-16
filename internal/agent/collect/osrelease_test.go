package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		name, in, wantID, wantVersion, wantIDLike string
	}{
		{
			name:        "debian 13",
			in:          "PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"13\"\nVERSION=\"13 (trixie)\"\nID=debian\nHOME_URL=\"https://www.debian.org/\"\n",
			wantID:      "debian",
			wantVersion: "13",
		},
		{
			name:        "ubuntu 24.04",
			in:          "NAME=\"Ubuntu\"\nVERSION=\"24.04.1 LTS (Noble Numbat)\"\nID=ubuntu\nID_LIKE=debian\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nVERSION_ID=\"24.04\"\n",
			wantID:      "ubuntu",
			wantVersion: "24.04",
			wantIDLike:  "debian",
		},
		{
			// built from the OpenWrt template: ID and VERSION_ID are quoted
			name:        "openwrt release",
			in:          "NAME=\"OpenWrt\"\nVERSION=\"24.10.1\"\nID=\"openwrt\"\nID_LIKE=\"lede openwrt\"\nPRETTY_NAME=\"OpenWrt 24.10.1\"\nVERSION_ID=\"24.10.1\"\nBUILD_ID=\"r28427-6df0135b79\"\nOPENWRT_BOARD=\"bcm27xx/bcm2712\"\n",
			wantID:      "openwrt",
			wantVersion: "24.10.1",
			wantIDLike:  "lede openwrt",
		},
		{
			name:        "openwrt single quotes",
			in:          "NAME=\"OpenWrt\"\nID=openwrt\nID_LIKE=lede\nVERSION=\"24.10.0\"\nVERSION_ID='24.10.0'\n",
			wantID:      "openwrt",
			wantVersion: "24.10.0",
			wantIDLike:  "lede",
		},
		{
			name:        "arch rolling has no VERSION_ID",
			in:          "NAME=\"Arch Linux\"\nPRETTY_NAME=\"Arch Linux\"\nID=arch\nBUILD_ID=rolling\nANSI_COLOR=\"38;2;23;147;209\"\n",
			wantID:      "arch",
			wantVersion: "",
		},
		{
			name:        "comments and blank lines ignored",
			in:          "# some comment\n\nID=alpine\nVERSION_ID=3.20\n",
			wantID:      "alpine",
			wantVersion: "3.20",
		},
		{
			name:        "malformed lines skipped",
			in:          "NOVALUE_HERE\nID=centos\nVERSION_ID=\"9\"\n",
			wantID:      "centos",
			wantVersion: "9",
		},
		{
			name:        "empty input",
			in:          "",
			wantID:      "",
			wantVersion: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, version, idLike := parseOSRelease(strings.NewReader(tc.in))
			if id != tc.wantID || version != tc.wantVersion || idLike != tc.wantIDLike {
				t.Fatalf("parseOSRelease = (%q, %q, %q), want (%q, %q, %q)", id, version, idLike, tc.wantID, tc.wantVersion, tc.wantIDLike)
			}
		})
	}
}

// writeTree lays out a fake filesystem root with the given release files.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDistroAt(t *testing.T) {
	cases := []struct {
		name            string
		files           map[string]string
		wantID, wantVer string
	}{
		{
			name: "official openwrt os-release",
			files: map[string]string{
				"etc/os-release": "NAME=\"OpenWrt\"\nVERSION=\"24.10.1\"\nID=\"openwrt\"\nID_LIKE=\"lede openwrt\"\nPRETTY_NAME=\"OpenWrt 24.10.1\"\nVERSION_ID=\"24.10.1\"\n",
			},
			wantID: "openwrt", wantVer: "24.10.1",
		},
		{
			// iStoreOS / ImmortalWrt rewrite ID but keep the family in ID_LIKE
			name: "fork distro normalizes into openwrt family",
			files: map[string]string{
				"etc/os-release":      "NAME=\"iStoreOS\"\nID=\"istoreos\"\nID_LIKE=\"lede openwrt\"\nVERSION_ID=\"24.10\"\n",
				"etc/openwrt_release": "DISTRIB_ID='iStoreOS'\nDISTRIB_RELEASE='24.10'\n",
			},
			wantID: "openwrt", wantVer: "24.10",
		},
		{
			// vendor builds sometimes strip os-release; openwrt_release remains
			name: "openwrt_release fallback when os-release missing",
			files: map[string]string{
				"etc/openwrt_release": "DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='23.05.5'\nDISTRIB_TARGET='ramips/mt7621'\n",
			},
			wantID: "openwrt", wantVer: "23.05.5",
		},
		{
			// snapshot builds carry no VERSION_ID; the version comes from
			// openwrt_release instead
			name: "snapshot version recovered from openwrt_release",
			files: map[string]string{
				"etc/os-release":      "NAME=\"OpenWrt\"\nVERSION=\"SNAPSHOT\"\nID=\"openwrt\"\nID_LIKE=\"lede openwrt\"\n",
				"etc/openwrt_release": "DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='24.10-SNAPSHOT'\n",
			},
			wantID: "openwrt", wantVer: "24.10-SNAPSHOT",
		},
		{
			name: "debian untouched by the openwrt fallbacks",
			files: map[string]string{
				"etc/os-release": "PRETTY_NAME=\"Debian GNU/Linux 13\"\nID=debian\nVERSION_ID=\"13\"\n",
			},
			wantID: "debian", wantVer: "13",
		},
		{
			// /usr/lib/os-release is the spec fallback location
			name: "os-release via usr/lib",
			files: map[string]string{
				"usr/lib/os-release": "ID=alpine\nVERSION_ID=3.20\n",
			},
			wantID: "alpine", wantVer: "3.20",
		},
		{
			name:   "no release files at all",
			files:  map[string]string{},
			wantID: "", wantVer: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			id, version := distroAt(root)
			if id != tc.wantID || version != tc.wantVer {
				t.Fatalf("distroAt = (%q, %q), want (%q, %q)", id, version, tc.wantID, tc.wantVer)
			}
		})
	}
}

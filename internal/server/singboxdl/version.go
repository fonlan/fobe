// Package singboxdl fetches and caches sing-box release artifacts on the
// server (design §9.2, §17).
//
// The panel never downloads from the network while rendering a page: it reads
// the cache under <DLDir>/singbox/<version>/ and, when it must fetch, it does
// so through this package with fail-closed integrity checking. The layout
// matches what the agent already expects from /dl:
//
//	<DLDir>/singbox/<version>/linux-amd64          (0755, static musl binary)
//	<DLDir>/singbox/<version>/linux-amd64.sha256   ("<hex>  linux-amd64")
//	<DLDir>/singbox/<version>/manifest.json        (version/sha256/size/…)
//
// Only the official "-linux-amd64-musl" single-file build is cached: a static
// binary that works on glibc and musl hosts alike and needs no side-car
// libraries (the standard tar.gz ships libcronet.so and does not match the
// agent's single-file install path).
package singboxdl

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// versionRE accepts a semver-ish tag with an optional leading "v", an
// optional minor/patch, an optional prerelease and build metadata.
// sing-box tags look like "v1.10.0" and "v1.11.0-beta.5".
var versionRE = regexp.MustCompile("^[vV]?([0-9]+)(?:[.]([0-9]+))?(?:[.]([0-9]+))?(?:-([0-9A-Za-z.-]+))?(?:[+]([0-9A-Za-z.-]+))?$")

// Version is a parsed semver version.
type Version struct {
	Major int
	Minor int
	Patch int
	// Pre is the prerelease identifier ("beta.5"), empty for stable releases.
	Pre string
	// Build is build metadata; it is ignored when comparing versions.
	Build string
}

// ParseVersion parses a release tag or version string. A leading "v" is
// accepted and dropped; "1.10" is treated as "1.10.0".
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("%w: %q", ErrInvalidVersion, s)
	}
	v := Version{Pre: m[4], Build: m[5]}
	atoi := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	v.Major = atoi(m[1])
	v.Minor = atoi(m[2])
	v.Patch = atoi(m[3])
	return v, nil
}

// String renders the canonical version without the leading "v" and without
// build metadata.
func (v Version) String() string {
	out := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		out += "-" + v.Pre
	}
	return out
}

// IsStable reports whether the version has no prerelease identifier.
func (v Version) IsStable() bool { return v.Pre == "" }

// Compare orders two versions per semver precedence: numeric fields first,
// then prerelease (a version without a prerelease sorts higher than one
// with), ignoring build metadata.
func (v Version) Compare(o Version) int {
	if c := cmpInt(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmpInt(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmpInt(v.Patch, o.Patch); c != 0 {
		return c
	}
	return comparePre(v.Pre, o.Pre)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePre implements the semver prerelease precedence rules.
func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1 // stable > prerelease
	}
	if b == "" {
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := comparePreIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(as), len(bs))
}

func comparePreIdent(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmpInt(an, bn)
	case aerr == nil:
		return -1 // numeric identifiers sort below alphanumeric ones
	case berr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// CompareVersions parses and compares two version strings. Invalid input
// compares as lower than any valid version.
func CompareVersions(a, b string) int {
	av, aerr := ParseVersion(a)
	bv, berr := ParseVersion(b)
	switch {
	case aerr != nil && berr != nil:
		return strings.Compare(a, b)
	case aerr != nil:
		return -1
	case berr != nil:
		return 1
	default:
		return av.Compare(bv)
	}
}

// IsStableVersion reports whether the version string parses and has no
// prerelease identifier.
func IsStableVersion(s string) bool {
	v, err := ParseVersion(s)
	return err == nil && v.IsStable()
}

// SortVersions sorts version strings in descending semver order (newest
// first); unparseable values sort last in lexical order.
func SortVersions(versions []string) {
	sort.SliceStable(versions, func(i, j int) bool {
		ai, aerr := ParseVersion(versions[i])
		bi, berr := ParseVersion(versions[j])
		switch {
		case aerr != nil && berr != nil:
			return versions[i] > versions[j]
		case aerr != nil:
			return false
		case berr != nil:
			return true
		default:
			if c := ai.Compare(bi); c != 0 {
				return c > 0
			}
			return versions[i] > versions[j]
		}
	})
}

package agentupdate

import (
	"regexp"
	"strings"
)

// versionCharset is the only shape allowed to become a path component. The build
// version is embedded in filesystem paths (<dl>/agent/<version>/linux-amd64) and
// in URL paths (/dl/agent/<version>/…, the §5.5 self-update target), and it
// arrives from a build argument (-X main.version=$VERSION) — a slug such as
// "1/../../etc" would escape the artifact volume entirely, and an empty one
// collapses the path onto its parent.
var versionCharset = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// SafeVersionPart reports whether version may be used as a single path segment
// and URL segment (§5.5 实现修订 2026-09-20). It is checked at startup and again
// at every path-building entry point.
func SafeVersionPart(version string) bool {
	if !versionCharset.MatchString(version) {
		return false
	}
	return !strings.Contains(version, "..")
}

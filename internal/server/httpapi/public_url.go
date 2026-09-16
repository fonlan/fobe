package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/fonlan/fobe/internal/server/store"
)

var errInvalidPublicURL = errors.New("invalid public url")

// publicBaseURL returns the operator-configured panel origin. When the
// setting is absent, it falls back to the current request origin so local
// development and existing installations keep working.
func (s *Server) publicBaseURL(r *http.Request) (string, error) {
	configured, err := s.Store.GetSetting("server.public_url")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if configured != "" {
		return normalizePublicURL(configured)
	}
	scheme, host := s.schemeHost(r)
	return normalizePublicURL(scheme + "://" + host)
}

// normalizePublicURL accepts only absolute HTTP(S) URLs with a host and no
// query/fragment. A path prefix is preserved for deployments mounted below a
// reverse-proxy path; trailing slashes are removed.
func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errInvalidPublicURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() == false || u.Host == "" {
		return "", errInvalidPublicURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errInvalidPublicURL
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errInvalidPublicURL
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// shellQuote makes a value one literal POSIX shell argument. It is used for
// URLs and tokens embedded in the one-line install command.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// shellDoubleQuote escapes content placed inside a double-quoted shell
// assignment in scripts/install.sh.tmpl.
func shellDoubleQuote(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch r {
		case '\\', '"', '$', '`':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

package geoip

import (
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang"
)

// MMDB resolves countries from a MaxMind GeoLite2-Country format file. The
// file may be absent or swapped at any time: lookups miss until a valid
// database is present, and the file is re-opened automatically when its
// mtime or size changes (§14: no restart needed after an upload).
type MMDB struct {
	path string

	mu      sync.Mutex
	reader  *geoip2.Reader
	modTime time.Time
	size    int64
}

// NewMMDB opens the database at path. A missing or corrupt file is not an
// error; the resolver answers misses until the file becomes usable.
func NewMMDB(path string) *MMDB {
	m := &MMDB{path: path}
	m.Reload()
	return m
}

// Reload re-opens the database now. Country also re-opens on its own when
// the file changes. It reports whether a database is loaded afterwards.
func (m *MMDB) Reload() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modTime, m.size = time.Time{}, 0 // force the next open attempt
	m.reload()
	return m.reader != nil
}

// Country implements Resolver.
func (m *MMDB) Country(ipStr string) (string, bool) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "", false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.changed() {
		m.reload()
	}
	if m.reader == nil {
		return "", false
	}
	rec, err := m.reader.Country(ip)
	if err != nil {
		return "", false
	}
	code := strings.ToUpper(strings.TrimSpace(rec.Country.IsoCode))
	if code == "" {
		return "", false // reserved / unassigned ranges carry no ISO code
	}
	return code, true
}

// changed reports whether the file on disk differs from the loaded database
// (caller holds mu; a vanished file counts as changed).
func (m *MMDB) changed() bool {
	st, err := os.Stat(m.path)
	if err != nil {
		return m.reader != nil || !m.modTime.IsZero()
	}
	return m.reader == nil || !st.ModTime().Equal(m.modTime) || st.Size() != m.size
}

// reload re-opens the database when the file changed (caller holds mu). A
// failed open keeps the previous database serving; the new stat is recorded
// anyway so a corrupt file isn't re-parsed on every lookup.
func (m *MMDB) reload() {
	st, err := os.Stat(m.path)
	if err != nil {
		if m.reader != nil {
			m.reader.Close()
			m.reader = nil
		}
		m.modTime, m.size = time.Time{}, 0
		return
	}
	if m.reader != nil && st.ModTime().Equal(m.modTime) && st.Size() == m.size {
		return
	}
	r, err := geoip2.Open(m.path)
	m.modTime, m.size = st.ModTime(), st.Size()
	if err != nil {
		return
	}
	if m.reader != nil {
		m.reader.Close()
	}
	m.reader = r
}

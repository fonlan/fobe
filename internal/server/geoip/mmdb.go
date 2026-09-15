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

// Loaded reports whether a database is currently serving lookups. Like
// Country it re-stats the file first, so a database that appeared on disk
// since the last lookup already counts.
func (m *MMDB) Loaded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.changed() {
		m.reload()
	}
	return m.reader != nil
}

// Metadata is the loaded database's self-description. It answers "how old is
// the data?" honestly, which the file's mtime only approximates: refreshing
// rewrites the file, but the data inside may be a week older than that.
type Metadata struct {
	BuildEpoch   int64  // when the provider built the data (unix seconds); 0 unknown
	DatabaseType string // e.g. "GeoLite2-Country"
	NodeCount    uint   // number of nodes in the search tree
}

// Metadata returns the loaded database's metadata; the zero value when no
// database is loaded.
func (m *MMDB) Metadata() Metadata {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.changed() {
		m.reload()
	}
	if m.reader == nil {
		return Metadata{}
	}
	md := m.reader.Metadata()
	return Metadata{
		BuildEpoch:   int64(md.BuildEpoch),
		DatabaseType: md.DatabaseType,
		NodeCount:    md.NodeCount,
	}
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

package geoipupdate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fobe-panel/fobe/internal/server/geoip"
)

// progressInterval throttles byte-level reports: a fast mirror would otherwise
// emit thousands of events, each one crossing a mutex and the panel's event
// broker. Phase changes bypass it (emit is called directly for those).
const progressInterval = 250 * time.Millisecond

// sources resolves the URLs to try, in order. One pinned URL replaces the
// mirror chain entirely: an operator who set it knows their network, and
// silently falling back to GitHub would defeat an internal mirror.
func (m *Manager) sources() []string {
	if u := strings.TrimSpace(m.cfg.SourceURL); u != "" {
		return []string{u}
	}
	if u, ok := m.setting(SettingSourceURL); ok {
		return []string{strings.TrimSpace(u)}
	}
	return m.cfg.Sources
}

// download tries every source in order and installs the first one that yields
// a valid database. A failed mirror costs one attempt, never the update: the
// next one is tried immediately.
func (m *Manager) download(ctx context.Context) Status {
	sources := m.sources()
	if len(sources) == 0 {
		return m.setStatus(Status{State: StateFailed, Error: "no download source configured"})
	}
	m.mu.Lock()
	m.lastBytesAt, m.lastBytes = time.Time{}, 0
	m.mu.Unlock()

	// Fail before spending bandwidth on a destination that cannot hold the file
	// — an unwritable FOBE_GEOIP_MMDB (the container default /data/geoip is
	// read-only when the binary runs on a host directly) would otherwise cost a
	// whole ~9MB download per mirror and then report the same local error three
	// times over.
	if f, err := geoip.CreateMMDBTemp(m.cfg.Path); err != nil {
		m.emit(Download{Active: false, Phase: PhaseFailed, Error: err.Error(), StartedAt: m.cfg.Now().Unix()})
		m.logWarn("geoipupdate: database path is not writable", "path", m.cfg.Path, "err", err)
		return m.setStatus(Status{State: StateFailed, Error: err.Error()})
	} else {
		name := f.Name()
		f.Close()
		os.Remove(name)
	}

	started := m.cfg.Now().Unix()
	var lastErr error
	for i, src := range sources {
		m.emit(Download{Active: true, Phase: PhaseConnecting, Source: src, StartedAt: started})
		st, err := m.fetch(ctx, src, started)
		if err == nil {
			return st
		}
		lastErr = err
		m.logWarn("geoipupdate: mirror failed", "source", src, "err", err, "mirrors_left", len(sources)-i-1)
	}
	m.emit(Download{Active: false, Phase: PhaseFailed, Source: sources[len(sources)-1], Error: lastErr.Error(), StartedAt: started})
	return m.setStatus(Status{State: StateFailed, Error: lastErr.Error()})
}

// fetch streams one mirror body into a temp file and, only after it parses as
// an MMDB, moves it onto the live path (§14: a bad download must never
// displace a working database).
func (m *Manager) fetch(ctx context.Context, src string, started int64) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("http %d from %s", resp.StatusCode, src)
	}
	total := resp.ContentLength

	f, err := geoip.CreateMMDBTemp(m.cfg.Path)
	if err != nil {
		return Status{}, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename below succeeded

	body := &progressReader{
		r:     io.LimitReader(resp.Body, m.cfg.MaxBytes+1),
		total: total,
		report: func(n, total int64) {
			m.emit(Download{
				Active: true, Phase: PhaseDownloading, Source: src,
				Downloaded: n, Total: total, StartedAt: started,
			})
		},
	}
	n, err := io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Status{}, fmt.Errorf("download %s: %w", src, err)
	}
	if n > m.cfg.MaxBytes {
		return Status{}, fmt.Errorf("%s: %w", src, geoip.ErrTooLarge)
	}

	m.emit(Download{Active: true, Phase: PhaseVerifying, Source: src, Downloaded: n, Total: total, StartedAt: started})
	if err := geoip.CommitMMDB(tmp, m.cfg.Path); err != nil {
		return Status{}, fmt.Errorf("%s: %w", src, err)
	}
	if m.cfg.Reload != nil {
		m.cfg.Reload()
	}

	m.emit(Download{Active: false, Phase: PhaseDone, Source: src, Downloaded: n, Total: total, StartedAt: started})
	m.logInfo("geoipupdate: database updated", "source", src, "bytes", n)
	return m.setStatus(Status{State: StateOK, Source: src, UpdatedAt: m.cfg.Now().Unix()}), nil
}

// progressReader reports how many body bytes have been read so far. It only
// observes: the read path and its error semantics are untouched.
type progressReader struct {
	r      io.Reader
	total  int64
	n      int64
	last   time.Time
	report func(downloaded, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	// last is the zero time on the first read, so the first bytes report
	// immediately instead of leaving the panel blank for one interval.
	if now := time.Now(); now.Sub(p.last) >= progressInterval {
		p.last = now
		p.report(p.n, p.total)
	}
	return n, err
}

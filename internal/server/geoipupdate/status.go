package geoipupdate

import (
	"time"
)

// Status is the §14 database's persisted state (settings key geoip.status).
// It is what the panel renders when no update is running: where the data came
// from, when it was last replaced, and why the last attempt failed.
type Status struct {
	// State is one of StateOK / StateDisabled / StatePending / StateFailed.
	State string `json:"state"`
	// Source is the mirror URL the current database came from.
	Source string `json:"source,omitempty"`
	// UpdatedAt is when a database was last installed (unix seconds).
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// CheckedAt is when the freshness policy last ran.
	CheckedAt int64 `json:"checked_at,omitempty"`
	// Error carries the failure reason, or why automatic updates are off.
	Error string `json:"error,omitempty"`
	// AutoUpdate is the effective switch (env and panel combined).
	AutoUpdate bool `json:"auto_update"`
	// EnvLocked is true when FOBE_GEOIP_AUTO_UPDATE=0 forbids automatic
	// updates, in which case the panel's own switch has no effect.
	EnvLocked bool `json:"env_locked"`
	// MaxAgeDays is the effective refresh threshold in days (0 = no schedule).
	MaxAgeDays int `json:"max_age_days"`
}

// Phases reported in Download.Phase.
const (
	// PhaseConnecting means a mirror request has started; Source names it.
	PhaseConnecting = "connecting"
	// PhaseDownloading streams the database body; Downloaded/Total are bytes.
	PhaseDownloading = "downloading"
	// PhaseVerifying parses the downloaded file before it may replace the live
	// one (the MMDB format has no magic header, so this is the only real check).
	PhaseVerifying = "verifying"
	// PhaseDone is terminal: the database on disk is the new one.
	PhaseDone = "done"
	// PhaseFailed is terminal: Error carries the reason, nothing was replaced.
	PhaseFailed = "failed"
)

// Download is one observation of an update in flight, and also the snapshot the
// panel polls. It is a value snapshot without timestamps of its own beyond the
// ones carried here: the progress callback runs inside the download loop, so
// producing one must stay cheap.
type Download struct {
	// Active is true while an update holds the manager (connecting, downloading
	// or verifying).
	Active bool `json:"active"`
	// Phase is one of the Phase* constants.
	Phase string `json:"phase,omitempty"`
	// Source is the mirror URL being tried.
	Source string `json:"source,omitempty"`
	// Downloaded / Total are body bytes. Total is 0 when the mirror sends no
	// Content-Length, and Percent is then 0 too.
	Downloaded int64 `json:"downloaded"`
	Total      int64 `json:"total,omitempty"`
	// Percent is Downloaded/Total*100 (0 when Total is unknown).
	Percent float64 `json:"percent,omitempty"`
	// Speed is the bytes/second observed over the last interval.
	Speed int64 `json:"speed,omitempty"`
	// StartedAt is when this attempt began (unix seconds); it is stable across
	// the phases of one attempt, so a consumer can compute its own rate.
	StartedAt int64 `json:"started_at,omitempty"`
	// UpdatedAt is when this snapshot was produced (unix seconds).
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// Error is set when Phase is PhaseFailed.
	Error string `json:"error,omitempty"`
}

// speedWindow ignores byte samples closer than this, so a burst of small reads
// cannot produce a meaningless rate.
const speedWindow = 200 * time.Millisecond

// emit records one observation and hands it to the configured callback.
//
// Byte ticks are already throttled by the progress reader, so nothing is
// dropped here; the snapshot is kept so a panel that loads mid-download still
// sees the live bar through GET /api/geoip/status.
func (m *Manager) emit(d Download) {
	now := m.cfg.Now()
	if d.UpdatedAt == 0 {
		d.UpdatedAt = now.Unix()
	}

	m.mu.Lock()
	prev := m.cur
	if d.StartedAt == 0 {
		d.StartedAt = prev.StartedAt
	}
	if d.Total > 0 {
		d.Percent = float64(d.Downloaded) / float64(d.Total) * 100
		if d.Percent > 100 {
			d.Percent = 100
		}
	}
	if d.Phase == PhaseDownloading {
		d.Speed = m.speedLocked(d.Downloaded, now)
	} else if prev.Phase == PhaseDownloading && d.Downloaded == 0 {
		// Post-download phases carry no byte count of their own: keep the last
		// one so the bar stays filled while the file is being verified.
		d.Downloaded, d.Total, d.Percent = prev.Downloaded, prev.Total, prev.Percent
	}
	m.cur = d
	cb := m.cfg.Progress
	m.mu.Unlock()

	if cb != nil {
		cb(d)
	}
}

// speedLocked computes bytes/second between two observed byte counts (caller
// holds mu).
func (m *Manager) speedLocked(downloaded int64, now time.Time) int64 {
	if downloaded <= 0 {
		return m.cur.Speed
	}
	since := now.Sub(m.lastBytesAt)
	if !m.lastBytesAt.IsZero() && since >= speedWindow {
		delta := downloaded - m.lastBytes
		m.lastBytesAt, m.lastBytes = now, downloaded
		if delta > 0 {
			return int64(float64(delta) / since.Seconds())
		}
		return 0
	}
	if m.lastBytesAt.IsZero() {
		m.lastBytesAt, m.lastBytes = now, downloaded
	}
	return m.cur.Speed
}

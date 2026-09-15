package httpapi

import (
	"sync"
	"time"

	"github.com/fobe-panel/fobe/internal/server/singboxdl"
)

// --- sing-box artifact download progress (design §9.5.3) ---
//
// The downloader reports raw phases through singboxdl.Config.Progress; this
// file turns them into the single snapshot the settings page renders and
// throttles the SSE pushes. It is in-memory on purpose: a byte counter would
// otherwise hit SQLite several times a second (single writer, §5). The
// persisted `singbox.cache_status` still carries start/finish/failure, so a
// page that loads mid-download gets the live bar from `download` and the
// outcome from `cache_status`.

// Progress event kinds on /ws/events.
const (
	// eventSingboxDownload carries one SingboxDownload snapshot.
	eventSingboxDownload = "singbox_download"
)

// downloadPublishInterval is the byte-tick throttle on the SSE channel (phase
// changes and terminal states always go out). The panel needs a smooth bar,
// not every chunk.
const downloadPublishInterval = 250 * time.Millisecond

// SingboxDownload is the panel's view of an artifact install in flight.
type SingboxDownload struct {
	// Active is true while an install holds the downloader (resolving,
	// downloading, verifying, extracting, publishing, or queued behind one).
	Active bool `json:"active"`
	// Version is the canonical version being installed.
	Version string `json:"version,omitempty"`
	// Phase is one of the singboxdl.Phase* names.
	Phase string `json:"phase,omitempty"`
	// Downloaded / Total are archive bytes; Total is 0 when upstream sends no
	// Content-Length and Percent is then 0 too.
	Downloaded int64 `json:"downloaded"`
	Total      int64 `json:"total,omitempty"`
	// Percent is Downloaded/Total*100 (0 when Total is unknown).
	Percent float64 `json:"percent,omitempty"`
	// Speed is the bytes/second observed over the last interval (download
	// phase only); it stays 0 until a second sample exists.
	Speed int64 `json:"speed,omitempty"`
	// StartedAt is when the install began (unix seconds).
	StartedAt int64 `json:"started_at,omitempty"`
	// UpdatedAt is when this snapshot was produced (unix seconds).
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// Error is set when Phase is "failed".
	Error string `json:"error,omitempty"`
}

// activePhase reports whether a phase means "the downloader is busy".
func activePhase(phase string) bool {
	switch phase {
	case singboxdl.PhaseWaiting, singboxdl.PhaseResolving, singboxdl.PhaseDownloading,
		singboxdl.PhaseVerifying, singboxdl.PhaseExtracting, singboxdl.PhasePublishing:
		return true
	default:
		return false
	}
}

// singboxProgress holds the latest snapshot and the throttle state. The zero
// value is ready to use (Active false, nothing ever ran).
type singboxProgress struct {
	mu      sync.Mutex
	cur     SingboxDownload
	lastPub time.Time
	// rate sampling: bytes and time of the previous observed tick
	lastBytesAt time.Time
	lastBytes   int64
}

// observe folds one downloader observation into the snapshot and reports
// whether it should be published. now is unix seconds for the payload, while
// the throttle uses the wall clock because it measures event spacing.
func (t *singboxProgress) observe(p singboxdl.Progress, now int64) (SingboxDownload, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	wall := time.Now()
	// A caller queued behind the running download of the same version must not
	// reset the bar the panel is already showing.
	if p.Phase == singboxdl.PhaseWaiting && t.cur.Active && t.cur.Version == p.Version {
		return t.cur, false
	}
	prev := t.cur

	next := SingboxDownload{
		Active:     activePhase(p.Phase),
		Version:    p.Version,
		Phase:      p.Phase,
		Downloaded: p.Downloaded,
		Total:      p.Total,
		StartedAt:  p.StartedAt,
		UpdatedAt:  now,
		Error:      p.Error,
	}
	if next.Version == "" {
		next.Version = prev.Version
	}
	if next.StartedAt == 0 {
		next.StartedAt = prev.StartedAt
	}
	// Phases after the download carry no byte count: keep the last one so the
	// bar stays filled while the server verifies/extracts.
	if next.Downloaded == 0 && next.Total == 0 && next.Version == prev.Version {
		next.Downloaded, next.Total, next.Percent = prev.Downloaded, prev.Total, prev.Percent
	}
	if next.Total > 0 {
		pct := float64(next.Downloaded) / float64(next.Total) * 100
		if pct > 100 {
			pct = 100
		}
		next.Percent = pct
		next.Speed = t.speedLocked(p, wall)
	} else if p.Phase == singboxdl.PhaseDownloading {
		next.Speed = t.speedLocked(p, wall)
	}

	phaseChanged := prev.Phase != next.Phase
	terminal := !next.Active
	// The first bytes are pushed immediately even when they land inside the
	// throttle window: a stalled download would otherwise sit at 0 B until the
	// next tick, which may never come.
	firstBytes := next.Phase == singboxdl.PhaseDownloading && prev.Downloaded == 0 && next.Downloaded > 0
	t.cur = next

	if phaseChanged || terminal || firstBytes || wall.Sub(t.lastPub) >= downloadPublishInterval {
		t.lastPub = wall
		return next, true
	}
	return next, false
}

// speedLocked computes bytes/second between two observed byte counts. Samples
// closer than 200ms are ignored so a burst of small reads cannot produce a
// meaningless rate.
func (t *singboxProgress) speedLocked(p singboxdl.Progress, wall time.Time) int64 {
	if p.Phase != singboxdl.PhaseDownloading || p.Downloaded <= 0 {
		return t.cur.Speed
	}
	since := wall.Sub(t.lastBytesAt)
	if !t.lastBytesAt.IsZero() && since >= 200*time.Millisecond {
		delta := p.Downloaded - t.lastBytes
		t.lastBytesAt, t.lastBytes = wall, p.Downloaded
		if delta > 0 {
			return int64(float64(delta) / since.Seconds())
		}
		return 0
	}
	if t.lastBytesAt.IsZero() {
		t.lastBytesAt, t.lastBytes = wall, p.Downloaded
	}
	return t.cur.Speed
}

// snapshot returns the latest snapshot; the zero value (Active false) when
// nothing ever ran.
func (t *singboxProgress) snapshot() SingboxDownload {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

// onSingboxProgress is the downloader's callback: it folds the observation
// into the snapshot and pushes it to every open panel (throttled).
func (s *Server) onSingboxProgress(p singboxdl.Progress) {
	snap, publish := s.sbProg.observe(p, nowUnix())
	if publish {
		s.publishEventData(eventSingboxDownload, snap.Version, snap)
	}
}

// failSingboxDownload records a terminal failure for work that never reached
// the downloader — today that is the release lookup before Install (an unknown
// version, or an unreachable release host). Without it the optimistic row the
// panel created would sit at "resolving" forever, because the downloader is
// the only other thing that emits a terminal phase.
func (s *Server) failSingboxDownload(version string, err error) {
	s.onSingboxProgress(singboxdl.Progress{
		Phase: singboxdl.PhaseFailed, Version: version, Error: err.Error(),
	})
}

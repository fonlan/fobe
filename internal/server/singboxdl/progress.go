package singboxdl

import (
	"io"
	"time"
)

// Progress reporting for one artifact install (design §9.5.3).
//
// The server used to expose a single `pending` state, so a 30 MB download was
// an opaque wait: the settings page could not tell "resolving the release"
// from "streaming the tarball" from "verifying". Config.Progress carries a
// plain snapshot of what the install is doing; the caller (httpapi) turns it
// into a throttled SSE push. The type stays in this package so the phase names
// have exactly one definition and the downloader owns the vocabulary.

// Phases reported through Config.Progress.
const (
	// PhaseWaiting means another install already holds this client's install
	// mutex: no second download starts, the call is queued behind the running
	// one (design §9.5.3 同一产物只有一次下载).
	PhaseWaiting = "waiting"
	// PhaseResolving fetches the expected sha256 (API digest or .sha256 sidecar).
	PhaseResolving = "resolving"
	// PhaseDownloading streams the release tarball; Downloaded/Total are bytes.
	PhaseDownloading = "downloading"
	// PhaseVerifying compares the archive digest with the expected one.
	PhaseVerifying = "verifying"
	// PhaseExtracting unpacks the static binary out of the tarball.
	PhaseExtracting = "extracting"
	// PhasePublishing renames the temp dir into <DLDir>/singbox/<version>.
	PhasePublishing = "publishing"
	// PhaseDone is terminal: the version is cached.
	PhaseDone = "done"
	// PhaseFailed is terminal: Error carries the reason, nothing was published.
	PhaseFailed = "failed"
)

// Progress is one observation of an install in flight. It is a value snapshot,
// deliberately without timestamps of its own: the callback runs inside the
// download loop, so it must stay cheap and must never block.
type Progress struct {
	// Version is the canonical version being installed (always set today, but
	// callers must tolerate an empty one).
	Version string
	// Phase is one of the Phase* constants.
	Phase string
	// Downloaded / Total are archive bytes. Total is 0 when upstream sends no
	// Content-Length; a consumer then shows bytes without a percentage.
	Downloaded int64
	Total      int64
	// StartedAt is when this install began (unix seconds); it is stable across
	// the phases of one install, so a consumer can compute its own rate.
	StartedAt int64
	// Error is set on PhaseFailed.
	Error string
}

// progressInterval throttles byte-level reports: at full speed a 30 MB body
// would otherwise emit thousands of events, each one crossing a mutex and the
// SSE broker. Phase changes bypass the throttle (emit always sends them).
const progressInterval = 250 * time.Millisecond

// emit hands one observation to the configured callback; a nil callback is the
// normal case for tests and one-off clients.
func (c *Client) emit(p Progress) {
	if c.cfg.Progress != nil {
		c.cfg.Progress(p)
	}
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

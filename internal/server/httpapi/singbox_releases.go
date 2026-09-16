package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fonlan/fobe/internal/server/singboxdl"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- upstream release listing + explicit version download (design §9.5) ---
//
// The panel renders the artifact cache from disk only: §9.2 「面板只提供磁盘上
// 真实存在的版本」. This file adds the one exception, and it is an explicit
// operator action, never a page render: "which versions exist upstream?" plus
// the download of one explicitly chosen version. Everything is version-explicit
// — there is no `latest` shortcut here, because a panel row must name the
// version it shows (§9.2 版本必须显式指定).

const (
	// singboxReleasesTTL is how long one upstream listing is reused. The
	// listing entry is heavy (~300 KB of JSON per release: every release
	// repeats ~170 assets), so flipping through the settings page must not
	// refetch it; the operator can force a refresh from the panel.
	singboxReleasesTTL = 10 * time.Minute
	// singboxReleasesLimit is the size of that one page. A version picker
	// needs the head of the list, not the history.
	singboxReleasesLimit = 30
	// singboxReleaseFetchTimeout caps the single upstream call behind the
	// picker. It matches the panel's own budget for an operator-triggered
	// lookup (singboxReleaseTimeout), so the UI timeout and the server timeout
	// cannot disagree.
	singboxReleaseFetchTimeout = singboxReleaseTimeout
)

// SingboxRelease is one selectable upstream version in the settings page.
type SingboxRelease struct {
	Version string `json:"version"`
	// Tag is the raw upstream tag (v1.12.0), shown as a tooltip only.
	Tag string `json:"tag,omitempty"`
	// Prerelease marks betas/rcs: downloadable on purpose, never preselected.
	Prerelease bool `json:"prerelease"`
	// PublishedAt is the upstream publication time (unix seconds; 0 unknown).
	PublishedAt int64 `json:"published_at,omitempty"`
	// Cached reports that this version is already on disk. The picker shows
	// those rows as already-downloaded instead of offering them again.
	Cached bool `json:"cached"`
	// LatestStable marks the newest non-prerelease entry of the listing.
	LatestStable bool `json:"latest_stable"`
}

// singboxReleasesCache memoizes one upstream listing. Holding the lock across
// the fetch is deliberate: it makes the endpoint single-flight, so ten open
// tabs (or a refresh storm) cannot fan out into ten ~10 MB API calls against
// the release host.
type singboxReleasesCache struct {
	mu      sync.Mutex
	at      time.Time
	fetched int64 // unix seconds of the served listing
	items   []SingboxRelease
}

// singboxReleases returns the upstream listing, from cache when it is fresh
// (or, on failure, stale rather than empty). stale is true when the caller is
// getting the last good copy because the refresh failed.
func (s *Server) singboxReleases(ctx context.Context, refresh bool) (items []SingboxRelease, fetchedAt int64, stale bool, err error) {
	s.sbRel.mu.Lock()
	defer s.sbRel.mu.Unlock()

	if !refresh && len(s.sbRel.items) > 0 && time.Since(s.sbRel.at) < singboxReleasesTTL {
		return s.sbRel.items, s.sbRel.fetched, false, nil
	}

	cctx, cancel := context.WithTimeout(ctx, singboxReleaseFetchTimeout)
	defer cancel()
	rels, ferr := s.SingboxDL().RecentReleases(cctx, singboxReleasesLimit)
	if ferr != nil {
		// A stale picker beats an empty one: the versions on disk are listed
		// from the cache anyway, and the operator may just want to download a
		// version they already know.
		if len(s.sbRel.items) > 0 {
			return s.sbRel.items, s.sbRel.fetched, true, ferr
		}
		return nil, 0, false, ferr
	}

	cached := map[string]bool{}
	for _, cv := range s.singboxVersions() {
		cached[cv.Version] = true
	}
	latest := ""
	for _, rel := range rels {
		if !rel.Prerelease && (latest == "" || singboxdl.CompareVersions(rel.Version, latest) > 0) {
			latest = rel.Version
		}
	}
	out := make([]SingboxRelease, 0, len(rels))
	for _, rel := range rels {
		out = append(out, SingboxRelease{
			Version:      rel.Version,
			Tag:          rel.Tag,
			Prerelease:   rel.Prerelease,
			PublishedAt:  rel.PublishedAt,
			Cached:       cached[rel.Version],
			LatestStable: rel.Version == latest,
		})
	}
	s.sbRel.items, s.sbRel.at, s.sbRel.fetched = out, time.Now(), nowUnix()
	return out, s.sbRel.fetched, false, nil
}

// handleSingboxReleases answers GET /api/singbox/releases — the version picker
// behind "download a new sing-box". ?refresh=1 bypasses the TTL (the refresh
// button in the panel).
//
// The endpoint is local-only in spirit: it is the one place the panel is
// allowed to talk upstream while an operator is watching, and a failure is
// reported as a stale list rather than a broken page.
func (s *Server) handleSingboxReleases(w http.ResponseWriter, r *http.Request) {
	if s.DLDir == "" {
		writeErr(w, http.StatusBadRequest, "dl_dir_unset")
		return
	}
	refresh := r.URL.Query().Get("refresh") != ""
	items, fetchedAt, stale, err := s.singboxReleases(r.Context(), refresh)
	if err != nil && len(items) == 0 {
		writeErr(w, http.StatusBadGateway, "release_unavailable")
		return
	}
	body := map[string]any{
		"releases":   items,
		"fetched_at": fetchedAt,
		"stale":      stale,
		"error":      nil,
	}
	if err != nil {
		body["error"] = "release_unavailable"
	}
	writeJSON(w, http.StatusOK, body)
}

// handleSingboxVersionDownload answers
// POST /api/singbox/versions/{version}/download: fetch one explicitly named
// release into the cache so it can be published to probes.
//
// The response returns as soon as the job is accepted — the download takes
// minutes — and the row it creates is fed by the same throttled progress
// snapshot the settings page already renders (GET /api/singbox/cache +
// the singbox_download event).
func (s *Server) handleSingboxVersionDownload(w http.ResponseWriter, r *http.Request) {
	v, err := singboxdl.ParseVersion(r.PathValue("version"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_version")
		return
	}
	if s.DLDir == "" {
		writeErr(w, http.StatusBadRequest, "dl_dir_unset")
		return
	}
	version := v.String()
	dl := s.SingboxDL()

	// Already cached: the operator asked for a state the server is already in.
	// Answering 200 (instead of 409) keeps a double click harmless.
	if st, err := os.Stat(filepath.Join(dl.VersionDir(version), singboxdl.BinaryName)); err == nil && st.Mode().IsRegular() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "version": version, "cached": true, "download": s.sbProg.snapshot(),
		})
		return
	}

	// One install at a time. The downloader serializes on its own mutex, but
	// the panel renders exactly one progress snapshot: letting a second,
	// *different* version in would make that row flip between two installs.
	// The same version may be requested again (a retry): it queues and finds
	// the version published, which Install reports as ErrVersionExists.
	if cur := s.sbProg.snapshot(); cur.Active && cur.Version != "" && cur.Version != version {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    map[string]string{"code": "download_in_progress"},
			"download": cur,
		})
		return
	}

	// The row must exist from the click, not from the first byte: looking the
	// release up in the upstream listing is a network call that reports no
	// progress of its own (it walks release pages until it finds the version).
	s.onSingboxProgress(singboxdl.Progress{Phase: singboxdl.PhaseResolving, Version: version})
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", Action: "singbox_version_download", Command: version, SourceIP: s.Trust.RealIP(r),
	})

	go func() {
		ctx := s.background()
		rel, err := dl.ReleaseByVersion(ctx, version)
		if err != nil {
			// Install never ran, so nothing else reports this failure: turn it
			// into the terminal phase the row renders.
			s.failSingboxDownload(version, err)
			if s.Log != nil {
				s.Log.Warn("singbox version download failed", "version", version, "err", err)
			}
			return
		}
		if _, err := dl.Install(ctx, rel, false); err != nil && !errors.Is(err, singboxdl.ErrVersionExists) {
			// Install already reported the failure through Progress; only the
			// server log needs it.
			if s.Log != nil {
				s.Log.Warn("singbox version download failed", "version", version, "err", err)
			}
			return
		}
		s.publishEvent("singbox_cache", "")
		if s.Log != nil {
			s.Log.Info("singbox version downloaded", "version", version)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "version": version, "cached": false, "download": s.sbProg.snapshot(),
	})
}

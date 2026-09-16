package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/server/singboxcache"
	"github.com/fonlan/fobe/internal/server/singboxdl"
	"github.com/fonlan/fobe/internal/server/singboxupdate"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- sing-box artifact cache + one-click batch update (design §9.2) ---

// singboxReleaseTimeout caps the single upstream call the panel makes while
// answering an explicit operator action (the confirmation dialog's "latest"
// lookup and a "latest" update). Rendering a page never calls out: only these
// endpoints do, so the release host can be unreachable without breaking the UI.
const singboxReleaseTimeout = 20 * time.Second

// SingboxUpdater returns the §9.2 batch-update manager, building it from the
// server's own dependencies on first use. Tests may pre-set Server.SingboxUpdate
// to substitute their own (e.g. a shorter convergence window).
func (s *Server) SingboxUpdater() *singboxupdate.Manager {
	s.sbUpdOnce.Do(func() {
		if s.SingboxUpdate != nil {
			return
		}
		var online singboxupdate.Online
		if s.Hub != nil {
			online = s.Hub
		}
		s.SingboxUpdate = singboxupdate.New(singboxupdate.Config{
			Store:  s.Store,
			DL:     s.SingboxDL(),
			Online: online,
			Log:    s.Log,
			Push: func(nodeID string) bool {
				ok := s.pushDesired(nodeID)
				s.publishEvent("node_updated", nodeID)
				return ok
			},
			Publish: func(job singboxupdate.Job) {
				// Progress rides /ws/events: the panel re-renders straight from
				// the snapshot instead of polling the job endpoint.
				s.publishEventData("singbox_update", job.ID, job)
			},
			CacheChanged: func() { s.publishEvent("singbox_cache", "") },
		})
	})
	return s.SingboxUpdate
}

// singboxAutoDownload reports the FOBE_SINGBOX_AUTO_DOWNLOAD switch the settings
// page shows. The status written by startup wins, because it is what actually
// happened on this boot.
func (s *Server) singboxAutoDownload() bool {
	if st, ok := singboxcache.LoadStatus(s.Store); ok {
		return st.AutoDownload
	}
	return s.SingboxAutoDownload
}

// singboxCacheVersion is one cached release plus the "newest on disk" badge.
// There is deliberately no network lookup here: the panel only ever offers
// versions that are already cached (§9.2 已有缓存则完全不联网).
type singboxCacheVersion struct {
	SingboxVersion
	IsLatest bool `json:"is_latest"`
}

// handleSingboxCache answers GET /api/singbox/cache: the cached releases with
// their size/download time/reference count, the newest one, the startup
// download state, the container mount verdict, the live download progress and
// the last batch update. It is entirely local.
func (s *Server) handleSingboxCache(w http.ResponseWriter, r *http.Request) {
	versions := s.singboxVersions()
	out := make([]singboxCacheVersion, 0, len(versions))
	latest := ""
	for i, v := range versions {
		out = append(out, singboxCacheVersion{SingboxVersion: v, IsLatest: i == 0})
		if i == 0 {
			latest = v.Version
		}
	}
	mountOK, mountApplicable := singboxcache.LoadMountOK(s.Store)
	body := map[string]any{
		"dl_dir":           s.DLDir,
		"versions":         out,
		"latest_cached":    latest,
		"auto_download":    s.singboxAutoDownload(),
		"mount_ok":         mountOK,
		"mount_applicable": mountApplicable,
		"last_update":      s.SingboxUpdater().Current(),
		// Live download progress. Deliberately not part of the persisted cache
		// status: a byte counter would write to SQLite several times a second
		// (§5 single writer). A page that loads mid-download gets its bar here
		// and the outcome from cache_status.
		"download": s.sbProg.snapshot(),
	}
	if st, ok := singboxcache.LoadStatus(s.Store); ok {
		body["cache_status"] = st
	} else {
		body["cache_status"] = nil
	}
	writeJSON(w, http.StatusOK, body)
}

// handleSingboxCacheRetry answers POST /api/singbox/cache/retry: the manual
// retry behind a failed startup download (§9.2 「失败只记日志 + 设置页状态 +
// 手动重试」). The download runs in the background — it can take minutes — and
// the panel learns the outcome from the singbox_cache event plus the refreshed
// cache status.
func (s *Server) handleSingboxCacheRetry(w http.ResponseWriter, r *http.Request) {
	if s.SingboxCache == nil {
		writeErr(w, http.StatusServiceUnavailable, "cache_unavailable")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", Action: "singbox_cache_retry", SourceIP: s.Trust.RealIP(r),
	})
	go func() {
		st := s.SingboxCache.Ensure(s.background())
		s.publishEvent("singbox_cache", "")
		if s.Log != nil {
			s.Log.Info("singbox cache retry finished", "state", st.State, "version", st.Version, "err", st.Error)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "status": s.SingboxCache.Status()})
}

// background is the cancellation source for handler-started work that outlives
// its response.
func (s *Server) background() context.Context {
	if s.Background != nil {
		return s.Background
	}
	return context.Background()
}

// resolveSingboxTarget turns the operator's request ("latest" or an explicit
// tag) into a canonical version. "latest" is the only case that needs the
// network, and it is resolved here rather than at page render time.
func (s *Server) resolveSingboxTarget(ctx context.Context, requested string) (string, *singboxdl.Release, error) {
	if requested == "" || strings.EqualFold(requested, "latest") {
		cctx, cancel := context.WithTimeout(ctx, singboxReleaseTimeout)
		defer cancel()
		rel, err := s.SingboxDL().LatestStable(cctx)
		if err != nil {
			return "", nil, err
		}
		v, err := singboxdl.ParseVersion(rel.Version)
		if err != nil {
			return "", nil, err
		}
		return v.String(), &rel, nil
	}
	v, err := singboxdl.ParseVersion(requested)
	if err != nil {
		return "", nil, err
	}
	return v.String(), nil, nil
}

// handleSingboxUpdateImpact answers GET /api/singbox/update/impact: the node
// list and counts behind the confirmation dialog. ?version=latest (or empty)
// resolves the current stable release; an explicit version is taken as-is.
func (s *Server) handleSingboxUpdateImpact(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimSpace(r.URL.Query().Get("version"))
	if requested == "" {
		requested = "latest"
	}
	target, _, err := s.resolveSingboxTarget(r.Context(), requested)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "release_unavailable")
		return
	}
	imp, err := s.SingboxUpdater().Impact(target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	imp.Requested = requested
	writeJSON(w, http.StatusOK, imp)
}

type singboxUpdateReq struct {
	// Version is an explicit cached release to distribute.
	Version string `json:"version"`
	// Latest asks for the current stable release instead of Version.
	Latest bool `json:"latest"`
	// Confirm must be true: the endpoint is the second gate of the two-step
	// confirmation (the dialog is the first).
	Confirm bool `json:"confirm"`
}

// handleSingboxUpdate answers POST /api/singbox/update. It resolves the target
// version, refuses to run without an explicit confirmation, and hands the
// distribution to the background job, which returns its id immediately.
func (s *Server) handleSingboxUpdate(w http.ResponseWriter, r *http.Request) {
	var req singboxUpdateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !req.Confirm {
		writeErr(w, http.StatusBadRequest, "confirm_required")
		return
	}
	requested := "latest"
	if !req.Latest {
		if strings.TrimSpace(req.Version) == "" {
			writeErr(w, http.StatusBadRequest, "bad_version")
			return
		}
		requested = strings.TrimSpace(req.Version)
	}

	mgr := s.SingboxUpdater()
	if job := mgr.InProgress(); job != nil {
		writeJSONJobErr(w, http.StatusConflict, "update_in_progress", job)
		return
	}
	target, release, err := s.resolveSingboxTarget(r.Context(), requested)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "release_unavailable")
		return
	}
	imp, err := mgr.Impact(target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if imp.Count == 0 {
		writeErr(w, http.StatusBadRequest, "no_targets")
		return
	}

	ip := s.Trust.RealIP(r)
	job, err := mgr.StartUpdate(singboxupdate.UpdateRequest{
		Version: target, Requested: requested, Release: release,
		Actor: "panel", SourceIP: ip,
	})
	switch {
	case errors.Is(err, singboxupdate.ErrInProgress):
		writeJSONJobErr(w, http.StatusConflict, "update_in_progress", job)
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, "bad_version")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", Action: "singbox_update", Command: target, SourceIP: ip,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// handleSingboxUpdateStatus answers GET /api/singbox/update/{job} — the id is
// the guard against a stale page showing a newer job's progress.
func (s *Server) handleSingboxUpdateStatus(w http.ResponseWriter, r *http.Request) {
	job := s.SingboxUpdater().Current()
	if job == nil || job.ID != r.PathValue("job") {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// handleSingboxDeleteVersion answers DELETE /api/singbox/versions/{version}.
// A version that nodes still desire needs ?force=1, so the confirmation the
// panel shows is a real gate rather than decoration (§9.2).
func (s *Server) handleSingboxDeleteVersion(w http.ResponseWriter, r *http.Request) {
	v, err := singboxdl.ParseVersion(r.PathValue("version"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_version")
		return
	}
	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
	err = s.SingboxUpdater().DeleteVersion(v.String(), force)
	var inUse *singboxupdate.InUseError
	switch {
	case err == nil:
	case errors.Is(err, singboxupdate.ErrInProgress):
		writeErr(w, http.StatusConflict, "update_in_progress")
		return
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   map[string]string{"code": "version_in_use", "message": inUse.Error()},
			"version": inUse.Version,
			"refs":    inUse.Refs,
		})
		return
	case errors.Is(err, singboxdl.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, singboxdl.ErrNoDLDir):
		writeErr(w, http.StatusBadRequest, "dl_dir_unset")
		return
	default:
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", Action: "singbox_version_deleted", Command: v.String(), SourceIP: s.Trust.RealIP(r),
	})
	s.publishEvent("singbox_cache", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": v.String()})
}

// writeJSONJobErr returns the standard error envelope plus the job the caller
// collided with, so a panel can jump straight to the running progress view.
func writeJSONJobErr(w http.ResponseWriter, status int, code string, job *singboxupdate.Job) {
	body := map[string]any{"error": map[string]string{"code": code}}
	if job != nil {
		body["job"] = job
	}
	writeJSON(w, status, body)
}

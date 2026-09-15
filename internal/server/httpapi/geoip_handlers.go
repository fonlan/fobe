package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/fobe-panel/fobe/internal/server/geoip"
	"github.com/fobe-panel/fobe/internal/server/geoipupdate"
)

// --- GeoIP MMDB (design §14/§14.1) ---
//
// One file, two writers: the manual upload (the offline escape hatch) and the
// automatic mirror download in geoipupdate. Both install through
// geoip.WriteMMDB, so a payload only replaces the live database once it parses
// — the MMDB format has no magic header, and a truncated or HTML error body
// must never displace a working database.

// eventGeoIPUpdate carries one geoipupdate.Download snapshot to the panels, so
// the settings page renders the mirror download without polling.
const eventGeoIPUpdate = "geoip_update"

// reloadableResolver matches the explicit reload hook of the concrete MMDB
// resolver without depending on it: after a successful install the Server's
// resolver is asserted to this interface and reloaded. Resolvers without a
// Reload method still pick the file up on their next lookup (the mmdb
// implementation re-stats the path per query).
type reloadableResolver interface {
	Reload() bool
}

// metadataResolver matches *geoip.MMDB's self-description hook.
type metadataResolver interface {
	Metadata() geoip.Metadata
}

// handleUploadMMDB accepts raw MMDB bytes and installs them atomically.
func (s *Server) handleUploadMMDB(w http.ResponseWriter, r *http.Request) {
	if s.GeoIPMMDBPath == "" {
		writeErr(w, http.StatusServiceUnavailable, "mmdb_not_configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, geoip.MaxMMDBBytes)
	n, err := geoip.WriteMMDB(s.GeoIPMMDBPath, r.Body)
	if !writeMMDBErr(w, err) {
		return
	}
	s.reloadGeoIP()
	// An upload replaces the database just as a download does, so it must also
	// replace the status: otherwise a failed auto-update would keep reporting
	// its error (and its mirror as the origin) over a file the operator just
	// installed by hand.
	if s.GeoIPUpdater != nil {
		s.GeoIPUpdater.NoteInstalled(geoipupdate.SourceUpload)
	}
	s.audit("geoip_mmdb_uploaded", fmt.Sprintf("%s (%d bytes)", s.GeoIPMMDBPath, n), s.Trust.RealIP(r))
	s.publishEvent("geoip_updated", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeMMDBErr maps an install error onto the API's error codes. It reports
// whether the install succeeded.
func writeMMDBErr(w http.ResponseWriter, err error) bool {
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.Is(err, geoip.ErrInvalidMMDB):
		writeErr(w, http.StatusBadRequest, "bad_mmdb")
	case errors.Is(err, geoip.ErrTooLarge), errors.As(err, &tooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "mmdb_too_large")
	default:
		writeErr(w, http.StatusInternalServerError, "internal")
	}
	return false
}

// reloadGeoIP makes the live resolver serve a swapped file immediately.
func (s *Server) reloadGeoIP() {
	if rr, ok := s.GeoIPResolver.(reloadableResolver); ok {
		rr.Reload()
	}
}

// geoIPStatusView is the panel's whole view of §14: the file on disk, what the
// resolver currently serves, and the updater's state. The updater's fields are
// flattened so the page reads one flat object.
type geoIPStatusView struct {
	Path string `json:"path"`
	// Configured is false when FOBE_GEOIP_MMDB is empty: no upload, no update.
	Configured bool  `json:"configured"`
	Exists     bool  `json:"exists"`
	SizeBytes  int64 `json:"size_bytes,omitempty"`
	ModTime    int64 `json:"mod_time,omitempty"`
	// BuildEpoch is when the provider built the data (unix seconds); it is the
	// honest "how old is this data" answer, unlike the file's mtime.
	BuildEpoch   int64  `json:"build_epoch,omitempty"`
	DatabaseType string `json:"database_type,omitempty"`
	// Live is true when the resolver is actually serving a database.
	Live bool `json:"live"`
	// State is one of geoipupdate.State*.
	State string `json:"state"`
	// Source is the mirror URL the current database came from.
	Source string `json:"source,omitempty"`
	// UpdatedAt is when a database was last installed, CheckedAt when the
	// freshness policy last ran (unix seconds).
	UpdatedAt  int64  `json:"updated_at,omitempty"`
	CheckedAt  int64  `json:"checked_at,omitempty"`
	Error      string `json:"error,omitempty"`
	AutoUpdate bool   `json:"auto_update"`
	// EnvLocked is true when FOBE_GEOIP_AUTO_UPDATE=0 disables automatic
	// updates regardless of the panel switch.
	EnvLocked bool `json:"env_locked"`
	// MaxAgeDays is the refresh threshold in days (0 = no schedule).
	MaxAgeDays int `json:"max_age_days"`
	// Download is the live (or last) update attempt.
	Download geoipupdate.Download `json:"download"`
}

// geoIPStatus assembles the status view from the file, the resolver and the
// updater. Every source is optional: an unwired updater or a missing file
// still yields a renderable answer.
func (s *Server) geoIPStatus() geoIPStatusView {
	v := geoIPStatusView{Path: s.GeoIPMMDBPath, Configured: s.GeoIPMMDBPath != ""}

	if info, err := os.Stat(s.GeoIPMMDBPath); err == nil {
		v.Exists = true
		v.SizeBytes = info.Size()
		v.ModTime = info.ModTime().Unix()
	}
	if mr, ok := s.GeoIPResolver.(metadataResolver); ok {
		md := mr.Metadata()
		v.BuildEpoch, v.DatabaseType, v.Live = md.BuildEpoch, md.DatabaseType, md.NodeCount > 0
	}
	if s.GeoIPUpdater == nil {
		v.State = geoipupdate.StateDisabled
		if v.Configured {
			v.Error = "geoip updater is not wired"
		}
		return v
	}
	st := s.GeoIPUpdater.Status()
	// The switch state is read live: the copies inside the persisted status go
	// stale the moment an operator flips one, before the next check runs.
	p := s.GeoIPUpdater.Policy()
	v.AutoUpdate, v.EnvLocked, v.MaxAgeDays = p.AutoUpdate, p.EnvLocked, p.MaxAgeDays

	v.State, v.Source = st.State, st.Source
	v.UpdatedAt, v.CheckedAt, v.Error = st.UpdatedAt, st.CheckedAt, st.Error
	if v.State == "" && v.Exists {
		// A database is on disk from an upload or an earlier run and the
		// updater has not run yet: that is "ok" in every sense the panel means.
		v.State = geoipupdate.StateOK
	}
	v.Download = s.GeoIPUpdater.Download()
	return v
}

func (s *Server) handleGeoIPStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.geoIPStatus())
}

// handleGeoIPUpdate starts a forced update (design §14.1 手动更新). The
// response must not wait for a ~9MB download: the panel follows the mirror
// progress over /ws/events (geoip_update) and the outcome through
// GET /api/geoip/status.
func (s *Server) handleGeoIPUpdate(w http.ResponseWriter, r *http.Request) {
	up := s.GeoIPUpdater
	if s.GeoIPMMDBPath == "" || up == nil {
		writeErr(w, http.StatusServiceUnavailable, "mmdb_not_configured")
		return
	}
	if up.Running() {
		// A second click while the first update runs is not an error: the
		// panel is already showing that update's progress.
		writeJSON(w, http.StatusAccepted, map[string]any{"accepted": false, "running": true})
		return
	}

	// Capture what the goroutine needs before the request is recycled.
	ip := s.Trust.RealIP(r)
	s.audit("geoip_update_started", s.GeoIPMMDBPath, ip)
	ctx := s.background()
	go func() {
		st := up.Ensure(ctx, true)
		if st.State == geoipupdate.StateFailed {
			s.Log.Warn("geoip update failed; the panel shows the reason", "err", st.Error)
			// An operator asked for this update: the reason belongs in the audit
			// trail, not only in the server log.
			s.audit("geoip_update_failed", st.Error, ip)
			return
		}
		// Other panels should pick the new file up; the hub's own resolver
		// re-stats the path per lookup, so it needs no nudge.
		s.publishEvent("geoip_updated", "")
		s.audit("geoip_mmdb_downloaded", fmt.Sprintf("%s (%s)", s.GeoIPMMDBPath, st.Source), ip)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "running": true})
}

// OnGeoIPProgress is the updater's progress callback: it forwards each
// observation to every open panel. The bytes are cheap to marshal and already
// throttled by the updater, so nothing else is throttled here.
func (s *Server) OnGeoIPProgress(d geoipupdate.Download) {
	s.publishEventData(eventGeoIPUpdate, "", d)
}

// validSourceURL accepts an absolute HTTP(S) download URL. Unlike the panel's
// public URL it may carry a query string: mirror URLs are often signed or
// versioned, and rejecting them would defeat a private mirror.
func validSourceURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// geoIPSettingKey reports whether a settings key changes the §14 update policy.
func geoIPSettingKey(key string) bool {
	switch key {
	case geoipupdate.SettingAutoUpdate, geoipupdate.SettingMaxAgeDays, geoipupdate.SettingSourceURL:
		return true
	default:
		return false
	}
}

// validGeoIPSetting applies the §14.1 value rules. It is the single source of
// truth for both entry points — PUT /api/settings and the §17 import, which
// must not be able to install a policy the API would have rejected.
func validGeoIPSetting(key, value string) bool {
	switch key {
	case geoipupdate.SettingAutoUpdate:
		return value == "1" || value == "0"
	case geoipupdate.SettingMaxAgeDays:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		return err == nil && n >= 0 && n <= 365
	case geoipupdate.SettingSourceURL:
		return strings.TrimSpace(value) == "" || validSourceURL(value)
	default:
		return true
	}
}

// geoIPSettingErrCode names the API error returned for a rejected value.
func geoIPSettingErrCode(key string) string {
	switch key {
	case geoipupdate.SettingAutoUpdate:
		return "bad_geoip_auto_update"
	case geoipupdate.SettingMaxAgeDays:
		return "bad_geoip_max_age_days"
	default:
		return "bad_geoip_url"
	}
}

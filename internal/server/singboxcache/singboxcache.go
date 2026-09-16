// Package singboxcache keeps the server-side sing-box artifact cache warm
// (design §9.2, §17).
//
// The panel never downloads while rendering a page: the cache under
// <DLDir>/singbox/<version>/ is filled ahead of time and the panel only offers
// versions that are actually on disk. This package owns that policy:
//
//   - when <DLDir>/singbox holds no valid version, fetch the current stable
//     release in the background — startup is never blocked by the network, and
//     a cache that is already warm never touches the network at all;
//   - record the outcome in settings (singbox.cache_status) so the panel can
//     show auto-download state, the cached version, the last attempt and the
//     failure reason, and offer a manual retry;
//   - verify, inside a container, that the artifact directory is on a mount of
//     its own, so an image upgrade cannot silently drop downloaded versions
//     (singbox.dl_mount_ok).
//
// Everything is optional-safe: a nil Settings, a nil *singboxdl.Client or an
// empty DLDir degrade to a status report, never to a panic.
package singboxcache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fonlan/fobe/internal/server/singboxdl"
)

// Settings keys written by the manager. They are server-owned state: the panel
// reads them (the sing-box settings section) and never lets a client write them
// through PUT /api/settings.
const (
	// SettingCacheStatus holds the JSON-encoded Status below.
	SettingCacheStatus = "singbox.cache_status"
	// SettingDLMountOK records whether the artifact directory lives on a mount
	// that survives a container upgrade ("true"/"false"). It is only written
	// inside a container, so its absence means "not applicable".
	SettingDLMountOK = "singbox.dl_mount_ok"
)

// States reported in Status.State.
const (
	// StateOK means a valid version is cached.
	StateOK = "ok"
	// StateDisabled means auto-download is off or there is no artifact dir.
	StateDisabled = "disabled"
	// StateFailed means the last download/install attempt failed; the reason is
	// in Status.Error and the panel offers a retry.
	StateFailed = "failed"
	// StatePending means a download is in flight.
	StatePending = "pending"
)

// Reasons recorded for StateDisabled.
const (
	reasonAutoDownloadOff = "FOBE_SINGBOX_AUTO_DOWNLOAD=0"
	reasonNoDLDir         = "FOBE_DL_DIR is not configured"
)

// Status is the startup auto-download state, persisted as JSON under
// SettingCacheStatus.
type Status struct {
	// State is one of StateOK / StateDisabled / StateFailed / StatePending.
	State string `json:"state"`
	// Version is the cached version the status refers to (set when ok).
	Version string `json:"version,omitempty"`
	// UpdatedAt is when this status was recorded (unix seconds).
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// Error carries the failure reason (or why auto-download is disabled).
	Error string `json:"error,omitempty"`
	// AutoDownload mirrors the FOBE_SINGBOX_AUTO_DOWNLOAD switch.
	AutoDownload bool `json:"auto_download"`
}

// Settings is the subset of *store.Store the manager needs. A nil Settings
// disables persistence; the manager still tracks the status in memory.
type Settings interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string, encrypted bool) error
}

// Config configures a Manager.
type Config struct {
	// DL is the artifact downloader/cache. Without it nothing can be fetched.
	DL *singboxdl.Client
	// Settings receives singbox.cache_status / singbox.dl_mount_ok.
	Settings Settings
	// Log receives the WARN/INFO notes; nil is fine.
	Log *slog.Logger
	// AutoDownload is FOBE_SINGBOX_AUTO_DOWNLOAD (enabled unless it is "0").
	AutoDownload bool
	// Now overrides time.Now (tests).
	Now func() time.Time
}

// Manager owns the artifact-cache policy. It is safe for concurrent use; at
// most one download runs at a time.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	status  Status
	mount   MountCheck
	running bool
}

// New returns a Manager with defaults applied. It never performs I/O.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg}
}

// Status returns the last known status (zero value before Start/Ensure).
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// MountCheck returns the container/mount verdict of the last Start.
func (m *Manager) MountCheck() MountCheck {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mount
}

func (m *Manager) dlDir() string {
	if m.cfg.DL == nil {
		return ""
	}
	return m.cfg.DL.DLDir()
}

func (m *Manager) logWarn(msg string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log.Warn(msg, args...)
	}
}

func (m *Manager) logInfo(msg string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log.Info(msg, args...)
	}
}

// Start applies the startup policy and returns immediately.
//
// The mount check and the "is anything cached?" probe are local and cheap, so
// they run inline; only the download runs in the background — a slow or
// unreachable release host must never delay the panel. A failure is logged as a
// WARN and recorded in settings; it does not affect the server. Cancelling ctx
// aborts an in-flight download.
func (m *Manager) Start(ctx context.Context) {
	m.checkMount()

	if m.cfg.DL == nil || m.dlDir() == "" {
		m.setStatus(Status{State: StateDisabled, Error: reasonNoDLDir})
		return
	}
	if !m.cfg.AutoDownload {
		m.setStatus(Status{State: StateDisabled, Error: reasonAutoDownloadOff})
		return
	}

	// A warm cache never touches the network (design §9.2: 已有缓存则完全不联网).
	cached, err := m.cfg.DL.LatestCached()
	switch {
	case err == nil:
		m.setStatus(Status{State: StateOK, Version: cached.Version})
		return
	case !errors.Is(err, singboxdl.ErrNotFound):
		m.logWarn("singboxcache: scan cache", "err", err)
	}

	// The cache is empty: drop leftovers from an interrupted install, report
	// "pending" and fetch in the background.
	if n, err := m.cfg.DL.CleanTemps(); err != nil {
		m.logWarn("singboxcache: clean interrupted install dirs", "err", err)
	} else if n > 0 {
		m.logInfo("singboxcache: removed interrupted install dirs", "count", n)
	}
	m.setStatus(Status{State: StatePending})
	go func() {
		st := m.Ensure(ctx)
		if st.State == StateOK {
			m.logInfo("singboxcache: artifact cache ready", "version", st.Version)
			return
		}
		m.logWarn("singboxcache: auto download failed; the panel shows the reason and a manual retry stays possible",
			"state", st.State, "err", st.Error)
	}()
}

// Ensure makes sure at least one version is cached, downloading the current
// stable release when the cache is empty. It blocks until the attempt is over
// and returns the resulting status. It deliberately ignores the AutoDownload
// switch: it is also the manual-retry path.
func (m *Manager) Ensure(ctx context.Context) Status {
	if m.cfg.DL == nil || m.dlDir() == "" {
		return m.setStatus(Status{State: StateDisabled, Error: reasonNoDLDir})
	}

	m.mu.Lock()
	if m.running { // a download is already in flight: report it, never stack
		st := m.status
		m.mu.Unlock()
		return st
	}
	m.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	cached, err := m.cfg.DL.LatestCached()
	switch {
	case err == nil:
		return m.setStatus(Status{State: StateOK, Version: cached.Version})
	case !errors.Is(err, singboxdl.ErrNotFound):
		m.logWarn("singboxcache: scan cache", "err", err)
	}

	m.setStatus(Status{State: StatePending})

	rel, err := m.cfg.DL.LatestStable(ctx)
	if err != nil {
		m.logWarn("singboxcache: resolve latest stable release", "err", err)
		return m.setStatus(Status{State: StateFailed, Error: err.Error()})
	}
	cv, err := m.cfg.DL.Install(ctx, rel, false)
	switch {
	case err == nil:
		return m.setStatus(Status{State: StateOK, Version: cv.Version})
	case errors.Is(err, singboxdl.ErrVersionExists):
		// A parallel attempt published it first: the cache is fine.
		return m.setStatus(Status{State: StateOK, Version: rel.Version})
	default:
		m.logWarn("singboxcache: download sing-box failed", "version", rel.Version, "err", err)
		return m.setStatus(Status{State: StateFailed, Error: err.Error()})
	}
}

// setStatus records and persists a status, stamping the time and the switch.
func (m *Manager) setStatus(s Status) Status {
	s.AutoDownload = m.cfg.AutoDownload
	if s.UpdatedAt == 0 {
		s.UpdatedAt = m.cfg.Now().Unix()
	}
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
	m.persist(s)
	return s
}

// persist writes the status to settings; failures are logged, never fatal.
func (m *Manager) persist(s Status) {
	if m.cfg.Settings == nil {
		return
	}
	raw, err := json.Marshal(s)
	if err != nil {
		m.logWarn("singboxcache: encode cache status", "err", err)
		return
	}
	if err := m.cfg.Settings.SetSetting(SettingCacheStatus, string(raw), false); err != nil {
		m.logWarn("singboxcache: persist cache status", "err", err)
	}
}

// checkMount validates the artifact directory inside a container and records
// the verdict. Outside a container it is skipped entirely: a developer's
// checkout must stay quiet.
func (m *Manager) checkMount() MountCheck {
	mc := CheckMount(m.dlDir())
	m.mu.Lock()
	m.mount = mc
	m.mu.Unlock()
	if !mc.Container {
		return mc
	}
	if m.cfg.Settings != nil {
		val := "false"
		if mc.Mounted {
			val = "true"
		}
		if err := m.cfg.Settings.SetSetting(SettingDLMountOK, val, false); err != nil {
			m.logWarn("singboxcache: persist mount check", "err", err)
		}
	}
	if !mc.Mounted {
		m.logWarn("singboxcache: artifact directory is on the container's writable layer; upgrading the container will lose downloaded sing-box versions",
			"dl_dir", mc.DLDir, "mount_point", mc.MountPoint, "hint", "mount a volume or bind mount at FOBE_DL_DIR")
	}
	return mc
}

// LoadStatus reads the persisted cache status. ok is false when nothing was
// written yet (fresh install) or the value is unreadable.
func LoadStatus(src Settings) (Status, bool) {
	if src == nil {
		return Status{}, false
	}
	raw, err := src.GetSetting(SettingCacheStatus)
	if err != nil || strings.TrimSpace(raw) == "" {
		return Status{}, false
	}
	var s Status
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Status{}, false
	}
	return s, true
}

// LoadMountOK reads the persisted mount verdict. ok is false when the server
// never ran inside a container, in which case the panel shows no warning.
func LoadMountOK(src Settings) (mounted bool, ok bool) {
	if src == nil {
		return false, false
	}
	raw, err := src.GetSetting(SettingDLMountOK)
	if err != nil {
		return false, false
	}
	return strings.EqualFold(strings.TrimSpace(raw), "true"), true
}

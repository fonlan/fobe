// Package geoipupdate keeps the §14 GeoLite2-Country database fresh without
// anyone remembering to upload it (design §14.1).
//
// The panel's upload button stays as the offline escape hatch; this package is
// the automatic path:
//
//   - a missing database is fetched in the background at startup — startup is
//     never blocked by the network;
//   - a daily check refreshes the database once the file is older than
//     geoip.max_age_days (default 7: MaxMind publishes weekly);
//   - the settings page can force an update at any time (Ensure force=true);
//   - every outcome lands in settings (geoip.status), so the panel can show the
//     live state, where the data came from and why the last attempt failed.
//
// Sources are keyless mirrors of the official MaxMind data, tried in order; an
// operator can pin a single URL (geoip.url, or FOBE_GEOIP_URL) for an internal
// mirror. No license key is involved, by design — the panel must work out of
// the box.
//
// Everything is optional-safe: a nil Settings, an empty Path or a nil log
// degrade to a status report, never to a panic.
package geoipupdate

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fobe-panel/fobe/internal/server/geoip"
)

// Settings keys. geoip.status is server-owned state (written here, never
// accepted through PUT /api/settings); the other three are operator settings
// the panel writes.
const (
	// SettingStatus holds the JSON-encoded Status below.
	SettingStatus = "geoip.status"
	// SettingAutoUpdate is the panel's on/off switch ("1"/"0"). Absent means on.
	SettingAutoUpdate = "geoip.auto_update"
	// SettingMaxAgeDays is how old the database may get before the daily check
	// refreshes it.
	SettingMaxAgeDays = "geoip.max_age_days"
	// SettingSourceURL pins one download URL, replacing the mirror chain.
	SettingSourceURL = "geoip.url"
)

// DefaultSourceURLs are keyless mirrors of MaxMind's GeoLite2-Country data,
// tried in order. They are automatic builds of the official database, so the
// codes are MaxMind's; only the delivery differs. Three independent hosts keep
// one blocked mirror from breaking automatic updates.
var DefaultSourceURLs = []string{
	// Branch "download" of the repository: the file is rebuilt weekly.
	"https://raw.githubusercontent.com/P3TERX/GeoLite.mmdb/download/GeoLite2-Country.mmdb",
	// The same file over a CDN, for networks where raw.githubusercontent.com
	// is unreliable.
	"https://cdn.jsdelivr.net/gh/P3TERX/GeoLite.mmdb@download/GeoLite2-Country.mmdb",
	// A different builder (adds CN/private ranges) as the last resort.
	"https://github.com/Loyalsoldier/geoip/releases/latest/download/Country.mmdb",
}

// Defaults for the freshness policy.
const (
	// DefaultMaxAgeDays is the refresh threshold: MaxMind rebuilds weekly, so
	// a 7-day threshold follows every release within a day.
	DefaultMaxAgeDays = 7
	// DefaultCheckEvery is how often the freshness policy is evaluated. The
	// check itself is a local stat; only an outdated file causes traffic.
	DefaultCheckEvery = 24 * time.Hour
	// defaultHTTPTimeout caps a whole mirror download (~9MB).
	defaultHTTPTimeout = 5 * time.Minute
	// userAgent identifies the panel to the mirrors.
	userAgent = "fobe-server"
)

// States reported in Status.State.
const (
	// StateOK means a database is on disk (and, when auto update is on, fresh).
	StateOK = "ok"
	// StateDisabled means automatic updates are off.
	StateDisabled = "disabled"
	// StatePending means an update is in flight.
	StatePending = "pending"
	// StateFailed means the last attempt failed; Status.Error carries the reason.
	StateFailed = "failed"
)

// Reasons recorded alongside StateDisabled.
const (
	reasonNoPath  = "FOBE_GEOIP_MMDB is not configured"
	reasonAutoOff = "geoip.auto_update is off"
	reasonEnvOff  = "FOBE_GEOIP_AUTO_UPDATE=0"
)

// Settings is the subset of *store.Store the manager needs. A nil Settings
// disables persistence; the manager still tracks state in memory.
type Settings interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string, encrypted bool) error
}

// Config configures a Manager. Only Path is required.
type Config struct {
	// Path is the MMDB destination (FOBE_GEOIP_MMDB).
	Path string
	// Settings receives geoip.status and supplies the operator settings.
	Settings Settings
	// Log receives the WARN/INFO notes; nil is fine.
	Log *slog.Logger
	// HTTPClient overrides the default client (tests point it at httptest).
	HTTPClient *http.Client
	// Sources is the mirror chain; DefaultSourceURLs when empty.
	Sources []string
	// SourceURL pins one URL and wins over SettingSourceURL (FOBE_GEOIP_URL).
	SourceURL string
	// AutoUpdate is the FOBE_GEOIP_AUTO_UPDATE switch: false stops every
	// automatic download, including the startup one. The panel's own switch is
	// SettingAutoUpdate and cannot override it.
	AutoUpdate bool
	// MaxAgeDays is the refresh threshold used when the setting is absent.
	MaxAgeDays int
	// CheckEvery is the freshness-check period (DefaultCheckEvery when zero).
	CheckEvery time.Duration
	// MaxBytes caps a download (geoip.MaxMMDBBytes when zero).
	MaxBytes int64
	// Reload forces the live resolver to re-open the database right after an
	// install, so a new file serves lookups immediately instead of at the next
	// stat-driven check. Optional.
	Reload func() bool
	// Progress observes an update in flight. It is called from the download
	// loop, so it must stay cheap and must never block; nil disables reporting.
	Progress func(Download)
	// Now overrides time.Now (tests).
	Now func() time.Time
}

// Manager owns the database-refresh policy. It is safe for concurrent use; at
// most one download runs at a time.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	status  Status
	cur     Download
	running bool
	// rate sampling for Download.Speed: the previous observed byte count and
	// when it was observed.
	lastBytesAt time.Time
	lastBytes   int64
}

// New returns a Manager with defaults applied. It never performs I/O.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = DefaultSourceURLs
	}
	if cfg.MaxAgeDays <= 0 {
		cfg.MaxAgeDays = DefaultMaxAgeDays
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = DefaultCheckEvery
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = geoip.MaxMMDBBytes
	}
	return &Manager{cfg: cfg}
}

// Status returns the last known status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Download returns the live snapshot of the update in flight (or the last one
// that ran). The zero value means nothing ever ran.
func (m *Manager) Download() Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// Running reports whether an update is in flight.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Start applies the startup policy and returns immediately.
//
// The decisions are local (stat + settings), so they run inline; only the
// download goes to a background goroutine. It also arms the daily freshness
// check, which re-reads the settings every tick so flipping the panel switch
// takes effect without a restart. Cancelling ctx stops both.
func (m *Manager) Start(ctx context.Context) {
	if st, ok := LoadStatus(m.cfg.Settings); ok {
		m.mu.Lock()
		m.status = st
		m.mu.Unlock()
	}

	switch {
	case m.cfg.Path == "":
		// No destination: nothing to check on a schedule either.
		m.setStatus(Status{State: StateDisabled, Error: reasonNoPath})
		return
	case !m.autoEnabled():
		// A database may still be on disk from an earlier run or an upload:
		// report it, but never touch the network.
		reason := reasonAutoOff
		if m.EnvLocked() {
			reason = reasonEnvOff
		}
		if _, err := os.Stat(m.cfg.Path); err == nil {
			m.setStatus(Status{State: StateOK})
		} else {
			m.setStatus(Status{State: StateDisabled, Error: reason})
		}
	case m.stale():
		m.setStatus(m.baseStatus(StatePending))
		go m.ensureLogged(ctx, false)
		m.logInfo("geoipupdate: refreshing the database in the background")
	default:
		m.setStatus(m.baseStatus(StateOK))
	}

	go m.loop(ctx)
}

// loop evaluates the freshness policy on a ticker.
func (m *Manager) loop(ctx context.Context) {
	t := time.NewTicker(m.cfg.CheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}

// Check applies the freshness policy once, honouring both switches. The
// settings page calls it after an operator changes a threshold, so a shorter
// interval does not have to wait for the next daily tick. A fresh file costs
// one stat, and a disabled policy costs one settings read.
func (m *Manager) Check(ctx context.Context) {
	if m.cfg.Path == "" || !m.autoEnabled() {
		return
	}
	if !m.stale() {
		m.setStatus(m.baseStatus(StateOK))
		return
	}
	m.ensureLogged(ctx, false)
}

// ensureLogged runs Ensure and logs the outcome, for work nothing waits on.
func (m *Manager) ensureLogged(ctx context.Context, force bool) {
	st := m.Ensure(ctx, force)
	if st.State == StateFailed {
		m.logWarn("geoipupdate: update failed; the panel shows the reason and a manual retry stays possible",
			"err", st.Error)
	}
}

// Ensure makes sure the database is present and fresh, downloading it when it
// is missing or older than the threshold. force skips the freshness check: that
// is the panel's manual-update path, and it is also what the startup check uses
// when nothing is installed yet. It blocks until the attempt is over and
// returns the resulting status; a concurrent call joins the running attempt
// instead of stacking a second download.
func (m *Manager) Ensure(ctx context.Context, force bool) Status {
	if m.cfg.Path == "" {
		return m.setStatus(Status{State: StateDisabled, Error: reasonNoPath})
	}

	m.mu.Lock()
	if m.running {
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

	if !force {
		if info, err := os.Stat(m.cfg.Path); err == nil && !m.tooOld(info) {
			return m.setStatus(m.baseStatus(StateOK))
		}
	}
	return m.download(ctx)
}

// stale reports whether the database is missing or older than the threshold.
func (m *Manager) stale() bool {
	info, err := os.Stat(m.cfg.Path)
	if err != nil {
		return true // missing or unreadable: fetch
	}
	return m.tooOld(info)
}

// tooOld applies the age threshold, re-reading the setting every time so a
// changed interval takes effect on the next check.
func (m *Manager) tooOld(info os.FileInfo) bool {
	days := m.maxAgeDays()
	if days <= 0 {
		return false // 0 disables the schedule; only a missing file triggers a fetch
	}
	return m.cfg.Now().Sub(info.ModTime()) >= time.Duration(days)*24*time.Hour
}

// maxAgeDays reads geoip.max_age_days, clamped to something sane.
func (m *Manager) maxAgeDays() int {
	if raw, ok := m.setting(SettingMaxAgeDays); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n >= 0 {
			if n > 365 {
				return 365
			}
			return n
		}
	}
	return m.cfg.MaxAgeDays
}

// autoEnabled resolves the two switches: the env var is the operator's hard
// kill switch, the setting is the panel's.
func (m *Manager) autoEnabled() bool {
	if !m.cfg.AutoUpdate {
		return false
	}
	raw, ok := m.setting(SettingAutoUpdate)
	if !ok {
		return true // absent means on (§14.1 default)
	}
	return !isFalsey(raw)
}

// EnvLocked reports whether the env switch forbids automatic updates, so the
// panel can explain why its own switch does nothing.
func (m *Manager) EnvLocked() bool { return !m.cfg.AutoUpdate }

// Policy is the live refresh policy. The panel reads it rather than the copies
// stamped into Status, so a switch the operator just flipped shows up before
// the next check runs.
type Policy struct {
	// AutoUpdate is the effective switch (env and panel combined).
	AutoUpdate bool
	// EnvLocked is true when FOBE_GEOIP_AUTO_UPDATE=0 overrides the panel.
	EnvLocked bool
	// MaxAgeDays is the effective refresh threshold (0 = no schedule).
	MaxAgeDays int
}

// Policy returns the current switch state and threshold.
func (m *Manager) Policy() Policy {
	return Policy{AutoUpdate: m.autoEnabled(), EnvLocked: m.EnvLocked(), MaxAgeDays: m.maxAgeDays()}
}

// SourceUpload names a database that arrived through the panel's upload button
// rather than a mirror. The panel translates it instead of rendering a URL.
const SourceUpload = "upload"

// NoteInstalled records an install that did not come from a mirror — today
// that is the panel's upload endpoint. Without it a successful upload would
// keep reporting the previous download's failure as if nothing had changed,
// and the panel would show the wrong origin for the data it is serving.
func (m *Manager) NoteInstalled(source string) Status {
	return m.setStatus(Status{State: StateOK, Source: source, UpdatedAt: m.cfg.Now().Unix()})
}

func (m *Manager) setting(key string) (string, bool) {
	if m.cfg.Settings == nil {
		return "", false
	}
	v, err := m.cfg.Settings.GetSetting(key)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}

// isFalsey reads the panel's on/off switch. The select sends "1"/"0", but a
// hand-written value should not silently mean "on".
func isFalsey(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}

// baseStatus builds a status carrying the current switch state and thresholds.
func (m *Manager) baseStatus(state string) Status {
	return Status{
		State:      state,
		AutoUpdate: m.autoEnabled(),
		EnvLocked:  m.EnvLocked(),
		MaxAgeDays: m.maxAgeDays(),
	}
}

// setStatus records and persists a status, stamping the check time.
func (m *Manager) setStatus(s Status) Status {
	now := m.cfg.Now().Unix()
	s.AutoUpdate = m.autoEnabled()
	s.EnvLocked = m.EnvLocked()
	s.MaxAgeDays = m.maxAgeDays()
	s.CheckedAt = now
	m.mu.Lock()
	if s.Source == "" {
		s.Source = m.status.Source
	}
	if s.UpdatedAt == 0 {
		s.UpdatedAt = m.status.UpdatedAt
	}
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
		m.logWarn("geoipupdate: encode status", "err", err)
		return
	}
	if err := m.cfg.Settings.SetSetting(SettingStatus, string(raw), false); err != nil {
		m.logWarn("geoipupdate: persist status", "err", err)
	}
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

// LoadStatus reads the persisted status. ok is false when nothing was written
// yet (fresh install) or the value is unreadable.
func LoadStatus(src Settings) (Status, bool) {
	if src == nil {
		return Status{}, false
	}
	raw, err := src.GetSetting(SettingStatus)
	if err != nil || strings.TrimSpace(raw) == "" {
		return Status{}, false
	}
	var s Status
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Status{}, false
	}
	return s, true
}

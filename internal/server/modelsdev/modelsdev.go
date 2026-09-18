// Package modelsdev fetches, parses, caches and indexes the models.dev metadata
// the panel's AI provider form needs (design.md §12.5).
//
// The source is https://models.dev/api.json — 4.7 MB, 221 providers, 7843 models.
// NOT models.json: that document's 406 models carry no reasoning_options at all
// (only the boolean reasoning), so it cannot answer "which thinking levels does
// this model support" — 拿不到思考档位就没有这个包存在的理由（2026-09-18 实测）。
//
// Lifecycle follows the §14.1 GeoIP precedent:
//
//   - the cache is a file at <dir>/models/api.json; the caller passes its data
//     directory (the server uses /data). It never goes into SQLite — 库是单写者
//     （SetMaxOpenConns(1)），4.7 MB blob 会把写路径、迁移与备份一起拖下水。
//   - startup reads that cache synchronously (a local read plus one parse) and
//     only a missing or stale cache triggers a background fetch; a daily ticker
//     refreshes afterwards. 启动永远不等网络：models.dev 不可达不能让面板起不来。
//   - a failed fetch keeps the previous cache file and the previous in-memory
//     index; the error is returned and logged, and nothing else changes (§12.5).
//   - FOBE_MODELS_URL swaps the mirror, FOBE_MODELS_AUTO_UPDATE=0 turns every
//     automatic fetch off (an explicit Ensure still works).
//
// Parsing is typed and streaming: a generic map[string]any parse of the same
// document holds ~22–27 MB resident, the typed one ~3 MB（§12.5 实测；本包在本机
// 复测：解析 221 providers / 7843 models 用 ~50 ms，索引常驻 ~3.1 MiB）。Parse
// decodes one provider at a time and keeps only the fields §12.5 lists, so those
// trimmed values are the only thing that survives.
//
// Matching is exact "<slug>/<id>" only. The same model id is re-exported by
// dozens of gateways (4503 of the 7843 ids even contain a slash, e.g.
// qwen/qwen3.5-397b-a17b), so a fuzzy match would attach one gateway's context
// window to another gateway's model: 猜出来的元数据比没有更糟（§12.5）。
package modelsdev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultURL is the complete models.dev document. §12.5: api.json, never
// models.json.
const DefaultURL = "https://models.dev/api.json"

// Environment variables. The caller may also pass these on Config; the package
// reads the environment itself so a bare New(Config{Dir: …}) already behaves
// like the deployment documented in §12.5.
const (
	// EnvURL overrides DefaultURL (an internal mirror).
	EnvURL = "FOBE_MODELS_URL"
	// EnvAutoUpdate=0 (false/off/no) disables the startup top-up and the daily
	// refresh. It is the operator's hard switch, like FOBE_GEOIP_AUTO_UPDATE.
	EnvAutoUpdate = "FOBE_MODELS_AUTO_UPDATE"
)

// Cache layout: <dir>/models/api.json.
const (
	cacheSubdir   = "models"
	cacheFileName = "api.json"
)

// Defaults.
const (
	// DefaultCheckEvery is the refresh period: one round a day (§12.5).
	DefaultCheckEvery = 24 * time.Hour
	// DefaultMaxBytes caps one download. The document is 4.7 MB today; 32 MiB
	// leaves room to grow while keeping a hostile or broken response (an
	// endless stream, a proxy error page) from filling memory.
	DefaultMaxBytes = 32 << 20
	// defaultHTTPTimeout covers the whole ~4.7 MB download.
	defaultHTTPTimeout = 5 * time.Minute
	// userAgent identifies the panel to models.dev.
	userAgent = "fobe-server"
)

// Errors callers may want to inspect.
var (
	// ErrDisabled means no data directory was configured; the feature is off,
	// which is a state and not a failure.
	ErrDisabled = errors.New("modelsdev: no data directory configured")
	// ErrEmpty means the document parsed but lists no provider. A mirror that
	// answers "{}" must not displace a working cache.
	ErrEmpty = errors.New("modelsdev: document contains no providers")
	// ErrTooLarge means the download exceeded Config.MaxBytes.
	ErrTooLarge = errors.New("modelsdev: document exceeds the size limit")
)

// CachePath is where the document lives for a given data directory; "" when dir
// is empty (feature disabled). Exported because the panel and backups both need
// to name the file.
func CachePath(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, cacheSubdir, cacheFileName)
}

// Config configures a Manager. Only Dir is required.
type Config struct {
	// Dir is the data directory; the cache lands in <Dir>/models/api.json.
	// Empty disables the whole feature (Start/Ensure become no-ops/ErrDisabled).
	Dir string
	// URL overrides the download address; empty falls back to FOBE_MODELS_URL
	// and then DefaultURL.
	URL string
	// DisableAutoUpdate turns the automatic fetch off (startup top-up and daily
	// round). The zero value keeps it ON: New(Config{Dir: …}) is supposed to
	// give §12.5's documented default without the caller remembering to set a
	// bool — geoipupdate.Config.AutoUpdate's zero-means-off shape is a trap and
	// is deliberately not copied here. FOBE_MODELS_AUTO_UPDATE=0 disables too.
	DisableAutoUpdate bool
	// Log receives the INFO/WARN notes; nil is fine.
	Log *slog.Logger
	// HTTPClient overrides the default client (tests point it at httptest).
	HTTPClient *http.Client
	// CheckEvery is the refresh period; DefaultCheckEvery when zero.
	CheckEvery time.Duration
	// MaxBytes caps one download; DefaultMaxBytes when zero.
	MaxBytes int64
	// Now overrides time.Now (tests).
	Now func() time.Time
}

// Status is a snapshot for the panel/health endpoint: what is loaded, how much
// of it, and why the last refresh failed. The daily refresh runs in the
// background, so its failure has to be observable somewhere other than a log
// line (§12.5 keeps such failures non-fatal).
type Status struct {
	// Path is the cache file, "" when the feature is disabled.
	Path string
	// URL is the address the next fetch would use.
	URL string
	// AutoUpdate reports whether automatic refreshing is enabled.
	AutoUpdate bool
	// Loaded is true when an index is in memory.
	Loaded bool
	// Providers and Models count the loaded index (0 when not loaded).
	Providers int
	Models    int
	// UpdatedAt is when the loaded cache was last written (zero = never).
	UpdatedAt time.Time
	// LastError is the most recent failure; cleared by a successful refresh.
	LastError string
}

// Manager owns the cache, the index and the refresh schedule. It is safe for
// concurrent use: readers go through an atomic index snapshot (no lock on the
// request path) while the background refresh swaps the whole index at once, so
// a request never observes half an index.
type Manager struct {
	cfg  Config
	url  string
	auto bool

	// index is the current immutable snapshot. atomic.Pointer rather than an
	// RWMutex keeps Lookup lock-free: metadata lookups sit on the provider-form
	// request path and would otherwise contend with a 4.7 MB refresh.
	index atomic.Pointer[Index]

	mu        sync.Mutex // serialises refreshes (single-flight) and guards the fields below
	inflight  bool
	lastErr   string
	updatedAt time.Time
}

// New returns a Manager with defaults applied. It performs no I/O: the caller
// decides when Start (or Ensure) touches the network.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = DefaultCheckEvery
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}

	url := strings.TrimSpace(cfg.URL)
	if url == "" {
		url = strings.TrimSpace(os.Getenv(EnvURL))
	}
	if url == "" {
		url = DefaultURL
	}

	auto := !cfg.DisableAutoUpdate && !envFalsey(os.Getenv(EnvAutoUpdate))

	return &Manager{cfg: cfg, url: url, auto: auto}
}

// Start applies the startup policy and returns immediately.
//
// The cache is read inline (local file, no network); only a missing or stale
// cache goes to a background goroutine, and the daily ticker is armed right
// after. Cancelling ctx stops both. A failed fetch is logged, never surfaced:
// the panel must come up with empty metadata rather than not come up at all
// (§12.5's accepted degradation).
func (m *Manager) Start(ctx context.Context) {
	if m.cfg.Dir == "" {
		m.logInfo("modelsdev: no data directory configured; model metadata is disabled")
		return
	}

	switch err := m.LoadCache(); {
	case err == nil:
		// Ready. The staleness check below still decides on a refresh, so a
		// server restarted more often than CheckEvery (a daily restart would
		// otherwise never let the ticker fire) still converges.
	case errors.Is(err, os.ErrNotExist):
		m.logInfo("modelsdev: no cached metadata yet; fetching in the background")
	default:
		m.logWarn("modelsdev: cached metadata is unusable; refetching",
			"path", m.Path(), "err", err)
	}

	if !m.auto {
		m.logInfo("modelsdev: automatic refresh is off; serving the cached copy only",
			"path", m.Path())
		return
	}
	if m.stale() {
		go m.ensureLogged(ctx, false)
	}
	go m.loop(ctx)
}

// loop runs the daily round.
func (m *Manager) loop(ctx context.Context) {
	t := time.NewTicker(m.cfg.CheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.ensureLogged(ctx, false)
		}
	}
}

// ensureLogged runs Ensure for work nobody waits on and logs the outcome; the
// error itself is kept in Status().LastError.
func (m *Manager) ensureLogged(ctx context.Context, force bool) {
	if err := m.Ensure(ctx, force); err != nil {
		m.logWarn("modelsdev: refresh failed; the previous metadata is still served",
			"path", m.Path(), "err", err)
	}
}

// Ensure makes sure an index is available: it fetches when the cache is missing
// or older than CheckEvery, or when force is set (the panel's "refresh now").
//
// A concurrent call joins the refresh already in flight instead of stacking a
// second download, and reports nil — it is not the one doing the work. A caller
// that needs to know whether metadata is available must ask Loaded/Status, since
// the joined round's failure lands in Status().LastError.
//
// It deliberately ignores the auto-update switch: an explicit call is an
// operator action, not the schedule.
func (m *Manager) Ensure(ctx context.Context, force bool) error {
	if m.cfg.Dir == "" {
		return ErrDisabled
	}

	m.mu.Lock()
	if m.inflight {
		m.mu.Unlock()
		return nil
	}
	m.inflight = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inflight = false
		m.mu.Unlock()
	}()

	if !force && !m.stale() {
		if m.Loaded() {
			return nil
		}
		// Fresh file but nothing in memory yet (a caller that never ran Start):
		// a local read is cheaper than a download.
		if err := m.LoadCache(); err == nil {
			return nil
		}
	}
	return m.fetchAndInstall(ctx)
}

// LoadCache reads and parses the cache file, swapping in the resulting index.
// A missing file returns an error wrapping os.ErrNotExist so Start can tell
// "first run" from "corrupt cache".
func (m *Manager) LoadCache() error {
	path := m.Path()
	if path == "" {
		return ErrDisabled
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open models cache %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat models cache %s: %w", path, err)
	}

	ix, err := Parse(f)
	if err != nil {
		// A corrupt cache is a failure worth showing in Status(): the panel would
		// otherwise silently serve nothing and look like an empty mirror.
		return m.fail(fmt.Errorf("parse models cache %s: %w", path, err))
	}

	m.swap(ix)
	m.setUpdated(info.ModTime())
	m.logInfo("modelsdev: metadata cache loaded",
		"path", path, "providers", ix.ProviderCount(), "models", ix.ModelCount())
	return nil
}

// Lookup resolves one model by exact "<provider slug>/<model id>". A miss (or an
// index that is not loaded yet) returns false and the caller falls back to
// manual entry — never to a guess (§12.5).
func (m *Manager) Lookup(providerSlug, modelID string) (ModelMeta, bool) {
	return m.Index().Lookup(providerSlug, modelID)
}

// Provider returns the provider-level metadata (name, api, npm) for a slug.
func (m *Manager) Provider(slug string) (ProviderMeta, bool) {
	return m.Index().Provider(slug)
}

// Providers lists every provider in the current snapshot, sorted by slug.
func (m *Manager) Providers() []ProviderMeta {
	return m.Index().Providers()
}

// Models lists the models one slug publishes, sorted by id.
func (m *Manager) Models(providerSlug string) []ModelMeta {
	return m.Index().Models(providerSlug)
}

// Index returns the current snapshot; it may be nil when nothing is loaded yet,
// and its methods are nil-safe.
func (m *Manager) Index() *Index {
	return m.index.Load()
}

// Loaded reports whether an index is in memory.
func (m *Manager) Loaded() bool {
	return m.index.Load() != nil
}

// Path is the cache file ("" when disabled).
func (m *Manager) Path() string {
	return CachePath(m.cfg.Dir)
}

// URL is the address the next fetch would use.
func (m *Manager) URL() string {
	return m.url
}

// AutoUpdate reports whether automatic refreshing is enabled.
func (m *Manager) AutoUpdate() bool {
	return m.auto
}

// Status returns the current snapshot.
func (m *Manager) Status() Status {
	ix := m.index.Load()

	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		Path:       m.Path(),
		URL:        m.url,
		AutoUpdate: m.auto,
		UpdatedAt:  m.updatedAt,
		LastError:  m.lastErr,
	}
	if ix != nil {
		s.Loaded = true
		s.Providers = ix.ProviderCount()
		s.Models = ix.ModelCount()
	}
	return s
}

// stale reports whether the cache file is missing or older than CheckEvery.
func (m *Manager) stale() bool {
	info, err := os.Stat(m.Path())
	if err != nil {
		return true // missing or unreadable: fetch
	}
	return m.cfg.Now().Sub(info.ModTime()) >= m.cfg.CheckEvery
}

// swap installs a new index snapshot and clears the previous failure.
func (m *Manager) swap(ix *Index) {
	m.index.Store(ix)
	m.mu.Lock()
	m.lastErr = ""
	m.mu.Unlock()
}

// fail records a non-fatal failure and returns it: the index in memory and the
// file on disk both stay as they were (§12.5 "拉取失败保留上次").
func (m *Manager) fail(err error) error {
	m.mu.Lock()
	m.lastErr = err.Error()
	m.mu.Unlock()
	return err
}

func (m *Manager) setUpdated(t time.Time) {
	m.mu.Lock()
	m.updatedAt = t
	m.mu.Unlock()
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

// envFalsey reads an on/off environment switch. A hand-written value must not
// silently mean "on".
func envFalsey(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}

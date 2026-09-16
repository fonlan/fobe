// Package httpapi wires the panel HTTP surface (design.md §2/§16): JSON API
// under /api, agent WebSocket under /ws/agent, browser terminal and events,
// install script, /dl artifacts, and the built frontend (or API-only mode).
package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/fobe-panel/fobe/internal/server/agentupdate"
	"github.com/fobe-panel/fobe/internal/server/feishureg"
	"github.com/fobe-panel/fobe/internal/server/geoip"
	"github.com/fobe-panel/fobe/internal/server/geoipupdate"
	"github.com/fobe-panel/fobe/internal/server/hub"
	"github.com/fobe-panel/fobe/internal/server/notify"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/singboxcache"
	"github.com/fobe-panel/fobe/internal/server/singboxdl"
	"github.com/fobe-panel/fobe/internal/server/singboxupdate"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// Server carries the shared dependencies of every handler.
type Server struct {
	Store           *store.Store
	Hub             *hub.Hub
	Trust           *security.TrustChain
	Crypt           *security.Cryptor
	Log             *slog.Logger
	WebDir          string // "" → API-only mode (dev without built frontend, §16)
	DLDir           string
	Version         string
	InstallTmplPath string // optional override of scripts/install.sh.tmpl
	AIHTTPClient    *http.Client

	// sing-box release cache (§9.2). Empty values fall back to the upstream
	// defaults (api.github.com / github.com) inside singboxdl.
	SingboxAPIBase      string
	SingboxDownloadBase string

	// SingboxAutoDownload mirrors FOBE_SINGBOX_AUTO_DOWNLOAD for the cache view
	// (a persisted cache status wins when it carries its own copy).
	SingboxAutoDownload bool

	// SingboxUpdate is the §9.2 one-click batch updater. It is built lazily
	// from the fields above when nil, so tests can substitute their own and the
	// API-only dev mode still works.
	SingboxUpdate *singboxupdate.Manager
	sbUpdOnce     sync.Once

	// SingboxCache is the §9.2 startup artifact downloader. The settings page
	// uses it to retry a failed download (design §9.2 手动重试); optional.
	SingboxCache *singboxcache.Manager

	// Background is the long-lived context for work a handler starts but whose
	// response does not wait for it (the cache retry). nil = context.Background().
	Background context.Context

	// GeoIP (§14): where MMDB uploads are written and the live resolver to
	// reload afterwards. An empty path disables the upload endpoint.
	GeoIPMMDBPath string
	GeoIPResolver geoip.Resolver
	// GeoIPUpdater is the §14.1 database updater (mirror download + freshness
	// policy). The settings page reads its status and drives manual updates;
	// optional — without it the section reports "not wired" instead of failing.
	GeoIPUpdater *geoipupdate.Manager

	// AgentUpdate is the §5.5 self-update manager. Optional: dev builds and
	// tests can run without one (no target is offered then).
	AgentUpdate *agentupdate.Manager

	// 飞书 notification surface (§15, 2026-09-16 修订). Feishu is the delivery
	// channel (shared with the scheduler); FeishuReg owns the scan-to-add
	// registration session. Both optional: the section degrades gracefully.
	Feishu    *notify.Feishu
	FeishuReg *feishureg.Manager
	// Channels is the same notifier set the scheduler delivers with, used by
	// the panel's test button (POST /api/settings/notify/test).
	Channels []notify.Notifier

	// events broker for /ws/events (live panel updates)
	evMu   sync.Mutex
	evSubs map[chan []byte]struct{}
	evOnce sync.Once

	// sbDL is the process-wide artifact cache client (§9.5.3). One client per
	// process is load-bearing, not an optimization: the startup auto-download
	// and an "update sing-box" job must serialize on the same install mutex,
	// otherwise both fetch the same tarball (see SingboxDL).
	sbDLOnce sync.Once
	sbDL     *singboxdl.Client
	// sbProg is the in-flight download snapshot pushed over /ws/events and
	// read by GET /api/singbox/cache.
	sbProg singboxProgress
	// sbRel memoizes the upstream release listing behind the version picker
	// (GET /api/singbox/releases). Memory-only: a listing is a convenience,
	// never a source of truth — the versions on disk are (§9.2).
	sbRel singboxReleasesCache
}

func NewServer(st *store.Store, h *hub.Hub, trust *security.TrustChain, crypt *security.Cryptor, log *slog.Logger) *Server {
	return &Server{
		Store: st, Hub: h, Trust: trust, Crypt: crypt, Log: log,
		evSubs: map[chan []byte]struct{}{},
	}
}

// Handler builds the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// health (no auth)
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// auth
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.requireSession(s.handleMe))
	mux.HandleFunc("POST /api/password", s.requireSession(s.handleChangePassword))

	// nodes
	mux.HandleFunc("GET /api/nodes", s.requireSession(s.handleListNodes))
	mux.HandleFunc("GET /api/nodes/{id}", s.requireSession(s.handleGetNode))
	mux.HandleFunc("PATCH /api/nodes/{id}", s.requireSession(s.handleUpdateNode))
	mux.HandleFunc("DELETE /api/nodes/{id}", s.requireSession(s.handleDeleteNode))
	mux.HandleFunc("GET /api/nodes/{id}/metrics", s.requireSession(s.handleNodeMetrics))
	mux.HandleFunc("GET /api/nodes/{id}/traffic", s.requireSession(s.handleNodeTraffic))
	mux.HandleFunc("GET /api/nodes/{id}/latency", s.requireSession(s.handleNodeLatency))
	mux.HandleFunc("GET /api/nodes/{id}/commands", s.requireSession(s.handleListCommands))
	// §5.5 agent self-update: cluster status, operator retry, reinstall command.
	mux.HandleFunc("GET /api/agent/update", s.requireSession(s.handleAgentUpdateStatus))
	mux.HandleFunc("POST /api/nodes/{id}/agent/retry", s.requireSession(s.handleAgentUpdateRetry))
	mux.HandleFunc("POST /api/nodes/{id}/agent/reinstall-command", s.requireSession(s.handleAgentReinstallCommand))

	// registration tokens (add-node flow, §4.2)
	mux.HandleFunc("GET /api/reg-tokens", s.requireSession(s.handleListRegTokens))
	mux.HandleFunc("POST /api/reg-tokens", s.requireSession(s.handleCreateRegToken))

	// latency targets (§13)
	mux.HandleFunc("GET /api/latency-targets", s.requireSession(s.handleListLatencyTargets))
	mux.HandleFunc("POST /api/latency-targets", s.requireSession(s.handleCreateLatencyTarget))
	mux.HandleFunc("DELETE /api/latency-targets/{id}", s.requireSession(s.handleDeleteLatencyTarget))

	// ops surfaces
	mux.HandleFunc("GET /api/blacklist", s.requireSession(s.handleListBlacklist))
	mux.HandleFunc("DELETE /api/blacklist/{ip}", s.requireSession(s.handleUnblockIP))
	mux.HandleFunc("GET /api/sessions", s.requireSession(s.handleListSessions))
	mux.HandleFunc("POST /api/sessions/revoke-all", s.requireSession(s.handleRevokeAllSessions))
	mux.HandleFunc("GET /api/audit", s.requireSession(s.handleListAudit))
	mux.HandleFunc("GET /api/alerts", s.requireSession(s.handleListAlerts))
	mux.HandleFunc("GET /api/settings", s.requireSession(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.requireSession(s.handlePutSettings))
	// §15 飞书 (2026-09-16): scan-to-add registration session, channel test,
	// unbind. The QR session is per-process state; no restart can resume it.
	mux.HandleFunc("POST /api/settings/feishu/qr", s.requireSession(s.handleFeishuQRStart))
	mux.HandleFunc("GET /api/settings/feishu/qr", s.requireSession(s.handleFeishuQRStatus))
	mux.HandleFunc("DELETE /api/settings/feishu/qr", s.requireSession(s.handleFeishuQRCancel))
	// §15 one test button per channel (body: {channel}), see handleNotifyTest.
	mux.HandleFunc("POST /api/settings/notify/test", s.requireSession(s.handleNotifyTest))
	mux.HandleFunc("DELETE /api/settings/feishu/config", s.requireSession(s.handleFeishuClear))
	mux.HandleFunc("POST /api/ai/chat", s.requireSession(s.handleAIChat))
	mux.HandleFunc("POST /api/ai/actions/{id}/confirm", s.requireSession(s.handleConfirmAIAction))

	// geoip MMDB upload/status/update (§14) + panel export / import (§17)
	mux.HandleFunc("POST /api/geoip/mmdb", s.requireSession(s.handleUploadMMDB))
	mux.HandleFunc("GET /api/geoip/status", s.requireSession(s.handleGeoIPStatus))
	mux.HandleFunc("POST /api/geoip/update", s.requireSession(s.handleGeoIPUpdate))
	mux.HandleFunc("GET /api/export", s.requireSession(s.handleExport))
	mux.HandleFunc("POST /api/import", s.requireSession(s.handleImport))

	// sing-box lifecycle (§9): desired-state management + release manifest
	mux.HandleFunc("GET /api/singbox/versions", s.requireSession(s.handleSingboxVersions))
	// §9.5 upstream listing + explicit per-version download (the settings page
	// picker). Both are operator-triggered: no page render calls upstream.
	mux.HandleFunc("GET /api/singbox/releases", s.requireSession(s.handleSingboxReleases))
	mux.HandleFunc("POST /api/singbox/versions/{version}/download", s.requireSession(s.handleSingboxVersionDownload))
	// §9.2 server-side artifact cache + §9.2 one-click batch update
	mux.HandleFunc("GET /api/singbox/cache", s.requireSession(s.handleSingboxCache))
	mux.HandleFunc("POST /api/singbox/cache/retry", s.requireSession(s.handleSingboxCacheRetry))
	mux.HandleFunc("GET /api/singbox/update/impact", s.requireSession(s.handleSingboxUpdateImpact))
	mux.HandleFunc("POST /api/singbox/update", s.requireSession(s.handleSingboxUpdate))
	mux.HandleFunc("GET /api/singbox/update/{job}", s.requireSession(s.handleSingboxUpdateStatus))
	mux.HandleFunc("DELETE /api/singbox/versions/{version}", s.requireSession(s.handleSingboxDeleteVersion))
	mux.HandleFunc("GET /api/nodes/{id}/singbox", s.requireSession(s.handleGetNodeSingbox))
	mux.HandleFunc("POST /api/nodes/{id}/singbox/install", s.requireSession(s.handleSingboxInstall))
	// literal beats the {action} wildcard below (Go 1.22 precedence); the
	// uninstall is not one of the start/stop/restart commands on purpose —
	// it is a desired-state write (§9.2 实现修订 2026-09-16).
	mux.HandleFunc("POST /api/nodes/{id}/singbox/uninstall", s.requireSession(s.handleSingboxUninstall))
	mux.HandleFunc("POST /api/nodes/{id}/singbox/{action}", s.requireSession(s.handleSingboxAction))
	mux.HandleFunc("PUT /api/nodes/{id}/singbox/port", s.requireSession(s.handleSingboxPort))
	// §14 manual primary IP + §16 detail-page 5s probe stream
	mux.HandleFunc("PUT /api/nodes/{id}/primary-ip", s.requireSession(s.handleSetPrimaryIP))
	mux.HandleFunc("POST /api/nodes/{id}/probe", s.requireSession(s.handleNodeProbe))

	// subscriptions & templates (§10)
	mux.HandleFunc("GET /api/subscriptions", s.requireSession(s.handleListSubscriptions))
	mux.HandleFunc("POST /api/subscriptions", s.requireSession(s.handleCreateSubscription))
	mux.HandleFunc("PUT /api/subscriptions/{id}", s.requireSession(s.handleUpdateSubscription))
	mux.HandleFunc("DELETE /api/subscriptions/{id}", s.requireSession(s.handleDeleteSubscription))
	mux.HandleFunc("PUT /api/subscriptions/{id}/nodes", s.requireSession(s.handleSetSubscriptionNodes))
	mux.HandleFunc("POST /api/subscriptions/{id}/rotate", s.requireSession(s.handleRotateSubscription))
	mux.HandleFunc("GET /api/subscriptions/{id}/access", s.requireSession(s.handleSubscriptionAccess))
	// §10 实现修订 2026-09-16: copy an existing subscription's URL at any time.
	mux.HandleFunc("GET /api/subscriptions/{id}/link", s.requireSession(s.handleSubscriptionLink))
	mux.HandleFunc("GET /api/templates", s.requireSession(s.handleListTemplates))
	mux.HandleFunc("POST /api/templates", s.requireSession(s.handleCreateTemplate))
	mux.HandleFunc("PUT /api/templates/{id}", s.requireSession(s.handleUpdateTemplate))
	mux.HandleFunc("DELETE /api/templates/{id}", s.requireSession(s.handleDeleteTemplate))

	// realtime
	mux.HandleFunc("GET /ws/events", s.requireSession(s.handleEvents))
	mux.HandleFunc("GET /ws/terminal", s.requireSession(s.handleTerminal))
	mux.HandleFunc("GET /ws/agent", s.handleAgentWS)

	// agent registration (token-authenticated, no session)
	mux.HandleFunc("POST /api/agent/register", s.handleAgentRegister)

	// install script + artifacts
	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.Handle("GET /dl/", s.dlHandler())

	// subscriptions (/sub/<token>, rendered per §10)
	mux.HandleFunc("GET /sub/{token}", s.handleSubscription)

	// frontend last: SPA fallback / API-only hint
	mux.HandleFunc("/", s.handleStatic)

	return logRequests(s.Log, mux)
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// skip the noisy agent heartbeat path
		if r.URL.Path != "/ws/agent" {
			log.Debug("http", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		}
		rec := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if rec.status >= 500 {
			log.Error("http 5xx", "method", r.Method, "path", r.URL.Path, "status", rec.status)
		}
	})
}

// statusWriter records the response status for access logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush/Hijack forward so SSE and WebSocket upgrades keep working through
// the logging wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// --- small helpers ---

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message,omitempty"`
	} `json:"error"`
}

func writeErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var b errBody
	b.Error.Code = code
	_ = json.NewEncoder(w).Encode(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Panel endpoints poll continuously; a cached 200 would freeze the UI on
	// stale numbers without any visible error. Never let a browser or an
	// intermediate proxy serve these from cache.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// requireSession guards panel endpoints with the session cookie.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := security.SessionIDFromRequest(r)
		if sid == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		sess, err := s.Store.GetSession(sid)
		if err != nil || sess.Revoked {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		s.Store.TouchSession(sid, nowUnix())
		next(w, r)
	}
}

func nowUnix() int64 { return timeNow().Unix() }

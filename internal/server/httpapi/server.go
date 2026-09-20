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
	"strings"
	"sync"

	"github.com/fonlan/fobe/internal/server/agentupdate"
	"github.com/fonlan/fobe/internal/server/feishureg"
	"github.com/fonlan/fobe/internal/server/geoip"
	"github.com/fonlan/fobe/internal/server/geoipupdate"
	"github.com/fonlan/fobe/internal/server/hub"
	"github.com/fonlan/fobe/internal/server/modelsdev"
	"github.com/fonlan/fobe/internal/server/notify"
	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/singboxcache"
	"github.com/fonlan/fobe/internal/server/singboxdl"
	"github.com/fonlan/fobe/internal/server/singboxupdate"
	"github.com/fonlan/fobe/internal/server/store"
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

	// loginGate bounds unauthenticated password verification (§4.3 实现修订
	// 2026-09-20): an identity-independent CPU + memory brake in front of the
	// 64 MiB-per-attempt argon2id check.
	loginGate loginGate
	// wsReg tracks live browser sockets so revocation can close them (§4.1
	// 实现修订 2026-09-20) and terminal sockets can be capped per node.
	wsReg wsRegistry

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

	// ModelsDev is the §12.5 models.dev metadata cache (typed api.json index).
	// Optional: without it the AI page reports "not wired" and matching is
	// unavailable, while hand-entered models keep working.
	ModelsDev *modelsdev.Manager

	// 飞书 notification surface (§15, 2026-09-16 修订). Feishu is the delivery
	// channel (shared with the scheduler); FeishuReg owns the scan-to-add
	// registration session. Both optional: the section degrades gracefully.
	Feishu    *notify.Feishu
	FeishuReg *feishureg.Manager
	// Channels is the same notifier set the scheduler delivers with, used by
	// the panel's test button (POST /api/settings/notify/test).
	Channels []notify.Notifier
	// UnreadableSecrets names stored-but-undecryptable settings (a wrong master
	// key), so the panel can say why a channel is dead instead of reporting it
	// as "not configured" (§15 实现修订 2026-09-20). Optional.
	UnreadableSecrets func() []string

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
	s := &Server{
		Store: st, Hub: h, Trust: trust, Crypt: crypt, Log: log,
		evSubs: map[chan []byte]struct{}{},
	}
	// §10.2 relay entries are derived from the probes' nftables rules, so a
	// forwards snapshot is the moment they can change — wire the reconciler
	// here, where both halves already exist.
	if h != nil {
		h.SetForwardsObserver(func() {
			if n := s.ReconcileSubscriptionEntries(); n > 0 {
				s.Log.Info("auto-enrolled relay subscription entries", "count", n)
			}
		})
	}
	return s
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
	mux.HandleFunc("PATCH /api/latency-targets/{id}", s.requireSession(s.handleUpdateLatencyTarget))
	mux.HandleFunc("DELETE /api/latency-targets/{id}", s.requireSession(s.handleDeleteLatencyTarget))

	// quick commands (terminal side panel snippets, 2026-09-19)
	mux.HandleFunc("GET /api/quick-commands", s.requireSession(s.handleListQuickCommands))
	mux.HandleFunc("POST /api/quick-commands", s.requireSession(s.handleCreateQuickCommand))
	mux.HandleFunc("PUT /api/quick-commands/order", s.requireSession(s.handleReorderQuickCommands))
	mux.HandleFunc("PUT /api/quick-commands/{id}", s.requireSession(s.handleUpdateQuickCommand))
	mux.HandleFunc("DELETE /api/quick-commands/{id}", s.requireSession(s.handleDeleteQuickCommand))

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
	// §12.6: a confirmation pauses the turn and the stream ENDS; this is how the
	// operator's decision resumes it without holding one connection open across
	// however long they take to decide.
	mux.HandleFunc("POST /api/ai/chat/continue", s.requireSession(s.handleAIChatContinue))
	// §12.6: the persisted transcript, so a reload can replay the turn
	// (including its collapsed thinking) instead of showing an empty panel.
	mux.HandleFunc("GET /api/ai/sessions/{id}", s.requireSession(s.handleGetAISession))
	// §12.1/§12.5 (2026-09-18): multi-provider + model catalog. The provider
	// CRUD is panel-only — §12.3 keeps AI's own configuration a meta operation
	// the model can never reach (§12.2 has no write tool for it).
	mux.HandleFunc("GET /api/ai/catalog", s.requireSession(s.handleGetAICatalog))
	mux.HandleFunc("POST /api/ai/providers", s.requireSession(s.handleCreateAIProvider))
	mux.HandleFunc("PATCH /api/ai/providers/{id}", s.requireSession(s.handleUpdateAIProvider))
	mux.HandleFunc("DELETE /api/ai/providers/{id}", s.requireSession(s.handleDeleteAIProvider))
	mux.HandleFunc("POST /api/ai/providers/{id}/models", s.requireSession(s.handleLinkAIProviderModels))
	mux.HandleFunc("DELETE /api/ai/providers/{id}/models/{modelID}", s.requireSession(s.handleUnlinkAIProviderModel))
	mux.HandleFunc("POST /api/ai/models", s.requireSession(s.handleSaveAIModel))
	mux.HandleFunc("DELETE /api/ai/models/{id}", s.requireSession(s.handleDeleteAIModel))
	mux.HandleFunc("PUT /api/ai/defaults", s.requireSession(s.handleSetAIDefaults))
	// §12.5 models.dev metadata: the slug list + cache status, a manual refresh,
	// and the match pass that fills model rows (freezing edited fields).
	mux.HandleFunc("GET /api/ai/modelsdev", s.requireSession(s.handleGetModelsDev))
	mux.HandleFunc("POST /api/ai/modelsdev/refresh", s.requireSession(s.handleRefreshModelsDev))
	mux.HandleFunc("POST /api/ai/models/match", s.requireSession(s.handleMatchAIModels))
	// "Add the models I selected": matches where it can, creates hand-entered
	// rows where it cannot, and links — one call, so a selection is never
	// half-applied.
	mux.HandleFunc("POST /api/ai/providers/{id}/import-models", s.requireSession(s.handleImportAIModels))
	// §12.5: ask the upstream which model ids it serves (GET {base}/models, with
	// Anthropic's cursor pagination handled inside the adapter).
	mux.HandleFunc("POST /api/ai/providers/{id}/fetch-models", s.requireSession(s.handleFetchAIModels))

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
	// §9.3: there is no adoption endpoint. The probe's file is the source of
	// truth; every reported inbound renders from that file with its own
	// credential, and none is a hidden primary entry.
	mux.HandleFunc("POST /api/nodes/{id}/singbox/{action}", s.requireSession(s.handleSingboxAction))
	// §9.3 实现修订 2026-09-17b (editor model): the probe's config.json is the
	// source of truth, so the panel reads it and edits it in place rather than
	// declaring a desired config of its own. Literal paths, so they keep winning
	// over the {action} wildcard above.
	mux.HandleFunc("GET /api/nodes/{id}/singbox/config", s.requireSession(s.handleGetNodeSingboxConfig))
	mux.HandleFunc("PUT /api/nodes/{id}/singbox/config", s.requireSession(s.handlePutNodeSingboxConfig))
	mux.HandleFunc("POST /api/nodes/{id}/singbox/refresh", s.requireSession(s.handleNodeSingboxRefresh))
	// §14 manual primary IP + §16 detail-page 5s probe stream
	mux.HandleFunc("PUT /api/nodes/{id}/primary-ip", s.requireSession(s.handleSetPrimaryIP))
	mux.HandleFunc("POST /api/nodes/{id}/probe", s.requireSession(s.handleNodeProbe))
	// §21 nftables port forwarding (nfpf.sh-compatible). GET reads the stored
	// snapshot; ?live=1 asks the probe. Mutations are one-shot commands because
	// the ruleset is shared with external tools.
	mux.HandleFunc("GET /api/nodes/{id}/forwards", s.requireSession(s.handleListNodeForwards))
	mux.HandleFunc("POST /api/nodes/{id}/forwards", s.requireSession(s.handleAddNodeForward))
	mux.HandleFunc("PUT /api/nodes/{id}/forwards", s.requireSession(s.handleUpdateNodeForward))
	mux.HandleFunc("DELETE /api/nodes/{id}/forwards", s.requireSession(s.handleDeleteNodeForward))

	// subscriptions & templates (§10)
	mux.HandleFunc("GET /api/subscriptions", s.requireSession(s.handleListSubscriptions))
	mux.HandleFunc("POST /api/subscriptions", s.requireSession(s.handleCreateSubscription))
	mux.HandleFunc("PUT /api/subscriptions/{id}", s.requireSession(s.handleUpdateSubscription))
	mux.HandleFunc("DELETE /api/subscriptions/{id}", s.requireSession(s.handleDeleteSubscription))
	mux.HandleFunc("PUT /api/subscriptions/{id}/nodes", s.requireSession(s.handleSetSubscriptionNodes))
	// §10.2 entry picker: candidates (including relay entries derived from the
	// fleet's nftables forwards) unioned with what is bound.
	mux.HandleFunc("GET /api/subscriptions/{id}/entries", s.requireSession(s.handleSubscriptionEntries))
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

	// Outermost first: headers and the cross-site guard are cheap; the body cap
	// must wrap the mux so every handler reads through a bounded reader.
	handler := securityHeaders(crossSiteGuard(limitRequestBody(mux)))
	return logRequests(s.Log, handler)
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
			log.Error("http 5xx", "method", r.Method, "path", logPath(r.URL.Path), "status", rec.status)
		}
	})
}

// logPath keeps credentials out of the access log. /sub/<token> IS that
// subscription's credential, and a 5xx used to write it verbatim — anyone able
// to read the log could then fetch the subscription (and the node's proxy
// credentials with it) without the panel password (§10 实现修订 2026-09-20).
func logPath(p string) string {
	if strings.HasPrefix(p, "/sub/") {
		return "/sub/[redacted]"
	}
	return p
}

// maxJSONBody / maxSnapshotBody bound request bodies (§16 实现修订 2026-09-20).
// Without a cap, the unauthenticated endpoints accepted an arbitrarily large
// JSON string and json.Decoder buffers it in full before any validation runs —
// a remote memory-exhaustion vector. Snapshot import and the GeoIP upload are
// the only legitimate multi-megabyte bodies.
const (
	maxJSONBody     = 8 << 20
	maxSnapshotBody = 64 << 20
)

func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			next.ServeHTTP(w, r)
			return
		}
		limit := int64(maxJSONBody)
		if r.URL.Path == "/api/import" || r.URL.Path == "/api/geoip/mmdb" {
			limit = maxSnapshotBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// stateChangingGet lists the GET endpoints that mutate state. SameSite=Lax keeps
// the cookie off cross-site POSTs, but a top-level navigation still carries it,
// so these two can be triggered from another site (§16 实现修订 2026-09-20).
func stateChangingGet(r *http.Request) bool {
	if r.URL.Path == "/api/export" {
		return true
	}
	return strings.HasSuffix(r.URL.Path, "/forwards") && r.URL.Query().Get("live") == "1"
}

// crossSiteGuard refuses cross-site requests that would change state. Browsers
// label every request with Sec-Fetch-Site, which page script cannot forge, so
// this needs no Origin/Host comparison — the Vite dev proxy rewrites Host
// (changeOrigin), which is exactly why an Origin check would break dev. Requests
// without the header (curl, the agent) pass: they carry no ambient cookie.
func crossSiteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") != "cross-site" {
			next.ServeHTTP(w, r)
			return
		}
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		if safe && !stateChangingGet(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "cross_site_request")
	})
}

// securityHeaders adds the defensive response headers. The CSP is deliberately
// modest: xterm injects a <style> element at runtime, so inline styles must stay
// allowed; everything else is same-origin (the bundle is self-hosted, §16
// 不变量 11). HSTS is only sent when the request arrived over TLS.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=()")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; font-src 'self' data:; connect-src 'self' ws: wss:; "+
				"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
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

// sessionIDKey carries the validated session id to handlers that need it: the
// browser WebSocket handlers register their socket under it so that revoking a
// session can actually close the connection.
type sessionIDKey struct{}

func withSessionID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sid)
}

// sessionIDFromRequest returns the session id validated by requireSession.
func sessionIDFromRequest(r *http.Request) string {
	sid, _ := r.Context().Value(sessionIDKey{}).(string)
	return sid
}

// passwordChangeExempt is what a session that still carries the one-time
// initial password may reach: exactly what the SPA needs to change it or log
// out. Everything else — including the terminal WS and the export snapshot —
// waits (§4.1 实现修订 2026-09-20).
func passwordChangeExempt(path string) bool {
	switch path {
	case "/api/me", "/api/password", "/api/logout":
		return true
	}
	return false
}

// requireSession guards panel endpoints with the session cookie. It enforces
// three things the cookie cannot: revocation, expiry, and the forced password
// change.
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
		now := nowUnix()
		if security.SessionExpired(sess.CreatedAt, now) {
			// The cookie's MaxAge is enforced by the browser alone, so a session
			// id copied out of the database or a backup used to stay valid
			// forever (§4.1 实现修订 2026-09-20). Revoke the row so the table
			// tells the same story as the answer.
			_ = s.Store.RevokeSession(sid)
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// A one-time initial password must actually be changed before the panel
		// is usable. The SPA redirect is a convenience, not the control: without
		// this, the password printed in the startup log keeps working against
		// every endpoint — the probe's root shell and the full export snapshot
		// included.
		if !passwordChangeExempt(r.URL.Path) {
			if user, uerr := s.Store.GetUser(); uerr == nil && user.MustChange {
				writeErr(w, http.StatusConflict, "password_change_required")
				return
			}
		}
		s.Store.TouchSession(sid, now)
		next(w, r.WithContext(withSessionID(r.Context(), sid)))
	}
}

func nowUnix() int64 { return timeNow().Unix() }

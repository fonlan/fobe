// Package feishureg implements the 飞书 "scan-to-add" onboarding for the
// notification bot (design §15, 2026-09-16 修订). It replicates the wire
// protocol of the official @larksuiteoapi/node-sdk registerApp(): an RFC 8628
// style device flow against accounts.feishu.cn/oauth/v1/app/registration.
//
// The panel shows a QR code of the verification URL; the user scans it with
// the 飞书 app and confirms app creation in their own tenant. Polling then
// returns a fresh app's client_id/client_secret, which the panel stores (via
// the Save callback) together with the scanning user's open id — so the bot
// can DM alerts to that user without any further setup.
package feishureg

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const regPath = "/oauth/v1/app/registration"

// Session states surfaced to the panel. qr_ready covers the whole "waiting for
// the scan" window (the protocol's authorization_pending); slow_down is folded
// in — the interval still grows, the UI does not care.
const (
	StateIdle      = "idle"
	StateQRReady   = "qr_ready"
	StateSaving    = "saving"
	StateSucceeded = "succeeded"
	StateExpired   = "expired"
	StateDenied    = "denied"
	StateCancelled = "cancelled"
	StateError     = "error"
)

// Status is the JSON view returned by the /api/settings/feishu/qr endpoints.
type Status struct {
	State            string `json:"state"`
	QRURL            string `json:"qr_url,omitempty"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds,omitempty"`
	BotName          string `json:"bot_name,omitempty"`
	Error            string `json:"error,omitempty"` // snake_case code, mapped to copy in the frontend
}

// Save persists one successfully registered app. domain is "feishu" or "lark"
// and selects the open-API host for token/send calls.
type Save func(appID, appSecret, openID, botName, domain string) error

type Config struct {
	HTTP *http.Client
	Log  *slog.Logger
	Save Save

	// Overridable bases for tests; production uses the feishu defaults.
	AccountsBase string // default https://accounts.feishu.cn
	LarkAccounts string // default https://accounts.larksuite.com
	OpenBase     string // default https://open.feishu.cn
	LarkOpen     string // default https://open.larksuite.com

	// Preset shown on the app-creation page after scanning; {user} in either
	// string is replaced by 飞书 with the scanning user's name.
	AppName string
	AppDesc string
	Source  string // analytics tag on the QR URL, e.g. "fobe"
}

// Manager owns at most one registration attempt at a time; Start supersedes
// any active one (RFC 8628 has no resume). State is memory-only: a restart
// during onboarding just means the user scans again.
type Manager struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger

	mu   sync.Mutex
	sess *session
}

type session struct {
	id        int64
	cancel    context.CancelFunc
	device    string
	qrURL     string
	expiresAt time.Time
	interval  time.Duration
	state     string
	errCode   string
	botName   string
}

func New(cfg Config) *Manager {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.AccountsBase == "" {
		cfg.AccountsBase = "https://accounts.feishu.cn"
	}
	if cfg.LarkAccounts == "" {
		cfg.LarkAccounts = "https://accounts.larksuite.com"
	}
	if cfg.OpenBase == "" {
		cfg.OpenBase = "https://open.feishu.cn"
	}
	if cfg.LarkOpen == "" {
		cfg.LarkOpen = "https://open.larksuite.com"
	}
	if cfg.Source == "" {
		cfg.Source = "fobe"
	}
	return &Manager{cfg: cfg, client: cfg.HTTP, log: cfg.Log}
}

// Start begins a fresh registration and returns the QR status. The begin POST
// runs synchronously (one round trip) so the caller can render the code at
// once; the poll loop then runs in the background until a terminal state.
func (m *Manager) Start(parent context.Context) Status {
	m.mu.Lock()
	if m.sess != nil {
		m.sess.cancel()
	}
	m.sess = nil
	m.mu.Unlock()

	res, err := m.begin()
	if err != nil {
		m.mu.Lock()
		m.sess = &session{id: nextID(), state: StateError, errCode: "registration_failed"}
		s := m.snapshotLocked()
		m.mu.Unlock()
		m.log.Warn("feishu registration begin", "err", err)
		return s
	}

	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	s := &session{
		id:        nextID(),
		cancel:    cancel,
		device:    res.deviceCode,
		qrURL:     res.qrURL,
		expiresAt: time.Now().Add(res.expiresIn),
		interval:  res.interval,
		state:     StateQRReady,
	}
	m.sess = s
	snap := m.snapshotLocked()
	m.mu.Unlock()

	go m.poll(ctx, s)
	return snap
}

// Status returns the current attempt's view, lazily expiring it.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sess; s != nil && s.state == StateQRReady && time.Now().After(s.expiresAt) {
		s.state = StateExpired
		s.errCode = "registration_expired"
	}
	return m.snapshotLocked()
}

// Cancel aborts the active attempt (no-op when idle).
func (m *Manager) Cancel() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sess; s != nil && (s.state == StateQRReady || s.state == StateSaving) {
		s.state = StateCancelled
		s.errCode = "registration_cancelled"
		if s.cancel != nil {
			s.cancel()
		}
	}
	return m.snapshotLocked()
}

var sessionCounter int64

func nextID() int64 { sessionCounter++; return sessionCounter }

// minPollInterval bounds the device-flow poll cadence from below. The API
// speaks whole seconds; tests shrink this to run the loop in milliseconds.
var minPollInterval = time.Second

func (m *Manager) snapshotLocked() Status {
	s := m.sess
	if s == nil {
		return Status{State: StateIdle}
	}
	st := Status{State: s.state, Error: s.errCode, BotName: s.botName}
	if s.state == StateQRReady || s.state == StateSaving {
		st.QRURL = s.qrURL
		st.ExpiresAt = s.expiresAt.Unix()
		st.RemainingSeconds = max(0, int64(time.Until(s.expiresAt).Seconds()))
	}
	return st
}

// --- wire protocol ---

type beginResult struct {
	qrURL      string
	deviceCode string
	expiresIn  time.Duration
	interval   time.Duration
}

type regResponse struct {
	VerificationURIComplete string `json:"verification_uri_complete"`
	DeviceCode              string `json:"device_code"`
	ExpiresIn               int    `json:"expires_in"` // seconds, default 600
	Interval                int    `json:"interval"`   // seconds, default 5
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret"`
	UserInfo                struct {
		OpenID      string `json:"open_id"`
		TenantBrand string `json:"tenant_brand"` // feishu | lark
	} `json:"user_info"`
}

// addons travels on the QR URL as gzip+base64url JSON; field names follow the
// app manifest. fobe needs exactly one permission: sending messages as the bot.
func addonsParam() string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(`{"preset":false,"scopes":{"tenant":["im:message:send_as_bot"]}}`))
	_ = gz.Close()
	return base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

func (m *Manager) begin() (beginResult, error) {
	form := url.Values{
		"action":            {"begin"},
		"archetype":         {"PersonalAgent"},
		"auth_method":       {"client_secret"},
		"request_user_info": {"open_id"},
	}
	var res regResponse
	if err := m.postForm(m.cfg.AccountsBase, form, &res); err != nil {
		return beginResult{}, err
	}
	if res.VerificationURIComplete == "" || res.DeviceCode == "" {
		return beginResult{}, fmt.Errorf("begin returned no verification url/device code")
	}

	u, err := url.Parse(res.VerificationURIComplete)
	if err != nil {
		return beginResult{}, fmt.Errorf("parse verification url: %w", err)
	}
	q := u.Query()
	// Same tags the node-sdk puts on the URL; the landing page accepts any
	// source string but the from/tp=sdk pair selects the SDK landing flow.
	q.Set("from", "sdk")
	q.Set("tp", "sdk")
	q.Set("source", "node-sdk/"+m.cfg.Source)
	q.Set("addons", addonsParam())
	if m.cfg.AppName != "" {
		q.Set("name", m.cfg.AppName)
	}
	if m.cfg.AppDesc != "" {
		q.Set("desc", m.cfg.AppDesc)
	}
	u.RawQuery = q.Encode()

	expiresIn := time.Duration(res.ExpiresIn) * time.Second
	if res.ExpiresIn <= 0 {
		expiresIn = 600 * time.Second
	}
	interval := time.Duration(res.Interval) * time.Second
	if res.Interval <= 0 {
		interval = 5 * time.Second
	}
	return beginResult{qrURL: u.String(), deviceCode: res.DeviceCode, expiresIn: expiresIn, interval: interval}, nil
}

// poll drives the device flow until a terminal state or the context dies.
// The brand switch mirrors the official SDK: the first poll response that says
// the tenant is lark moves every later call (including verification) to the
// larksuite hosts; that response itself is dropped and re-polled.
func (m *Manager) poll(ctx context.Context, s *session) {
	base := m.cfg.AccountsBase
	openBase := m.cfg.OpenBase
	brand := ""
	interval := s.interval

	for {
		var res regResponse
		err := m.postForm(base, url.Values{"action": {"poll"}, "device_code": {s.device}}, &res)

		m.mu.Lock()
		if m.sess != s {
			m.mu.Unlock() // superseded by a newer Start / cancelled
			return
		}
		if s.state != StateQRReady {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.finish(s, StateError, "registration_failed")
			m.log.Warn("feishu registration poll", "err", err)
			return
		}

		if res.UserInfo.TenantBrand == "lark" && brand == "" {
			brand = "lark"
			base = m.cfg.LarkAccounts
			openBase = m.cfg.LarkOpen
			continue
		}
		if res.ClientID != "" && res.ClientSecret != "" {
			m.accept(s, openBase, brand, res)
			return
		}
		switch res.Error {
		case "", "authorization_pending":
			// keep waiting
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			m.finish(s, StateDenied, "registration_denied")
			return
		case "expired_token":
			m.finish(s, StateExpired, "registration_expired")
			return
		default:
			m.finish(s, StateError, "registration_failed")
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(max(interval, minPollInterval)):
		}
	}
}

// accept verifies the fresh credentials against the open API (bot name only —
// the credentials themselves come straight from the platform and are saved
// regardless), hands them to Save and lands the session on succeeded.
func (m *Manager) accept(s *session, openBase, brand string, res regResponse) {
	m.mu.Lock()
	s.state = StateSaving
	m.mu.Unlock()

	botName := m.fetchBotName(openBase, res.ClientID, res.ClientSecret)

	m.mu.Lock()
	s.botName = botName
	m.mu.Unlock()

	if m.cfg.Save != nil {
		domain := brand
		if domain == "" {
			domain = "feishu"
		}
		if err := m.cfg.Save(res.ClientID, res.ClientSecret, res.UserInfo.OpenID, botName, domain); err != nil {
			m.log.Warn("feishu registration save", "err", err)
			m.finish(s, StateError, "save_failed")
			return
		}
	}
	m.finish(s, StateSucceeded, "")
}

// fetchBotName is best-effort: a failure here costs the display name only.
func (m *Manager) fetchBotName(openBase, appID, appSecret string) string {
	var token struct {
		Code              int    `json:"code"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	tokBody, _ := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	resp, err := m.client.Post(openBase+"/open-apis/auth/v3/tenant_access_token/internal", "application/json; charset=utf-8", bytes.NewReader(tokBody))
	if err != nil {
		m.log.Warn("feishu bot verify token", "err", err)
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if json.Unmarshal(raw, &token) != nil || token.Code != 0 || token.TenantAccessToken == "" {
		m.log.Warn("feishu bot verify token", "code", token.Code)
		return ""
	}

	req, _ := http.NewRequest(http.MethodGet, openBase+"/open-apis/bot/v3/info/", nil)
	req.Header.Set("Authorization", "Bearer "+token.TenantAccessToken)
	resp, err = m.client.Do(req)
	if err != nil {
		m.log.Warn("feishu bot info", "err", err)
		return ""
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	var info struct {
		Code int `json:"code"`
		Bot  struct {
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Code != 0 {
		m.log.Warn("feishu bot info", "code", info.Code)
		return ""
	}
	return strings.TrimSpace(info.Bot.AppName)
}

func (m *Manager) finish(s *session, state, errCode string) {
	m.mu.Lock()
	if m.sess == s {
		s.state = state
		s.errCode = errCode
		if s.cancel != nil {
			s.cancel()
		}
	}
	m.mu.Unlock()
}

func (m *Manager) postForm(base string, form url.Values, out *regResponse) error {
	// RFC 8628 error cases (authorization_pending & co) arrive as HTTP 400
	// with a JSON body — always decode the body, judge by its error field.
	resp, err := m.client.Post(base+regPath, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("feishu registration: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return fmt.Errorf("feishu registration: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("feishu registration: status %d: %.200s", resp.StatusCode, string(raw))
	}
	return nil
}

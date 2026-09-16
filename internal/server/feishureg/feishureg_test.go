package feishureg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// stubFeishu spins up fake accounts + open hosts and a Manager wired to them.
// The accounts host answers begin once and then serves poll replies in order;
// the open host serves tenant_access_token and bot/v3/info.
type stubFeishu struct {
	accounts *httptest.Server
	open     *httptest.Server
	mgr      *Manager

	mu        sync.Mutex
	polls     []pollReply
	pollIndex int
	saved     *savedCall
}

type pollReply struct {
	status int
	body   map[string]any
}

type savedCall struct {
	appID, appSecret, openID, botName, domain string
}

func (s *stubFeishu) beginHandler(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("action") != "begin" || r.FormValue("archetype") != "PersonalAgent" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"verification_uri_complete": "https://example.feishu.cn/suite/passport/page/confirm?token=t1",
		"device_code":               "dev-1",
		"expires_in":                600,
		"interval":                  1, // protocol minimum; keeps test waits short
	})
}

// testErr satisfies Save's error contract for the failure-path test.
type testErr struct{ msg string }

func (e testErr) Error() string { return e.msg }

func (s *stubFeishu) pollHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	idx := s.pollIndex
	if idx < len(s.polls) {
		s.pollIndex++
	}
	s.mu.Unlock()

	// Past the end of the script the platform keeps answering with its last
	// word (the brand-switch path in the manager drops one response and
	// re-polls, so the success reply must survive a re-read).
	if idx >= len(s.polls) {
		idx = len(s.polls) - 1
	}
	if idx < 0 {
		// No script at all: an empty 200 reads as "keep waiting".
		w.WriteHeader(http.StatusOK)
		return
	}
	reply := s.polls[idx]

	if reply.status != 0 && reply.status != http.StatusOK {
		w.WriteHeader(reply.status)
	}
	if reply.body != nil {
		_ = json.NewEncoder(w).Encode(reply.body)
	}
}

func (s *stubFeishu) openHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/open-apis/auth/v3/tenant_access_token/internal":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tok", "expire": 7200})
	case "/open-apis/bot/v3/info/":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "bot": map[string]any{"app_name": "fobe 探针告警"}})
	default:
		http.NotFound(w, r)
	}
}

// t.Errorf2 shim removed: stub methods carry no *testing.T.

func newStub(t *testing.T, polls []pollReply) *stubFeishu {
	t.Helper()
	oldFloor := minPollInterval
	minPollInterval = time.Millisecond
	t.Cleanup(func() { minPollInterval = oldFloor })

	s := &stubFeishu{polls: polls}
	s.accounts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.FormValue("action") {
		case "begin":
			s.beginHandler(w, r)
		default:
			s.pollHandler(w, r)
		}
	}))
	s.open = httptest.NewServer(http.HandlerFunc(s.openHandler))
	s.mgr = New(Config{
		AccountsBase: s.accounts.URL,
		LarkAccounts: s.accounts.URL,
		OpenBase:     s.open.URL,
		LarkOpen:     s.open.URL,
		AppName:      "fobe 探针告警",
		AppDesc:      "desc",
		Save: func(appID, appSecret, openID, botName, domain string) error {
			s.mu.Lock()
			s.saved = &savedCall{appID, appSecret, openID, botName, domain}
			s.mu.Unlock()
			return nil
		},
	})
	t.Cleanup(func() {
		s.accounts.Close()
		s.open.Close()
	})
	return s
}

func (s *stubFeishu) savedCallSnap() *savedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		return nil
	}
	c := *s.saved
	return &c
}

func waitState(t *testing.T, m *Manager, state string) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := m.Status(); st.State == state {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("state %s not reached, last = %+v", state, m.Status())
	return Status{}
}

func TestScanFlowSucceedsAndSaves(t *testing.T) {
	s := newStub(t, []pollReply{
		{status: 400, body: map[string]any{"error": "authorization_pending"}}, // not scanned yet
		{status: 200, body: map[string]any{
			"client_id": "cli_new", "client_secret": "sec_new",
			"user_info": map[string]any{"open_id": "ou_scanner", "tenant_brand": "feishu"},
		}},
	})

	st := s.mgr.Start(context.Background())
	if st.State != StateQRReady || st.QRURL == "" {
		t.Fatalf("Start state = %+v", st)
	}
	if st.RemainingSeconds <= 0 || st.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiry missing: %+v", st)
	}

	final := waitState(t, s.mgr, StateSucceeded)
	if final.BotName != "fobe 探针告警" {
		t.Fatalf("bot name = %q", final.BotName)
	}
	saved := s.savedCallSnap()
	if saved == nil {
		t.Fatal("Save never called")
	}
	if saved.appID != "cli_new" || saved.appSecret != "sec_new" || saved.openID != "ou_scanner" {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.domain != "feishu" {
		t.Fatalf("domain = %q", saved.domain)
	}
	if s.pollIndex < 2 {
		t.Fatalf("poll ran %d times, want >= 2", s.pollIndex)
	}
}

func TestScanFlowDenied(t *testing.T) {
	s := newStub(t, []pollReply{
		{status: 400, body: map[string]any{"error": "access_denied"}},
	})
	s.mgr.Start(context.Background())
	st := waitState(t, s.mgr, StateDenied)
	if st.Error != "registration_denied" {
		t.Fatalf("error = %q", st.Error)
	}
	if s.savedCallSnap() != nil {
		t.Fatal("Save must not run on denial")
	}
}

func TestScanFlowExpired(t *testing.T) {
	s := newStub(t, []pollReply{
		{status: 400, body: map[string]any{"error": "expired_token"}},
	})
	s.mgr.Start(context.Background())
	st := waitState(t, s.mgr, StateExpired)
	if st.Error != "registration_expired" {
		t.Fatalf("error = %q", st.Error)
	}
}

func TestScanFlowSaveFailureSurfaces(t *testing.T) {
	s := newStub(t, []pollReply{
		{status: 200, body: map[string]any{
			"client_id": "cli_new", "client_secret": "sec_new",
			"user_info": map[string]any{"open_id": "ou_scanner"},
		}},
	})
	s.mgr.cfg.Save = func(string, string, string, string, string) error { return testErr{"db down"} }
	s.mgr.Start(context.Background())
	st := waitState(t, s.mgr, StateError)
	if st.Error != "save_failed" {
		t.Fatalf("error = %q", st.Error)
	}
}

func TestCancelAndSupersede(t *testing.T) {
	s := newStub(t, []pollReply{
		{status: 400, body: map[string]any{"error": "authorization_pending"}},
	})
	s.mgr.Start(context.Background())
	if st := s.mgr.Cancel(); st.State != StateCancelled {
		t.Fatalf("Cancel state = %+v", st)
	}
	// A second Start supersedes anything left over and reaches qr_ready again.
	st := s.mgr.Start(context.Background())
	if st.State != StateQRReady {
		t.Fatalf("restart state = %+v", st)
	}
	s.mgr.Cancel()
}

func TestLarkBrandSwitchesVerificationHost(t *testing.T) {
	s := newStub(t, []pollReply{
		{status: 200, body: map[string]any{
			"client_id": "cli_lark", "client_secret": "sec_lark",
			"user_info": map[string]any{"open_id": "ou_lark", "tenant_brand": "lark"},
		}},
	})
	s.mgr.Start(context.Background())
	waitState(t, s.mgr, StateSucceeded)
	saved := s.savedCallSnap()
	if saved == nil || saved.domain != "lark" {
		t.Fatalf("saved = %+v, want domain=lark", saved)
	}
}

func TestStatusIdleAndExpiry(t *testing.T) {
	s := newStub(t, nil)
	if st := s.mgr.Status(); st.State != StateIdle {
		t.Fatalf("idle status = %+v", st)
	}
	// Lazy expiry: a session whose window passed reports expired even though
	// the poll loop is still sleeping.
	old := minPollInterval
	minPollInterval = time.Hour
	defer func() { minPollInterval = old }()
	s.mgr.Start(context.Background())
	s.mgr.mu.Lock()
	s.mgr.sess.expiresAt = time.Now().Add(-time.Second)
	s.mgr.mu.Unlock()
	if got := s.mgr.Status(); got.State != StateExpired {
		t.Fatalf("status = %+v, want expired", got)
	}
}

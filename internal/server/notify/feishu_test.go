package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFeishu builds a Feishu notifier whose open-API host is the given test
// server (production picks open.feishu.cn / open.larksuite.com by setting).
func newFeishu(settings map[string]string, srvURL string) *Feishu {
	f := NewFeishu(decoder(settings), newClient())
	f.openBase = func() string { return srvURL }
	return f
}

func TestFeishuAppModeSendsMessage(t *testing.T) {
	var tokenCalls, sendCalls atomic.Int32
	var gotAuth, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tok-1", "expire": 7200})
		case "/open-apis/im/v1/messages":
			sendCalls.Add(1)
			gotAuth = r.Header.Get("Authorization")
			gotType = r.URL.Query().Get("receive_id_type")
			body, _ := io.ReadAll(r.Body)
			gotBody = string(body)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	f := newFeishu(map[string]string{
		KeyFeishuAppID:     "cli_a",
		KeyFeishuAppSecret: "sec",
		KeyFeishuReceiveID: "oc_group", // chat id prefix → chat_id type
	}, srv.URL)
	if !f.Configured() {
		t.Fatal("Configured = false, want true")
	}
	if err := f.Deliver(Event{Kind: "node_offline", NodeName: "n1", Event: EventAlert, CreatedAt: 1700000000}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotAuth != "Bearer tok-1" || gotType != "chat_id" {
		t.Fatalf("auth=%q type=%q, want bearer token + chat_id", gotAuth, gotType)
	}
	var sent struct {
		ReceiveID string `json:"receive_id"`
		MsgType   string `json:"msg_type"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.ReceiveID != "oc_group" || sent.MsgType != "text" {
		t.Fatalf("sent = %+v", sent)
	}
	// The content field is itself JSON-encoded, so the newline inside the text
	// arrives escaped — build the expectation the same way the sender does.
	mt := MessageText(Event{Kind: "node_offline", NodeName: "n1", Event: EventAlert, CreatedAt: 1700000000}, TextOptionsFor(f.Decrypt))
	wantContent, _ := json.Marshal(map[string]string{"text": mt})
	if sent.Content != string(wantContent) {
		t.Fatalf("content = %q, want %q", sent.Content, string(wantContent))
	}

	// The second delivery rides the cached token: one token fetch total.
	if err := f.Deliver(Event{Kind: "node_offline", Event: EventRecovery, CreatedAt: 1700000001}); err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if tokenCalls.Load() != 1 || sendCalls.Load() != 2 {
		t.Fatalf("token calls=%d send calls=%d, want 1/2", tokenCalls.Load(), sendCalls.Load())
	}
}

func TestFeishuWebhookModeSignsTimestamp(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer srv.Close()

	f := newFeishu(map[string]string{
		KeyFeishuWebhookURL:    srv.URL,
		KeyFeishuWebhookSecret: "s3cret",
	}, srv.URL) // open base unused in webhook mode
	if err := f.SendText("hello"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	var sent struct {
		Timestamp string `json:"timestamp"`
		Sign      string `json:"sign"`
		MsgType   string `json:"msg_type"`
		Content   struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent.MsgType != "text" || sent.Content.Text != "hello" {
		t.Fatalf("sent = %+v", sent)
	}
	// 飞书加签: HMAC-SHA256 keyed with "<ts>\n<secret>" over an empty message.
	mac := hmac.New(sha256.New, []byte(sent.Timestamp+"\ns3cret"))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); sent.Sign != want {
		t.Fatalf("sign = %q, want %q", sent.Sign, want)
	}
}

func TestFeishuAppModeWinsOverWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tok", "expire": 7200})
		case "/open-apis/im/v1/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		}
	}))
	defer srv.Close()

	f := newFeishu(map[string]string{
		KeyFeishuAppID: "cli_a", KeyFeishuAppSecret: "sec", KeyFeishuReceiveID: "ou_x",
		KeyFeishuWebhookURL: "http://127.0.0.1:1/hook", // would fail loudly if used
	}, srv.URL)
	if err := f.SendText("hi"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
}

func TestFeishuNotConfigured(t *testing.T) {
	f := NewFeishu(decoder(map[string]string{KeyFeishuAppID: "cli_a"}), newClient())
	if f.Configured() {
		t.Fatal("Configured = true with app id only")
	}
	err := f.SendText("hi")
	if err == nil || err.Error() != ErrNotConfigured.Error() {
		t.Fatalf("SendText err = %v, want ErrNotConfigured", err)
	}
}

func TestFeishuTokenErrorRetriesFreshToken(t *testing.T) {
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			if tokenCalls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "stale", "expire": 7200})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "fresh", "expire": 7200})
		case "/open-apis/im/v1/messages":
			if r.Header.Get("Authorization") == "Bearer stale" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 99991661, "msg": "invalid access token for authorization"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		}
	}))
	defer srv.Close()

	f := newFeishu(map[string]string{
		KeyFeishuAppID: "cli_a", KeyFeishuAppSecret: "sec", KeyFeishuReceiveID: "ou_x",
	}, srv.URL)
	if err := f.SendText("hi"); err != nil {
		t.Fatalf("SendText after token invalidation: %v", err)
	}
	if tokenCalls.Load() != 2 {
		t.Fatalf("token calls = %d, want 2 (refresh + retry)", tokenCalls.Load())
	}
}

func TestFeishuWebhookUpstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 19021, "msg": "sign match fail"})
	}))
	defer srv.Close()

	f := newFeishu(map[string]string{KeyFeishuWebhookURL: srv.URL}, srv.URL)
	err := f.SendText("hi")
	if err == nil || !strings.Contains(err.Error(), "19021") || !strings.Contains(err.Error(), "sign match fail") {
		t.Fatalf("err = %v, want upstream code+msg", err)
	}
}

func TestFeishuMessageTextTestEvent(t *testing.T) {
	got := MessageText(Event{Event: EventTest, CreatedAt: time.Now().Unix()}, TextOptions{Lang: LangEnglish})
	if want := "🔔 fobe · Test notification"; !strings.HasPrefix(got, want) {
		t.Fatalf("MessageText(test) = %q", got)
	}
}

// --- §15 channel + event switches (2026-09-16 修订) ---

func TestSwitchesDefaultOn(t *testing.T) {
	// Unset switches must not silence a channel that was already delivering.
	for _, key := range []string{KeyTelegramEnabled, KeyWebhookEnabled, KeyFeishuEnabled} {
		if !flagOn(decoder(map[string]string{}), key) {
			t.Fatalf("%s: unset switch should mean on", key)
		}
	}
	for _, group := range EventGroups {
		if !flagOn(decoder(map[string]string{}), EventSwitchKey(group)) {
			t.Fatalf("event %s: unset switch should mean on", group)
		}
	}
}

func TestEventGroupMapping(t *testing.T) {
	cases := map[string]string{
		"node_offline":        GroupNodeStatus,
		"traffic_warn":        GroupTraffic,
		"traffic_crit":        GroupTraffic,
		"billing_due":         GroupBilling,
		"due_overdue":         GroupBilling,
		"singbox_down":        GroupSingbox,
		"singbox_rollback":    GroupSingbox,
		"counter_reset":       GroupCounterReset,
		"login_failed":        GroupSecurity, // §15 实现修订 2026-09-20
		"login_blacklisted":   GroupSecurity,
		"agent_update_failed": GroupUpdates, // unknown kinds stay switchable
	}
	for kind, want := range cases {
		if got := EventGroup(kind); got != want {
			t.Fatalf("EventGroup(%s) = %s, want %s", kind, got, want)
		}
	}
}

func TestFeishuAcceptsHonoursBothSwitches(t *testing.T) {
	base := map[string]string{
		KeyFeishuAppID: "cli_a", KeyFeishuAppSecret: "sec", KeyFeishuReceiveID: "ou_x",
	}
	ev := Event{Kind: "node_offline", Event: EventAlert}

	// channel switch off: no delivery, but the channel is still "configured"
	// (the panel's test button must keep working while a channel is parked).
	off := newFeishu(mergeSettings(base, map[string]string{KeyFeishuEnabled: "0"}), "")
	if off.Accepts(ev) {
		t.Fatal("channel switch off must not accept")
	}
	if !off.Configured() {
		t.Fatal("Configured must stay true: switching off is not unconfiguring")
	}
	// event switch off: only that group is muted.
	evOff := newFeishu(mergeSettings(base, map[string]string{EventSwitchKey(GroupNodeStatus): "off"}), "")
	if evOff.Accepts(ev) {
		t.Fatal("event switch off must not accept this kind")
	}
	if !evOff.Accepts(Event{Kind: "traffic_warn"}) {
		t.Fatal("other event groups must stay accepted")
	}
	// defaults: both unset → accepted.
	if !newFeishu(base, "").Accepts(ev) {
		t.Fatal("unset switches must accept")
	}
}

func mergeSettings(base, over map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

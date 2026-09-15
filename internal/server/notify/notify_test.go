package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decoder(settings map[string]string) DecryptFunc {
	return func(key string) (string, bool) {
		v, ok := settings[key]
		return v, ok
	}
}

func newClient() *http.Client { return &http.Client{Timeout: DeliveryTimeout} }

func TestSignMatchesHMACSHA256(t *testing.T) {
	body := []byte(`{"kind":"node_offline"}`)
	got := Sign(body, "s3cret")
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
}

func TestWebhookDeliverSignsBody(t *testing.T) {
	var (
		gotSig    string
		gotCT     string
		gotBody   []byte
		signature string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Fobe-Signature")
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secret := "s3cret"
	hook := NewWebhook(decoder(map[string]string{
		KeyWebhookURL:    srv.URL,
		KeyWebhookSecret: secret,
	}), newClient())
	ev := Event{
		Kind:      "traffic_warn",
		NodeID:    "n1",
		Payload:   `{"pct":95,"mode":"both"}`,
		CreatedAt: 1730000000,
		Event:     EventAlert,
	}
	if err := hook.Deliver(ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotCT != "application/json" {
		t.Fatalf("content type = %q", gotCT)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	signature = hex.EncodeToString(mac.Sum(nil))
	if gotSig != signature {
		t.Fatalf("signature = %q, want %q (verified against received body)", gotSig, signature)
	}

	var decoded struct {
		Kind      string          `json:"kind"`
		NodeID    string          `json:"node_id"`
		Payload   json.RawMessage `json:"payload"`
		CreatedAt int64           `json:"created_at"`
		Event     string          `json:"event"`
	}
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("decode body %s: %v", gotBody, err)
	}
	if decoded.Kind != "traffic_warn" || decoded.NodeID != "n1" ||
		decoded.CreatedAt != 1730000000 || decoded.Event != EventAlert {
		t.Fatalf("decoded = %+v", decoded)
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
		t.Fatalf("payload not embedded as JSON object: %v (%s)", err, decoded.Payload)
	}
	if payload["pct"] != float64(95) {
		t.Fatalf("payload pct = %v", payload["pct"])
	}
}

func TestWebhookInvalidPayloadBecomesString(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hook := NewWebhook(decoder(map[string]string{KeyWebhookURL: srv.URL}), newClient())
	if err := hook.Deliver(Event{Kind: "k", NodeID: "n", Payload: "not json"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if decoded["payload"] != "not json" {
		t.Fatalf("payload = %v, want the fallback string", decoded["payload"])
	}
}

func TestWebhookNotConfigured(t *testing.T) {
	hook := NewWebhook(decoder(map[string]string{}), newClient())
	if hook.Configured() {
		t.Fatal("Configured with no url")
	}
	if err := hook.Deliver(Event{Kind: "k"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestWebhookServerErrorRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hook := NewWebhook(decoder(map[string]string{KeyWebhookURL: srv.URL}), newClient())
	if err := hook.Deliver(Event{Kind: "k", NodeID: "n"}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestWebhookUnsignedWithoutSecret(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Fobe-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hook := NewWebhook(decoder(map[string]string{KeyWebhookURL: srv.URL}), newClient())
	if err := hook.Deliver(Event{Kind: "k", NodeID: "n"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotSig != "" {
		t.Fatalf("signature header set without secret: %q", gotSig)
	}
}

func TestTelegramDeliverPostsForm(t *testing.T) {
	var gotPath string
	var gotChat, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotChat = r.PostFormValue("chat_id")
		gotText = r.PostFormValue("text")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tg := NewTelegram(decoder(map[string]string{
		KeyTelegramToken:  "TOK",
		KeyTelegramChatID: "42",
	}), newClient())
	tg.apiBase = srv.URL
	if !tg.Configured() {
		t.Fatal("Configured = false")
	}
	ev := Event{
		Kind:      "node_offline",
		NodeID:    "n1",
		NodeName:  "edge-1",
		Payload:   `{"x":1}`,
		CreatedAt: 1730000000,
		Event:     EventAlert,
	}
	if err := tg.Deliver(ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotPath != "/botTOK/sendMessage" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotChat != "42" {
		t.Fatalf("chat_id = %q", gotChat)
	}
	for _, want := range []string{"[fobe] ALERT node_offline - edge-1", "node_offline", `{"x":1}`} {
		if !strings.Contains(gotText, want) {
			t.Fatalf("text %q missing %q", gotText, want)
		}
	}
}

func TestTelegramRecoveryText(t *testing.T) {
	text := MessageText(Event{Kind: "node_offline", NodeID: "n1", NodeName: "edge-1", CreatedAt: 1730000000, Event: EventRecovery})
	if !strings.Contains(text, "RECOVERED node_offline") {
		t.Fatalf("text = %q", text)
	}
}

func TestTelegramNotConfigured(t *testing.T) {
	tg := NewTelegram(decoder(map[string]string{KeyTelegramToken: "TOK"}), newClient())
	if tg.Configured() {
		t.Fatal("Configured without chat id")
	}
	if err := tg.Deliver(Event{Kind: "k"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestTelegramServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "chat not found", http.StatusBadRequest)
	}))
	defer srv.Close()

	tg := NewTelegram(decoder(map[string]string{
		KeyTelegramToken:  "TOK",
		KeyTelegramChatID: "42",
	}), newClient())
	tg.apiBase = srv.URL
	if err := tg.Deliver(Event{Kind: "k", NodeID: "n"}); err == nil {
		t.Fatal("expected error on 400")
	}
}

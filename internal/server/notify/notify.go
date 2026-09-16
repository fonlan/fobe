// Package notify delivers alerts to the external channels from design.md §15:
// the Telegram Bot API and a generic JSON webhook signed with HMAC-SHA256.
//
// Concrete notifiers resolve their configuration lazily through a DecryptFunc
// (built in cmd/server/main.go, where the AES-GCM Cryptor lives), so settings
// changed at runtime take effect on the next delivery without a restart.
package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// settings keys (design §4.4 / §15).
const (
	KeyTelegramToken  = "notify.telegram_bot_token" // encrypted at rest
	KeyTelegramChatID = "notify.telegram_chat_id"
	KeyWebhookURL     = "notify.webhook_url"
	KeyWebhookSecret  = "notify.webhook_secret" // encrypted at rest

	// 飞书 (design §15, 2026-09-16 修订). App mode = 自建应用 from the QR scan
	// flow (feishureg); webhook mode = group custom-bot webhook URL.
	KeyFeishuAppID         = "notify.feishu_app_id"
	KeyFeishuAppSecret     = "notify.feishu_app_secret"     // encrypted at rest
	KeyFeishuReceiveID     = "notify.feishu_receive_id"     // ou_/oc_/on_ prefixed
	KeyFeishuBotName       = "notify.feishu_bot_name"       // display cache only
	KeyFeishuDomain        = "notify.feishu_domain"         // feishu | lark
	KeyFeishuWebhookURL    = "notify.feishu_webhook_url"    // embeds a token → encrypted at rest
	KeyFeishuWebhookSecret = "notify.feishu_webhook_secret" // encrypted at rest
)

// Event kinds of the webhook/telegram body ("alert" or "recovery").
const (
	EventAlert    = "alert"
	EventRecovery = "recovery"
	EventTest     = "test" // panel test button / QR welcome message only
)

// ErrNotConfigured is returned by Deliver when the channel has no settings;
// the scheduler treats it as "skip this channel", not as a failure to retry.
var ErrNotConfigured = errors.New("notify channel not configured")

// DecryptFunc resolves a settings key to its plaintext value.
// ok is false when the key is unset or cannot be decrypted.
type DecryptFunc func(settingKey string) (value string, ok bool)

// Event is one alert delivery. Payload carries the raw JSON stored in
// alerts.payload; notifiers decide how to embed it (object vs inline text).
type Event struct {
	Kind      string `json:"kind"`
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name,omitempty"` // telegram text only
	Payload   string `json:"payload"`
	CreatedAt int64  `json:"created_at"`
	Event     string `json:"event"` // alert | recovery
}

// Notifier is one delivery channel. Deliver must be safe for the 10s client
// timeout and never block longer; failures are retried on the next loop pass.
type Notifier interface {
	Name() string
	// Configured reports whether the channel has settings; the scheduler
	// skips unconfigured channels without touching the delivery queue.
	Configured() bool
	Deliver(ev Event) error
}

// DeliveryTimeout bounds every outbound notification request.
const DeliveryTimeout = 10 * time.Second

// Sign returns hex(HMAC-SHA256(body, secret)) for the X-Fobe-Signature header.
func Sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

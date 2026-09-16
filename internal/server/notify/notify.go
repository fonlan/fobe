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
	"strings"
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

	// Per-channel on/off switches (design §15, 2026-09-16 修订: 通知独立成页).
	// Unset means on — an upgrade must never silently silence a channel that
	// was already delivering.
	KeyTelegramEnabled = "notify.telegram_enabled"
	KeyWebhookEnabled  = "notify.webhook_enabled"
	KeyFeishuEnabled   = "notify.feishu_enabled"
)

// EventSwitchPrefix + group is the per-event-type switch key. Groups are
// coarser than alert kinds on purpose: an operator thinks in "traffic", not in
// traffic_warn vs traffic_crit.
const EventSwitchPrefix = "notify.event."

// Event groups (also the switch slugs shown on the notifications page).
const (
	GroupNodeStatus   = "node_status"
	GroupTraffic      = "traffic"
	GroupBilling      = "billing"
	GroupSingbox      = "singbox"
	GroupUpdates      = "updates"
	GroupCounterReset = "counter_reset"
)

// EventGroups is the ordered set the panel renders: every group is exposed,
// including ones whose kinds no feature currently raises (an off switch for a
// silent event is harmless; a missing switch is a surprise).
var EventGroups = []string{
	GroupNodeStatus, GroupTraffic, GroupBilling, GroupSingbox, GroupUpdates, GroupCounterReset,
}

// EventGroup maps an alert kind to its switch group. Unknown kinds land in
// GroupUpdates ("something changed on a probe/sing-box") instead of being
// always-on: a new kind must be switchable by default, and the fallback is the
// closest thing to "operational noise".
func EventGroup(kind string) string {
	switch kind {
	case "node_offline":
		return GroupNodeStatus
	case "traffic_warn", "traffic_crit":
		return GroupTraffic
	case "billing_due", "due_overdue":
		return GroupBilling
	case "singbox_down", "singbox_rollback":
		return GroupSingbox
	case "counter_reset":
		return GroupCounterReset
	default:
		return GroupUpdates
	}
}

// EventSwitchKey is the settings key behind one event group's switch.
func EventSwitchKey(group string) string { return EventSwitchPrefix + group }

// flagOn reads a boolean switch: unset/empty/invalid means on, which keeps the
// pre-switch behaviour of every channel and event. Only the explicit off
// spellings (the ones validBoolSetting accepts) disable something.
func flagOn(decrypt DecryptFunc, key string) bool {
	v, ok := decrypt(key)
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

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
	// Accepts reports whether the channel wants this event: its own on/off
	// switch plus the per-event-type switch (§15, 2026-09-16 修订). A channel
	// that is switched off or does not take this event is skipped, and the
	// alert is *not* left in the queue for later — see schedule/deliverOne.
	Accepts(ev Event) bool
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

package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Webhook POSTs each alert as JSON {kind, node_id, payload, created_at, event}
// with an X-Fobe-Signature header of hex(HMAC-SHA256(body, secret)) when a
// secret is configured (design §15).
type Webhook struct {
	Decrypt DecryptFunc
	HTTP    *http.Client
}

func NewWebhook(decrypt DecryptFunc, client *http.Client) *Webhook {
	if client == nil {
		client = &http.Client{Timeout: DeliveryTimeout}
	}
	return &Webhook{Decrypt: decrypt, HTTP: client}
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Configured() bool {
	url, ok := w.Decrypt(KeyWebhookURL)
	return ok && strings.TrimSpace(url) != ""
}

// webhookEvent is the exact wire format consumed by generic receivers.
type webhookEvent struct {
	Kind      string          `json:"kind"`
	NodeID    string          `json:"node_id"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt int64           `json:"created_at"`
	Event     string          `json:"event"`
}

// Accepts gates delivery on the channel switch plus the per-event-type switch.
func (w *Webhook) Accepts(ev Event) bool {
	return flagOn(w.Decrypt, KeyWebhookEnabled) && flagOn(w.Decrypt, EventSwitchKey(EventGroup(ev.Kind)))
}

func (w *Webhook) Deliver(ev Event) error {
	if !w.Configured() {
		return ErrNotConfigured
	}
	endpoint, _ := w.Decrypt(KeyWebhookURL)
	body, err := json.Marshal(webhookEvent{
		Kind:      ev.Kind,
		NodeID:    ev.NodeID,
		Payload:   rawJSON(ev.Payload),
		CreatedAt: ev.CreatedAt,
		Event:     ev.Event,
	})
	if err != nil {
		return fmt.Errorf("webhook encode: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret, ok := w.Decrypt(KeyWebhookSecret); ok && secret != "" {
		req.Header.Set("X-Fobe-Signature", Sign(body, secret))
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook post: status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

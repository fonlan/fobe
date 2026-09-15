package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultTelegramAPI = "https://api.telegram.org"

// Telegram posts alert text to the Bot API sendMessage endpoint
// (design §15). The bot token is read decrypted from the settings store.
type Telegram struct {
	Decrypt DecryptFunc
	HTTP    *http.Client

	// apiBase is overridable for tests; production uses api.telegram.org.
	apiBase string
}

func NewTelegram(decrypt DecryptFunc, client *http.Client) *Telegram {
	if client == nil {
		client = &http.Client{Timeout: DeliveryTimeout}
	}
	return &Telegram{Decrypt: decrypt, HTTP: client, apiBase: defaultTelegramAPI}
}

func (t *Telegram) Name() string { return "telegram" }

func (t *Telegram) Configured() bool {
	token, ok := t.Decrypt(KeyTelegramToken)
	if !ok || strings.TrimSpace(token) == "" {
		return false
	}
	chatID, ok := t.Decrypt(KeyTelegramChatID)
	return ok && strings.TrimSpace(chatID) != ""
}

func (t *Telegram) Deliver(ev Event) error {
	if !t.Configured() {
		return ErrNotConfigured
	}
	token, _ := t.Decrypt(KeyTelegramToken)
	chatID, _ := t.Decrypt(KeyTelegramChatID)
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", t.apiBase, token)
	resp, err := t.HTTP.PostForm(endpoint, url.Values{
		"chat_id": {chatID},
		"text":    {MessageText(ev)},
	})
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram sendMessage: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// MessageText renders a plain-text notification; the Bot API takes no markup
// by default, so this stays readable on any client.
func MessageText(ev Event) string {
	var b strings.Builder
	b.WriteString("[fobe] ")
	if ev.Event == EventRecovery {
		b.WriteString("RECOVERED ")
	} else {
		b.WriteString("ALERT ")
	}
	b.WriteString(ev.Kind)
	subject := ev.NodeName
	if subject == "" {
		subject = ev.NodeID
	}
	if subject != "" {
		b.WriteString(" - ")
		b.WriteString(subject)
	}
	b.WriteString("\n")
	b.WriteString(time.Unix(ev.CreatedAt, 0).UTC().Format(time.RFC3339))
	if payload := strings.TrimSpace(ev.Payload); payload != "" && payload != "{}" {
		b.WriteString("\n")
		b.WriteString(payload)
	}
	return b.String()
}

// rawJSON normalizes the stored payload string into a JSON value for the
// webhook body: valid JSON passes through, anything else becomes a string.
func rawJSON(payload string) json.RawMessage {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(payload)) {
		return json.RawMessage(payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage("{}")
	}
	return encoded
}

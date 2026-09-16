package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Feishu delivers alerts to 飞书 through one of two sub-channels (design §15,
// 2026-09-16 修订):
//
//  1. app mode — a 自建应用 provisioned by the QR scan flow (feishureg): sends
//     im/v1/messages as the bot to the stored receive id. Preferred when set,
//     because the QR flow also pins the receiver.
//  2. webhook mode — a group 自定义机器人 webhook URL with optional 加签.
//
// Both resolve settings lazily via DecryptFunc, so configuration changes take
// effect on the next delivery without a restart (same contract as Telegram).
type Feishu struct {
	Decrypt DecryptFunc
	HTTP    *http.Client

	// tenant access token cache: one process-wide token per configured app,
	// refreshed ~60s before expiry. Sends happen on the scheduler loop, so a
	// plain mutex is enough.
	tokMu     sync.Mutex
	tokValue  string
	tokExpiry time.Time

	// openBase is overridable for tests; production picks by domain setting.
	openBase func() string
}

func NewFeishu(decrypt DecryptFunc, client *http.Client) *Feishu {
	if client == nil {
		client = &http.Client{Timeout: DeliveryTimeout}
	}
	f := &Feishu{Decrypt: decrypt, HTTP: client}
	f.openBase = func() string {
		if domain, _ := f.Decrypt(KeyFeishuDomain); domain == "lark" {
			return "https://open.larksuite.com"
		}
		return "https://open.feishu.cn"
	}
	return f
}

func (f *Feishu) Name() string { return "feishu" }

// Configured reports whether either sub-channel has settings. The scheduler
// skips the channel entirely when this is false.
func (f *Feishu) Configured() bool {
	if id, ok := f.Decrypt(KeyFeishuAppID); ok && strings.TrimSpace(id) != "" {
		if sec, ok := f.Decrypt(KeyFeishuAppSecret); ok && strings.TrimSpace(sec) != "" {
			if rcv, ok := f.Decrypt(KeyFeishuReceiveID); ok && strings.TrimSpace(rcv) != "" {
				return true
			}
		}
	}
	url, ok := f.Decrypt(KeyFeishuWebhookURL)
	return ok && strings.TrimSpace(url) != ""
}

// receiveIDType maps the stored receive id to the im/v1 receive_id_type.
// Feishu prefixes are stable: ou_ = open id, oc_ = chat id, on_ = union id.
// Anything else is treated as an open id (the QR flow only ever stores ou_).
func receiveIDType(id string) string {
	switch {
	case strings.HasPrefix(id, "oc_"):
		return "chat_id"
	case strings.HasPrefix(id, "on_"):
		return "union_id"
	default:
		return "open_id"
	}
}

// SendText posts one plain-text message through whichever sub-channel is
// configured. Used by Deliver, the panel's test button and the QR flow's
// welcome message.
func (f *Feishu) SendText(text string) error {
	if id, ok := f.Decrypt(KeyFeishuAppID); ok && strings.TrimSpace(id) != "" {
		secret, _ := f.Decrypt(KeyFeishuAppSecret)
		receive, _ := f.Decrypt(KeyFeishuReceiveID)
		if strings.TrimSpace(secret) != "" && strings.TrimSpace(receive) != "" {
			return f.sendAsApp(strings.TrimSpace(text))
		}
	}
	url, ok := f.Decrypt(KeyFeishuWebhookURL)
	if !ok || strings.TrimSpace(url) == "" {
		return ErrNotConfigured
	}
	return f.sendWebhook(strings.TrimSpace(url), text)
}

func (f *Feishu) Deliver(ev Event) error {
	if !f.Configured() {
		return ErrNotConfigured
	}
	return f.SendText(MessageText(ev))
}

// apiErr is the common shape of Feishu open-API responses: HTTP 200 with a
// non-zero code meaning business failure.
type apiErr struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// tenantToken returns a cached tenant_access_token, refreshing it when missing
// or within 60s of expiry. Token errors invalidate the cache so the next send
// retries with a fresh token.
func (f *Feishu) tenantToken(appID, appSecret string) (string, error) {
	f.tokMu.Lock()
	if f.tokValue != "" && time.Now().Before(f.tokExpiry) {
		tok := f.tokValue
		f.tokMu.Unlock()
		return tok, nil
	}
	f.tokMu.Unlock()

	body, _ := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	resp, err := f.HTTP.Post(f.openBase()+"/open-apis/auth/v3/tenant_access_token/internal", "application/json; charset=utf-8", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("feishu token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var tr struct {
		apiErr
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", fmt.Errorf("feishu token: status %d: %.200s", resp.StatusCode, string(raw))
	}
	if tr.Code != 0 || tr.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu token: code %d: %s", tr.Code, tr.Msg)
	}
	f.tokMu.Lock()
	f.tokValue = tr.TenantAccessToken
	// Feishu tokens live ~2h; refresh early so a send never rides an
	// about-to-expire token.
	f.tokExpiry = time.Now().Add(time.Duration(tr.Expire-60) * time.Second)
	f.tokMu.Unlock()
	return tr.TenantAccessToken, nil
}

func (f *Feishu) invalidateToken() {
	f.tokMu.Lock()
	f.tokValue = ""
	f.tokExpiry = time.Time{}
	f.tokMu.Unlock()
}

func (f *Feishu) sendAsApp(text string) error {
	appID, _ := f.Decrypt(KeyFeishuAppID)
	appSecret, _ := f.Decrypt(KeyFeishuAppSecret)
	receive, _ := f.Decrypt(KeyFeishuReceiveID)
	receive = strings.TrimSpace(receive)

	send := func() error {
		tok, err := f.tenantToken(strings.TrimSpace(appID), strings.TrimSpace(appSecret))
		if err != nil {
			return err
		}
		content, _ := json.Marshal(map[string]string{"text": text})
		payload, _ := json.Marshal(map[string]any{
			"receive_id": receive,
			"msg_type":   "text",
			"content":    string(content),
		})
		req, err := http.NewRequest(http.MethodPost,
			f.openBase()+"/open-apis/im/v1/messages?receive_id_type="+receiveIDType(receive),
			bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("feishu send: %w", err)
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+tok)
		return f.doAPI(req)
	}
	err := send()
	// A token invalidated server-side (e.g. secret rotated) must not poison the
	// cache: drop it and retry once before giving up.
	if err != nil && strings.Contains(err.Error(), "invalid access token") {
		f.invalidateToken()
		err = send()
	}
	return err
}

func (f *Feishu) doAPI(req *http.Request) error {
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("feishu send: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var ae apiErr
	_ = json.Unmarshal(raw, &ae)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || ae.Code != 0 {
		return fmt.Errorf("feishu send: status %d code %d: %s", resp.StatusCode, ae.Code, strings.TrimSpace(ae.Msg))
	}
	return nil
}

// sendWebhook posts the group custom-bot message. 加签 (when a secret is set)
// follows the Feishu custom-bot scheme: HMAC-SHA256 keyed with
// "<unix seconds>\n<secret>" over an empty message, base64-encoded.
func (f *Feishu) sendWebhook(endpoint, text string) error {
	body := map[string]any{
		"timestamp": fmt.Sprintf("%d", time.Now().Unix()),
		"msg_type":  "text",
		"content":   map[string]string{"text": text},
	}
	if secret, ok := f.Decrypt(KeyFeishuWebhookSecret); ok && strings.TrimSpace(secret) != "" {
		mac := hmac.New(sha256.New, []byte(body["timestamp"].(string)+"\n"+strings.TrimSpace(secret)))
		body["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("feishu webhook: %w", err)
	}
	resp, err := f.HTTP.Post(endpoint, "application/json; charset=utf-8", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("feishu webhook: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var ae apiErr
	_ = json.Unmarshal(raw, &ae)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || ae.Code != 0 {
		return fmt.Errorf("feishu webhook: status %d code %d: %s", resp.StatusCode, ae.Code, strings.TrimSpace(ae.Msg))
	}
	return nil
}

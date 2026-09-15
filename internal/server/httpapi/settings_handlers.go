package httpapi

import (
	"net/http"
	"strings"
)

// Settings groups (design §4.4 / §12.1 / §15 / §16):
//   ai.base_url, ai.model, ai.api_key (encrypted), ai.default_policy
//   anytls_password (encrypted; global shared proxy password, §10)
//   notify.telegram_bot_token (encrypted), notify.telegram_chat_id,
//   notify.webhook_url, notify.webhook_secret
//   retention.metrics_days, geoip.mmdb (binary, via upload later)
//   ui.theme (light|dark|system; localStorage + server dual-write, §16)
// Sensitive keys are AES-GCM encrypted at rest and never returned in GET.

type settingView struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive"`
	Set       bool   `json:"set"`
}

// sensitiveKeys are write-only: GET returns set/unset, never the value.
var sensitiveKeys = map[string]bool{
	"ai.api_key":                true,
	"anytls_password":           true,
	"notify.telegram_bot_token": true,
	"notify.webhook_secret":     true,
}

// allowedKeys guards PUT against arbitrary key injection.
var allowedKeys = func() map[string]bool {
	m := map[string]bool{}
	for k := range sensitiveKeys {
		m[k] = true
	}
	for _, k := range []string{
		"server.public_url",
		"ai.base_url", "ai.model", "ai.default_policy",
		"notify.telegram_chat_id", "notify.webhook_url",
		"retention.metrics_days", "retention.latency_days",
		"alert.traffic_warn_pct", "alert.traffic_crit_pct",
		"ai.kill_switch",
		"ui.theme", // light|dark|system; validated below (§16 dual-write)
	} {
		m[k] = true
	}
	return m
}()

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	out := []settingView{}
	for key := range allowedKeys {
		val, err := s.Store.GetSetting(key)
		set := err == nil
		v := ""
		if set && !sensitiveKeys[key] {
			v = val
		}
		out = append(out, settingView{Key: key, Value: v, Sensitive: sensitiveKeys[key], Set: set})
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": out})
}

type putSettingsReq struct {
	Settings map[string]string `json:"settings"`
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req putSettingsReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	for key, value := range req.Settings {
		if !allowedKeys[key] {
			writeErr(w, http.StatusBadRequest, "unknown_key")
			return
		}
		if key == "server.public_url" {
			if _, err := normalizePublicURL(value); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_public_url")
				return
			}
		}
		if key == "ui.theme" {
			switch value {
			case "light", "dark", "system":
			default:
				writeErr(w, http.StatusBadRequest, "bad_theme")
				return
			}
		}
		stored := value
		encrypted := false
		if sensitiveKeys[key] && value != "" {
			enc, err := s.Crypt.Encrypt(value)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			stored = enc
			encrypted = true
		}
		if err := s.Store.SetSetting(key, stored, encrypted); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	s.audit("settings_updated", joinKeys(req.Settings), s.Trust.RealIP(r))
	s.publishEvent("settings_updated", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GetDecryptedSetting is the internal accessor for outbound integrations
// (AI proxy, Telegram, webhook signing) — never exposed via HTTP.
func (s *Server) GetDecryptedSetting(key string) (string, bool) {
	val, err := s.Store.GetSetting(key)
	if err != nil {
		return "", false
	}
	if !sensitiveKeys[key] {
		return val, true
	}
	plain, err := s.Crypt.Decrypt(val)
	if err != nil {
		return "", false
	}
	return plain, true
}

func joinKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

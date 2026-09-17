package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/fonlan/fobe/internal/server/agentupdate"
)

// Settings groups (design §4.4 / §12.1 / §13 / §14 / §15 / §16):
//   ai.base_url, ai.model, ai.api_key (encrypted), ai.default_policy
//   notify.telegram_bot_token (encrypted), notify.telegram_chat_id,
//   notify.webhook_url, notify.webhook_secret
//   notify.feishu_app_id / app_secret (encrypted) / receive_id / bot_name /
//   domain / webhook_url (encrypted) / webhook_secret (encrypted) (§15,
//   2026-09-16 修订: the QR flow itself writes these via feishureg.Save)
//   retention.metrics_days
//   geoip.auto_update, geoip.max_age_days, geoip.url (§14.1; the MMDB itself
//   arrives via the upload/download endpoints, and geoip.status is server-owned)
//   ui.theme (light|dark|system; localStorage + server dual-write, §16)
// Sensitive keys are AES-GCM encrypted at rest and never returned in GET.
// GET additionally returns the derived ai_configured flag (not a setting) so
// the frontend can hide the assistant UI without re-implementing §12.1.

type settingView struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive"`
	Set       bool   `json:"set"`
}

// sensitiveKeys are write-only: GET returns set/unset, never the value.
var sensitiveKeys = map[string]bool{
	"ai.api_key":                   true,
	"notify.telegram_bot_token":    true,
	"notify.webhook_secret":        true,
	"notify.feishu_app_secret":     true,
	"notify.feishu_webhook_url":    true, // embeds the bot token in its path
	"notify.feishu_webhook_secret": true,
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
		// §15 飞书 (2026-09-16): app mode + group custom-bot webhook mode.
		"notify.feishu_app_id", "notify.feishu_app_secret", "notify.feishu_receive_id",
		"notify.feishu_bot_name", "notify.feishu_domain",
		"notify.feishu_webhook_url", "notify.feishu_webhook_secret",
		// §15 switches (2026-09-16, 通知独立成页): one per channel, one per
		// event group. Unset = on, so only explicit choices are stored.
		"notify.telegram_enabled", "notify.webhook_enabled", "notify.feishu_enabled",
		"notify.event.node_status", "notify.event.traffic", "notify.event.billing",
		"notify.event.singbox", "notify.event.updates", "notify.event.counter_reset",
		"retention.metrics_days", "retention.latency_days",
		"latency.interval_seconds",
		"alert.traffic_warn_pct", "alert.traffic_crit_pct",
		"ai.kill_switch",
		"ui.theme", // light|dark|system; validated below (§16 dual-write)
		// The §10 rules snippets (sub.rules_singbox / sub.rules_clash) are gone
		// with the {{rules}} placeholder — their values were inlined into the
		// stored templates by MigrateLegacyRules (实现修订 2026-09-16b).
		// §14.1 GeoIP database refresh policy.
		"geoip.auto_update", "geoip.max_age_days", "geoip.url",
		// §5.5 agent self-update switch (default on).
		agentupdate.SettingAutoUpdate,
		// §10.2 relay entries: how they are named, and whether a detected relay
		// is enrolled automatically.
		SettingRelayNameFormat, SettingRelayAutoInclude,
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
	// ai_configured is derived (not a setting) so the frontend never has to
	// re-implement the §12.1 "configured" rule: the terminal page hides the
	// assistant sidebar whenever the chat endpoint would 503 anyway.
	writeJSON(w, http.StatusOK, map[string]any{"settings": out, "ai_configured": s.aiConfigured()})
}

type putSettingsReq struct {
	Settings map[string]string `json:"settings"`
}

// validBoolSetting accepts the spellings the panel and env-style config use.
func validBoolSetting(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "0", "true", "false", "on", "off", "yes", "no":
		return true
	default:
		return false
	}
}

func validLatencyInterval(value string) bool {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	return err == nil && seconds >= 1 && seconds <= 3600
}

// isNotifySwitch matches the per-channel and per-event-type switches of §15.
func isNotifySwitch(key string) bool {
	switch key {
	case "notify.telegram_enabled", "notify.webhook_enabled", "notify.feishu_enabled":
		return true
	}
	return strings.HasPrefix(key, "notify.event.")
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
		// §5.5 switch. Anything truthy-but-unparsable would silently mean "on"
		// (the default), so refuse it instead of pretending to have turned it off.
		if key == agentupdate.SettingAutoUpdate && !validBoolSetting(value) {
			writeErr(w, http.StatusBadRequest, "bad_switch")
			return
		}
		// §15 飞书 host selector; anything but the two known brands is refused.
		if key == "notify.feishu_domain" && value != "feishu" && value != "lark" {
			writeErr(w, http.StatusBadRequest, "bad_feishu_domain")
			return
		}
		// §15 switches: like the §5.5 switch, an unparsable truthy value would
		// silently mean "on", so refuse anything that is not a known spelling.
		if isNotifySwitch(key) && !validBoolSetting(value) {
			writeErr(w, http.StatusBadRequest, "bad_switch")
			return
		}
		if key == "latency.interval_seconds" && !validLatencyInterval(value) {
			writeErr(w, http.StatusBadRequest, "bad_latency_interval")
			return
		}
		// §10.2: the auto-name template travels verbatim into every client
		// config the subscription hands out, so a typo'd placeholder is refused
		// here rather than shipped; the switch follows the same rule as §5.5.
		if key == SettingRelayNameFormat && !validRelayNameFormat(value) {
			writeErr(w, http.StatusBadRequest, "bad_relay_name_format")
			return
		}
		if key == SettingRelayAutoInclude && !validBoolSetting(value) {
			writeErr(w, http.StatusBadRequest, "bad_switch")
			return
		}
		// §14.1 policy values; validated by the same helper the §17 import uses.
		if geoIPSettingKey(key) && !validGeoIPSetting(key, value) {
			writeErr(w, http.StatusBadRequest, geoIPSettingErrCode(key))
			return
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
	if _, changed := req.Settings["latency.interval_seconds"]; changed && s.Hub != nil {
		s.Hub.PushLatencyConfig()
	}
	// A change to the §14.1 policy can make the database immediately outdated
	// (a shorter threshold, or the switch turned back on): evaluate it now
	// instead of waiting for the next daily tick. Check only downloads when the
	// policy actually says so, so a no-op change costs one stat.
	if s.GeoIPUpdater != nil {
		for key := range req.Settings {
			if geoIPSettingKey(key) {
				go s.GeoIPUpdater.Check(s.background())
				break
			}
		}
	}
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

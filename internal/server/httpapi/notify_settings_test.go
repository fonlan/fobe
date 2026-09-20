package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/fonlan/fobe/internal/server/notify"
)

// §15 pushed-text settings (2026-09-19 修订): the copy language and the zone
// every timestamp renders in. The write path is strict on purpose — a value the
// delivery side would silently ignore (an unknown tag, a zone this image cannot
// resolve) must be refused, not stored and forgotten.
func TestNotifyTextSettingsValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := panelCookie(t, srv)

	for _, v := range []string{"en-US", "zh-CN"} {
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{"notify.language": v}})
		if r.Status != 200 {
			t.Fatalf("notify.language=%q: %d %s", v, r.Status, r.Body)
		}
	}
	// Only the canonical tags: "zh"/"en" would work at delivery time but would
	// make the page's select and the stored value disagree about the spelling.
	for _, v := range []string{"en", "zh", "fr", ""} {
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{"notify.language": v}})
		if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_notify_language" {
			t.Fatalf("notify.language=%q: got %d %s, want 400 bad_notify_language", v, r.Status, r.Body)
		}
	}

	for _, v := range []string{"", "UTC", "utc", "Asia/Shanghai", "America/New_York"} {
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{"notify.timezone": v}})
		if r.Status != 200 {
			t.Fatalf("notify.timezone=%q: %d %s", v, r.Status, r.Body)
		}
	}
	for _, v := range []string{"Mars/Olympus", "Europe/Nowhere"} {
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{"notify.timezone": v}})
		if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_timezone" {
			t.Fatalf("notify.timezone=%q: got %d %s, want 400 bad_timezone", v, r.Status, r.Body)
		}
	}

	// Both are non-sensitive, so GET reports the stored values back.
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
		map[string]any{"settings": map[string]string{"notify.language": "zh-CN", "notify.timezone": "Asia/Shanghai"}})
	if r.Status != 200 {
		t.Fatalf("final put: %d %s", r.Status, r.Body)
	}
	got := getSettingsMap(t, srv.URL, cookie)
	if got["notify.language"] != "zh-CN" || got["notify.timezone"] != "Asia/Shanghai" {
		t.Fatalf("GET settings = %v", got)
	}
}

// A hand-crafted snapshot must not install a tag or zone the API would have
// rejected (the §17 import rule the public_url and GeoIP settings already
// follow): an unreadable zone would otherwise sit in the database looking like
// a working setting while every message rendered on UTC.
func TestImportDropsUnusableNotifyTextSettings(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginCookie(t, srv.URL)

	snapshot := "{\"version\":" + strconv.Itoa(exportFormatVersion) + ",\"settings\":{" +
		"\"notify.language\":\"fr\",\"notify.timezone\":\"Mars/Olympus\"," +
		"\"alert.traffic_warn_pct\":\"85\"}}"
	resp, raw := doAuthed(t, "POST", srv.URL+"/api/import", cookie, []byte(snapshot))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import: %d %s", resp.StatusCode, raw)
	}

	got := getSettingsMap(t, srv.URL, cookie)
	if v := got["notify.language"]; v != "" {
		t.Fatalf("unusable notify.language was imported: %q", v)
	}
	if v := got["notify.timezone"]; v != "" {
		t.Fatalf("unusable notify.timezone was imported: %q", v)
	}
	// Control: an ordinary setting in the same snapshot still lands, so the
	// assertions above cannot pass because the whole import was a no-op.
	if got["alert.traffic_warn_pct"] != "85" {
		t.Fatalf("control setting missing after import: %v", got)
	}

	// The usable half is imported.
	snapshot = "{\"version\":" + strconv.Itoa(exportFormatVersion) +
		",\"settings\":{\"notify.language\":\"zh-CN\",\"notify.timezone\":\"Asia/Shanghai\"}}"
	if resp, raw = doAuthed(t, "POST", srv.URL+"/api/import", cookie, []byte(snapshot)); resp.StatusCode != http.StatusOK {
		t.Fatalf("import 2: %d %s", resp.StatusCode, raw)
	}
	if got = getSettingsMap(t, srv.URL, cookie); got["notify.language"] != "zh-CN" || got["notify.timezone"] != "Asia/Shanghai" {
		t.Fatalf("usable settings not imported: %v", got)
	}
}

// Every §15 event group the server advertises must also be writable: the panel
// builds its switch list from notify.EventGroups, so a group missing from
// allowedKeys renders a toggle the API rejects and GET never returns (the
// 2026-09-20 "security" group hit exactly that). Pinning the two together is
// what keeps the next group from shipping half-registered.
func TestEveryEventGroupSwitchIsWritable(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := panelCookie(t, srv)
	for _, group := range notify.EventGroups {
		key := notify.EventSwitchKey(group)
		if !allowedKeys[key] {
			t.Fatalf("event group %s: %s is not in allowedKeys", group, key)
		}
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{key: "0"}})
		if r.Status != 200 {
			t.Fatalf("PUT %s: %d %s", key, r.Status, r.Body)
		}
	}
	got := getSettingsMap(t, srv.URL, cookie)
	for _, group := range notify.EventGroups {
		key := notify.EventSwitchKey(group)
		if got[key] != "0" {
			t.Fatalf("switch %s did not round-trip: %q", key, got[key])
		}
	}
}

// getSettingsMap flattens GET /api/settings into key -> value (an empty string
// for keys that are unset). It decodes into generic maps on purpose: this file
// has no business depending on settingView's tags.
func getSettingsMap(t *testing.T, base, cookie string) map[string]string {
	t.Helper()
	r := doReq(t, &http.Client{}, "GET", base+"/api/settings", cookie, nil)
	if r.Status != 200 {
		t.Fatalf("GET settings: %d %s", r.Status, r.Body)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	out := map[string]string{}
	list, _ := body["settings"].([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		key, _ := m["key"].(string)
		value, _ := m["value"].(string)
		out[key] = value
	}
	return out
}

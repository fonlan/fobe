// Tests for design §10 subscription UA filtering and §16 ui.theme validation:
// a filtered subscription must 404 mismatched clients exactly like unknown
// tokens (?format= must not bypass), and ui.theme only accepts light/dark/system.
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSubscriptionUAFilterAllowsMatchingClient(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	nodeID, _ := seedNode(t, api, "ua-hit", "m-ua-hit", "203.0.113.30")
	seedSingbox(t, api, nodeID, 24101)
	subID, token := createSubscription(t, srv, cookie, "ua-hit")
	if r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}}); r.Status != 200 {
		t.Fatalf("set sub nodes: %d %s", r.Status, r.Body)
	}

	// set the filter (with messy spacing to exercise normalization)
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"ua_filter": "  Clash ,  sing-box  "})
	if r.Status != 200 {
		t.Fatalf("set ua_filter: %d %s", r.Status, r.Body)
	}

	// the list echoes the normalized filter back
	r = doReq(t, &http.Client{}, "GET", srv.URL+"/api/subscriptions", cookie, nil)
	var list struct {
		Subscriptions []subscriptionView `json:"subscriptions"`
	}
	_ = json.Unmarshal(r.Body, &list)
	if len(list.Subscriptions) != 1 || list.Subscriptions[0].UAFilter != "Clash,sing-box" {
		t.Fatalf("ua_filter not persisted/normalized: %s", r.Body)
	}

	// matching UA (case-insensitive substring) → 200
	if r := fetchSub(t, srv, token, "", "ClashVerge/2.0.0 mihomo"); r.Status != 200 {
		t.Fatalf("clash UA blocked: %d %s", r.Status, r.Body)
	}
	if r := fetchSub(t, srv, token, "", "sing-box/1.10.0 (linux-amd64)"); r.Status != 200 {
		t.Fatalf("sing-box UA blocked: %d %s", r.Status, r.Body)
	}
	// a matching UA with an explicit format still renders
	if r := fetchSub(t, srv, token, "?format=singbox", "sing-box/1.10.0"); r.Status != 200 {
		t.Fatalf("matching UA + format blocked: %d %s", r.Status, r.Body)
	}
}

func TestSubscriptionUAFilterBlocksOtherClients(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	nodeID, _ := seedNode(t, api, "ua-miss", "m-ua-miss", "203.0.113.31")
	seedSingbox(t, api, nodeID, 24102)
	subID, token := createSubscription(t, srv, cookie, "ua-miss")
	doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}})
	if r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"ua_filter": "clash,sing-box"}); r.Status != 200 {
		t.Fatalf("set ua_filter: %d %s", r.Status, r.Body)
	}

	// non-matching UA → 404, identical body to an unknown token (no existence leak)
	blocked := fetchSub(t, srv, token, "", "curl/8.4.0")
	unknown := fetchSub(t, srv, "definitely-not-a-real-token", "", "curl/8.4.0")
	if blocked.Status != http.StatusNotFound {
		t.Fatalf("filtered UA: got %d, want 404", blocked.Status)
	}
	if blocked.Status != unknown.Status || string(blocked.Body) != string(unknown.Body) {
		t.Fatalf("filtered response differs from unknown token: %d %q vs %d %q",
			blocked.Status, blocked.Body, unknown.Status, unknown.Body)
	}

	// explicit ?format= must not bypass the filter (§10)
	if r := fetchSub(t, srv, token, "?format=singbox", "curl/8.4.0"); r.Status != http.StatusNotFound {
		t.Fatalf("format=singbox bypassed filter: got %d, want 404", r.Status)
	}
	if r := fetchSub(t, srv, token, "?format=clash", "curl/8.4.0"); r.Status != http.StatusNotFound {
		t.Fatalf("format=clash bypassed filter: got %d, want 404", r.Status)
	}
	// empty UA is not on the allow-list either
	if r := fetchSub(t, srv, token, "", ""); r.Status != http.StatusNotFound {
		t.Fatalf("empty UA: got %d, want 404", r.Status)
	}

	// clearing the filter opens the URL again
	if r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"ua_filter": ""}); r.Status != 200 {
		t.Fatalf("clear ua_filter: %d %s", r.Status, r.Body)
	}
	if r := fetchSub(t, srv, token, "", "curl/8.4.0"); r.Status != 200 {
		t.Fatalf("curl UA after clearing filter: %d %s", r.Status, r.Body)
	}
}

func TestSettingsThemeValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := panelCookie(t, srv)

	// the three legal values are accepted
	for _, v := range []string{"light", "dark", "system"} {
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{"ui.theme": v}})
		if r.Status != 200 {
			t.Fatalf("ui.theme=%q: %d %s", v, r.Status, r.Body)
		}
	}

	// GET reports the stored value (non-sensitive)
	r := doReq(t, &http.Client{}, "GET", srv.URL+"/api/settings", cookie, nil)
	var got struct {
		Settings []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
			Set   bool   `json:"set"`
		} `json:"settings"`
	}
	_ = json.Unmarshal(r.Body, &got)
	found := ""
	for _, s := range got.Settings {
		if s.Key == "ui.theme" {
			found = s.Value
		}
	}
	if found != "system" {
		t.Fatalf("ui.theme in GET = %q, want system", found)
	}

	// anything else is a 400 bad_theme
	for _, v := range []string{"purple", "LIGHT", ""} {
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{"ui.theme": v}})
		if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_theme" {
			t.Fatalf("ui.theme=%q: got %d %s, want 400 bad_theme", v, r.Status, r.Body)
		}
	}
}

func TestNodeProbeOfflineConflict(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "probe-off", "m-probe-off", "203.0.113.32")

	// node exists but no agent socket → 409 node_offline (never a silent queue)
	r := doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/probe", cookie,
		map[string]any{"enabled": true})
	if r.Status != http.StatusConflict || r.errCode(t) != "node_offline" {
		t.Fatalf("offline probe: got %d %s, want 409 node_offline", r.Status, r.Body)
	}

	// malformed body → 400
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/"+nodeID+"/probe", cookie, map[string]any{})
	if r.Status != http.StatusBadRequest {
		t.Fatalf("probe without enabled: got %d, want 400", r.Status)
	}

	// unknown node → 404
	r = doReq(t, &http.Client{}, "POST", srv.URL+"/api/nodes/nope/probe", cookie,
		map[string]any{"enabled": true})
	if r.Status != http.StatusNotFound {
		t.Fatalf("unknown node probe: got %d, want 404", r.Status)
	}
}

// Tests for design.md §10 实现修订 2026-09-16: the subscription URL stays
// copyable (encrypted token next to the hash) and {{rules}} is filled from the
// per-format rules settings instead of being preserved verbatim.
package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSubscriptionLinkStaysCopyable(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	client := &http.Client{}
	subID, token := createSubscription(t, srv, cookie, "copyable")

	linkURL := srv.URL + "/api/subscriptions/" + subID + "/link"
	r := doReq(t, client, "GET", linkURL, cookie, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("link: %d %s", r.Status, r.Body)
	}
	m := r.JSONMap(t)
	if got, _ := m["token"].(string); got != token {
		t.Fatalf("revealed token = %q, want the created one %q", got, token)
	}
	if u, _ := m["url"].(string); !strings.HasSuffix(u, "/sub/"+token) {
		t.Fatalf("revealed url = %q, want suffix /sub/%s", u, token)
	}

	// the list tells the panel whether the URL can be re-shown at all
	r = doReq(t, client, "GET", srv.URL+"/api/subscriptions", cookie, nil)
	var list struct {
		Subscriptions []subscriptionView `json:"subscriptions"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Subscriptions) != 1 || !list.Subscriptions[0].LinkAvailable {
		t.Fatalf("link_available not set on a fresh row: %s", r.Body)
	}

	// rotating rewrites the ciphertext too, so the new URL is copyable
	r = doReq(t, client, "POST", srv.URL+"/api/subscriptions/"+subID+"/rotate", cookie, nil)
	var rot struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(r.Body, &rot)
	r = doReq(t, client, "GET", linkURL, cookie, nil)
	if got, _ := r.JSONMap(t)["token"].(string); got != rot.Token {
		t.Fatalf("token after rotate = %q, want %q", got, rot.Token)
	}
	if r := fetchSub(t, srv, rot.Token, "?format=singbox", ""); r.Status != http.StatusOK {
		t.Fatalf("rotated token must still resolve: %d %s", r.Status, r.Body)
	}

	// a row written before the token_enc column: hash only, URL unrecoverable
	if err := api.Store.CreateSubscription("legacy-sub", "legacy", tokenHash("legacy-token"), ""); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	r = doReq(t, client, "GET", srv.URL+"/api/subscriptions/legacy-sub/link", cookie, nil)
	if r.Status != http.StatusConflict || r.errCode(t) != "link_unavailable" {
		t.Fatalf("legacy link: %d %s (want 409 link_unavailable)", r.Status, r.Body)
	}
	r = doReq(t, client, "GET", srv.URL+"/api/subscriptions", cookie, nil)
	_ = json.Unmarshal(r.Body, &list)
	for _, s := range list.Subscriptions {
		if s.ID == "legacy-sub" && s.LinkAvailable {
			t.Fatalf("legacy row must not claim a copyable link: %s", r.Body)
		}
	}

	// unknown id still 404s
	if r := doReq(t, client, "GET", srv.URL+"/api/subscriptions/nope/link", cookie, nil); r.Status != http.StatusNotFound {
		t.Fatalf("unknown subscription link: %d %s", r.Status, r.Body)
	}
	// and the endpoint is session-guarded
	if r := doReq(t, client, "GET", linkURL, "", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("anonymous link reveal: %d %s", r.Status, r.Body)
	}
}

func TestSubscriptionRulesInjection(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	client := &http.Client{}
	setAnytlsPassword(t, srv, cookie)

	nodeID, _ := seedNode(t, api, "probe-rules", "m-rules-1", "203.0.113.20")
	seedSingbox(t, api, nodeID, 25000)

	// one template per format, both splicing {{rules}} where the snippet belongs
	sbTpl := `{"outbounds": [{{nodes}}], "route": {"rules": [{{rules}}]}}`
	clashTpl := "mode: rule\nproxies:\n{{nodes}}\nrules:\n{{rules}}\n"
	sbTplID, clashTplID := "", ""
	for i, tc := range []struct{ format, content string }{
		{FormatSingbox, sbTpl}, {FormatClash, clashTpl},
	} {
		r := doReq(t, client, "POST", srv.URL+"/api/templates", cookie,
			map[string]any{"name": "tpl-" + tc.format, "format": tc.format, "content": tc.content})
		if r.Status != http.StatusOK {
			t.Fatalf("create %s template: %d %s", tc.format, r.Status, r.Body)
		}
		id, _ := r.JSONMap(t)["id"].(string)
		if i == 0 {
			sbTplID = id
		} else {
			clashTplID = id
		}
	}

	subID, token := createSubscription(t, srv, cookie, "rules")
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"template_id": sbTplID}); r.Status != 200 {
		t.Fatalf("bind singbox template: %d %s", r.Status, r.Body)
	}
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}}); r.Status != 200 {
		t.Fatalf("bind nodes: %d %s", r.Status, r.Body)
	}

	sbRules := `{"protocol":"dns","action":"hijack-dns"},{"ip_is_private":true,"outbound":"direct"}`
	clashRules := "  - DOMAIN-SUFFIX,example.com,DIRECT\n  - MATCH,PROXY"
	r := doReq(t, client, "PUT", srv.URL+"/api/settings", cookie,
		map[string]any{"settings": map[string]string{SettingRulesSingbox: sbRules, SettingRulesClash: clashRules}})
	if r.Status != http.StatusOK {
		t.Fatalf("save rules: %d %s", r.Status, r.Body)
	}

	// sing-box: the snippet lands as route.rules entries and no placeholder survives
	body := fetchSub(t, srv, token, "?format=singbox", "")
	if body.Status != http.StatusOK {
		t.Fatalf("singbox fetch: %d %s", body.Status, body.Body)
	}
	if strings.Contains(string(body.Body), "{{rules}}") {
		t.Fatalf("{{rules}} leaked into the rendered config:\n%s", body.Body)
	}
	var cfg struct {
		Route struct {
			Rules []map[string]any `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(body.Body, &cfg); err != nil {
		t.Fatalf("rendered singbox config is not JSON: %v\n%s", err, body.Body)
	}
	if len(cfg.Route.Rules) != 2 || cfg.Route.Rules[0]["action"] != "hijack-dns" {
		t.Fatalf("rules not injected: %s", body.Body)
	}

	// clash: list items keep their own indentation and markers
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"template_id": clashTplID}); r.Status != 200 {
		t.Fatalf("bind clash template: %d %s", r.Status, r.Body)
	}
	y := fetchSub(t, srv, token, "?format=clash", "")
	if y.Status != http.StatusOK {
		t.Fatalf("clash fetch: %d %s", y.Status, y.Body)
	}
	for _, want := range []string{"  - DOMAIN-SUFFIX,example.com,DIRECT", "  - MATCH,PROXY"} {
		if !strings.Contains(string(y.Body), want) {
			t.Fatalf("clash rules missing %q:\n%s", want, y.Body)
		}
	}
	if strings.Contains(string(y.Body), "{{rules}}") {
		t.Fatalf("{{rules}} leaked into the clash config:\n%s", y.Body)
	}

	// a fragment that cannot be spliced in is refused, and the stored one stays
	for _, bad := range []struct{ key, value string }{
		{SettingRulesSingbox, "not json"},
		{SettingRulesSingbox, `{"protocol":"dns"},`},
		{SettingRulesClash, "MATCH,PROXY"}, // missing the list marker
	} {
		r := doReq(t, client, "PUT", srv.URL+"/api/settings", cookie,
			map[string]any{"settings": map[string]string{bad.key: bad.value}})
		if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_rules" {
			t.Fatalf("%s=%q: %d %s (want 400 bad_rules)", bad.key, bad.value, r.Status, r.Body)
		}
	}

	// clearing is allowed and expands the placeholder to nothing
	r = doReq(t, client, "PUT", srv.URL+"/api/settings", cookie,
		map[string]any{"settings": map[string]string{SettingRulesSingbox: "", SettingRulesClash: ""}})
	if r.Status != http.StatusOK {
		t.Fatalf("clear rules: %d %s", r.Status, r.Body)
	}
	body = fetchSub(t, srv, token, "?format=clash", "")
	if strings.Contains(string(body.Body), "{{rules}}") {
		t.Fatalf("{{rules}} survived an empty rules setting:\n%s", body.Body)
	}
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"template_id": sbTplID}); r.Status != 200 {
		t.Fatalf("rebind singbox template: %d %s", r.Status, r.Body)
	}
	body = fetchSub(t, srv, token, "?format=singbox", "")
	var empty struct {
		Route struct {
			Rules []map[string]any `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(body.Body, &empty); err != nil {
		t.Fatalf("empty-rules render is not JSON: %v\n%s", err, body.Body)
	}
	if len(empty.Route.Rules) != 0 {
		t.Fatalf("cleared rules must render as an empty array: %s", body.Body)
	}
}

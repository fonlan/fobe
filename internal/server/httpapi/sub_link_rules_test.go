// Tests for design.md §10 实现修订 2026-09-16: the subscription URL stays
// copyable (encrypted token next to the hash), {{rules}} is filled from the
// per-format rules settings instead of being preserved verbatim, and the node
// picker offers exactly the nodes that would render.
package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fobe-panel/fobe/internal/server/store"
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

// TestSubscriptionPickerReadyFlag pins the node picker's contract to the
// renderer: `singbox_ready` must be true for exactly the nodes that end up in
// a subscription, otherwise the picker could offer a node that renders
// nothing (or hide one that does).
func TestSubscriptionPickerReadyFlag(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	client := &http.Client{}
	setAnytlsPassword(t, srv, cookie)

	// ready: port + reported certificate (seeded by seedSingbox)
	readyID, _ := seedNode(t, api, "ready", "m-ready", "203.0.113.30")
	seedSingbox(t, api, readyID, 26000)
	// no sing-box state at all
	noneID, _ := seedNode(t, api, "no-singbox", "m-none", "203.0.113.31")
	// running with a port, but the certificate has not been reported yet
	noCertID, _ := seedNode(t, api, "no-cert", "m-nocert", "203.0.113.32")
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: noCertID, Version: "1.10.0", Status: "running", Port: 26001,
	}); err != nil {
		t.Fatalf("seed port-only singbox: %v", err)
	}
	// has sing-box, but no primary IP → nothing to point a client at
	noIPID, _ := seedNode(t, api, "no-ip", "m-noip", "")
	seedSingbox(t, api, noIPID, 26002)

	r := doReq(t, client, "GET", srv.URL+"/api/nodes", cookie, nil)
	var list struct {
		Nodes []nodeView `json:"nodes"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		t.Fatalf("node list: %v", err)
	}
	got := map[string]bool{}
	for _, n := range list.Nodes {
		got[n.Name] = n.SingboxReady
	}
	for name, want := range map[string]bool{
		"ready": true, "no-singbox": false, "no-cert": false, "no-ip": false,
	} {
		if got[name] != want {
			t.Fatalf("singbox_ready[%s] = %v, want %v\n%s", name, got[name], want, r.Body)
		}
	}

	// the renderer agrees: binding all four yields exactly one outbound
	subID, token := createSubscription(t, srv, cookie, "picker")
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{readyID, noneID, noCertID, noIPID}}); r.Status != 200 {
		t.Fatalf("bind nodes: %d %s", r.Status, r.Body)
	}
	body := fetchSub(t, srv, token, "?format=singbox", "")
	if body.Status != http.StatusOK {
		t.Fatalf("fetch: %d %s", body.Status, body.Body)
	}
	var cfg struct {
		Outbounds []struct {
			Tag    string `json:"tag"`
			Server string `json:"server"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(body.Body, &cfg); err != nil {
		t.Fatalf("render: %v\n%s", err, body.Body)
	}
	if len(cfg.Outbounds) != 1 || cfg.Outbounds[0].Server != "203.0.113.30" {
		t.Fatalf("only the ready node must render, got %+v", cfg.Outbounds)
	}
}

// TestSubscriptionFormatResolution pins the §10 实现修订 2026-09-16 order:
// ?format= > the subscription's pinned format > (auto) the bound template's
// format > the User-Agent. The middle step is the reported bug: a subscription
// bound to a Clash template used to answer with sing-box JSON whenever the
// client's User-Agent was not recognised.
func TestSubscriptionFormatResolution(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	client := &http.Client{}
	setAnytlsPassword(t, srv, cookie)

	nodeID, _ := seedNode(t, api, "probe-fmt", "m-fmt-1", "203.0.113.40")
	seedSingbox(t, api, nodeID, 27000)

	clashTplID := ""
	r := doReq(t, client, "POST", srv.URL+"/api/templates", cookie,
		map[string]any{"name": "fmt-clash", "format": FormatClash,
			"content": "proxies:\n{{nodes}}\nrules:\n{{rules}}\n"})
	if r.Status != http.StatusOK {
		t.Fatalf("create clash template: %d %s", r.Status, r.Body)
	}
	clashTplID, _ = r.JSONMap(t)["id"].(string)

	subID, token := createSubscription(t, srv, cookie, "fmt")
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}}); r.Status != 200 {
		t.Fatalf("bind nodes: %d %s", r.Status, r.Body)
	}

	// no template, unknown UA → sing-box default (unchanged behaviour)
	if got := fetchSub(t, srv, token, "", "curl/8.4.0"); !strings.HasPrefix(got.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("auto without template should sniff: %q", got.Header.Get("Content-Type"))
	}

	// bind the Clash template: an unknown UA must now get YAML, not JSON
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"template_id": clashTplID}); r.Status != 200 {
		t.Fatalf("bind template: %d %s", r.Status, r.Body)
	}
	body := fetchSub(t, srv, token, "", "curl/8.4.0")
	if !strings.HasPrefix(body.Header.Get("Content-Type"), "text/yaml") {
		t.Fatalf("bound clash template must drive the format, got %q\n%s", body.Header.Get("Content-Type"), body.Body)
	}
	if !strings.Contains(string(body.Body), "type: anytls") {
		t.Fatalf("clash template did not render:\n%s", body.Body)
	}

	// ?format= still beats the template
	if got := fetchSub(t, srv, token, "?format=singbox", "curl/8.4.0"); !strings.HasPrefix(got.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("explicit ?format= must win: %q", got.Header.Get("Content-Type"))
	}

	// pinning a format beats the template and the UA (litmus: pin singbox while
	// a Clash template is bound → the built-in sing-box default is rendered)
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"format": FormatSingbox}); r.Status != 200 {
		t.Fatalf("pin format: %d %s", r.Status, r.Body)
	}
	body = fetchSub(t, srv, token, "", "clash-verge/2.0.0 mihomo")
	if !strings.HasPrefix(body.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("pinned singbox must ignore the clash UA: %q", body.Header.Get("Content-Type"))
	}
	// but the explicit query still overrides the pin
	if got := fetchSub(t, srv, token, "?format=clash", "curl/8.4.0"); !strings.HasPrefix(got.Header.Get("Content-Type"), "text/yaml") {
		t.Fatalf("?format= must override the pin: %q", got.Header.Get("Content-Type"))
	}

	// the pin is visible in the list, and clearing it is allowed
	r = doReq(t, client, "GET", srv.URL+"/api/subscriptions", cookie, nil)
	var list struct {
		Subscriptions []subscriptionView `json:"subscriptions"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Subscriptions) != 1 || list.Subscriptions[0].Format != FormatSingbox {
		t.Fatalf("format not exposed: %s", r.Body)
	}
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"format": ""}); r.Status != 200 {
		t.Fatalf("clear format: %d %s", r.Status, r.Body)
	}
	// back to auto → the bound Clash template drives it again
	if got := fetchSub(t, srv, token, "", "curl/8.4.0"); !strings.HasPrefix(got.Header.Get("Content-Type"), "text/yaml") {
		t.Fatalf("clearing the pin must return to auto: %q", got.Header.Get("Content-Type"))
	}

	// a bogus format is refused
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"format": "surge"}); r.Status != http.StatusBadRequest || r.errCode(t) != "bad_format" {
		t.Fatalf("bad format: %d %s", r.Status, r.Body)
	}
}

// TestSubscriptionAccessLogRefusalReasons covers the §10 实现修订 2026-09-16
// access log: a refused fetch answers the client with exactly the same 404 as
// an unknown token, so the reason column is the only explanation the operator
// ever gets.
func TestSubscriptionAccessLogRefusalReasons(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	client := &http.Client{}
	setAnytlsPassword(t, srv, cookie)

	nodeID, _ := seedNode(t, api, "probe-log", "m-log-1", "203.0.113.50")
	seedSingbox(t, api, nodeID, 28000)

	subID, token := createSubscription(t, srv, cookie, "logged")
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}}); r.Status != 200 {
		t.Fatalf("bind nodes: %d %s", r.Status, r.Body)
	}

	// 1. served
	if r := fetchSub(t, srv, token, "?format=singbox", "curl/8.4.0"); r.Status != http.StatusOK {
		t.Fatalf("served fetch: %d %s", r.Status, r.Body)
	}

	// 2. UA filter refuses the same client
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"ua_filter": "clash,mihomo"}); r.Status != 200 {
		t.Fatalf("set ua filter: %d %s", r.Status, r.Body)
	}
	refusedUA := fetchSub(t, srv, token, "?format=singbox", "curl/8.4.0")
	if refusedUA.Status != http.StatusNotFound {
		t.Fatalf("ua mismatch must 404: %d %s", refusedUA.Status, refusedUA.Body)
	}

	// 3. disabled subscription
	if r := doReq(t, client, "PUT", srv.URL+"/api/subscriptions/"+subID, cookie,
		map[string]any{"enabled": false}); r.Status != 200 {
		t.Fatalf("disable: %d %s", r.Status, r.Body)
	}
	refusedDisabled := fetchSub(t, srv, token, "?format=singbox", "clash-verge/2.0.0 mihomo")
	if refusedDisabled.Status != http.StatusNotFound {
		t.Fatalf("disabled must 404: %d %s", refusedDisabled.Status, refusedDisabled.Body)
	}
	// the two refusals must stay indistinguishable to the client
	if string(refusedUA.Body) != string(refusedDisabled.Body) {
		t.Fatalf("refusal bodies leak the reason: %s vs %s", refusedUA.Body, refusedDisabled.Body)
	}

	// an unknown token belongs to no subscription → nothing to attribute it to
	if r := fetchSub(t, srv, "totally-unknown-token", "", ""); r.Status != http.StatusNotFound {
		t.Fatalf("unknown token: %d", r.Status)
	}

	r := doReq(t, client, "GET", srv.URL+"/api/subscriptions/"+subID+"/access", cookie, nil)
	var al struct {
		Logs []struct {
			TS     int64  `json:"ts"`
			IP     string `json:"ip"`
			UA     string `json:"ua"`
			Reason string `json:"reason"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(r.Body, &al); err != nil {
		t.Fatalf("access log: %v", err)
	}
	got := make([]string, 0, len(al.Logs))
	for _, l := range al.Logs {
		got = append(got, l.Reason)
	}
	// newest first: disabled, ua_mismatch, served — and nothing for the unknown
	// token, which is the documented trade-off (no subscription to show it on)
	want := []string{"disabled", "ua_mismatch", ""}
	if len(got) != len(want) {
		t.Fatalf("access rows = %v, want %v (%s)", got, want, r.Body)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("access reasons = %v, want %v", got, want)
		}
	}
	if al.Logs[0].IP == "" || al.Logs[2].UA != "curl/8.4.0" {
		t.Fatalf("access rows lost their ip/ua: %s", r.Body)
	}
}

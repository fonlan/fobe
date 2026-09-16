// Tests for design.md §10.2: relay entries derived from a probe's nftables
// forwards, auto-enrolment with tombstones, entry naming and the renderer.
package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/store"
)

// seedNodeWithIP seeds a node and the address inventory the relay detector
// matches a forward's destination against (the hub fills this in production).
func seedNodeWithIP(t *testing.T, api *Server, name, machineID, ip string, port int) string {
	t.Helper()
	id, _ := seedNode(t, api, name, machineID, ip)
	if ip != "" {
		if err := api.Store.ReplaceNodeIPs(id, []store.IPRow{
			{IP: ip, Family: 4, Scope: "public", IsPrimary: true},
		}); err != nil {
			t.Fatalf("seed node ips: %v", err)
		}
	}
	if port > 0 {
		seedSingbox(t, api, id, port)
	}
	return id
}

// seedForward records one reported DNAT rule on a node (§21 snapshot).
func seedForward(t *testing.T, api *Server, nodeID, proto string, srcPort int, dstIP string, dstPort int) {
	t.Helper()
	status := store.NodeForwardStatus{Supported: true, Initialized: true, ReportedAt: 1700000000}
	rows := []store.NodeForward{{
		Handle: 10, Proto: proto, SrcPort: srcPort, DstIP: dstIP, DstPort: dstPort, Comment: "relay",
	}}
	if err := api.Store.ReplaceNodeForwards(nodeID, status, rows); err != nil {
		t.Fatalf("seed forwards: %v", err)
	}
}

// bindEntries PUTs the picker payload and asserts it was accepted.
func bindEntries(t *testing.T, srv *httptest.Server, cookie, subID string, entries []subEntryInput) {
	t.Helper()
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"entries": entries})
	if r.Status != 200 {
		t.Fatalf("bind entries: %d %s", r.Status, r.Body)
	}
}

func listEntries(t *testing.T, srv *httptest.Server, cookie, subID string) []subEntryView {
	t.Helper()
	r := doReq(t, &http.Client{}, "GET", srv.URL+"/api/subscriptions/"+subID+"/entries", cookie, nil)
	if r.Status != 200 {
		t.Fatalf("list entries: %d %s", r.Status, r.Body)
	}
	var out struct {
		Entries []subEntryView `json:"entries"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("entries body: %v\n%s", err, r.Body)
	}
	return out.Entries
}

// outboundCount counts the anytls outbounds in the rendered config.
func outboundCount(t *testing.T, srv *httptest.Server, token string) int {
	t.Helper()
	return strings.Count(string(fetchSub(t, srv, token, "", "").Body), `"type": "anytls"`)
}

// relayEntryOf finds the "B via A" row among the picker's entries.
func relayEntryOf(t *testing.T, entries []subEntryView, targetID string) subEntryView {
	t.Helper()
	for _, e := range entries {
		if e.NodeID == targetID && e.RelayNodeID != "" {
			return e
		}
	}
	t.Fatalf("no relay entry for node %s in %+v", targetID, entries)
	return subEntryView{}
}

// TestRelayEntryAutoEnrolAndRender is the §10.2 headline: A forwards to B's
// anytls inbound, so a subscription binding B grows a third entry — B through
// A — which renders as a second outbound pointing at A while still pinning B's
// certificate (DNAT is layer 4; TLS terminates on B).
func TestRelayEntryAutoEnrolAndRender(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, token := createSubscription(t, srv, cookie, "main")
	// The operator picks both nodes; the third entry (B through A) enrols
	// itself, because the forward says A reaches B's inbound.
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: aID, Selected: true},
		{NodeID: bID, Selected: true},
	})

	entries := listEntries(t, srv, cookie, subID)
	relay := relayEntryOf(t, entries, bID)
	if relay.RelayNodeID != aID || relay.SrcPort != 8080 || relay.Proto != "tcp" {
		t.Fatalf("relay entry = %+v, want relay=%s src_port=8080 tcp", relay, aID)
	}
	if !relay.Discovered {
		t.Errorf("relay entry should be marked discovered: %+v", relay)
	}
	if !relay.Selected {
		t.Errorf("detected relay should be auto-enrolled (§10.2): %+v", relay)
	}
	if !relay.Available {
		t.Errorf("relay entry should be available: %+v", relay)
	}
	if relay.NodeName != "B" || relay.RelayName != "A" {
		t.Errorf("entry names = %q/%q, want B/A", relay.NodeName, relay.RelayName)
	}
	if relay.AutoName != "B · A:8080" {
		t.Errorf("auto name = %q, want %q", relay.AutoName, "B · A:8080")
	}

	body := string(fetchSub(t, srv, token, "", "").Body)
	if got := outboundCount(t, srv, token); got != 3 {
		t.Fatalf("outbound count = %d, want 3\n%s", got, body)
	}
	for _, want := range []string{"fobe-A", "fobe-B", "fobe-B · A:8080", `"server_port": 8080`} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered subscription missing %q\n%s", want, body)
		}
	}
	// The relayed outbound must dial A but keep B's certificate: two outbounds
	// carry A's address, all three carry the pinned PEM.
	if got := strings.Count(body, `"server": "203.0.113.10"`); got != 2 {
		t.Errorf("outbounds pointing at A = %d, want 2\n%s", got, body)
	}
	if got := strings.Count(body, "BEGIN CERTIFICATE"); got != 3 {
		t.Errorf("pinned certificates = %d, want 3\n%s", got, body)
	}
}

// TestRelayEntryTombstoneSurvivesReconcile pins the rule that unchecking an
// entry sticks: the reconciler must treat the tombstone as the operator's
// decision, not as "absent, therefore enrol".
func TestRelayEntryTombstoneSurvivesReconcile(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})
	if relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID); !relay.Selected {
		t.Fatalf("precondition failed: relay not auto-enrolled: %+v", relay)
	}
	if got := outboundCount(t, srv, token); got != 2 {
		t.Fatalf("outbound count before opting out = %d, want 2", got)
	}

	// Uncheck it, then reconcile again (reading the picker triggers reconcile).
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: bID, Selected: true},
		{NodeID: bID, RelayNodeID: aID, Proto: "tcp", SrcPort: 8080, Selected: false},
	})
	if n := api.ReconcileSubscriptionEntries(); n != 0 {
		t.Errorf("reconcile re-added %d entries after an explicit opt-out", n)
	}
	relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID)
	if relay.Selected {
		t.Errorf("relay entry should stay unchecked: %+v", relay)
	}
	if got := outboundCount(t, srv, token); got != 1 {
		t.Errorf("outbound count = %d, want 1 after opting out (only B direct)", got)
	}
}

// TestRelayDetectionRequiresMatchingTarget keeps the detector honest: the
// destination must be one of the target's own addresses, on the right port and
// protocol, and the rule must live on a *different* node.
func TestRelayDetectionRequiresMatchingTarget(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.99", 20002) // wrong destination host
	seedForward(t, api, aID, "udp", 8081, "198.51.100.7", 20002)  // wrong protocol
	seedForward(t, api, aID, "tcp", 8082, "198.51.100.7", 20003)  // wrong destination port
	seedForward(t, api, bID, "tcp", 8083, "198.51.100.7", 20002)  // self-loop

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	for _, e := range listEntries(t, srv, cookie, subID) {
		if e.RelayNodeID != "" {
			t.Errorf("unexpected relay entry: %+v", e)
		}
	}
}

// TestRelayEntryNotEnrolledWhileUnrenderable: a relay entry is neither offered
// nor enrolled while its landing node cannot render (no certificate) — §10's
// "only nodes that appear in the output" rule, applied to the landing node.
func TestRelayEntryNotEnrolledWhileUnrenderable(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0) // port+cert reported below
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: bID, Version: "1.10.0", Status: "installing", Port: 20002, // no cert yet
	}); err != nil {
		t.Fatal(err)
	}
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	for _, e := range listEntries(t, srv, cookie, subID) {
		if e.RelayNodeID != "" {
			t.Errorf("relay entry offered while its landing node cannot render: %+v", e)
		}
	}
	// The *bound* direct entry stays listed, marked unrenderable, so it can be
	// unchecked (§10).
	var bound subEntryView
	found := false
	for _, e := range listEntries(t, srv, cookie, subID) {
		if e.NodeID == bID && e.RelayNodeID == "" {
			bound, found = e, true
		}
	}
	if !found || bound.Available || bound.Reason != "not_ready" {
		t.Fatalf("bound-but-unrenderable direct entry = %+v (found=%v)", bound, found)
	}

	// Once the certificate arrives, the next read offers it and enrols it.
	seedSingbox(t, api, bID, 20002)
	relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID)
	if !relay.Selected || !relay.Available {
		t.Errorf("relay should be offered and enrolled once renderable: %+v", relay)
	}
}

// TestPickerOnlyOffersRenderableNodes keeps the §10 filter intact: a node with
// no sing-box (or no certificate) is not offered for a new binding — it only
// shows up once it is bound, and then only so it can be unbound.
func TestPickerOnlyOffersRenderableNodes(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	readyID := seedNodeWithIP(t, api, "ready", "machine-ready", "203.0.113.10", 20001)
	plainID := seedNodeWithIP(t, api, "plain", "machine-plain", "198.51.100.7", 0) // no sing-box at all
	subID, _ := createSubscription(t, srv, cookie, "main")

	offered := func() map[string]subEntryView {
		t.Helper()
		out := map[string]subEntryView{}
		for _, e := range listEntries(t, srv, cookie, subID) {
			out[e.NodeID] = e
		}
		return out
	}
	got := offered()
	if _, ok := got[plainID]; ok {
		t.Errorf("node without sing-box was offered: %+v", got[plainID])
	}
	if _, ok := got[readyID]; !ok {
		t.Errorf("renderable node missing from the picker: %+v", got)
	}

	// Bind it while it is unrenderable, then look again: now it is listed, and
	// only because it is bound (it is still marked unavailable).
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: plainID, Selected: true}})
	got = offered()
	entry, ok := got[plainID]
	if !ok {
		t.Fatalf("bound node disappeared from the picker: %+v", got)
	}
	if entry.Available || entry.Reason != "not_ready" {
		t.Errorf("bound unrenderable entry = %+v, want unavailable/not_ready", entry)
	}
}

// TestRelayIngressNeedsAnAddress: the landing node renders, but the relay has no
// primary IP to dial — nothing usable, so nothing is offered.
func TestRelayIngressNeedsAnAddress(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "", 20001) // no address
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})
	for _, e := range listEntries(t, srv, cookie, subID) {
		if e.RelayNodeID != "" {
			t.Errorf("relay without an address was offered: %+v", e)
		}
	}
}

// TestEntryAliasValidationAndTag covers the naming half of §10.2: an alias
// becomes the outbound tag verbatim, duplicates are refused instead of silently
// suffixed, and a node's sub_name is the fallback below the alias.
func TestEntryAliasValidationAndTag(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, token := createSubscription(t, srv, cookie, "main")

	// Duplicate explicit aliases are refused (§10.2: no silent "-2").
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"entries": []subEntryInput{
			{NodeID: aID, Alias: "same", Selected: true},
			{NodeID: bID, Alias: "same", Selected: true},
		}})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "alias_conflict" {
		t.Fatalf("duplicate alias: %d %s, want 400 alias_conflict", r.Status, r.Body)
	}

	// Unrenderable alias spellings are refused too.
	for _, bad := range []string{"line\nbreak", strings.Repeat("x", 65)} {
		r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
			map[string]any{"entries": []subEntryInput{{NodeID: bID, Alias: bad, Selected: true}}})
		if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_alias" {
			t.Fatalf("alias %q: %d %s, want 400 bad_alias", bad, r.Status, r.Body)
		}
	}

	// sub_name is the middle rung of the fallback chain.
	r = doReq(t, &http.Client{}, "PATCH", srv.URL+"/api/nodes/"+aID, cookie,
		map[string]string{"sub_name": "香港-01"})
	if r.Status != 200 {
		t.Fatalf("set sub_name: %d %s", r.Status, r.Body)
	}

	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: aID, Selected: true},
		{NodeID: bID, Selected: true},
		{NodeID: bID, RelayNodeID: aID, Proto: "tcp", SrcPort: 8080, Alias: "B 走香港", Selected: true},
	})

	relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID)
	if relay.AutoName != "B · 香港-01:8080" {
		t.Errorf("relay auto name = %q, want it built from sub_name", relay.AutoName)
	}
	body := string(fetchSub(t, srv, token, "", "").Body)
	if !strings.Contains(body, "fobe-B 走香港") {
		t.Errorf("alias should become the tag\n%s", body)
	}
	if !strings.Contains(body, "fobe-香港-01") {
		t.Errorf("A's outbound should use its sub_name\n%s", body)
	}
}

// TestRelayNameFormatSetting: the template travels verbatim into client configs,
// so an unknown placeholder is refused rather than shipped.
func TestRelayNameFormatSetting(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
		map[string]any{"settings": map[string]string{SettingRelayNameFormat: "{relayname} -> {name}"}})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "bad_relay_name_format" {
		t.Fatalf("unknown placeholder: %d %s, want 400 bad_relay_name_format", r.Status, r.Body)
	}
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
		map[string]any{"settings": map[string]string{SettingRelayNameFormat: "{name} via {relay} [{port}]"}})
	if r.Status != 200 {
		t.Fatalf("set relay format: %d %s", r.Status, r.Body)
	}
	if relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID); relay.AutoName != "B via A [8080]" {
		t.Errorf("auto name = %q, want %q", relay.AutoName, "B via A [8080]")
	}
}

// TestRelayAutoIncludeCanBeDisabled: with the switch off, a detected relay is
// offered but stays unbound until the operator picks it.
func TestRelayAutoIncludeCanBeDisabled(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/settings", cookie,
		map[string]any{"settings": map[string]string{SettingRelayAutoInclude: "off"}})
	if r.Status != 200 {
		t.Fatalf("set relay switch: %d %s", r.Status, r.Body)
	}
	// A fresh subscription keeps the switch's meaning: detection alone does not
	// bind anything.
	sub2, _ := createSubscription(t, srv, cookie, "manual")
	bindEntries(t, srv, cookie, sub2, []subEntryInput{{NodeID: bID, Selected: true}})
	relay := relayEntryOf(t, listEntries(t, srv, cookie, sub2), bID)
	if relay.Selected {
		t.Errorf("with auto-include off, relay must stay unbound: %+v", relay)
	}
	if !relay.Discovered || !relay.Available {
		t.Errorf("it should still be offered as a discovered candidate: %+v", relay)
	}
}

// TestSubscriptionEntriesPayloadKeepsUnboundCandidatesAbsent guards the rule the
// picker depends on: listing an untouched candidate as "off" must not write a
// tombstone, or the first save would freeze auto-enrolment forever.
func TestSubscriptionEntriesPayloadKeepsUnboundCandidatesAbsent(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	subID, _ := createSubscription(t, srv, cookie, "main")

	// Operator saves "nothing checked" — A and B are candidates, not bindings.
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: aID, Selected: false},
		{NodeID: bID, Selected: true},
	})
	entries, err := api.Store.SubscriptionEntries(subID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].NodeID != bID || !entries[0].Enabled {
		t.Fatalf("entries = %+v, want only B bound", entries)
	}
}

// TestLegacyNodeIDsPathKeepsRelayEntries: §17 snapshots and old frontends still
// speak node_ids; that path owns the direct half only.
func TestLegacyNodeIDsPathKeepsRelayEntries(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})
	if relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID); !relay.Selected {
		t.Fatalf("precondition: relay should be auto-enrolled")
	}

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{bID}})
	if r.Status != 200 {
		t.Fatalf("legacy bind: %d %s", r.Status, r.Body)
	}
	entries, err := api.Store.SubscriptionEntries(subID)
	if err != nil {
		t.Fatal(err)
	}
	relays := 0
	for _, e := range entries {
		if e.RelayNodeID != "" && e.Enabled {
			relays++
		}
	}
	if relays != 1 {
		t.Errorf("legacy node_ids write dropped the relay entry: %+v", entries)
	}
}

// TestDirectEntryShadowedWarning: a node whose own forward steals its inbound
// port cannot be reached directly — reported (§21.9), never enforced.
func TestDirectEntryShadowedWarning(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	// A forwards its own anytls port to B: A's direct entry is gone.
	seedForward(t, api, aID, "tcp", 20001, "198.51.100.7", 20002)

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: aID, Selected: true}})

	var direct subEntryView
	found := false
	for _, e := range listEntries(t, srv, cookie, subID) {
		if e.NodeID == aID && e.RelayNodeID == "" {
			direct, found = e, true
		}
	}
	if !found {
		t.Fatal("A's direct entry missing from the picker")
	}
	if direct.Warning != "shadowed" {
		t.Errorf("A direct entry warning = %q, want shadowed", direct.Warning)
	}
	if !direct.Available {
		t.Errorf("shadowing is a warning, not an unavailability: %+v", direct)
	}
	_ = bID
}

// TestRelayEntrySurvivesItsRelayNode: deleting the relay node leaves a bound
// entry that can no longer render. It must stay visible (and unbindable)
// instead of vanishing, and it must not take the direct entry down with it.
func TestRelayEntrySurvivesItsRelayNode(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: bID, Selected: true},
		{NodeID: bID, RelayNodeID: aID, Proto: "tcp", SrcPort: 8080, Selected: true},
	})
	if err := api.Store.DeleteNode(aID); err != nil {
		t.Fatalf("delete relay node: %v", err)
	}

	relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID)
	if relay.Available {
		t.Errorf("entry whose relay is gone must not claim availability: %+v", relay)
	}
	if got := outboundCount(t, srv, token); got != 1 {
		t.Errorf("outbound count = %d, want 1 (B direct only)", got)
	}
}

// TestRelayWorksWithANonAnytlsIngress is the other half of the landing-node
// rule: A needs no sing-box at all to be a relay (it only forwards packets), so
// a pure router still yields the "B via A" entry — while A itself is not
// offered as a node, because it would not render.
func TestRelayWorksWithANonAnytlsIngress(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	setAnytlsPassword(t, srv, cookie)

	// A: address + forward rule, no sing-box installed.
	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 0)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 20002)
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 20002)

	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	var sawADirect bool
	relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID)
	if !relay.Selected || !relay.Available {
		t.Fatalf("relay through a sing-box-less ingress = %+v", relay)
	}
	for _, e := range listEntries(t, srv, cookie, subID) {
		if e.NodeID == aID && e.RelayNodeID == "" {
			sawADirect = true
		}
	}
	if sawADirect {
		t.Error("a node without sing-box was offered as a direct entry")
	}
	if got := outboundCount(t, srv, token); got != 2 {
		t.Errorf("outbound count = %d, want 2 (B direct + B via A)", got)
	}
}

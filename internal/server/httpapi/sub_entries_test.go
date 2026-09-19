// Tests for design.md §10.2: relay entries derived from a probe's nftables
// forwards, auto-enrolment with tombstones, entry naming and the renderer.
package httpapi

import (
	"encoding/json"
	"fmt"
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

// seedDiscoveredConfig stores what the probe itself would report (§9.3): a
// config.json the panel never wrote — one-sing.sh's own file — plus the
// certificate bytes for its anytls listeners. The node keeps an unmanaged
// node_singbox row (port 0, no cert): exactly what a pure one-sing probe has.
func seedDiscoveredConfig(t *testing.T, api *Server, nodeID, configJSON string, certs map[int]string) {
	t.Helper()
	if err := api.Store.SetNodeSingboxLocal(nodeID, store.NodeSingboxLocal{
		LocalPresent: true, LocalRunning: true,
		ConfigJSON: configJSON, LocalHash: "hash-" + nodeID,
		AnytlsCerts: certs,
	}, api.Crypt); err != nil {
		t.Fatalf("seed local config: %v", err)
	}
}

// TestEntryProtocolsFollowTheReportedFile: the picker's protocol column is
// read from the probe's reported config (the running config is the authority,
// §9.3), not from the panel's managed state. A mixed anytls+vless probe lists
// each inbound with its own protocol, a node known only through the managed
// pair reads as anytls, and a row whose inbound is gone names nothing.
func TestEntryProtocolsFollowTheReportedFile(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001) // managed pair, no file yet
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"anytls","tag":"b-in","listen_port":28711,"users":[{"password":"pw"}]},
		{"type":"vless","tag":"b-vless","listen_port":16929,"users":[{"uuid":"b2f0a2f4-1111-2222-3333-444455556666"}]}
	]}`, map[int]string{28711: testCertPEM})
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 28711)

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	entries := listEntries(t, srv, cookie, subID)
	if got := directEntryAt(t, entries, aID, 20001).Protocols; len(got) != 1 || got[0] != "anytls" {
		t.Errorf("managed-pair row protocols = %v, want [anytls]", got)
	}
	if got := directEntryAt(t, entries, bID, 28711).Protocols; len(got) != 1 || got[0] != "anytls" {
		t.Errorf("anytls row protocols = %v, want [anytls]", got)
	}
	if got := directEntryAt(t, entries, bID, 16929).Protocols; len(got) != 1 || got[0] != "vless" {
		t.Errorf("vless row protocols = %v, want [vless]", got)
	}
	// A relay leg terminates on the target's anytls inbound — the only protocol
	// it can render as.
	if got := relayEntryOf(t, entries, bID).Protocols; len(got) != 1 || got[0] != "anytls" {
		t.Errorf("relay row protocols = %v, want [anytls]", got)
	}

	// The probe's file drops the anytls listener: that row must stop claiming
	// a protocol — the running config no longer serves one.
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"vless","tag":"b-vless","listen_port":16929,"users":[{"uuid":"b2f0a2f4-1111-2222-3333-444455556666"}]}
	]}`, nil)
	gone := directEntryAt(t, listEntries(t, srv, cookie, subID), bID, 28711)
	if gone.Available || gone.Reason != "inbound_gone" {
		t.Fatalf("stale row = %+v, want inbound_gone", gone)
	}
	if len(gone.Protocols) != 0 {
		t.Errorf("stale row protocols = %v, want empty", gone.Protocols)
	}
}

// TestRelayEntryFromDiscoveredConfig is the one-sing.sh pairing: neither node
// was ever installed by the panel — both sing-boxes are the script's own, so
// the panel knows them only through discovery (reported config.json, no
// managed port or certificate). A's forward onto B's anytls inbound must still
// yield the "B via A" entry, and B's direct entry must be offered as
// available, because the renderer serves both from the reported file.
func TestRelayEntryFromDiscoveredConfig(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 0) // no managed sing-box
	seedDiscoveredConfig(t, api, aID, `{"inbounds":[
		{"type":"anytls","tag":"a-in","listen_port":21001,"users":[{"password":"a-pw"}]}
	]}`, map[int]string{21001: testCertPEM})
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"anytls","tag":"b-in","listen_port":28711,"users":[{"password":"script-pw"}]},
		{"type":"vless","tag":"b-vless","listen_port":16929,"users":[{"uuid":"b2f0a2f4-1111-2222-3333-444455556666"}]}
	]}`, map[int]string{28711: testCertPEM})
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 28711)

	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	entries := listEntries(t, srv, cookie, subID)
	relay := relayEntryOf(t, entries, bID)
	if relay.RelayNodeID != aID || relay.SrcPort != 8080 || relay.Proto != "tcp" {
		t.Fatalf("relay entry = %+v, want relay=%s src_port=8080 tcp", relay, aID)
	}
	if !relay.Available || !relay.Selected {
		t.Errorf("discovered relay entry must be offered and auto-enrolled: %+v", relay)
	}
	var direct subEntryView
	found := false
	for _, e := range entries {
		if e.NodeID == bID && e.RelayNodeID == "" {
			direct, found = e, true
		}
	}
	if !found || !direct.Available || direct.Reason != "" {
		t.Errorf("discovered direct entry = %+v, want available without a reason", direct)
	}

	body := string(fetchSub(t, srv, token, "", "").Body)
	for _, want := range []string{"fobe-B", "fobe-B · A:8080", `"server_port": 8080`} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered subscription missing %q\n%s", want, body)
		}
	}
	if got := strings.Count(body, "BEGIN CERTIFICATE"); got != 2 {
		t.Errorf("pinned certificates = %d, want 2 (B direct + B via A)\n%s", got, body)
	}
}

// TestRelayPinsTheInboundTheRuleLandsOn: listeners are equal peers and may
// carry different certificates, so the relayed outbound must pin the cert of
// the anytls inbound the rule's *destination* names — not just the first one
// in the file.
func TestRelayPinsTheInboundTheRuleLandsOn(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	certA, certB := "-----BEGIN CERTIFICATE-----\nPORT28711\n-----END CERTIFICATE-----\n",
		"-----BEGIN CERTIFICATE-----\nPORT28811\n-----END CERTIFICATE-----\n"

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 0)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"anytls","tag":"b-1","listen_port":28711,"users":[{"password":"pw-1"}]},
		{"type":"anytls","tag":"b-2","listen_port":28811,"users":[{"password":"pw-2"}]}
	]}`, map[int]string{28711: certA, 28811: certB})
	seedForward(t, api, aID, "tcp", 8081, "198.51.100.7", 28811)

	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})
	if relay := relayEntryOf(t, listEntries(t, srv, cookie, subID), bID); !relay.Available {
		t.Fatalf("relay onto the second anytls inbound = %+v", relay)
	}

	body := string(fetchSub(t, srv, token, "", "").Body)
	if !strings.Contains(body, "fobe-B · A:8081") {
		t.Fatalf("relay outbound missing\n%s", body)
	}
	// The relayed outbound is the third anytls block; what matters is that the
	// payload as a whole carries both certificates and the relayed entry pins
	// the 28811 one. Count pins: 3 outbounds, exactly one may carry 28811's
	// cert body — and it must sit in the block whose server_port is 8081.
	if got := strings.Count(body, "PORT28811"); got != 2 {
		t.Errorf("PORT28811 occurrences = %d, want 2 (certificate + sha256 pin line)\n%s", got, body)
	}
	if !strings.Contains(body, "PORT28711") {
		t.Errorf("the direct 28711 inbound lost its certificate\n%s", body)
	}
}

// TestRelayEntryAutoEnrolAndRender is the §10.2 headline: A forwards to B's
// anytls inbound, so a subscription binding B grows a third entry — B through
// A — which renders as a second outbound pointing at A while still pinning B's
// certificate (DNAT is layer 4; TLS terminates on B).
func TestRelayEntryAutoEnrolAndRender(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

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

// --- §9.3/§10.2 实现修订 2026-09-18: a direct entry is one *inbound* ---

// directEntryAt finds a direct row of one node by dial port (0 = the legacy
// node-level row).
func directEntryAt(t *testing.T, entries []subEntryView, nodeID string, port int) subEntryView {
	t.Helper()
	for _, e := range entries {
		if e.NodeID == nodeID && e.RelayNodeID == "" && e.SrcPort == port {
			return e
		}
	}
	t.Fatalf("no direct entry for node %s port %d in %+v", nodeID, port, entries)
	return subEntryView{}
}

// directEntriesOf lists a node's direct rows, in the order the server emits.
func directEntriesOf(entries []subEntryView, nodeID string) []subEntryView {
	out := []subEntryView{}
	for _, e := range entries {
		if e.NodeID == nodeID && e.RelayNodeID == "" {
			out = append(out, e)
		}
	}
	return out
}

// twoInboundConfig is one probe serving two anytls listeners, which is the
// whole point of the split: they are two entries, not one node.
const twoInboundConfig = `{"inbounds":[
	{"type":"anytls","tag":"anytls-28711","listen_port":28711,"users":[{"password":"pw-a"}]},
	{"type":"anytls","tag":"anytls-28712","listen_port":28712,"users":[{"password":"pw-b"}]}
]}`

// TestDirectEntriesAreOneRowPerInbound: a probe with two anytls listeners is
// offered as two separately selectable rows, named apart by port, and each one
// renders its own outbound. Before the split the picker had a single "B" row
// whose two outbounds were only told apart by the renderer's -2 suffix.
func TestDirectEntriesAreOneRowPerInbound(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, twoInboundConfig, map[int]string{
		28711: testCertPEM, 28712: testCertPEM,
	})
	subID, token := createSubscription(t, srv, cookie, "main")
	// The operator binds the node through the legacy shape (no port): the
	// reconciler has to split that row into the two inbounds.
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	rows := directEntriesOf(listEntries(t, srv, cookie, subID), bID)
	if len(rows) != 2 || rows[0].SrcPort != 28711 || rows[1].SrcPort != 28712 {
		t.Fatalf("direct rows = %+v, want one per inbound port", rows)
	}
	for _, r := range rows {
		if !r.Selected || !r.Available {
			t.Errorf("row %d should be bound and available: %+v", r.SrcPort, r)
		}
	}
	if rows[0].AutoName != "B:28711" || rows[1].AutoName != "B:28712" {
		t.Errorf("auto names = %q/%q, want B:28711 and B:28712", rows[0].AutoName, rows[1].AutoName)
	}

	body := string(fetchSub(t, srv, token, "", "").Body)
	if got := outboundCount(t, srv, token); got != 2 {
		t.Fatalf("outbound count = %d, want 2\n%s", got, body)
	}
	for _, want := range []string{"fobe-B:28711", "fobe-B:28712", `"server_port": 28711`, `"server_port": 28712`} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered subscription missing %q\n%s", want, body)
		}
	}
	// No dedupe suffix: the names are already distinct, which is the whole
	// reason the port comes back after 2026-09-17j removed it.
	if strings.Contains(body, "fobe-B-2") || strings.Contains(body, "fobe-B \"") {
		t.Errorf("the portless name is still rendering alongside:\n%s", body)
	}

	// Unchecking one inbound leaves the other alone: that is what "separately
	// selectable" has to mean.
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: bID, SrcPort: 28711, Selected: true},
		{NodeID: bID, SrcPort: 28712, Selected: false},
	})
	if got := outboundCount(t, srv, token); got != 1 {
		t.Fatalf("outbound count after unbinding one inbound = %d, want 1", got)
	}
	if body := string(fetchSub(t, srv, token, "", "").Body); !strings.Contains(body, `"server_port": 28711`) {
		t.Errorf("the surviving inbound is not the one still checked:\n%s", body)
	}
}

// TestSingleInboundKeepsThePlainName: the suffix is conditional, not a return of
// the unconditional `协议:端口` that 2026-09-17j removed — a node with one
// inbound still renders the operator's subscription name verbatim.
func TestSingleInboundKeepsThePlainName(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"anytls","tag":"anytls-28711","listen_port":28711,"users":[{"password":"pw-a"}]}
	]}`, map[int]string{28711: testCertPEM})
	if err := api.Store.SetNodeSubName(bID, "东京"); err != nil {
		t.Fatalf("set sub name: %v", err)
	}
	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, SrcPort: 28711, Selected: true}})

	row := directEntryAt(t, listEntries(t, srv, cookie, subID), bID, 28711)
	if row.AutoName != "东京" {
		t.Errorf("auto name = %q, want the subscription name verbatim", row.AutoName)
	}
	body := string(fetchSub(t, srv, token, "", "").Body)
	if !strings.Contains(body, `"tag": "fobe-东京"`) {
		t.Errorf("single-inbound node lost its plain name:\n%s", body)
	}
	// An explicit alias still wins over the suffix, on a node with two inbounds.
	if err := api.Store.SetNodeSubName(bID, ""); err != nil {
		t.Fatal(err)
	}
	seedDiscoveredConfig(t, api, bID, twoInboundConfig, map[int]string{
		28711: testCertPEM, 28712: testCertPEM,
	})
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: bID, SrcPort: 28711, Alias: "东京-主", Selected: true},
		{NodeID: bID, SrcPort: 28712, Selected: true},
	})
	body = string(fetchSub(t, srv, token, "", "").Body)
	for _, want := range []string{"fobe-东京-主", "fobe-B:28712"} {
		if !strings.Contains(body, want) {
			t.Errorf("alias is not verbatim / suffix missing: %q\n%s", want, body)
		}
	}
}

// TestBoundEntryWhoseInboundDisappeared: the probe's file is the truth, so a
// bound row naming a listener that is no longer declared is listed, marked and
// renders nothing — the operator keeps the binding visible and can drop it.
func TestBoundEntryWhoseInboundDisappeared(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, twoInboundConfig, map[int]string{
		28711: testCertPEM, 28712: testCertPEM,
	})
	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: bID, SrcPort: 28711, Selected: true},
		{NodeID: bID, SrcPort: 28712, Selected: true},
	})
	if got := outboundCount(t, srv, token); got != 2 {
		t.Fatalf("precondition: outbound count = %d, want 2", got)
	}

	// The probe loses one listener (hand-edited file, a `jq` rewrite).
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"anytls","tag":"anytls-28711","listen_port":28711,"users":[{"password":"pw-a"}]}
	]}`, map[int]string{28711: testCertPEM})

	rows := directEntriesOf(listEntries(t, srv, cookie, subID), bID)
	if len(rows) != 2 {
		t.Fatalf("bound rows = %+v, want both kept on screen", rows)
	}
	gone := directEntryAt(t, listEntries(t, srv, cookie, subID), bID, 28712)
	if gone.Available || gone.Reason != "inbound_gone" {
		t.Errorf("stale row = %+v, want available=false reason=inbound_gone", gone)
	}
	if !gone.Selected {
		t.Errorf("the binding was dropped instead of reported: %+v", gone)
	}
	// The surviving inbound is still served, and the gone one is not: one
	// outbound, at the port the probe actually declares.
	if got := outboundCount(t, srv, token); got != 1 {
		t.Fatalf("outbound count = %d, want 1", got)
	}
	if body := string(fetchSub(t, srv, token, "", "").Body); !strings.Contains(body, `"server_port": 28711`) {
		t.Errorf("the wrong inbound survived:\n%s", body)
	}
}

// TestPortMoveCarriesSubscriptionEntries: moving a listener's port in the editor
// must not silently drop the node from every subscription that bound it. The
// rows follow the move (name included), and the transient window — the probe
// still serving the old port until it applies the document — is reported as
// inbound_gone rather than rendering a port that is about to disappear.
func TestPortMoveCarriesSubscriptionEntries(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, twoInboundConfig, map[int]string{
		28711: testCertPEM, 28712: testCertPEM,
	})
	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: bID, SrcPort: 28711, Alias: "东京", Selected: true},
		{NodeID: bID, SrcPort: 28712, Selected: false},
	})

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+bID+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": "hash-" + bID, // seedDiscoveredConfig's stored hash
			"update": []map[string]any{
				{"number": 0, "type": "anytls", "tag": "anytls-28711", "port": 28811},
			},
		})
	if r.Status != http.StatusOK {
		t.Fatalf("port move: %d %s", r.Status, r.Body)
	}

	moved := directEntryAt(t, listEntries(t, srv, cookie, subID), bID, 28811)
	if !moved.Selected || moved.Alias != "东京" {
		t.Errorf("the binding did not follow the move: %+v", moved)
	}
	// The probe has not applied the document yet: the new port is not served, so
	// the row says so instead of pretending.
	if moved.Available || moved.Reason != "inbound_gone" {
		t.Errorf("row before the report = %+v, want available=false reason=inbound_gone", moved)
	}

	// The agent applies it and reports the new file (with the certificate for
	// the new listener, which is what makes pinning possible).
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"anytls","tag":"anytls-28811","listen_port":28811,"users":[{"password":"pw-a"}]},
		{"type":"anytls","tag":"anytls-28712","listen_port":28712,"users":[{"password":"pw-b"}]}
	]}`, map[int]string{28811: testCertPEM, 28712: testCertPEM})

	moved = directEntryAt(t, listEntries(t, srv, cookie, subID), bID, 28811)
	if !moved.Selected || !moved.Available {
		t.Fatalf("row after the report = %+v, want bound and available", moved)
	}
	if moved.AutoName != "B:28811" {
		t.Errorf("auto name = %q, want B:28811", moved.AutoName)
	}
	body := string(fetchSub(t, srv, token, "", "").Body)
	if !strings.Contains(body, "fobe-东京") || !strings.Contains(body, `"server_port": 28811`) {
		t.Errorf("the moved inbound is not rendered at its new port:\n%s", body)
	}
}

// TestDuplicateRelayRulesAreOneEntry: two nftables rules can share one tuple
// (§21 keeps the handle so they stay editable apart), but for the subscription
// they describe the same ingress. The picker used to list them as two rows of
// one identity; the panel keys rows by identity, so a name typed into one row
// was written into the other too, and the save came back alias_conflict for two
// names the operator believed were different (2026-09-19 修正).
func TestDuplicateRelayRulesAreOneEntry(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, `{"inbounds":[
		{"type":"anytls","tag":"anytls-28711","listen_port":28711,"users":[{"password":"pw-a"}]}
	]}`, map[int]string{28711: testCertPEM})

	// Same source tuple, different handle and match terms: one entry.
	status := store.NodeForwardStatus{Supported: true, Initialized: true, ReportedAt: 1700000000}
	rows := []store.NodeForward{
		{Handle: 10, Proto: "tcp", SrcPort: 8080, DstIP: "198.51.100.7", DstPort: 28711},
		{Handle: 11, Proto: "tcp", SrcPort: 8080, DstIP: "198.51.100.7", DstPort: 28711, Comment: "双份规则", ExtraMatch: true},
	}
	if err := api.Store.ReplaceNodeForwards(aID, status, rows); err != nil {
		t.Fatalf("seed forwards: %v", err)
	}

	subID, _ := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: bID, Selected: true}})

	entries := listEntries(t, srv, cookie, subID)
	counts := map[string]int{}
	for _, e := range entries {
		counts[identityOf(e)]++
	}
	for key, n := range counts {
		if n > 1 {
			t.Errorf("entry %s listed %d times, want once\n%+v", key, n, entries)
		}
	}
	if relay := relayEntryOf(t, entries, bID); relay.Source != "双份规则" {
		t.Errorf("relay source = %q, want the later rule's comment when the first had none", relay.Source)
	}
}

// TestDifferentRelaysStayTwoEntries: the same target reached through two
// *different* relays is two entries, not one. The picker keys rows by identity
// (`node|relay|proto|src_port|iface`), so deduping candidates by identity must
// never collapse rows whose relay differs — the operator sees "C · A:8080" and
// "C · B:9090" and has to be able to name them apart.
func TestDifferentRelaysStayTwoEntries(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 20001)
	bID := seedNodeWithIP(t, api, "B", "machine-b", "203.0.113.20", 20002)
	cID := seedNodeWithIP(t, api, "C", "machine-c", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, cID, `{"inbounds":[
		{"type":"anytls","tag":"anytls-28711","listen_port":28711,"users":[{"password":"pw-a"}]}
	]}`, map[int]string{28711: testCertPEM})
	seedForward(t, api, aID, "tcp", 8080, "198.51.100.7", 28711)
	seedForward(t, api, bID, "tcp", 9090, "198.51.100.7", 28711)

	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: cID, Selected: true}})

	relays := map[string]subEntryView{}
	for _, e := range listEntries(t, srv, cookie, subID) {
		if e.NodeID == cID && e.RelayNodeID != "" {
			relays[e.RelayNodeID] = e
		}
	}
	if len(relays) != 2 {
		t.Fatalf("relay rows = %+v, want one per relay node", relays)
	}
	if relays[aID].AutoName != "C · A:8080" || relays[bID].AutoName != "C · B:9090" {
		t.Errorf("auto names = %q / %q, want one per relay", relays[aID].AutoName, relays[bID].AutoName)
	}

	// Distinct names save, and both render.
	bindEntries(t, srv, cookie, subID, []subEntryInput{
		{NodeID: cID, Selected: true},
		{NodeID: cID, RelayNodeID: aID, Proto: "tcp", SrcPort: 8080, Alias: "经A", Selected: true},
		{NodeID: cID, RelayNodeID: bID, Proto: "tcp", SrcPort: 9090, Alias: "经B", Selected: true},
	})
	body := string(fetchSub(t, srv, token, "", "").Body)
	for _, want := range []string{"fobe-经A", "fobe-经B"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in the rendered config:\n%s", want, body)
		}
	}
}

// identityOf renders a picker row the way both sides key it.
func identityOf(e subEntryView) string {
	return fmt.Sprintf("%s|%s|%s|%d|%s", e.NodeID, e.RelayNodeID, e.Proto, e.SrcPort, e.Iface)
}

// TestAliasConflictScope: what the save-time duplicate-name rule covers after
// the 2026-09-19 revision (§10.2). Only entries the operator enabled can
// collide (a tombstone renders nothing); the name is compared as a string, so
// the node (or relay) an entry belongs to is irrelevant — two relays to one
// target are checked like any other pair; and the same entry listed twice in one
// payload is one entry, not two claims on a name.
func TestAliasConflictScope(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	aID := seedNodeWithIP(t, api, "A", "machine-a", "203.0.113.10", 0)
	seedDiscoveredConfig(t, api, aID, `{"inbounds":[
		{"type":"anytls","tag":"anytls-28711","listen_port":28711,"users":[{"password":"pw-a"}]}
	]}`, map[int]string{28711: testCertPEM})
	bID := seedNodeWithIP(t, api, "B", "machine-b", "198.51.100.7", 0)
	seedDiscoveredConfig(t, api, bID, twoInboundConfig, map[int]string{28711: testCertPEM, 28712: testCertPEM})

	subID, _ := createSubscription(t, srv, cookie, "main")

	// The name is compared as a string, whatever the node: two inbounds of one
	// node may not share one either — the renderer would suffix the loser with
	// "-2", which is exactly what a template author cannot predict.
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"entries": []subEntryInput{
			{NodeID: bID, SrcPort: 28711, Alias: "同机", Selected: true},
			{NodeID: bID, SrcPort: 28712, Alias: "同机", Selected: true},
		}})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "alias_conflict" {
		t.Fatalf("same-node duplicate: %d %s, want 400 alias_conflict", r.Status, r.Body)
	}

	// Same name on two different nodes: also refused, and the entries are
	// unrelated to each other (see TestDifferentRelaysStayTwoEntries for the
	// relay case, where different relays are different entries by identity).
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"entries": []subEntryInput{
			{NodeID: aID, SrcPort: 28711, Alias: "同机", Selected: true},
			{NodeID: bID, SrcPort: 28711, Alias: "同机", Selected: true},
		}})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "alias_conflict" {
		t.Fatalf("cross-node duplicate: %d %s, want 400 alias_conflict", r.Status, r.Body)
	}

	// A name parked on an unchecked row collides with nothing.
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"entries": []subEntryInput{
			{NodeID: aID, SrcPort: 28711, Alias: "同机", Selected: false},
			{NodeID: bID, SrcPort: 28711, Alias: "同机", Selected: true},
		}})
	if r.Status != http.StatusOK {
		t.Fatalf("tombstoned name: %d %s, want 200", r.Status, r.Body)
	}

	// One entry sent twice is one entry.
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"entries": []subEntryInput{
			{NodeID: bID, SrcPort: 28711, Alias: "独一份", Selected: true},
			{NodeID: bID, SrcPort: 28711, Alias: "独一份", Selected: true},
		}})
	if r.Status != http.StatusOK {
		t.Fatalf("duplicate identity: %d %s, want 200", r.Status, r.Body)
	}
	if got := directEntriesOf(listEntries(t, srv, cookie, subID), bID); len(got) != 2 {
		t.Errorf("direct rows after the deduped save = %+v, want the node's two inbounds", got)
	}
}

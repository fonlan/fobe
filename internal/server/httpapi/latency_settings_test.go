package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

func TestLatencyIntervalDefaultsAndPushesToOnlineAgent(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, secret := seedNode(t, api, "latency", "m-latency", "198.51.100.5")

	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	headers := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
	}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var initial protocol.Envelope
	if err := ws.ReadJSON(&initial); err != nil {
		t.Fatalf("read initial hello_ack: %v", err)
	}
	if initial.Type != protocol.TypeHelloAck {
		t.Fatalf("initial type = %q, want hello_ack", initial.Type)
	}
	var ack protocol.HelloAck
	if err := json.Unmarshal(initial.Payload, &ack); err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	if ack.LatencyIntervalSec != 5 {
		t.Fatalf("default interval = %d, want 5", ack.LatencyIntervalSec)
	}

	req, _ := http.NewRequest(
		http.MethodPut,
		srv.URL+"/api/settings",
		bytes.NewReader([]byte(`{"settings":{"latency.interval_seconds":"17"}}`)),
	)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("set latency interval: %v %d", err, statusCode(resp))
	}
	resp.Body.Close()

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var pushed protocol.Envelope
	if err := ws.ReadJSON(&pushed); err != nil {
		t.Fatalf("read latency config: %v", err)
	}
	if pushed.Type != protocol.TypeLatencyCfg {
		t.Fatalf("pushed type = %q, want %q", pushed.Type, protocol.TypeLatencyCfg)
	}
	var cfg protocol.LatencyConfig
	if err := json.Unmarshal(pushed.Payload, &cfg); err != nil {
		t.Fatalf("decode latency config: %v", err)
	}
	if cfg.IntervalSec != 17 {
		t.Fatalf("pushed interval = %d, want 17", cfg.IntervalSec)
	}
}

// Regression (design §13 实现修订 2026-09-17k): editing a node's latency
// targets used to write node_latency_targets and push nothing — the list only
// rode hello_ack, so a connected probe went on probing its stale list and the
// detail page never grew a line for the new target.
func TestNodeLatencyTargetEditPushesToOnlineAgent(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, secret := seedNode(t, api, "latency-edit", "m-latency-edit", "198.51.100.9")
	tcpID, err := api.Store.CreateLatencyTarget("edge", "tcp", "203.0.113.10", 443)
	if err != nil {
		t.Fatal(err)
	}
	icmpID, err := api.Store.CreateLatencyTarget("backbone", "icmp", "203.0.113.11", 0)
	if err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	headers := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
	}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var initial protocol.Envelope
	if err := ws.ReadJSON(&initial); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	if initial.Type != protocol.TypeHelloAck {
		t.Fatalf("initial type = %q, want hello_ack", initial.Type)
	}
	var ack protocol.HelloAck
	if err := json.Unmarshal(initial.Payload, &ack); err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	if len(ack.LatencyTargets) != 0 {
		t.Fatalf("fresh node should probe nothing: %+v", ack.LatencyTargets)
	}

	body := []byte(fmt.Sprintf(`{"latency_target_ids":[%d,%d]}`, tcpID, icmpID))
	if resp, _ := doAuthed(t, http.MethodPatch, srv.URL+"/api/nodes/"+nodeID, cookie, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("update node: %d", resp.StatusCode)
	}

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var pushed protocol.Envelope
	if err := ws.ReadJSON(&pushed); err != nil {
		t.Fatalf("read pushed latency_config: %v", err)
	}
	if pushed.Type != protocol.TypeLatencyCfg {
		t.Fatalf("pushed type = %q, want latency_config", pushed.Type)
	}
	var cfg protocol.LatencyConfig
	if err := json.Unmarshal(pushed.Payload, &cfg); err != nil {
		t.Fatalf("decode latency_config: %v", err)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("pushed targets = %+v, want both ids", cfg.Targets)
	}
	got := map[int64]bool{cfg.Targets[0].ID: true, cfg.Targets[1].ID: true}
	if !got[tcpID] || !got[icmpID] {
		t.Fatalf("pushed targets missing ids: %+v", cfg.Targets)
	}

	// Clearing the selection must arrive as an explicit empty list: an omitted
	// field would mean "keep probing" and the node could never opt out.
	if resp, _ := doAuthed(t, http.MethodPatch, srv.URL+"/api/nodes/"+nodeID, cookie,
		[]byte(`{"latency_target_ids":[]}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("clear targets: %d", resp.StatusCode)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ws.ReadJSON(&pushed); err != nil {
		t.Fatalf("read cleared latency_config: %v", err)
	}
	if pushed.Type != protocol.TypeLatencyCfg {
		t.Fatalf("cleared type = %q, want latency_config", pushed.Type)
	}
	if err := json.Unmarshal(pushed.Payload, &cfg); err != nil {
		t.Fatalf("decode cleared latency_config: %v", err)
	}
	if cfg.Targets == nil || len(cfg.Targets) != 0 {
		t.Fatalf("cleared targets = %#v, want empty non-nil", cfg.Targets)
	}
}

// Same revision: deleting a global target must stop the probes that still
// select it, not wait for a reconnect that may never come.
func TestDeleteLatencyTargetPushesUpdatedListToOnlineAgent(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, secret := seedNode(t, api, "latency-del", "m-latency-del", "198.51.100.10")
	targetID, err := api.Store.CreateLatencyTarget("gone", "icmp", "203.0.113.12", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetNodeLatencyTargets(nodeID, []int64{targetID}); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	headers := http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {secret},
	}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var initial protocol.Envelope
	if err := ws.ReadJSON(&initial); err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}

	if resp, _ := doAuthed(t, http.MethodDelete, fmt.Sprintf("%s/api/latency-targets/%d", srv.URL, targetID), cookie, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete target: %d", resp.StatusCode)
	}

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var pushed protocol.Envelope
	if err := ws.ReadJSON(&pushed); err != nil {
		t.Fatalf("read pushed latency_config: %v", err)
	}
	if pushed.Type != protocol.TypeLatencyCfg {
		t.Fatalf("pushed type = %q, want latency_config", pushed.Type)
	}
	var cfg protocol.LatencyConfig
	if err := json.Unmarshal(pushed.Payload, &cfg); err != nil {
		t.Fatalf("decode latency_config: %v", err)
	}
	if cfg.Targets == nil || len(cfg.Targets) != 0 {
		t.Fatalf("targets after delete = %#v, want empty non-nil", cfg.Targets)
	}
}

func TestLatencyIntervalRejectsOutOfRangeValues(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := panelCookie(t, srv)

	for _, value := range []string{"0", "3601", "five"} {
		t.Run(value, func(t *testing.T) {
			req, _ := http.NewRequest(
				http.MethodPut,
				srv.URL+"/api/settings",
				bytes.NewBufferString(`{"settings":{"latency.interval_seconds":"`+value+`"}}`),
			)
			req.Header.Set("Cookie", cookie)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("interval %q accepted: %v %d", value, err, statusCode(resp))
			}
			resp.Body.Close()
		})
	}
}

func TestNodeListExposesBillingConfiguredAndDueTime(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "billing", "m-billing", "198.51.100.7")
	due := time.Now().Add(48 * time.Hour).Unix()

	if err := api.Store.UpsertNodeBilling(&store.NodeBilling{
		NodeID: nodeID, CycleType: "none", NextDueAt: &due, Note: "¥30/月",
	}); err != nil {
		t.Fatal(err)
	}
	_, list := authedGet(t, srv, cookie, "/api/nodes")
	nodes := list["nodes"].([]any)
	node := nodes[0].(map[string]any)
	if node["billing_configured"] != false {
		t.Fatalf("cycle none should be unconfigured: %#v", node)
	}
	// 费用 is independent of the cycle type: the overview card tags it whenever
	// the operator filled it in, even when the cycle is "none".
	if node["billing_note"] != "¥30/月" {
		t.Fatalf("billing note missing from node list: %#v", node)
	}

	if err := api.Store.UpsertNodeBilling(&store.NodeBilling{
		NodeID: nodeID, CycleType: "month", CycleDays: ptrInt64(1), NextDueAt: &due,
	}); err != nil {
		t.Fatal(err)
	}
	_, list = authedGet(t, srv, cookie, "/api/nodes")
	nodes = list["nodes"].([]any)
	node = nodes[0].(map[string]any)
	if node["billing_configured"] != true || int64(node["next_due_at"].(float64)) != due {
		t.Fatalf("billing details missing from node list: %#v", node)
	}
}

func ptrInt64(value int64) *int64 { return &value }

// Regression: NodeBilling used to marshal with Go field names (no json tags),
// so the edit form read back blank right after a successful save.
func TestNodeDetailBillingRoundTripsThroughAPI(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "billing-rt", "m-billing-rt", "198.51.100.8")
	due := time.Now().Add(72 * time.Hour).Unix()

	body := fmt.Sprintf(
		`{"billing":{"cycle_type":"month","cycle_days":3,"next_due_at":%d,"note":"renew"}}`, due)
	if resp, _ := doAuthed(t, http.MethodPatch, srv.URL+"/api/nodes/"+nodeID, cookie, []byte(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("update node: %d", resp.StatusCode)
	}

	_, out := authedGet(t, srv, cookie, "/api/nodes/"+nodeID)
	b, ok := out["billing"].(map[string]any)
	if !ok {
		t.Fatalf("billing missing from node detail: %#v", out["billing"])
	}
	if b["cycle_type"] != "month" || int64(b["cycle_days"].(float64)) != 3 ||
		int64(b["next_due_at"].(float64)) != due || b["note"] != "renew" {
		t.Fatalf("billing keys/values wrong: %#v", b)
	}
}

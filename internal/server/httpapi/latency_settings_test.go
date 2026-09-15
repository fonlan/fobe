package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/store"
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
		NodeID: nodeID, CycleType: "none", NextDueAt: &due,
	}); err != nil {
		t.Fatal(err)
	}
	_, list := authedGet(t, srv, cookie, "/api/nodes")
	nodes := list["nodes"].([]any)
	node := nodes[0].(map[string]any)
	if node["billing_configured"] != false {
		t.Fatalf("cycle none should be unconfigured: %#v", node)
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

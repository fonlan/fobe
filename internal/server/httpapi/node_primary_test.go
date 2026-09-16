// Tests for design §14 manual primary IP: PUT /api/nodes/{id}/primary-ip
// validates against the reported set, and a manual pick survives full agent
// IP replacements (hub onState → store.ReplaceNodeIPs).
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

func TestSetPrimaryIPValidatesAgainstReportedSet(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "pin", "m-pin-1", "198.51.100.5")
	if err := api.Store.ReplaceNodeIPs(nodeID, []store.IPRow{
		{IP: "198.51.100.5", Family: 4, Scope: "public"},
		{IP: "198.51.100.6", Family: 4, Scope: "public"},
	}); err != nil {
		t.Fatal(err)
	}

	// an IP the node never reported is refused
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/primary-ip", cookie,
		map[string]string{"ip": "203.0.113.99"})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "ip_not_reported" {
		t.Fatalf("unreported ip: got %d %s, want 400 ip_not_reported", r.Status, r.Body)
	}

	// pinning a reported one updates node primary + row flags
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/primary-ip", cookie,
		map[string]string{"ip": "198.51.100.6"})
	if r.Status != 200 {
		t.Fatalf("set primary: %d %s", r.Status, r.Body)
	}
	detail := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID, cookie, nil)
	m := detail.JSONMap(t)
	if m["node"].(map[string]any)["primary_ip"] != "198.51.100.6" {
		t.Fatalf("primary_ip not updated: %s", detail.Body)
	}
	primaries := 0
	for _, ipv := range m["ips"].([]any) {
		ip := ipv.(map[string]any)
		if ip["is_primary"] == true {
			primaries++
			if ip["ip"] != "198.51.100.6" {
				t.Fatalf("wrong row flagged primary: %s", detail.Body)
			}
		}
	}
	if primaries != 1 {
		t.Fatalf("primaries = %d, want 1: %s", primaries, detail.Body)
	}

	// unknown node → 404
	r = doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/nope/primary-ip", cookie,
		map[string]string{"ip": "198.51.100.6"})
	if r.Status != http.StatusNotFound {
		t.Fatalf("unknown node: got %d, want 404", r.Status)
	}
}

func TestManualPrimarySurvivesAgentStateReport(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, secret := seedNode(t, api, "pin-e2e", "m-pin-2", "")

	// fake agent connects and reports its address set
	hdr := http.Header{"X-Fobe-Node-ID": {nodeID}, "X-Fobe-Node-Secret": {secret}}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()
	ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{MachineID: "m-pin-2", Version: "dev"}))
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var env protocol.Envelope
	for {
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatalf("read hello_ack: %v", err)
		}
		if env.Type == protocol.TypeHelloAck {
			break
		}
	}

	// reportIPs mimics the real agent (netinfo marks the first IP primary) and
	// waits until the frame is definitely ingested via a visible side effect.
	reportIPs := func(expectIPs int, ips ...protocol.IPInfo) {
		t.Helper()
		if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{IPs: ips})); err != nil {
			t.Fatalf("write state: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			detail := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID, cookie, nil)
			m := detail.JSONMap(t)
			if got := len(m["ips"].([]any)); got == expectIPs {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("state report not ingested")
	}

	reportIPs(2,
		protocol.IPInfo{IP: "198.51.100.5", Family: 4, Scope: "public", IsPrimary: true},
		protocol.IPInfo{IP: "198.51.100.6", Family: 4, Scope: "public"},
	)
	detail := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID, cookie, nil)
	var reported struct {
		Node struct {
			PrimaryIP string `json:"primary_ip"`
		} `json:"node"`
	}
	_ = json.Unmarshal(detail.Body, &reported)
	if reported.Node.PrimaryIP != "198.51.100.5" {
		t.Fatalf("agent-reported primary IP not persisted: %q", reported.Node.PrimaryIP)
	}

	// manual pin to the address the agent would not have picked
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/primary-ip", cookie,
		map[string]string{"ip": "198.51.100.6"})
	if r.Status != 200 {
		t.Fatalf("set primary: %d %s", r.Status, r.Body)
	}

	// the next full report (a third address appears, proving ingestion) must
	// keep the manual choice: hub only re-pins when the current primary is no
	// longer among the reported addresses, and the store restores the flag
	// after its rebuild (§14)
	reportIPs(3,
		protocol.IPInfo{IP: "198.51.100.5", Family: 4, Scope: "public", IsPrimary: true},
		protocol.IPInfo{IP: "198.51.100.6", Family: 4, Scope: "public"},
		protocol.IPInfo{IP: "198.51.100.7", Family: 4, Scope: "public"},
	)
	detail = doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID, cookie, nil)
	m := detail.JSONMap(t)
	var b struct {
		Node struct {
			PrimaryIP string `json:"primary_ip"`
		} `json:"node"`
	}
	_ = json.Unmarshal(detail.Body, &b)
	if b.Node.PrimaryIP != "198.51.100.6" {
		t.Fatalf("manual primary lost after agent report: primary=%q body=%s", b.Node.PrimaryIP, detail.Body)
	}
	primaries := 0
	for _, ipv := range m["ips"].([]any) {
		ip := ipv.(map[string]any)
		if ip["is_primary"] == true {
			primaries++
			if ip["ip"] != "198.51.100.6" {
				t.Fatalf("wrong row flagged primary: %s", detail.Body)
			}
		}
	}
	if primaries != 1 {
		t.Fatalf("primaries = %d, want 1 (198.51.100.6): %s", primaries, detail.Body)
	}
}

// TestPrimaryIPDriftHealsOnNextReport reproduces the field bug behind
// "服务器列表主IP列不显示用户选择": a build that overwrote nodes.primary_ip with
// the agent's suggestion (before the hub guard existed) leaves the drift
// inside the reported set, where the hub guard can never see it. The next full
// report must write the manual pick back into nodes.primary_ip.
func TestPrimaryIPDriftHealsOnNextReport(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, secret := seedNode(t, api, "pin-drift", "m-pin-3", "")

	hdr := http.Header{"X-Fobe-Node-ID": {nodeID}, "X-Fobe-Node-Secret": {secret}}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()
	ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{MachineID: "m-pin-3", Version: "dev"}))
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var env protocol.Envelope
	for {
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatalf("read hello_ack: %v", err)
		}
		if env.Type == protocol.TypeHelloAck {
			break
		}
	}

	waitPrimary := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			detail := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID, cookie, nil)
			m := detail.JSONMap(t)
			if got, _ := m["node"].(map[string]any)["primary_ip"].(string); got == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("primary_ip never became %q", want)
	}

	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{IPs: []protocol.IPInfo{
		{IP: "198.51.100.5", Family: 4, Scope: "public", IsPrimary: true},
		{IP: "198.51.100.6", Family: 4, Scope: "public"},
	}})); err != nil {
		t.Fatal(err)
	}
	waitPrimary("198.51.100.5")

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/primary-ip", cookie,
		map[string]string{"ip": "198.51.100.6"})
	if r.Status != 200 {
		t.Fatalf("set primary: %d %s", r.Status, r.Body)
	}
	waitPrimary("198.51.100.6")

	// simulate the old build's damage: primary_ip clobbered to the agent's
	// suggestion while manual_primary survives in node_ips
	if _, err := api.Store.Exec(`UPDATE nodes SET primary_ip = '198.51.100.5' WHERE id = ?`, nodeID); err != nil {
		t.Fatal(err)
	}

	// the very next full report must heal the list column, not just the flags
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{IPs: []protocol.IPInfo{
		{IP: "198.51.100.5", Family: 4, Scope: "public", IsPrimary: true},
		{IP: "198.51.100.6", Family: 4, Scope: "public"},
	}})); err != nil {
		t.Fatal(err)
	}
	waitPrimary("198.51.100.6")
}

// TestManualPrimaryKeptWhenAgentMarksNone covers agents that report no
// IsPrimary at all (old agents): the first-public-v4 fallback in hub.onState
// must not overwrite a still-reported manual pick, or it would fight the
// store's reconciliation on every report.
func TestManualPrimaryKeptWhenAgentMarksNone(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, secret := seedNode(t, api, "pin-nomark", "m-pin-4", "")

	hdr := http.Header{"X-Fobe-Node-ID": {nodeID}, "X-Fobe-Node-Secret": {secret}}
	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer ws.Close()
	ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{MachineID: "m-pin-4", Version: "dev"}))
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var env protocol.Envelope
	for {
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatalf("read hello_ack: %v", err)
		}
		if env.Type == protocol.TypeHelloAck {
			break
		}
	}

	waitPrimary := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			detail := doReq(t, &http.Client{}, "GET", srv.URL+"/api/nodes/"+nodeID, cookie, nil)
			m := detail.JSONMap(t)
			if got, _ := m["node"].(map[string]any)["primary_ip"].(string); got == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("primary_ip never became %q", want)
	}

	// no IsPrimary anywhere: the fallback pins the first public v4
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{IPs: []protocol.IPInfo{
		{IP: "198.51.100.5", Family: 4, Scope: "public"},
		{IP: "198.51.100.6", Family: 4, Scope: "public"},
	}})); err != nil {
		t.Fatal(err)
	}
	waitPrimary("198.51.100.5")

	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+nodeID+"/primary-ip", cookie,
		map[string]string{"ip": "198.51.100.6"})
	if r.Status != 200 {
		t.Fatalf("set primary: %d %s", r.Status, r.Body)
	}

	// the next no-mark report must leave the manual pick alone
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{IPs: []protocol.IPInfo{
		{IP: "198.51.100.5", Family: 4, Scope: "public"},
		{IP: "198.51.100.6", Family: 4, Scope: "public"},
	}})); err != nil {
		t.Fatal(err)
	}
	waitPrimary("198.51.100.6")
}

// End-to-end cover for the port-move flow (§9.3 实现修订 2026-09-18).
//
// The unit tests in config_editor_test.go assert what the panel *shows* once an
// edit has been recorded; this one drives the whole chain with a live agent
// session on a real websocket, because the move only completes when the pushed
// document actually reaches the probe: the panel's 添加中 row has to become
// 运行中 off the probe's own report, and the old row has to disappear.
package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/gorilla/websocket"
)

const portMoveConfig = `{
  "log": {"level": "info"},
  "inbounds": [
    {"type": "anytls", "tag": "anytls-in-22039", "listen": "::", "listen_port": 22039,
     "users": [{"password": "panel-generated-pw"}],
     "tls": {"enabled": true, "certificate_path": "/etc/one-sing/cert/cert.crt", "key_path": "/etc/one-sing/cert/private.key"}}
  ],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}`

func TestPortMoveReachesTheProbeAndConvergesOnThePanel(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	id, secret := seedNode(t, api, "HK-Sharon", "m-hk", "203.0.113.9")

	wsURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	ws, hsResp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Fobe-Node-ID":     {id},
		"X-Fobe-Node-Secret": {secret},
	})
	if err != nil {
		code := 0
		if hsResp != nil {
			code = hsResp.StatusCode
		}
		t.Fatalf("agent dial: %v (status %d)", err, code)
	}
	defer ws.Close()

	send := func(typ string, payload any) {
		t.Helper()
		if err := ws.WriteJSON(protocol.NewEnvelope(typ, "", payload)); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
	}
	readType := func(want string) protocol.Envelope {
		t.Helper()
		for {
			ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			var env protocol.Envelope
			if err := ws.ReadJSON(&env); err != nil {
				t.Fatalf("read %s: %v", want, err)
			}
			if env.Type == want {
				return env
			}
		}
	}
	// reportLocal is the agent's discovery frame: the file it just read, plus
	// the listeners it could reach locally.
	reportLocal := func(cfg string, effective []int) {
		t.Helper()
		send(protocol.TypeState, protocol.State{
			SingboxLocal: &protocol.SingboxLocal{
				Present: true, Running: true, UnitActive: true, UnitKnown: true,
				Version: "1.13.0-beta.7", ConfigPath: "/etc/one-sing/config.json",
				ConfigJSON: cfg, ConfigSHA256: hashOf(cfg),
				EffectiveInboundPorts: effective, InboundChecksKnown: true,
				AnytlsCerts: map[int]string{22039: testCertPEM, 22040: testCertPEM},
			},
		})
	}

	send(protocol.TypeHello, protocol.Hello{
		MachineID: "m-hk", Hostname: "hk-sharon", Version: "dev", OS: "linux", Arch: "amd64",
		IPs: []protocol.IPInfo{{IP: "203.0.113.9", Family: 4, Scope: "public", IsPrimary: true}},
	})
	readType(protocol.TypeHelloAck)
	reportLocal(portMoveConfig, []int{22039})

	// Wait for the report to land: the listener is confirmed running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := configRows(t, srv, cookie, id); got[22039] == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reported listener never became running: %v", configRows(t, srv, cookie, id))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The operator changes the port.
	r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/nodes/"+id+"/singbox/config", cookie,
		map[string]any{
			"reported_hash": hashOf(portMoveConfig),
			"update": []map[string]any{
				{"number": 0, "type": "anytls", "tag": "anytls-in-22039", "port": 22040},
			},
		})
	if r.Status != http.StatusOK {
		t.Fatalf("move port = %d %s", r.Status, r.Body)
	}

	// The page shows the move without a reload: the new port is pending and the
	// old one is retiring, even though the probe has reported nothing yet.
	got := configRows(t, srv, cookie, id)
	if got[22040] != "pending" || got[22039] != "deleting" {
		t.Fatalf("the panel does not show the move: %v", got)
	}

	// The push really reaches the probe, carrying the new port and the original
	// credential (the whole point of the merge).
	env := readType(protocol.TypeDesired)
	var desired protocol.DesiredState
	if err := json.Unmarshal(env.Payload, &desired); err != nil {
		t.Fatalf("decode desired frame: %v", err)
	}
	if desired.Singbox == nil {
		t.Fatal("desired frame carries no sing-box state")
	}
	if !strings.Contains(desired.Singbox.ConfigJSON, "22040") {
		t.Fatalf("desired config lost the new port: %s", desired.Singbox.ConfigJSON)
	}
	if !strings.Contains(desired.Singbox.ConfigJSON, "panel-generated-pw") {
		t.Fatalf("desired config lost the credential: %s", desired.Singbox.ConfigJSON)
	}
	if !strings.Contains(desired.Singbox.ConfigJSON, "22039") {
		t.Fatalf("the move should only rename the port, not drop the inbound: %s", desired.Singbox.ConfigJSON)
	}

	// The probe applies and reports the file it wrote: the move is confirmed.
	reportLocal(strings.Replace(portMoveConfig, `"listen_port": 22039`, `"listen_port": 22040`, 1), []int{22040})
	deadline = time.Now().Add(5 * time.Second)
	for {
		got = configRows(t, srv, cookie, id)
		if got[22040] == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the moved listener never became running: %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The retired row disappearing is a SEPARATE effect of the same reconcile as
	// the new port becoming running, and the two are not visible atomically.
	// Asserting it instantly made this test fail under full-suite load (the read
	// that saw "running" could still contain the retiring row). The contract is
	// "eventually gone" — the page polls — so bound the wait instead of assuming
	// one read sees both.
	retireDeadline := time.Now().Add(5 * time.Second)
	for {
		got = configRows(t, srv, cookie, id)
		if _, listed := got[22039]; !listed {
			break
		}
		if time.Now().After(retireDeadline) {
			t.Fatalf("the retired port is still on the page: %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/security"
	"github.com/gorilla/websocket"
)

func createTerminalTestNode(t *testing.T, srv *httptest.Server, cookie, machineID string) (*websocket.Conn, string) {
	t.Helper()
	token := freshToken(t, srv, cookie, "node-"+machineID)
	_, reg := postJSON(t, http.DefaultClient, srv.URL+"/api/agent/register", map[string]any{
		"token": token, "machine_id": machineID, "hostname": "host-" + machineID,
		"os": "linux", "arch": "amd64", "version": "dev", "tz": "UTC", "cpu_cores": 2,
	})
	nodeID, _ := reg["node_id"].(string)
	nodeSecret, _ := reg["node_secret"].(string)
	if nodeID == "" || nodeSecret == "" {
		t.Fatalf("agent register failed: %v", reg)
	}

	agentURL := "ws" + srv.URL[len("http"):] + "/ws/agent"
	agentWS, response, err := websocket.DefaultDialer.Dial(agentURL, http.Header{
		"X-Fobe-Node-ID":     {nodeID},
		"X-Fobe-Node-Secret": {nodeSecret},
	})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("agent dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { agentWS.Close() })
	if err := agentWS.WriteJSON(protocol.NewEnvelope(protocol.TypeHello, "", protocol.Hello{MachineID: machineID})); err != nil {
		t.Fatal(err)
	}
	agentWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ack protocol.Envelope
	if err := agentWS.ReadJSON(&ack); err != nil || ack.Type != protocol.TypeHelloAck {
		t.Fatalf("hello_ack: %v (type %s)", err, ack.Type)
	}
	return agentWS, nodeID
}

func terminalTestCookie(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	response, _ := postJSON(t, http.DefaultClient, srv.URL+"/api/login", map[string]string{"password": "test-password-123"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", response.StatusCode)
	}
	for _, cookie := range readCookies(response) {
		if cookie.Name == security.SessionCookieName {
			return cookie.Name + "=" + cookie.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func dialTerminalBrowser(t *testing.T, srv *httptest.Server, cookie, nodeID string) *websocket.Conn {
	t.Helper()
	url := "ws" + srv.URL[len("http"):] + "/ws/terminal?node=" + nodeID
	ws, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Cookie": {cookie}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("browser dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func TestTerminalRelayUsesAgentPTY(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := terminalTestCookie(t, srv)
	agentWS, nodeID := createTerminalTestNode(t, srv, cookie, "machine-terminal")
	browserWS := dialTerminalBrowser(t, srv, cookie, nodeID)

	// Deliberately use the old UI value: the server must normalize it to PTY
	// rather than asking for stored credentials or invoking an SSH backend.
	if err := browserWS.WriteJSON(protocol.NewEnvelope(protocol.TypeTermOpen, "", protocol.TerminalOpen{
		Mode: "ssh", Cols: 100, Rows: 30,
	})); err != nil {
		t.Fatal(err)
	}
	agentWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	var openEnvelope protocol.Envelope
	if err := agentWS.ReadJSON(&openEnvelope); err != nil {
		t.Fatal(err)
	}
	if openEnvelope.Type != protocol.TypeTermOpen {
		t.Fatalf("frame type = %q, want terminal_open", openEnvelope.Type)
	}
	var open protocol.TerminalOpen
	if err := json.Unmarshal(openEnvelope.Payload, &open); err != nil {
		t.Fatal(err)
	}
	if open.Mode != "pty" || open.SessionID == "" || open.Cols != 100 || open.Rows != 30 {
		t.Fatalf("agent terminal open = %#v", open)
	}

	if err := agentWS.WriteJSON(protocol.NewEnvelope(protocol.TypeTerminalOutput, "", protocol.TerminalOutput{
		SessionID: open.SessionID,
		Data:      "ready\r\n",
	})); err != nil {
		t.Fatal(err)
	}
	browserWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	var outputEnvelope protocol.Envelope
	if err := browserWS.ReadJSON(&outputEnvelope); err != nil {
		t.Fatal(err)
	}
	var output protocol.TerminalOutput
	if err := json.Unmarshal(outputEnvelope.Payload, &output); err != nil {
		t.Fatal(err)
	}
	if output.SessionID != open.SessionID || output.Data != "ready\r\n" {
		t.Fatalf("browser terminal output = %#v", output)
	}
}

func TestStampTerminalFrameRejectsInvalidInput(t *testing.T) {
	srv, api := newTestServer(t)
	defer srv.Close()

	env := protocol.NewEnvelope(protocol.TypeTermInput, "", protocol.TerminalInput{Data: string(make([]byte, maxTerminalBrowserFrameBytes+1))})
	closed := false
	if api.stampTerminalFrame("session", &env, &closed) {
		t.Fatal("oversize terminal input was accepted")
	}

	open := protocol.NewEnvelope(protocol.TypeTermOpen, "", protocol.TerminalOpen{Mode: "ssh", Cols: 80, Rows: 24})
	if !api.stampTerminalFrame("session", &open, &closed) {
		t.Fatal("terminal open was rejected")
	}
	var payload protocol.TerminalOpen
	if err := json.Unmarshal(open.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Mode != "pty" || payload.SessionID != "session" {
		t.Fatalf("stamped terminal open = %#v", payload)
	}
}

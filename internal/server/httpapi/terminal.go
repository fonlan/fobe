package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

// Browser terminal relay (design §11): xterm.js ↔ /ws/terminal?node=<id> ↔
// server ↔ agent. Frames are opaque JSON envelopes; the server only routes
// them and writes audit entries for session open/close.

var terminalUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     sameOrigin,
}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	nodeID := r.URL.Query().Get("node")
	if _, err := s.Store.GetNode(nodeID); err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if !s.Hub.IsOnline(nodeID) {
		writeErr(w, http.StatusConflict, "node_offline")
		return
	}
	ws, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	sessionID, err := security.RandomToken(8)
	if err != nil {
		return
	}
	subs, unsub := s.Hub.SubscribeTerminal(sessionID)
	defer unsub()

	ip := s.Trust.RealIP(r)
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: nodeID, Action: "terminal_open", SourceIP: ip,
	})
	defer s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: nodeID, Action: "terminal_close", SourceIP: ip,
	})

	// agent → browser, plus locally generated control frames (the ssh-mode
	// credential check below) on a second channel, so the read loop never
	// touches the socket concurrently with the writer goroutine.
	inject := make(chan protocol.Envelope, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case env := <-subs:
				if !writeTerminalFrame(ws, env) {
					return
				}
			case env := <-inject:
				if !writeTerminalFrame(ws, env) {
					return
				}
			}
		}
	}()

	// browser → agent; the server stamps the session id, the browser never picks it
	var closeSent bool
	for {
		var env protocol.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			break
		}
		switch env.Type {
		case protocol.TypeTermOpen:
			var p protocol.TerminalOpen
			_ = json.Unmarshal(env.Payload, &p)
			if p.Mode == "ssh" {
				// SSH mode (design §11): the server injects the stored
				// credentials just before the frame leaves for the agent.
				// Without usable credentials the frame is not forwarded at
				// all; the browser gets terminal_closed and the socket stays
				// open so it can retry (e.g. with local-PTY mode) once the
				// operator saved credentials on the node page.
				if err := s.injectSSH(nodeID, &p); err != nil {
					if !errors.Is(err, errSSHCredentialsMissing) {
						s.Log.Warn("ssh credential injection failed", "node_id", nodeID, "err", err)
					}
					select {
					case inject <- protocol.NewEnvelope(protocol.TypeTerminalClosed, sessionID, protocol.TerminalClosed{
						SessionID: sessionID,
						Reason:    reasonSSHCredentialsMissing,
					}):
					default:
					}
					continue
				}
			}
			p.SessionID = sessionID
			env.Payload, _ = json.Marshal(p)
		case protocol.TypeTermInput:
			var p protocol.TerminalInput
			_ = json.Unmarshal(env.Payload, &p)
			p.SessionID = sessionID
			env.Payload, _ = json.Marshal(p)
		case protocol.TypeTermResize:
			var p protocol.TerminalResize
			_ = json.Unmarshal(env.Payload, &p)
			p.SessionID = sessionID
			env.Payload, _ = json.Marshal(p)
		case protocol.TypeTermClose:
			var p protocol.TerminalClose
			_ = json.Unmarshal(env.Payload, &p)
			p.SessionID = sessionID
			env.Payload, _ = json.Marshal(p)
			closeSent = true
		default:
			continue
		}
		env.ID = sessionID
		env.TS = protocol.Now()
		s.Hub.Send(nodeID, env)
		if closeSent {
			break
		}
	}
	// Browser went away without an explicit close (tab close, network drop):
	// tell the agent so it tears the PTY down instead of leaking the shell.
	if !closeSent {
		closePayload, _ := json.Marshal(protocol.TerminalClose{SessionID: sessionID})
		s.Hub.Send(nodeID, protocol.Envelope{
			V: protocol.Version, Type: protocol.TypeTermClose, ID: sessionID,
			TS: protocol.Now(), Payload: closePayload,
		})
	}
	<-done
}

// writeTerminalFrame pushes one frame to the browser; false means the socket
// is gone and the writer goroutine should stop.
func writeTerminalFrame(ws *websocket.Conn, env protocol.Envelope) bool {
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteJSON(env) == nil
}

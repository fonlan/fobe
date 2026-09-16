package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

// Browser terminal relay (design §11): xterm.js ↔ /ws/terminal?node=<id> ↔
// server ↔ agent. The agent always creates its local PTY; the server only
// authorizes and routes opaque terminal frames.

const maxTerminalBrowserFrameBytes = 1 << 20

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
	ws.SetReadLimit(maxTerminalBrowserFrameBytes)

	sessionID, err := security.RandomToken(16)
	if err != nil {
		return
	}
	subs, unsub := s.Hub.SubscribeTerminal(sessionID)
	defer unsub()

	ip := s.Trust.RealIP(r)
	s.Store.InsertAudit(&store.AuditEntry{
		Actor:    "panel",
		NodeID:   nodeID,
		Action:   "terminal_open",
		Command:  sessionID,
		SourceIP: ip,
	})
	defer s.Store.InsertAudit(&store.AuditEntry{
		Actor:    "panel",
		NodeID:   nodeID,
		Action:   "terminal_close",
		Command:  sessionID,
		SourceIP: ip,
	})

	// Keep browser writes in one goroutine. Closing relayDone fixes the former
	// idle-agent leak: browser disconnects now stop the relay without waiting
	// for the agent to emit a terminal frame.
	relayDone := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-relayDone:
				return
			case env := <-subs:
				if !writeTerminalFrame(ws, env) {
					// Closing the socket unblocks the browser read loop as well;
					// otherwise a failed writer could leave this handler parked.
					_ = ws.Close()
					return
				}
			}
		}
	}()

	// browser → agent; the server stamps the session id, the browser never picks it
	closeSent := false
	for {
		var env protocol.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			break
		}
		if env.V != protocol.Version {
			continue
		}
		if !s.stampTerminalFrame(sessionID, &env, &closeSent) {
			continue
		}
		if !s.Hub.Send(nodeID, env) {
			break
		}
		if closeSent {
			break
		}
	}

	// Browser went away without an explicit close (tab close, network drop):
	// tell the agent so it tears the PTY down instead of leaking the shell.
	if !closeSent {
		closePayload, _ := json.Marshal(protocol.TerminalClose{SessionID: sessionID})
		s.Hub.Send(nodeID, protocol.Envelope{
			V:       protocol.Version,
			Type:    protocol.TypeTermClose,
			ID:      sessionID,
			TS:      protocol.Now(),
			Payload: closePayload,
		})
	}
	close(relayDone)
	<-writerDone
}

// stampTerminalFrame ignores browser-supplied session IDs and terminal modes.
// During rolling updates, a stale browser may still request "ssh"; the server
// normalizes it to "pty" so neither credentials nor an SSH daemon are needed.
func (s *Server) stampTerminalFrame(sessionID string, env *protocol.Envelope, closeSent *bool) bool {
	switch env.Type {
	case protocol.TypeTermOpen:
		var p protocol.TerminalOpen
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return false
		}
		p.SessionID = sessionID
		p.Mode = "pty"
		env.Payload, _ = json.Marshal(p)
	case protocol.TypeTermInput:
		var p protocol.TerminalInput
		if err := json.Unmarshal(env.Payload, &p); err != nil || len(p.Data) > maxTerminalBrowserFrameBytes {
			return false
		}
		p.SessionID = sessionID
		env.Payload, _ = json.Marshal(p)
	case protocol.TypeTermResize:
		var p protocol.TerminalResize
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return false
		}
		p.SessionID = sessionID
		env.Payload, _ = json.Marshal(p)
	case protocol.TypeTermClose:
		var p protocol.TerminalClose
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return false
		}
		p.SessionID = sessionID
		env.Payload, _ = json.Marshal(p)
		*closeSent = true
	default:
		return false
	}
	env.ID = sessionID
	env.TS = protocol.Now()
	return true
}

// writeTerminalFrame pushes one frame to the browser; false means the socket
// is gone and the writer goroutine should stop.
func writeTerminalFrame(ws *websocket.Conn, env protocol.Envelope) bool {
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteJSON(env) == nil
}

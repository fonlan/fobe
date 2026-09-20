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

// maxTerminalSocketsPerNode bounds concurrent browser terminals for one node.
// The agent keeps a single active PTY and every open kills the previous shell
// (design §11), so an authenticated client looping on /ws/terminal could churn
// the probe's PTY — and the audit log — indefinitely.
const maxTerminalSocketsPerNode = 4

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

	if s.wsReg.countTerminals(nodeID) >= maxTerminalSocketsPerNode {
		writeErr(w, http.StatusConflict, "too_many_terminals")
		return
	}

	ws, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(maxTerminalBrowserFrameBytes)
	s.wsReg.add(ws, wsConnInfo{sessionID: sessionIDFromRequest(r), nodeID: nodeID})
	defer s.wsReg.remove(ws)

	sessionID, err := security.RandomToken(16)
	if err != nil {
		return
	}
	subs, unsub := s.Hub.SubscribeTerminal(sessionID)
	defer unsub()

	// Server-initiated queries (design §12.7.1): the AI's terminal tools need to
	// ask *this browser* for its xterm buffer, because the screen exists nowhere
	// else. The request goes through pushCh and is written by the same single
	// goroutine that writes agent frames — gorilla forbids concurrent writers on
	// one Conn, so a second writer here would corrupt the stream.
	pushCh := make(chan protocol.Envelope, 8)
	unpush := s.Hub.RegisterTerminalPush(sessionID, func(env protocol.Envelope) bool {
		select {
		case pushCh <- env:
			return true
		default: // buffer full: the caller times out rather than blocking the hub
			return false
		}
	})
	defer unpush()

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
			case env := <-pushCh:
				if !writeTerminalFrame(ws, env) {
					_ = ws.Close()
					return
				}
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
		// The answer to a server-initiated buffer query (design §12.7.1). It is a
		// browser↔server frame and must NEVER be forwarded to the agent, which
		// would only log it as unhandled.
		if env.Type == protocol.TypeTermBuffer {
			var answer protocol.TerminalBuffer
			if json.Unmarshal(env.Payload, &answer) == nil {
				s.Hub.DeliverTerminalBuffer(answer)
			}
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

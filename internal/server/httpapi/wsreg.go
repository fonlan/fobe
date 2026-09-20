package httpapi

import (
	"sync"

	"github.com/gorilla/websocket"
)

// wsRegistry tracks live browser sockets so that revoking a session can
// actually close them.
//
// A WebSocket authenticates once, at the handshake (requireSession), and then
// lives outside the request lifecycle — so "revoke all sessions" used to leave
// every open terminal and event stream connected, and a password change could
// not evict a thief who already had one open (§4.1 实现修订 2026-09-20).
//
// It also carries the per-node terminal count, so an authenticated client
// cannot churn the probe's single PTY by opening sockets in a loop.
type wsConnInfo struct {
	sessionID string
	nodeID    string // "" for /ws/events
}

type wsRegistry struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]wsConnInfo
}

func (r *wsRegistry) add(ws *websocket.Conn, info wsConnInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = map[*websocket.Conn]wsConnInfo{}
	}
	r.conns[ws] = info
}

func (r *wsRegistry) remove(ws *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, ws)
}

// countTerminals reports live terminal sockets for one node (events streams are
// not terminals and carry an empty node id).
func (r *wsRegistry) countTerminals(nodeID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, info := range r.conns {
		if info.nodeID != "" && info.nodeID == nodeID {
			n++
		}
	}
	return n
}

// closeExcept closes every socket whose session is not keep ("" closes all).
// The handlers own the close for their own socket; closing from here is safe
// because gorilla surfaces it to the blocked writer as a write error.
func (r *wsRegistry) closeExcept(keep string) int {
	r.mu.Lock()
	targets := make([]*websocket.Conn, 0, len(r.conns))
	for ws, info := range r.conns {
		if info.sessionID != keep {
			targets = append(targets, ws)
		}
	}
	r.mu.Unlock()
	for _, ws := range targets {
		_ = ws.Close()
	}
	return len(targets)
}

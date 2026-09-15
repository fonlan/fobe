package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// --- /ws/events: live panel updates (design §16) ---

var eventsUpgrader = websocket.Upgrader{
	// same-origin SPA: accept the panel's own origin (and non-browser
	// clients, which send no Origin header)
	CheckOrigin: sameOrigin,
}

// sameOrigin permits requests whose Origin host matches the request host,
// plus Origin-less (non-browser) requests.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// publishEvent fans an event out to all connected panels.
func (s *Server) publishEvent(kind, ref string) {
	payload, _ := json.Marshal(map[string]any{"kind": kind, "ref": ref, "ts": nowUnix()})
	s.evMu.Lock()
	defer s.evMu.Unlock()
	for ch := range s.evSubs {
		select {
		case ch <- payload:
		default: // drop for slow clients
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ws, err := eventsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	ch := make(chan []byte, 32)
	s.evMu.Lock()
	s.evSubs[ch] = struct{}{}
	s.evMu.Unlock()
	defer func() {
		s.evMu.Lock()
		delete(s.evSubs, ch)
		s.evMu.Unlock()
	}()

	// reader: client only closes / pings
	go func() {
		ws.SetReadLimit(1024)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case msg := <-ch:
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

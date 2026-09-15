// Package hub owns the agent WebSocket connections (design.md §7): hello /
// hello_ack, desired-state push, heartbeat tracking, command dispatch,
// traffic accounting with counter-reset detection, and terminal relay.
package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/geoip"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
	"github.com/gorilla/websocket"
)

const (
	offlineAfter              = 90 * time.Second // §7: 90s without heartbeat = offline
	writeWait                 = 10 * time.Second
	maxFrameBytes             = 8 << 20 // cert PEM + batched latency stay well under this
	defaultLatencyIntervalSec = 5
)

// Hub tracks live agent connections and routes frames.
type Hub struct {
	store *store.Store
	trust *security.TrustChain
	log   *slog.Logger
	geo   geoip.Resolver // country lookup for node IPs (§14); nil keeps old behaviour

	// agentUp decides §5.5 agent self-update per node. Optional: without one no
	// target is ever offered (dev builds, unit tests) and agents simply keep
	// whatever they run.
	agentUp AgentUpdater

	mu    sync.RWMutex
	conns map[string]*Conn

	// terminalSubs routes agent terminal frames to browser sessions.
	termMu   sync.Mutex
	termSubs map[string]chan protocol.Envelope // sessionID -> sink
}

// AgentUpdater is the §5.5 decision surface the hub needs.
// *agentupdate.Manager implements it.
type AgentUpdater interface {
	// Desired fills the self-update fields of a desired state and returns the
	// flat values for hello_ack; ok=false means "no target right now".
	Desired(nodeID string, d *protocol.DesiredState) (version string, after int64)
	// Reconcile re-evaluates the plan after the node reported its build (a hello
	// arrives *after* hello_ack was already assembled: hub registers the socket,
	// answers, then pumps frames), so a probe that converged on its own stops
	// showing a stale verdict.
	Reconcile(nodeID string)
	// OnReport records what an agent said about one self-update attempt.
	OnReport(nodeID string, r *protocol.AgentUpdate)
}

// SetAgentUpdater wires (or replaces) the §5.5 self-update manager.
func (h *Hub) SetAgentUpdater(u AgentUpdater) { h.agentUp = u }

// New builds a Hub. geo is optional (variadic) so existing callers keep
// compiling: pass a geoip.Resolver to enable §14 country resolution; without
// one, country_code is left untouched.
func New(st *store.Store, trust *security.TrustChain, log *slog.Logger, geo ...geoip.Resolver) *Hub {
	h := &Hub{
		store:    st,
		trust:    trust,
		log:      log,
		conns:    map[string]*Conn{},
		termSubs: map[string]chan protocol.Envelope{},
	}
	for _, r := range geo {
		if r != nil {
			h.geo = r
		}
	}
	return h
}

// --- connection lifecycle ---

type Conn struct {
	nodeID string
	ws     *websocket.Conn
	send   chan protocol.Envelope
	notify chan struct{} // nudges the command pump
	done   chan struct{}
	once   sync.Once
}

func (c *Conn) close() {
	c.once.Do(func() { close(c.done); close(c.send) })
}

func (h *Hub) IsOnline(nodeID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[nodeID]
	return ok
}

// OnlineCount returns how many agents are currently connected.
func (h *Hub) OnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// HandleAgentWS upgrades and takes over the agent connection. The request has
// already been authenticated: nodeID is verified against the stored secret.
func (h *Hub) HandleAgentWS(w http.ResponseWriter, r *http.Request, nodeID string) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already replied
	}

	c := &Conn{
		nodeID: nodeID,
		ws:     ws,
		send:   make(chan protocol.Envelope, 64),
		notify: make(chan struct{}, 8),
		done:   make(chan struct{}),
	}

	h.mu.Lock()
	if old, ok := h.conns[nodeID]; ok {
		old.close() // re-registration/reconnect replaces a stale socket
	}
	h.conns[nodeID] = c
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.conns[nodeID] == c {
			delete(h.conns, nodeID)
		}
		h.mu.Unlock()
		c.close()
	}()

	// hello_ack with the full desired state (§7).
	targets, _ := h.store.TargetsForNode(nodeID)
	specs := make([]protocol.TargetSpec, 0, len(targets))
	for _, t := range targets {
		specs = append(specs, protocol.TargetSpec{ID: t.ID, Name: t.Name, Kind: t.Kind, Host: t.Host, Port: t.Port})
	}
	desired := h.buildDesiredState(nodeID)
	ack := protocol.HelloAck{
		NodeID:             nodeID,
		ProbeMetrics:       false,
		Desired:            desired,
		LatencyTargets:     specs,
		LatencyIntervalSec: h.latencyInterval(),
		// §5.5: the flat fields mirror Desired so both carriers can never
		// disagree, and old agents that ignore the new fields are unaffected.
		AgentTargetVersion: desired.AgentTargetVersion,
		AgentUpdateAfter:   desired.AgentUpdateAfter,
	}
	if !h.sendEnvelope(c, protocol.NewEnvelope(protocol.TypeHelloAck, "", ack)) {
		return
	}
	h.drainCommandQueue(c)

	go h.writePump(c)
	h.readPump(c)
}

func (h *Hub) latencyInterval() int {
	value, err := h.store.GetSetting("latency.interval_seconds")
	if err != nil {
		return defaultLatencyIntervalSec
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 || seconds > 3600 {
		return defaultLatencyIntervalSec
	}
	return seconds
}

// HandleAgentSelfCheck serves the §5.5 bypass handshake of a downloaded binary.
//
// It must not behave like a normal connection: registering would close the live
// socket (HandleAgentWS replaces a node's existing connection) and TouchNode
// would write the *self-checking* build's version into the panel — claiming an
// update that has not been committed. So: upgrade, read one hello, answer with
// the target, close. Nothing is persisted.
func (h *Hub) HandleAgentSelfCheck(w http.ResponseWriter, r *http.Request, nodeID string) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already replied
	}
	defer ws.Close()
	// A self-check is a local process with a 60s budget; refuse to hang here.
	_ = ws.SetReadDeadline(time.Now().Add(20 * time.Second))
	_ = ws.SetWriteDeadline(time.Now().Add(writeWait))

	var env protocol.Envelope
	if err := ws.ReadJSON(&env); err != nil {
		h.log.Info("agent self-check handshake failed", "node", nodeID, "err", err)
		return
	}
	if env.Type != protocol.TypeHello {
		h.log.Warn("agent self-check sent a non-hello frame", "node", nodeID, "type", env.Type)
		return
	}
	var hello protocol.Hello
	_ = json.Unmarshal(env.Payload, &hello)
	if !hello.SelfCheck {
		// The header said self-check; be strict about the payload too, or a
		// mislabelled real connection would silently never register.
		h.log.Warn("agent self-check header without payload flag", "node", nodeID)
		return
	}
	desired := protocol.DesiredState{}
	version, after := "", int64(0)
	if h.agentUp != nil {
		version, after = h.agentUp.Desired(nodeID, &desired)
	}
	ack := protocol.HelloAck{
		NodeID:             nodeID,
		Desired:            desired,
		AgentTargetVersion: version,
		AgentUpdateAfter:   after,
	}
	if err := ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHelloAck, "", ack)); err != nil {
		h.log.Warn("agent self-check reply failed", "node", nodeID, "err", err)
		return
	}
	h.log.Info("agent self-check ok", "node", nodeID, "version", hello.Version, "target", version)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin is irrelevant for agent connections (non-browser client).
	CheckOrigin: func(*http.Request) bool { return true },
}

func (h *Hub) writePump(c *Conn) {
	for {
		select {
		case <-c.done:
			return
		case env := <-c.send:
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteJSON(env); err != nil {
				h.log.Warn("agent write failed", "node", c.nodeID, "err", err)
				c.close()
				return
			}
		}
	}
}

func (h *Hub) readPump(c *Conn) {
	c.ws.SetReadLimit(maxFrameBytes)
	for {
		var env protocol.Envelope
		if err := c.ws.ReadJSON(&env); err != nil {
			h.log.Info("agent disconnected", "node", c.nodeID, "err", err)
			return
		}
		h.handleFrame(c, env)
	}
}

func (h *Hub) handleFrame(c *Conn, env protocol.Envelope) {
	if env.V != protocol.Version {
		h.log.Warn("unknown protocol version", "node", c.nodeID, "v", env.V)
		return
	}
	switch env.Type {
	case protocol.TypePing:
		// heartbeat: liveness only; DB last_seen drives offline detection
		h.store.TouchNode(c.nodeID, "", protocol.Now())
		// mirror back as pong-ish ack so the agent learns the link is alive
		h.sendEnvelope(c, protocol.Envelope{V: protocol.Version, Type: "pong", ID: env.ID, TS: protocol.Now()})
	case protocol.TypeHello:
		var p protocol.Hello
		if json.Unmarshal(env.Payload, &p) == nil {
			h.onHello(c, &p)
		}
	case protocol.TypeMetrics:
		var p protocol.Metrics
		if json.Unmarshal(env.Payload, &p) == nil {
			h.onMetrics(c.nodeID, env.TS, &p)
		}
	case protocol.TypeTraffic:
		var p protocol.Traffic
		if json.Unmarshal(env.Payload, &p) == nil {
			h.onTraffic(c.nodeID, env.TS, &p)
		}
	case protocol.TypeLatency:
		var p protocol.LatencyBatch
		if json.Unmarshal(env.Payload, &p) == nil {
			h.onLatency(c.nodeID, &p)
		}
	case protocol.TypeState:
		var p protocol.State
		if json.Unmarshal(env.Payload, &p) == nil {
			h.onState(c.nodeID, &p)
		}
	case protocol.TypeCmdResult:
		var p protocol.CmdResult
		if json.Unmarshal(env.Payload, &p) == nil {
			h.onCmdResult(c.nodeID, &p)
		}
	case protocol.TypeAgentUpdate:
		var p protocol.AgentUpdate
		if json.Unmarshal(env.Payload, &p) == nil && h.agentUp != nil {
			h.agentUp.OnReport(c.nodeID, &p)
		}
	case protocol.TypeTerminalOutput, protocol.TypeTerminalClosed:
		h.relayTerminal(env)
	default:
		h.log.Debug("unhandled frame", "node", c.nodeID, "type", env.Type)
	}
}

func (h *Hub) sendEnvelope(c *Conn, env protocol.Envelope) bool {
	select {
	case c.send <- env:
		return true
	case <-c.done:
		return false
	default:
		h.log.Warn("agent send queue full, dropping", "node", c.nodeID, "type", env.Type)
		return false
	}
}

// Send delivers an envelope to an online agent; returns false when offline.
func (h *Hub) Send(nodeID string, env protocol.Envelope) bool {
	h.mu.RLock()
	c, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	return h.sendEnvelope(c, env)
}

// PushLatencyConfig applies the current panel cadence to every online probe.
// It uses a dedicated frame so a global measurement preference cannot alter a
// node's per-node desired sing-box state.
func (h *Hub) PushLatencyConfig() {
	interval := h.latencyInterval()
	env := protocol.NewEnvelope(protocol.TypeLatencyCfg, "", protocol.LatencyConfig{IntervalSec: interval})

	// Keep the read lock while enqueueing: connection teardown removes the entry
	// under the write lock before it closes c.send, so this avoids a send-on-
	// closed-channel race without blocking on a slow websocket writer.
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.conns {
		h.sendEnvelope(c, env)
	}
}

// PushDesired sends the *complete* current desired state (§7) to an online
// agent and reports whether it reached one. The §5.5 operator retry uses it to
// nudge a probe without waiting for its next handshake; sending the whole state
// is what keeps that nudge from accidentally clearing sing-box management.
func (h *Hub) PushDesired(nodeID string) bool {
	desired := h.buildDesiredState(nodeID)
	if desired.Singbox == nil && desired.TrafficIface == nil && desired.AgentTargetVersion == "" {
		return false
	}
	return h.Send(nodeID, protocol.NewEnvelope(protocol.TypeDesired, "", desired))
}

// NotifyCommand wakes the command pump after the API enqueues something.
func (h *Hub) NotifyCommand(nodeID string) {
	h.mu.RLock()
	c, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// drainCommandQueue sends every pending (non-expired) command for the node.
func (h *Hub) drainCommandQueue(c *Conn) {
	for {
		cmd, err := h.store.NextPendingCommand(c.nodeID)
		if err != nil {
			return // no more commands (or transient error: pump will retry)
		}
		wire := protocol.Cmd{ID: cmd.ID, Kind: cmd.Kind, Payload: json.RawMessage(cmd.Payload)}
		h.sendEnvelope(c, protocol.Envelope{
			V: protocol.Version, Type: protocol.TypeCmd, ID: cmd.ID, TS: protocol.Now(),
			Payload: mustMarshal(wire),
		})
	}
}

func mustMarshal(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// PumpCommands periodically drains the queue for a connected agent so that
// offline-queued commands arrive even without an API nudge.
func (h *Hub) PumpCommands(interval time.Duration) {
	for range time.Tick(interval) {
		h.mu.RLock()
		conns := make([]*Conn, 0, len(h.conns))
		for _, c := range h.conns {
			conns = append(conns, c)
		}
		h.mu.RUnlock()
		for _, c := range conns {
			select {
			case <-c.done:
				continue
			default:
			}
			h.drainCommandQueue(c)
		}
	}
}

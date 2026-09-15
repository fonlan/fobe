package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fobe-panel/fobe/internal/agent/collect"
	"github.com/fobe-panel/fobe/internal/agent/service"
	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/gorilla/websocket"
)

const (
	pingEvery         = 15 * time.Second // §7 heartbeat
	reportEvery       = 60 * time.Second // metrics + traffic cadence
	probeMetricsEvery = 5 * time.Second  // §16 high-frequency stream while a detail page is open
	stateEvery        = 5 * time.Minute  // IP-set change detection (§14)
	backoffStart      = 1 * time.Second  // §7 reconnect: 1s → 5min, jittered
	backoffMax        = 5 * time.Minute
)

// Version is the agent build version, injected via
// -ldflags "-X github.com/fobe-panel/fobe/internal/agent.Version=...".
var Version = "dev"

// Register exchanges a reg token for node credentials and persists them.
// When the config already holds node credentials (reinstall), they travel
// along so the server can rebind the same machine (design §4.2).
func Register(cfg *Config, configPath, regToken string) error {
	machineID, err := LoadOrCreateMachineID(DefaultMachineIDPath)
	if err != nil {
		return err
	}
	cfg.MachineID = machineID

	body, _ := json.Marshal(map[string]any{
		"token":       regToken,
		"machine_id":  cfg.MachineID,
		"hostname":    hostname(),
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"kernel":      collect.Kernel(),
		"version":     Version,
		"tz":          localTZ(),
		"cpu_cores":   collect.CPUCores(),
		"node_id":     cfg.NodeID,
		"node_secret": cfg.NodeSecret,
	})
	resp, err := http.Post(cfg.ServerURL+"/api/agent/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("register failed: %s (%s)", resp.Status, e.Error.Code)
	}
	var out struct {
		NodeID     string `json:"node_id"`
		NodeSecret string `json:"node_secret"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.NodeID == "" {
		return fmt.Errorf("register response malformed")
	}
	cfg.NodeID = out.NodeID
	cfg.NodeSecret = out.NodeSecret
	return SaveConfig(configPath, cfg)
}

// Run maintains the WSS connection forever: reconnect with jittered
// exponential backoff, hello on connect, periodic reporting, command serving.
// The sing-box manager (§9) outlives individual sessions so fallback-mode
// processes and convergence state survive a reconnect.
func Run(cfg *Config, configPath string, log *slog.Logger) error {
	coll := collect.New()

	// Adopt an installation left by an older agent before the first
	// convergence: the layout moved to the one-sing paths (§9.3 实现修订), and
	// re-downloading a 30 MB artifact on every upgraded probe (or losing the
	// rollback copy) would be a self-inflicted regression.
	if notes, err := service.MigrateSingboxLayout(); err != nil {
		log.Warn("sing-box layout migration incomplete", "err", err)
	} else {
		for _, n := range notes {
			log.Info("sing-box layout migrated", "move", n)
		}
	}

	sbx := newSingboxManager(cfg, log)
	go sbx.Run()

	// §5.5 self-update lives at process scope: its state file and its "already
	// updating" guard must survive session reconnects (and the restart the
	// update itself performs).
	updater := newSelfUpdater(cfg, configPath, log)

	metricsEvery := reportEvery
	if cfg.MetricsIntervalSec > 0 {
		metricsEvery = time.Duration(cfg.MetricsIntervalSec) * time.Second
	}

	backoff := backoffStart
	for {
		started := time.Now()
		err := connectAndServe(cfg, configPath, coll, sbx, updater, metricsEvery, log)
		if err != nil {
			log.Warn("agent link lost", "err", err)
		}
		// a session that lasted a while means the network works: reset backoff
		if time.Since(started) > backoffMax {
			backoff = backoffStart
		}
		sleep := backoff + time.Duration(rand.Int63n(int64(backoff/2)+1))
		log.Info("reconnecting", "in", sleep.String())
		time.Sleep(sleep)
		if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

func connectAndServe(cfg *Config, configPath string, coll *collect.Collector, sbx *singboxManager, updater *selfUpdater, metricsEvery time.Duration, log *slog.Logger) error {
	wsURL := wsEndpoint(cfg.ServerURL)
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	reqHeader := http.Header{
		"X-Fobe-Node-ID":     {cfg.NodeID},
		"X-Fobe-Node-Secret": {cfg.NodeSecret},
	}
	ws, _, err := dialer.Dial(wsURL, reqHeader)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer ws.Close()
	log.Info("agent connected", "server", wsURL, "version", Version)

	session := newSession(cfg, configPath, coll, ws, sbx, updater, log)
	defer session.shutdown("agent websocket disconnected")
	session.hello()
	session.reportOnce() // fresh nodes appear immediately
	session.sendState()

	go session.writeLoop()
	go session.reportLoop(metricsEvery)
	go session.latencyLoop()
	go session.stateChangeLoop()

	return session.readLoop()
}

type agentSession struct {
	cfg          *Config
	configPath   string
	coll         *collect.Collector
	ws           *websocket.Conn
	log          *slog.Logger
	send         chan protocol.Envelope
	urgentSend   chan protocol.Envelope
	done         chan struct{}
	shutdownOnce sync.Once
	term         *terminalManager
	sbx          *singboxManager
	updater      *selfUpdater

	probeMetrics atomic.Bool   // §16: stream 5s samples while a detail page is open
	trafficIface atomic.Value  // string: empty = agent-detected default route
	cadence      chan struct{} // nudges reportLoop to re-arm its ticker (buffered 1)

	latencyInterval atomic.Int64
	latencyCadence  chan struct{} // nudges latencyLoop to re-arm its ticker (buffered 1)
	latencyMu       sync.RWMutex
	targets         []protocol.TargetSpec
	execMu          chan struct{} // one command at a time; also serves as idempotency guard
}

func newSession(cfg *Config, configPath string, coll *collect.Collector, ws *websocket.Conn, sbx *singboxManager, updater *selfUpdater, log *slog.Logger) *agentSession {
	s := &agentSession{
		cfg:            cfg,
		configPath:     configPath,
		coll:           coll,
		ws:             ws,
		log:            log,
		send:           make(chan protocol.Envelope, 64),
		urgentSend:     make(chan protocol.Envelope, 8),
		done:           make(chan struct{}),
		execMu:         make(chan struct{}, 1),
		cadence:        make(chan struct{}, 1),
		latencyCadence: make(chan struct{}, 1),
	}
	s.trafficIface.Store(cfg.Iface)
	s.latencyInterval.Store(int64(defaultLatencyInterval / time.Second))
	s.sbx = sbx
	s.updater = updater
	s.term = newTerminalManager(s)
	return s
}

func (s *agentSession) hello() {
	detect := service.Detect()
	hello := protocol.Hello{
		MachineID: s.cfg.MachineID,
		Hostname:  hostname(),
		Version:   Version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Kernel:    collect.Kernel(),
		CPUCores:  collect.CPUCores(),
		TZ:        localTZ(),
		Caps: protocol.Caps{
			ICMP:     hasRawSocket(),
			Systemd:  detect == service.KindSystemd,
			Procd:    detect == service.KindProcd,
			Fallback: detect == service.KindFallback,
			// §5.5: agents built before this never send the bit, which is how
			// the panel tells "will follow" from "needs a reinstall".
			SelfUpdate: selfUpdateSupported(),
		},
		IPs:        LocalIPs(),
		Interfaces: collect.NetworkInterfaces(),
	}
	s.sendEnvelope(protocol.NewEnvelope(protocol.TypeHello, "", hello))
}

func (s *agentSession) sendEnvelope(env protocol.Envelope) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case <-s.done:
		return
	case s.send <- env:
	default:
		s.log.Warn("send queue full, dropping frame", "type", env.Type)
	}
}

// sendTerminalClosed uses a separate bounded queue so a full metrics/output
// queue cannot suppress the terminal lifecycle event.
func (s *agentSession) sendTerminalClosed(closed protocol.TerminalClosed) {
	env := protocol.NewEnvelope(protocol.TypeTerminalClosed, "", closed)
	select {
	case <-s.done:
		return
	case s.urgentSend <- env:
	default:
		s.log.Warn("terminal close queue full, dropping frame", "session_id", closed.SessionID)
	}
}

func (s *agentSession) writeLoop() {
	for {
		select {
		case <-s.done:
			return
		default:
		}
		select {
		case <-s.done:
			return
		case env := <-s.urgentSend:
			s.writeEnvelope(env)
		case env := <-s.send:
			s.writeEnvelope(env)
		}
	}
}

func (s *agentSession) writeEnvelope(env protocol.Envelope) {
	s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := s.ws.WriteJSON(env); err != nil {
		s.log.Warn("write failed", "err", err)
		s.ws.Close()
	}
}

func (s *agentSession) shutdown(reason string) {
	s.shutdownOnce.Do(func() {
		if s.term != nil {
			s.term.close(reason)
		}
		close(s.done)
	})
}

// reportLoop emits ping / metrics+traffic / state on their cadences. The
// metrics ticker re-arms when the probe_metrics toggle flips (§16): 5s while
// a detail page is open, back to the configured base afterwards.
func (s *agentSession) reportLoop(metricsEvery time.Duration) {
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	state := time.NewTicker(stateEvery)
	defer state.Stop()

	metrics := time.NewTicker(s.metricsInterval(metricsEvery))
	defer metrics.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ping.C:
			s.sendEnvelope(protocol.Envelope{V: protocol.Version, Type: protocol.TypePing, TS: protocol.Now()})
		case <-metrics.C:
			s.reportOnce()
		case <-state.C:
			s.sendState()
		case <-s.cadence:
			metrics.Stop()
			metrics = time.NewTicker(s.metricsInterval(metricsEvery))
		}
	}
}

func (s *agentSession) metricsInterval(base time.Duration) time.Duration {
	if s.probeMetrics.Load() {
		return probeMetricsEvery
	}
	return base
}

// setProbeMetrics flips the §16 toggle and wakes the reporting loop. The
// nudge is dropped when the loop is gone (session over) — nothing leaks.
func (s *agentSession) setProbeMetrics(enabled bool) {
	s.probeMetrics.Store(enabled)
	select {
	case s.cadence <- struct{}{}:
	default:
	}
}

// stateChangeLoop forwards sing-box state changes (§9) as state frames. It
// drains the manager's shared notification channel; the manager only ever
// sends non-blocking, so a dead session cannot stall it.
func (s *agentSession) stateChangeLoop() {
	for {
		select {
		case <-s.done:
			return
		case <-s.sbx.Changed():
			s.sendState()
		}
	}
}

func (s *agentSession) reportOnce() {
	iface, _ := s.trafficIface.Load().(string)
	snap := s.coll.Read(iface)
	if snap.HasIface {
		s.sendEnvelope(protocol.NewEnvelope(protocol.TypeTraffic, "", protocol.Traffic{
			Iface: snap.TrafficIface, Rx: snap.TrafficRx, Tx: snap.TrafficTx,
		}))
	}
	s.sendEnvelope(protocol.NewEnvelope(protocol.TypeMetrics, "", snap.Metrics))
}

func (s *agentSession) sendState() {
	s.sendEnvelope(protocol.NewEnvelope(protocol.TypeState, "", protocol.State{
		IPs:        LocalIPs(),
		Interfaces: collect.NetworkInterfaces(),
		BootID:     collect.BootID(),
		Singbox:    s.sbx.Snapshot(), // nil while sing-box is unmanaged (§9)
	}))
}

func (s *agentSession) readLoop() error {
	s.ws.SetReadLimit(8 << 20)
	for {
		var env protocol.Envelope
		if err := s.ws.ReadJSON(&env); err != nil {
			return err
		}
		s.handleServerFrame(env)
	}
}

func (s *agentSession) handleServerFrame(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeHelloAck:
		var ack protocol.HelloAck
		if json.Unmarshal(env.Payload, &ack) == nil {
			s.log.Info("hello_ack", "node", ack.NodeID, "targets", len(ack.LatencyTargets),
				"agent_target", ack.AgentTargetVersion)
			s.setLatencyTargets(ack.LatencyTargets)
			s.setLatencyInterval(ack.LatencyIntervalSec)
			s.setProbeMetrics(ack.ProbeMetrics)
			s.applyDesired(ack.Desired) // full desired state rides along (§7)
		}
	case protocol.TypeDesired:
		s.applyDesiredFrame(env)
	case protocol.TypeCmd:
		go s.executeCommand(env)
	case protocol.TypeProbeMetr:
		var p protocol.ProbeMetrics
		if json.Unmarshal(env.Payload, &p) == nil {
			s.setProbeMetrics(p.Enabled)
		}
	case protocol.TypeLatencyCfg:
		var cfg protocol.LatencyConfig
		if json.Unmarshal(env.Payload, &cfg) == nil {
			s.setLatencyInterval(cfg.IntervalSec)
		}
	case protocol.TypeTermOpen, protocol.TypeTermInput, protocol.TypeTermResize, protocol.TypeTermClose:
		s.handleTerminalFrame(env)
	case "pong":
		// link liveness confirmed
	default:
		s.log.Debug("unhandled frame", "type", env.Type)
	}
}

// applyDesired hands the declaration to the sing-box manager, which converges
// to it through the three gates (§9.2) and reports the resulting state.
func (s *agentSession) applyDesired(desired protocol.DesiredState) {
	if desired.Singbox != nil {
		s.log.Info("desired singbox state",
			"version", desired.Singbox.Version, "port", desired.Singbox.Port)
	}
	s.sbx.SetDesired(desired.Singbox)
	if desired.TrafficIface != nil {
		s.setTrafficIface(*desired.TrafficIface)
	}
	// §5.5: the same declaration carries the agent build this server wants, so
	// an operator retry can arrive as a plain desired frame.
	if s.updater != nil {
		s.updater.SetReporter(s.reportAgentUpdate)
		s.updater.Consider(desired.AgentTargetVersion, desired.AgentUpdateAfter)
	}
}

// reportAgentUpdate narrates one §5.5 attempt to the server. Fire-and-forget:
// the frame may be dropped while the socket is being torn down by the very
// restart it announces, and the definitive evidence is the version the new
// binary reports at its next handshake.
func (s *agentSession) setTrafficIface(iface string) {
	current, _ := s.trafficIface.Load().(string)
	if current == iface {
		return
	}
	s.trafficIface.Store(iface)
	s.cfg.Iface = iface
	if err := SaveConfig(s.configPath, s.cfg); err != nil {
		s.log.Warn("persist traffic interface", "err", err)
	}
	// Make the newly selected interface visible immediately instead of waiting
	// for the next scheduled report.
	s.reportOnce()
}

func (s *agentSession) reportAgentUpdate(target, phase, class string, attempts int, errMsg string) {
	s.sendEnvelope(protocol.NewEnvelope(protocol.TypeAgentUpdate, "", protocol.AgentUpdate{
		Target: target, Phase: phase, Class: class, Error: errMsg, Attempts: attempts,
	}))
}

func (s *agentSession) applyDesiredFrame(env protocol.Envelope) {
	var desired protocol.DesiredState
	if err := json.Unmarshal(env.Payload, &desired); err != nil {
		return
	}
	s.applyDesired(desired)
}

func (s *agentSession) handleTerminalFrame(env protocol.Envelope) {
	if env.V != 0 && env.V != protocol.Version {
		s.log.Warn("unknown terminal protocol version", "v", env.V)
		return
	}
	s.term.handle(env)
}

func wsEndpoint(serverURL string) string {
	u := serverURL

	switch {
	case len(u) > 8 && u[:8] == "https://":
		u = "wss://" + u[8:]
	case len(u) > 7 && u[:7] == "http://":
		u = "ws://" + u[7:]
	}
	return u + "/ws/agent"
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// localTZ resolves an IANA zone name. OpenWrt keeps it in /etc/TZ; common
// Linux distributions point /etc/localtime into zoneinfo instead.
func localTZ() string {
	if tz := validTZ(os.Getenv("TZ")); tz != "" {
		return tz
	}
	for _, path := range []string{"/etc/TZ", "/etc/timezone"} {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			if tz := validTZ(string(trimBytes(data))); tz != "" {
				return tz
			}
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if idx := strings.Index(target, "/zoneinfo/"); idx >= 0 {
			if tz := validTZ(target[idx+len("/zoneinfo/"):]); tz != "" {
				return tz
			}
		}
	}
	return "UTC"
}

func validTZ(tz string) string {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return ""
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ""
	}
	return tz
}

func trimBytes(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

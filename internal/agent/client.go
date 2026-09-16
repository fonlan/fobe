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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fonlan/fobe/internal/agent/collect"
	"github.com/fonlan/fobe/internal/agent/service"
	"github.com/fonlan/fobe/internal/protocol"
	"github.com/gorilla/websocket"
)

const (
	pingEvery         = 15 * time.Second // §7 heartbeat
	reportEvery       = 60 * time.Second // metrics + traffic cadence
	probeMetricsEvery = 5 * time.Second  // §16 high-frequency stream while a detail page is open
	stateEvery        = 5 * time.Minute  // IP-set change detection (§14)
	// forwardsEvery is faster than stateEvery on purpose: nftables port
	// forwards are also edited outside the panel (nfpf.sh, hand-written nft),
	// so the panel's list would otherwise be up to five minutes stale. One
	// `nft list` a minute costs nothing.
	forwardsEvery = 1 * time.Minute
	backoffStart  = 1 * time.Second // §7 reconnect: 1s → 5min, jittered
	backoffMax    = 5 * time.Minute
)

// Version is the agent build version, injected via
// -ldflags "-X github.com/fonlan/fobe/internal/agent.Version=...".
var Version = "dev"

// Register exchanges a reg token for node credentials and persists them.
// When the config already holds node credentials (reinstall), they travel
// along so the server can rebind the same machine (design §4.2).
func Register(cfg *Config, configPath, regToken string) error {
	machineID, err := LoadOrCreateMachineID(MachineIDPathFor(configPath))
	if err != nil {
		return err
	}
	cfg.MachineID = machineID

	distroID, distroVersion := collect.Distro()
	body, _ := json.Marshal(map[string]any{
		"token":          regToken,
		"machine_id":     cfg.MachineID,
		"hostname":       hostname(),
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"kernel":         collect.Kernel(),
		"version":        Version,
		"tz":             localTZ(),
		"cpu_cores":      collect.CPUCores(),
		"distro_id":      distroID,
		"distro_version": distroVersion,
		"node_id":        cfg.NodeID,
		"node_secret":    cfg.NodeSecret,
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

// singboxHome resolves the sing-box layout root (§9.3): FOBE_SINGBOX_HOME is
// the explicit override; an unprivileged agent derives the layout from the
// config directory it already owns (the unprivileged install is defined by
// owning that directory); root keeps /etc/one-sing untouched.
func singboxHome(configPath string) string {
	if v := strings.TrimSpace(os.Getenv("FOBE_SINGBOX_HOME")); v != "" {
		return v
	}
	if !privileged() {
		return filepath.Join(filepath.Dir(configPath), "one-sing")
	}
	return service.SingboxWorkDir
}

// Run maintains the WSS connection forever: reconnect with jittered
// exponential backoff, hello on connect, periodic reporting, command serving.
// The sing-box manager (§9) outlives individual sessions so fallback-mode
// processes and convergence state survive a reconnect.
func Run(cfg *Config, configPath string, log *slog.Logger) error {
	coll := collect.New()

	// sing-box layout root: FOBE_SINGBOX_HOME wins explicitly; an unprivileged
	// agent derives it from the config directory it already owns; root keeps
	// the one-sing-compatible /etc/one-sing (§9.3). Runs before the manager
	// starts so every SingboxPaths() read sees the final layout.
	service.SetWorkDir(singboxHome(configPath))

	// Adopt an installation left by an older agent before the first
	// convergence: the layout moved to the one-sing paths (§9.3 实现修订), and
	// re-downloading a 30 MB artifact on every upgraded probe (or losing the
	// rollback copy) would be a self-inflicted regression. The legacy tree is
	// root-owned /etc, so an unprivileged agent skips the walk instead of
	// harvesting EACCES warnings.
	if privileged() {
		if notes, err := service.MigrateSingboxLayout(); err != nil {
			log.Warn("sing-box layout migration incomplete", "err", err)
		} else {
			for _, n := range notes {
				log.Info("sing-box layout migrated", "move", n)
			}
		}
	}
	// §9.3 实现修订 2026-09-16: the service moved to one-sing.service, the name
	// one-sing.sh uses. A probe updated from before the rename still has the old
	// unit enabled and running — retire it before the manager writes the new
	// one, or both supervise the same binary and port. The manager repeats this
	// guarded by its own flag (a service can also be left over from an older
	// agent that never converged), but doing it here means it happens even when
	// no desired state ever arrives.
	if privileged() {
		if retired, err := service.RetireLegacySingboxUnit(); err != nil {
			log.Warn("legacy sing-box service could not be retired", "err", err)
		} else if retired {
			log.Info("retired the legacy fobe-singbox service; fobe now owns one-sing.service")
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
	session.refreshForwards()
	session.sendState()

	go session.writeLoop()
	go session.reportLoop(metricsEvery)
	go session.latencyLoop()
	go session.stateChangeLoop()
	go session.forwardsLoop()
	go session.localChangeLoop()

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

	// §21 nftables port forwarding: the last inventory read from this probe.
	// Cached rather than read inside sendState so a slow/hanging nft can never
	// stall the state cadence (forwardsLoop owns the refresh).
	fwdEnv   nftEnv
	fwdMu    sync.Mutex
	forwards *protocol.ForwardsState
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
	s.fwdEnv = defaultNftEnv()
	return s
}

func (s *agentSession) hello() {
	detect := service.Detect()
	distroID, distroVersion := collect.Distro()
	hello := protocol.Hello{
		MachineID: s.cfg.MachineID,
		Hostname:  hostname(),
		Version:   Version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Kernel:    collect.Kernel(),
		CPUCores:  collect.CPUCores(),
		TZ:        localTZ(),
		// §16 基本信息: the panel shows "Debian 13"-style + a distro badge.
		DistroID:      distroID,
		DistroVersion: distroVersion,
		Caps: protocol.Caps{
			ICMP:     icmpAvailable(),
			Systemd:  detect == service.KindSystemd,
			Procd:    detect == service.KindProcd,
			Fallback: detect == service.KindFallback,
			// §5.5: agents built before this never send the bit, which is how
			// the panel tells "will follow" from "needs a reinstall". Since the
			// exec replace (实现修订 2026-09-16) the verdict is "may rename in
			// the binary's directory", not "a supervisor exists".
			SelfUpdate: selfUpdateCapBit(),
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

// localChangeLoop does the same for the discovery half (§9.3 实现修订
// 2026-09-17): a separate channel because "the operator edited config.json by
// hand" and "the managed instance changed state" are different events, and
// only the second one drives the panel's sing-box alerts.
func (s *agentSession) localChangeLoop() {
	for {
		select {
		case <-s.done:
			return
		case <-s.sbx.LocalChanged():
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
		// The discovery half rides along in the same frame: it is what tells
		// the panel about a sing-box fobe does not manage (§9.3 实现修订
		// 2026-09-17), and a separate frame type would only mean two ways to
		// say "here is this node's host".
		SingboxLocal: s.sbx.Local(),
		// nil until the first read; a nil field keeps an older server from
		// wiping what it knows about this node (§21).
		Forwards: s.forwardsCache(),
	}))
}

// forwardsLoop re-reads the port-forward inventory and reports it when it
// changed (§21). External edits (nfpf.sh, manual nft) are the reason this is a
// loop and not just a post-command refresh.
func (s *agentSession) forwardsLoop() {
	t := time.NewTicker(forwardsEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if s.refreshForwards() {
				s.sendState()
			}
		}
	}
}

// refreshForwards re-reads the probe's ruleset and reports whether the result
// differs from what was last reported.
func (s *agentSession) refreshForwards() bool {
	st := readForwardState(s.fwdEnv)
	s.fwdMu.Lock()
	defer s.fwdMu.Unlock()
	if s.forwards != nil && sameForwardsState(*s.forwards, st) {
		return false
	}
	s.forwards = &st
	return true
}

// setForwards adopts the state a forward command just observed and reports it.
func (s *agentSession) setForwards(st protocol.ForwardsState) {
	s.fwdMu.Lock()
	s.forwards = &st
	s.fwdMu.Unlock()
	s.sendState()
}

func (s *agentSession) forwardsCache() *protocol.ForwardsState {
	s.fwdMu.Lock()
	defer s.fwdMu.Unlock()
	return s.forwards
}

// sameForwardsState compares the fields a state frame carries, so the 60s loop
// stops reporting identical inventories.
func sameForwardsState(a, b protocol.ForwardsState) bool {
	if a.Supported != b.Supported || a.Initialized != b.Initialized ||
		a.Code != b.Code || a.Message != b.Message || len(a.Rules) != len(b.Rules) {
		return false
	}
	for i := range a.Rules {
		if a.Rules[i] != b.Rules[i] {
			return false
		}
	}
	return true
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

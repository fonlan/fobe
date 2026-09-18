// Package protocol defines the shared agent↔server message envelopes and
// payload types (design.md §7). Both cmd/server and cmd/agent import it, so
// every wire type lives here and nowhere else.
package protocol

import (
	"encoding/json"
	"time"
)

// Version is the wire protocol version carried in every envelope.
const Version = 1

// Message types agent → server.
const (
	TypeHello     = "hello"
	TypeMetrics   = "metrics"
	TypeTraffic   = "traffic"
	TypeLatency   = "latency"
	TypeState     = "state"
	TypeCmdResult = "cmd_result"
	TypePing      = "ping"
	// TypeAgentUpdate reports one agent self-update attempt (§5.5).
	TypeAgentUpdate = "agent_update"
	// Terminal (agent → server side of the browser session).
	TypeTerminalOutput = "terminal_output"
	TypeTerminalClosed = "terminal_closed"
)

// Message types server → agent.
const (
	TypeHelloAck   = "hello_ack"
	TypeDesired    = "desired"
	TypeCmd        = "cmd"
	TypeProbeMetr  = "probe_metrics"
	TypeLatencyCfg = "latency_config"
	TypeTermOpen   = "terminal_open"
	TypeTermInput  = "terminal_input"
	TypeTermResize = "terminal_resize"
	TypeTermClose  = "terminal_close"
)

// Message types that exist ONLY on the browser ↔ server relay and are never
// forwarded to the agent (design §12.7.1): the only real screen model lives in
// the browser's xterm instance — the agent just pumps raw PTY bytes and the
// server keeps no terminal state — so every observation the AI makes travels
// this correlated round trip.
const (
	// TypeTermQuery is server → browser: "hand me a window of your buffer".
	TypeTermQuery = "terminal_query"
	// TypeTermBuffer is browser → server: the answer, keyed by payload id.
	TypeTermBuffer = "terminal_buffer"
)

// Envelope is the JSON frame carried on the WebSocket in both directions.
type Envelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	TS      int64           `json:"ts"` // unix seconds
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope wraps a payload; payload may be nil for ping-style frames.
func NewEnvelope(typ, id string, payload any) Envelope {
	var raw json.RawMessage
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	return Envelope{V: Version, Type: typ, ID: id, TS: Now(), Payload: raw}
}

// Now is unix seconds; a var so tests can pin time.
var Now = func() int64 { return time.Now().Unix() }

// --- agent → server payloads ---

// Hello is the first frame after the WSS handshake authenticates the node.
type Hello struct {
	MachineID string `json:"machine_id"`
	Hostname  string `json:"hostname"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Kernel    string `json:"kernel"`
	CPUCores  int    `json:"cpu_cores"`
	TZ        string `json:"tz"` // IANA name, e.g. Asia/Shanghai
	// Linux distribution from the agent's /etc/os-release read (§16 basic
	// info): "debian" + "13", "ubuntu" + "24.04". Empty on non-Linux probes
	// and on agents older than the field (omitempty keeps the wire compatible).
	DistroID      string             `json:"distro_id,omitempty"`
	DistroVersion string             `json:"distro_version,omitempty"`
	Caps          Caps               `json:"caps"`
	IPs           []IPInfo           `json:"ips"`
	Interfaces    []NetworkInterface `json:"interfaces"`
	// SelfCheck marks the bypass handshake of a freshly downloaded binary
	// (§5.5). The server answers hello_ack and closes without registering the
	// connection or touching last_seen/agent_version — a plain handshake would
	// kick the running agent off the wire and make the panel report a version
	// that is not actually serving.
	SelfCheck bool `json:"selfcheck,omitempty"`
}

// Caps reports what the agent can do on this host (design §5.1).
type Caps struct {
	ICMP     bool `json:"icmp"` // raw socket available
	Systemd  bool `json:"systemd"`
	Procd    bool `json:"procd"`
	Fallback bool `json:"fallback"` // pidfile watchdog mode
	// SelfUpdate reports that this agent may replace its own binary (§5.5).
	// Agents built before §5.5 do not send it, which is exactly how the panel
	// tells "needs a manual reinstall" from "will follow on its own".
	SelfUpdate bool `json:"self_update"`
}

// IPInfo is one address of the probe (loopback / link-local excluded).
type IPInfo struct {
	IP        string `json:"ip"`
	Family    int    `json:"family"` // 4 | 6
	Scope     string `json:"scope"`  // "public" | "private"
	IsPrimary bool   `json:"is_primary,omitempty"`
}

// NetworkInterface is a selectable traffic interface discovered by the agent.
// Default marks the host's default-route interface; the panel may override it.
type NetworkInterface struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// Metrics is the 60s hardware sample (design §8.1).
type Metrics struct {
	CPU       float64 `json:"cpu"` // percent 0-100
	Load1     float64 `json:"load1"`
	MemUsed   uint64  `json:"mem_used"`
	MemTotal  uint64  `json:"mem_total"`
	SwapUsed  uint64  `json:"swap_used"`
	SwapTotal uint64  `json:"swap_total"`
	Disks     []Disk  `json:"disks"`
	NetRxRate float64 `json:"net_rx_rate"` // bytes/s on selected iface
	NetTxRate float64 `json:"net_tx_rate"`
	Uptime    uint64  `json:"uptime"` // seconds
}

// Disk is one statfs sample; Path is the mount point.
type Disk struct {
	Path       string `json:"path"`
	Total      uint64 `json:"total"`
	Used       uint64 `json:"used"`
	InodeTotal uint64 `json:"inode_total"`
	InodeUsed  uint64 `json:"inode_used"`
}

// Traffic is the counter report for the selected interface (design §8.2).
type Traffic struct {
	Iface string `json:"iface"`
	Rx    uint64 `json:"rx"` // raw cumulative counters
	Tx    uint64 `json:"tx"`
}

// LatencyBatch carries local samples accumulated over the last 60s.
type LatencyBatch struct {
	Samples []LatencySample `json:"samples"`
}

type LatencySample struct {
	TargetID int64   `json:"target_id"`
	TS       int64   `json:"ts"`
	ICMPMs   float64 `json:"icmp_ms"` // -1 = not measured
	TCPMs    float64 `json:"tcp_ms"`  // -1 = not measured
	Loss     float64 `json:"loss"`    // 0-1 for this point
}

// State is the slow-moving node info (design §7 `state`).
type State struct {
	IPs        []IPInfo           `json:"ips"`
	Interfaces []NetworkInterface `json:"interfaces"`
	BootID     string             `json:"boot_id"`
	Singbox    *SingboxState      `json:"singbox,omitempty"`
	// Forwards is the nftables port-forward inventory (§21). A nil pointer
	// means "this agent predates the feature" — the server then keeps the last
	// known rows instead of wiping them, exactly like Interfaces above.
	Forwards *ForwardsState `json:"forwards,omitempty"`
	// SingboxLocal is what sing-box this probe already runs *outside* fobe's
	// desired state — the one-sing.sh case (§9.3 实现修订 2026-09-17).
	//
	// It rides in every state frame and is deliberately independent of
	// Singbox: Singbox is nil until the panel declares a desired version, so
	// before this field existed a probe managed by one-sing.sh reported
	// nothing at all and the panel showed "not installed" for a host whose
	// sing-box was up and serving. A nil pointer means "this agent predates
	// the field" (the server then keeps what it has instead of wiping it).
	SingboxLocal *SingboxLocal `json:"singbox_local,omitempty"`
}

// SingboxLocal is the read-only local discovery report (§9.3 实现修订
// 2026-09-17): which sing-box is on disk, whether its unit is up, and the raw
// config.json bytes. The agent ships the file verbatim — parsing it is the
// server's job (it already owns sing-box config knowledge in §9.1/§9.4), so a
// new inbound type is one server-side parser change instead of an agent
// release on every probe.
type SingboxLocal struct {
	// Present is "a sing-box binary exists at the layout path". It stays true
	// after fobe takes the node over — the operator still needs to know what
	// is on disk.
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
	// Running is the unit/process state as the agent sees it. In unprivileged
	// mode the agent cannot ask systemd for it, so it falls back to its own
	// process scan (findProcByExe) — the same answer by a different route.
	Running bool `json:"running,omitempty"`
	// UnitActive / UnitKnown describe the platform service manager's view
	// (one-sing.sh's one-sing.service). UnitKnown=false means "could not ask"
	// (no systemd, no privileges), not "absent".
	UnitActive bool   `json:"unit_active,omitempty"`
	UnitKnown  bool   `json:"unit_known,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	// ConfigJSON is the file's content, capped by the agent (see
	// maxLocalConfigBytes). Empty when unreadable — Error then says why.
	ConfigJSON string `json:"config_json,omitempty"`
	// ConfigSHA256 is over the exact bytes above, so the server can tell an
	// unchanged report from a real edit without diffing the whole document.
	ConfigSHA256 string `json:"config_sha256,omitempty"`
	// AnytlsCerts carries the PEM of each anytls inbound's certificate file,
	// keyed by the inbound's port. Config.json names a *path*; a subscription
	// client needs the *bytes* to pin the server, and only the probe can read
	// that path (§9.3 实现修订 2026-09-17b — before this, a hand-managed anytls
	// inbound was skipped from every subscription because fobe had no
	// certificate to pin).
	//
	// Certificates are public data — the private key never leaves the probe
	// (§9.3), and this map is the same PEM the node reports for the inbound the
	// panel installed.
	AnytlsCerts map[int]string `json:"anytls_certs,omitempty"`
	// EffectiveInboundPorts is the set of configured listeners that the agent
	// could reach locally while sing-box was running. It is deliberately an
	// agent observation rather than a server-side guess from ConfigJSON: a
	// running process may still have failed to bind one of several inbounds.
	// Missing means an older agent, so the server must keep an entry pending.
	EffectiveInboundPorts []int `json:"effective_inbound_ports,omitempty"`
	// InboundChecksKnown distinguishes a new agent that checked every listener
	// and found none from an older one that has no listener-check capability.
	// It lets the server demote a previously running entry when it later stops
	// accepting connections, without treating an omitted old-agent field as bad.
	InboundChecksKnown bool   `json:"inbound_checks_known,omitempty"`
	Error              string `json:"error,omitempty"`
}

// SingboxState is what the agent actually observes locally. Optional fields
// are omitempty so old servers simply ignore them and old agents leave them
// zero for new servers (wire-compatible in both directions).
type SingboxState struct {
	Running      bool   `json:"running"`
	Version      string `json:"version"`
	Port         int    `json:"port"`
	CertSHA256   string `json:"cert_sha256,omitempty"`
	CertPEM      string `json:"cert_pem,omitempty"`
	CertNotAfter int64  `json:"cert_not_after,omitempty"` // unix
	LastError    string `json:"last_error,omitempty"`
	// FirewallHint carries the manual command text when the agent could not
	// open the inbound port itself (§9.2); empty = allowed / nothing to do.
	FirewallHint string `json:"firewall_hint,omitempty"`
	// RollbackHappened is a one-shot event flag set when gate ③ failed and
	// the .prev binary was restored (§9.2 rollback → §15 告警"回滚已执行").
	RollbackHappened bool `json:"rollback_happened,omitempty"`
}

// CmdResult answers a server cmd (design §7; stdout/stderr are truncated).
type CmdResult struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// Self-update phases carried by AgentUpdate (§5.5). planned/downloading/
// verifying are progress, committed is success; failed carries Class. The two
// extra conditions are terminal judgements the agent makes on its own and the
// panel renders as-is.
const (
	UpdatePlanned     = "planned"
	UpdateDownloading = "downloading"
	UpdateVerifying   = "verifying"
	UpdateCommitted   = "committed"
	UpdateFailed      = "failed"
	// UpdateSuppressed: this target already exhausted its attempt budget; the
	// agent stopped until the target changes or an operator hits retry.
	UpdateSuppressed = "suppressed"
	// UpdateUnsupported: no supervisor to restart under (nohup/fallback mode),
	// so the agent never replaces its own binary.
	UpdateUnsupported = "unsupported"
)

// Failure classes (§5.5): terminal stops retrying until the target changes or
// an operator retries; transient backs off and tries again.
const (
	ClassTerminal  = "terminal"
	ClassTransient = "transient"
)

// AgentUpdate reports one self-update attempt to the server (§5.5). The server
// keeps it in nodes.agent_update_* for the panel, writes an audit line, and
// raises the terminal/transient alerts.
type AgentUpdate struct {
	Target   string `json:"target"`
	Phase    string `json:"phase"`
	Class    string `json:"class,omitempty"`
	Error    string `json:"error,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// --- server → agent payloads ---

// HelloAck answers hello with the full desired state (design §7).
type HelloAck struct {
	NodeID         string       `json:"node_id"`
	Name           string       `json:"name"`
	ProbeMetrics   bool         `json:"probe_metrics"` // stream 5s samples while a detail page is open
	Desired        DesiredState `json:"desired"`
	LatencyTargets []TargetSpec `json:"latency_targets"`
	// LatencyIntervalSec controls local probes. Zero remains wire-compatible
	// with agents released before this setting and means their default (5s).
	LatencyIntervalSec int `json:"latency_interval_sec,omitempty"`
	// AgentTargetVersion is the agent build this server wants the probe to run
	// (§5.5). Empty means "self-update is disabled / not offered right now":
	// non-release server version, missing artifact, panel switch off, or the
	// kill switch is on. Agents compare it against their own build and switch
	// when it differs — in either direction.
	AgentTargetVersion string `json:"agent_target_version,omitempty"`
	// AgentUpdateAfter is the unix second before which the agent must not
	// start (§5.5 stagger). A server restart drops every agent connection at
	// once, so without a server-computed offset they would all fetch the same
	// 10 MB artifact in the same instant.
	AgentUpdateAfter int64 `json:"agent_update_after,omitempty"`
}

// DesiredState is declarative (design §7): the agent converges to this.
type DesiredState struct {
	Singbox *SingboxDesired `json:"singbox,omitempty"`
	// TrafficIface is nil for servers released before interface selection was
	// declarative. A non-nil empty string means "use the detected default";
	// any other value is the panel-selected interface.
	TrafficIface *string `json:"traffic_iface,omitempty"`
	// Agent self-update target (§5.5). These mirror the flat HelloAck fields so
	// an operator retry can nudge an online agent with a plain `desired` frame
	// instead of waiting for its next handshake. Both carriers are always
	// filled by the server; the agent reads them from here.
	AgentTargetVersion string `json:"agent_target_version,omitempty"`
	AgentUpdateAfter   int64  `json:"agent_update_after,omitempty"`
}

// SingboxDesired: install/update the given version and apply this config.
// Empty Version means "sing-box not managed / not installed on this node".
type SingboxDesired struct {
	Version    string `json:"version,omitempty"`
	ConfigJSON string `json:"config_json,omitempty"`
	Port       int    `json:"port,omitempty"`
	// Uninstall asks the probe to remove sing-box and everything fobe put next
	// to it (§9.2 实现修订 2026-09-16). Version is empty whenever this is set.
	//
	// It is deliberately part of the *declared* state rather than a one-shot
	// command: the operator may press 卸载 while the probe is offline, and a
	// queued command expires (10 min TTL) while hello_ack re-delivers the
	// desired state forever. An agent that predates this field ignores it
	// (empty version = unmanaged), so the panel keeps showing 卸载中 until the
	// probe catches up — no data is lost in either direction.
	Uninstall bool `json:"uninstall,omitempty"`
}

// TargetSpec is a latency target pushed to the agent (design §13).
type TargetSpec struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // icmp | tcp
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Cmd is a one-shot command (AI execution, panel actions, offline-queued).
type Cmd struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"` // run_shell | restart_singbox | ...
	Payload json.RawMessage `json:"payload,omitempty"`
}

// --- nftables port forwarding (design §21) ---

// CmdKindNftForwards is the §21 command kind: the payload is a
// ForwardsRequest, and the agent answers with a ForwardsResult encoded as JSON
// in CmdResult.Stdout (the same carrier tail_logs uses for text).
//
// A command — not desired state — is the right shape here: the ruleset is
// shared with other tools (nfpf.sh, hand-written nft), so the panel must not
// declare "the complete set". Each edit is one targeted transaction, and the
// agent reports the resulting ruleset in `state` right after it.
const CmdKindNftForwards = "nft_forwards"

// CmdKindSingboxScan asks the agent to re-read its local sing-box right now and
// push a state frame, instead of waiting for its next scan tick (design §9.3
// 实现修订 2026-09-17b). The panel uses it right after an operator edits
// config.json by hand — the editor model's subscription is rendered from the
// last reported file, so "look again" has to be a round trip, not a minute.
const CmdKindSingboxScan = "scan_singbox"

// Forwards actions carried by ForwardsRequest.Action.
const (
	ForwardsList   = "list"
	ForwardsAdd    = "add"
	ForwardsUpdate = "update"
	ForwardsDelete = "delete"
)

// ForwardRule is one IPv4 port forward as it exists on the probe, in the
// layout github.com/fonlan/nfpf (`nfpf.sh`) uses: a DNAT rule in
// `ip nat prerouting` plus a masquerade rule in `ip nat postrouting`.
//
// Handle is the nft rule handle of the DNAT rule. It is advisory — a ruleset
// reload renumbers handles — so the agent validates it against the rest of the
// rule before acting on it.
type ForwardRule struct {
	Proto   string `json:"proto"` // tcp | udp
	SrcPort int    `json:"src_port"`
	// Iface is the inbound interface (`iifname`); empty = all interfaces.
	Iface string `json:"iface,omitempty"`
	DstIP string `json:"dst_ip"`
	// DstPort is the rewritten port. nfpf also accepts a bare
	// `dnat to <ip>` (port unchanged); the agent normalizes that to
	// DstPort == SrcPort so the panel always shows both.
	DstPort int    `json:"dst_port"`
	Comment string `json:"comment,omitempty"`
	Handle  int    `json:"handle,omitempty"`
	// ExtraMatch marks a DNAT rule carrying match terms the panel does not
	// model (source address, port ranges, counters, …). It is listed so the
	// operator can see — and delete — it, but editing it would silently drop
	// those terms, so the panel refuses.
	ExtraMatch bool `json:"extra_match,omitempty"`
}

// ForwardsState is the probe's own view of the port-forward set (§21).
// Supported=false means the probe cannot manage forwards at all; Code explains
// why with a snake_case token ("nft_missing", "need_root", …) and Message
// carries the raw nft output for the panel's hint text.
type ForwardsState struct {
	Supported   bool          `json:"supported"`
	Initialized bool          `json:"initialized"`
	Code        string        `json:"code,omitempty"`
	Message     string        `json:"message,omitempty"`
	Rules       []ForwardRule `json:"rules"`
}

// ForwardsRequest is the payload of CmdKindNftForwards. Old is the rule to
// replace/delete (ForwardsUpdate / ForwardsDelete); Rule is the new state.
type ForwardsRequest struct {
	Action string       `json:"action"`
	Rule   *ForwardRule `json:"rule,omitempty"`
	Old    *ForwardRule `json:"old,omitempty"`
}

// ForwardsResult is the JSON body the agent writes into CmdResult.Stdout.
// Error is a snake_case token the server maps onto its own error codes; State
// always carries the post-action ruleset so the panel updates without a
// second round trip.
type ForwardsResult struct {
	OK       bool          `json:"ok"`
	Error    string        `json:"error,omitempty"`
	Message  string        `json:"message,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	State    ForwardsState `json:"state"`
}

// Terminal frames (design §11). Server→agent open/input/resize/close;
// agent→server output/closed. The agent opens its own local PTY, so no SSH
// target or authentication data ever travels through this protocol.
// SessionID ties frames to the browser socket.
type TerminalOpen struct {
	SessionID string `json:"session_id"`
	// Mode remains on the v1 wire as "pty" for compatibility with agents
	// released before Web Terminal replaced Web SSH. It is server-controlled;
	// clients must not use it to select another terminal backend.
	Mode string `json:"mode,omitempty"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type TerminalInput struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
}

type TerminalResize struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

type TerminalClose struct {
	SessionID string `json:"session_id"`
}

type TerminalOutput struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
}

type TerminalClosed struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

// TerminalQuery asks the browser for a window of its xterm buffer. It is the
// server's only way to see a screen: the PTY's bytes vanish into the browser
// (the server neither stores nor parses them), so "what is on the terminal" is
// answered by the client that holds the emulator.
type TerminalQuery struct {
	// ID correlates the answer (TerminalBuffer.ID); one per request.
	ID string `json:"id"`
	// Offset counts lines up from the very bottom of the buffer (0 = bottom).
	// Deliberately measured from the bottom rather than from the operator's
	// scroll position: a viewport the operator scrolled up to read is stale
	// state, not "what the terminal currently says" (§12.7.2).
	Offset int `json:"offset"`
	// Lines is the window height. <=0 means "the browser's own viewport height"
	// — the server does not know the terminal geometry (the browser resizes it).
	Lines int `json:"lines"`
}

// TerminalBuffer is the browser's answer to one TerminalQuery: the requested
// window as plain text lines, already stripped of ANSI by the emulator.
type TerminalBuffer struct {
	ID string `json:"id"`
	// OK is false for a query the client could not serve (e.g. the terminal was
	// torn down between ask and answer); Error says why.
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Length int    `json:"length,omitempty"`
	// Lines is the window in top-to-bottom order, right-trimmed per line.
	Lines []string `json:"lines,omitempty"`
}

// ProbeMetrics toggles the temporary 5s high-frequency stream (design §16).
type ProbeMetrics struct {
	Enabled bool `json:"enabled"`
}

// LatencyConfig updates local latency probing without reconnecting the agent.
// IntervalSec is the global cadence; the server broadcasts it when the panel
// setting changes. Targets is the per-node measurement list (design §13):
// nil — what a global broadcast carries — keeps the current list, while a
// non-nil slice, *including empty*, replaces it, so a node can actually opt
// out. Without the per-node push an edited target list only reached the agent
// via its next hello_ack, i.e. possibly never on a long-lived connection.
type LatencyConfig struct {
	IntervalSec int          `json:"interval_sec"`
	Targets     []TargetSpec `json:"targets"`
}

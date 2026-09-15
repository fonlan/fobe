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
	TypeTermOpen   = "terminal_open"
	TypeTermInput  = "terminal_input"
	TypeTermResize = "terminal_resize"
	TypeTermClose  = "terminal_close"
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
	MachineID string   `json:"machine_id"`
	Hostname  string   `json:"hostname"`
	Version   string   `json:"version"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	Kernel    string   `json:"kernel"`
	CPUCores  int      `json:"cpu_cores"`
	TZ        string   `json:"tz"` // IANA name, e.g. Asia/Shanghai
	Caps      Caps     `json:"caps"`
	IPs       []IPInfo `json:"ips"`
}

// Caps reports what the agent can do on this host (design §5.1).
type Caps struct {
	ICMP     bool `json:"icmp"` // raw socket available
	Systemd  bool `json:"systemd"`
	Procd    bool `json:"procd"`
	Fallback bool `json:"fallback"` // pidfile watchdog mode
}

// IPInfo is one address of the probe (loopback / link-local excluded).
type IPInfo struct {
	IP        string `json:"ip"`
	Family    int    `json:"family"` // 4 | 6
	Scope     string `json:"scope"`  // "public" | "private"
	IsPrimary bool   `json:"is_primary,omitempty"`
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

// LatencyBatch carries the 5s-local samples accumulated over the last 60s.
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
	IPs     []IPInfo      `json:"ips"`
	BootID  string        `json:"boot_id"`
	Singbox *SingboxState `json:"singbox,omitempty"`
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

// --- server → agent payloads ---

// HelloAck answers hello with the full desired state (design §7).
type HelloAck struct {
	NodeID         string       `json:"node_id"`
	Name           string       `json:"name"`
	ProbeMetrics   bool         `json:"probe_metrics"` // stream 5s samples while a detail page is open
	Desired        DesiredState `json:"desired"`
	LatencyTargets []TargetSpec `json:"latency_targets"`
}

// DesiredState is declarative (design §7): the agent converges to this.
type DesiredState struct {
	Singbox *SingboxDesired `json:"singbox,omitempty"`
}

// SingboxDesired: install/update the given version and apply this config.
// Empty Version means "sing-box not managed / not installed on this node".
type SingboxDesired struct {
	Version    string `json:"version,omitempty"`
	ConfigJSON string `json:"config_json,omitempty"`
	Port       int    `json:"port,omitempty"`
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

// ProbeMetrics toggles the temporary 5s high-frequency stream (design §16).
type ProbeMetrics struct {
	Enabled bool `json:"enabled"`
}

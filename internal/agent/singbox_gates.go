// Acceptance gates: gate ① config check and gate ③ port observation window
// (design §9.2), plus the inbound-port parsing they share.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
)

func checkConfig(bin, configPath string) error {
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("no config to check: %w", err)
	}
	if out, err := runCmd(bin, checkTimeout, "check", "-c", configPath); err != nil {
		return fmt.Errorf("config check: %w: %s", err, tailStr(out, 200))
	}
	return nil
}

// report assembles the reportable state and publishes it, attaching the
// sticky firewall hint (§9.2) and draining the one-shot rollback flag so
// rollback is reported exactly once (§15 告警"回滚已执行"). The error text is
// remembered for reportOnly (see there): while the node is off its desired
// version, "no news" must not be reported as "no problem".

func (m *singboxManager) verifyWindow(bin string, ports []int) error {
	deadline := time.Now().Add(observeWindow)
	for {
		if !m.processAlive(bin) {
			return errors.New("sing-box process exited during observation window")
		}
		if missing := firstUnreachablePort(ports); missing == 0 {
			return nil
		} else if !time.Now().Before(deadline) {
			return fmt.Errorf("port %d not reachable within %s", missing, observeWindow)
		}
		time.Sleep(observeStep)
	}
}

func firstUnreachablePort(ports []int) int {
	for _, port := range ports {
		if !tcpConnectable(port) {
			return port
		}
	}
	return 0
}

// configListenPorts returns every distinct inbound listen_port a config
// declares, in file order. These are the listeners sing-box will actually
// open — the thing gate ③ can hold the config to.
func configListenPorts(configJSON string) []int {
	var cfg struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil
	}
	seen := map[int]bool{}
	ports := make([]int, 0, len(cfg.Inbounds))
	for _, inbound := range cfg.Inbounds {
		if inbound.ListenPort <= 0 || seen[inbound.ListenPort] {
			continue
		}
		seen[inbound.ListenPort] = true
		ports = append(ports, inbound.ListenPort)
	}
	return ports
}

// gate3Ports decides what gate ③ may demand of this apply.
//
// It verifies the *config's* listeners, never the desired frame's bookkeeping
// port: `node_singbox.port` is sticky by design (an uninstall/reinstall round
// trip keeps the operator's port), so it can name a listener the config no
// longer contains — and then every apply on the node fails verification for a
// port nobody asked sing-box to open, rolled back after 30s each time. That is
// not hypothetical: a panel that deleted an inbound put every later edit of
// that node into exactly that dead loop (verify: port 22039 not reachable).
//
// When sing-box was already running, only listeners that answered *before* the
// restart are evidence that this apply broke something: a listener the probe
// never served (one bound to a specific non-loopback address, say) cannot get
// better or worse from an unrelated edit, and demanding it anyway let one
// stuck listener block every edit of the file. A fresh start has no baseline,
// so there the config is held to its full promise: every declared listener
// must come up.
func gate3Ports(newConfig string, wasRunning bool, probe func(int) bool) []int {
	ports := configListenPorts(newConfig)
	if len(ports) == 0 || !wasRunning {
		return ports
	}
	required := make([]int, 0, len(ports))
	for _, port := range ports {
		if probe(port) {
			required = append(required, port)
		}
	}
	return required
}

// gate3TargetPorts is gate3Ports against the config this apply is about to
// serve: the desired document when there is one, the on-disk file otherwise
// (a version-only apply re-serves what is already there).
func gate3TargetPorts(d *protocol.SingboxDesired, configPath string, wasRunning bool) []int {
	served := d.ConfigJSON
	if served == "" {
		if raw, err := os.ReadFile(configPath); err == nil {
			served = string(raw)
		}
	}
	return gate3Ports(served, wasRunning, tcpConnectable)
}

func tcpConnectable(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), tcpDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// effectivePort is the firewall pass's target: the desired port, falling back
// to the first inbound listen_port in the config. It is bookkeeping for the
// one-shot allow rule (§9.2), deliberately not gate ③'s demand set — a sticky
// port the config no longer declares must not fail an apply (see gate3Ports).
func effectivePort(d *protocol.SingboxDesired) int {
	if d.Port > 0 {
		return d.Port
	}
	return parseListenPort(d.ConfigJSON)
}

// parseListenPort extracts the first inbound listen_port from a config JSON.
func parseListenPort(configJSON string) int {
	var cfg struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return 0
	}
	for _, in := range cfg.Inbounds {
		if in.ListenPort > 0 {
			return in.ListenPort
		}
	}
	return 0
}

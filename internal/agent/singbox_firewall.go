// Firewall auto-allow for managed sing-box ports (design §9.2).
package agent

import (
	"os/exec"
	"strconv"
)

// --- firewall auto-allow (§9.2) ---

// fwProbes bundles the environment lookups the firewall pass needs so tests
// can inject fake detection and command execution.
type fwProbes struct {
	lookPath   func(string) (string, error)
	fileExists func(string) bool
	run        func(name string, args ...string) error // bounded by fwCmdTimeout
}

func (p fwProbes) available(name string) bool {
	_, err := p.lookPath(name)
	return err == nil
}

// hasNft is deliberately cautious (§9.2): the nft rule is only attempted when
// both the binary and an existing ruleset are present, and failure is never
// fatal — an "inet filter input" chain may simply not exist.
func (p fwProbes) hasNft() bool {
	return p.available("nft") && p.fileExists(nftablesConfPath)
}

// fwAttempt is one firewall front-end: the commands to run (all must succeed,
// in order) and the raw command text suggested to the operator when nothing
// worked (§9.2 失败返回命令原文).
type fwAttempt struct {
	name string
	cmds [][]string
	hint string
}

func (a fwAttempt) apply(run func(name string, args ...string) error) bool {
	for _, cmd := range a.cmds {
		if err := run(cmd[0], cmd[1:]...); err != nil {
			return false
		}
	}
	return true
}

// firewallCandidates lists the port allow rules in priority order (§9.2):
// ufw → firewalld → nft → iptables (OpenWrt/兜底). Pure over injected probe
// results so the selection is unit-testable.
func firewallCandidates(port int, hasUfw, hasFirewalld, hasNft, hasIptables bool) []fwAttempt {
	p := strconv.Itoa(port)
	proto := p + "/tcp"
	out := []fwAttempt{}
	if hasUfw {
		out = append(out, fwAttempt{
			name: "ufw",
			cmds: [][]string{{"ufw", "allow", proto}},
			hint: "ufw allow " + proto,
		})
	}
	if hasFirewalld {
		out = append(out, fwAttempt{
			name: "firewalld",
			cmds: [][]string{
				{"firewall-cmd", "--add-port=" + proto, "--permanent"},
				{"firewall-cmd", "--reload"},
			},
			hint: "firewall-cmd --add-port=" + proto + " --permanent && firewall-cmd --reload",
		})
	}
	if hasNft {
		out = append(out, fwAttempt{
			name: "nft",
			cmds: [][]string{{"nft", "add", "rule", "inet", "filter", "input", "tcp", "dport", p, "accept"}},
			hint: "nft add rule inet filter input tcp dport " + p + " accept",
		})
	}
	if hasIptables {
		out = append(out, fwAttempt{
			name: "iptables",
			cmds: [][]string{{"iptables", "-I", "INPUT", "-p", "tcp", "--dport", p, "-j", "ACCEPT"}},
			hint: "iptables -I INPUT -p tcp --dport " + p + " -j ACCEPT",
		})
	}
	return out
}

// maybeAllowFirewall opens the inbound port once per distinct port (§9.2:
// 首次安装/端口变化时尝试一次,不随每次收敛重跑). On total failure the
// manual command is kept and reported in every state frame until the next
// port resets it.
func (m *singboxManager) maybeAllowFirewall(port int) {
	if port <= 0 {
		return
	}
	m.mu.Lock()
	attempted := port == m.lastFwPort
	m.lastFwPort = port
	m.mu.Unlock()
	if attempted {
		return
	}
	hint := m.openFw(port)
	m.mu.Lock()
	m.lastFwHint = hint
	m.mu.Unlock()
	if hint != "" {
		m.log.Warn("firewall auto-allow failed; manual command required", "port", port, "cmd", hint)
		return
	}
	m.log.Info("firewall allow ok", "port", port)
}

// openFirewallPort runs every available front-end in order, each command
// directly via exec (no shell) with a 10s budget. Returns the raw manual
// command when all candidates failed, "" when a rule was applied — or when
// the host has no firewall manager at all (nothing to suggest).
func (m *singboxManager) openFirewallPort(port int) string {
	probe := fwProbes{
		lookPath:   exec.LookPath,
		fileExists: fileExists,
		run: func(name string, args ...string) error {
			_, err := runCmd(name, fwCmdTimeout, args...)
			return err
		},
	}
	return runFirewallCandidates(port, probe)
}

func runFirewallCandidates(port int, probe fwProbes) string {
	candidates := firewallCandidates(
		port,
		probe.available("ufw"),
		probe.available("firewall-cmd"),
		probe.hasNft(),
		probe.available("iptables"),
	)
	if len(candidates) == 0 {
		return ""
	}
	for _, c := range candidates {
		if c.apply(probe.run) {
			return ""
		}
	}
	return candidates[0].hint // suggest the highest-priority front-end's command
}

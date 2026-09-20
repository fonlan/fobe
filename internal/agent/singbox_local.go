// Local sing-box discovery: read the one-sing.sh instance this machine already
// runs (design §9.3 实现修订 2026-09-17).
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/fonlan/fobe/internal/agent/service"
	"github.com/fonlan/fobe/internal/protocol"
)

// --- local discovery (§9.3 实现修订 2026-09-17) ---

// scanLocal reads the on-disk sing-box and publishes it when it changed.
// Cheap enough for the 60s tick: two stats, one `version` exec and one file
// read (capped). Errors are reported in the payload rather than logged away —
// "the file is there but unreadable" is operator-relevant on a host whose
// config an operator wrote by hand.
func (m *singboxManager) scanLocal() {
	got := detectLocal()
	m.mu.Lock()
	same := localEqual(m.local, got)
	m.local = got
	m.mu.Unlock()
	if !same {
		m.nudge(m.changedLocal)
	}
}

// ScanLocal re-reads the local sing-box immediately and publishes the report
// when it changed. It is `scanLocal` for other packages (the panel's
// "refresh from probe" command, §9.3 实现修订 2026-09-17b).
func (m *singboxManager) ScanLocal() { m.scanLocal() }

// Local returns the last discovery report, or nil before the first scan.
func (m *singboxManager) Local() *protocol.SingboxLocal {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.local == nil {
		return nil
	}
	cp := *m.local
	return &cp
}

// LocalChanged is the discovery-side notification stream (buffered, never
// closed — like Changed, the manager outlives sessions).
func (m *singboxManager) LocalChanged() <-chan struct{} { return m.changedLocal }

// localEqual compares two reports, ignoring nothing: the caller only wants a
// frame when something an operator could see has changed, and the config hash
// covers edits inside the file.
func localEqual(a, b *protocol.SingboxLocal) bool {
	if a == nil || b == nil {
		return a == b
	}
	// Not a struct comparison: the payload carries a map (the anytls
	// certificates), which Go refuses to compare with ==.
	if a.Present != b.Present || a.Version != b.Version || a.Running != b.Running ||
		a.UnitActive != b.UnitActive || a.UnitKnown != b.UnitKnown ||
		a.ConfigPath != b.ConfigPath || a.ConfigJSON != b.ConfigJSON ||
		a.ConfigSHA256 != b.ConfigSHA256 || a.InboundChecksKnown != b.InboundChecksKnown || a.Error != b.Error ||
		len(a.AnytlsCerts) != len(b.AnytlsCerts) ||
		len(a.EffectiveInboundPorts) != len(b.EffectiveInboundPorts) {
		return false
	}
	for port, pem := range a.AnytlsCerts {
		if b.AnytlsCerts[port] != pem {
			return false
		}
	}
	for i, port := range a.EffectiveInboundPorts {
		if b.EffectiveInboundPorts[i] != port {
			return false
		}
	}
	return true
}

// maxCertBytes caps one certificate read. A self-signed cert is ~1 KB; anything
// past this is not a certificate, and shipping it in every state frame would be
// pure wire cost.
const maxCertBytes = 64 << 10

// readAnytlsCerts reads the certificate file each anytls inbound points at,
// keyed by the inbound's port.
//
// The file names a path; a subscription client needs the bytes to pin the
// server, and the probe is the only party that can read that path. Best-effort
// per inbound: a missing or oversized file is simply omitted, and the renderer
// then skips that inbound (rendering it insecure=true is the one thing this
// project does not do, §9.3).
func readAnytlsCerts(configJSON []byte) map[int]string {
	var doc struct {
		Inbounds []struct {
			Type       string `json:"type"`
			ListenPort int    `json:"listen_port"`
			TLS        *struct {
				CertificatePath string `json:"certificate_path"`
			} `json:"tls"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(configJSON, &doc); err != nil {
		return nil
	}
	out := map[int]string{}
	for _, in := range doc.Inbounds {
		if in.Type != "anytls" || in.ListenPort <= 0 || in.TLS == nil || in.TLS.CertificatePath == "" {
			continue
		}
		if len(out) >= 8 {
			break // a config with more anytls inbounds than this is not one we can serve anyway
		}
		raw, err := os.ReadFile(in.TLS.CertificatePath)
		if err != nil || len(raw) == 0 || len(raw) > maxCertBytes {
			continue
		}
		if !bytes.Contains(raw, []byte("BEGIN CERTIFICATE")) {
			continue
		}
		out[in.ListenPort] = string(raw)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maxLocalConfigBytes caps the config file the agent will read into a report.
// one-sing.sh's own output is a few KiB; anything past this is either not a
// sing-box config or an operator's hand-written monster, and shipping it in
// every state frame would be pure wire cost.
const maxLocalConfigBytes = 1 << 20

// detectLocal is the read-only probe behind SingboxLocal.
//
// It answers from the *effective* layout (service.SingboxPaths), so an
// unprivileged probe reports its own relocation and a root probe reports
// /etc/one-sing — the same paths one-sing.sh uses, which is what makes the
// takeover possible at all.
func detectLocal() *protocol.SingboxLocal {
	bin, configPath, _ := service.SingboxPaths()
	out := &protocol.SingboxLocal{ConfigPath: configPath}
	var problems []string

	if _, err := os.Stat(bin); err != nil {
		out.Present = false
		if !os.IsNotExist(err) {
			problems = append(problems, fmt.Sprintf("stat %s: %v", bin, err))
		}
		out.Error = strings.Join(problems, "; ")
		// A binary that is not there cannot be running: skip the rest rather
		// than reporting a unit state for someone else's install.
		return out
	}
	out.Present = true

	if raw, err := runCmd(bin, versionTimeout, "version"); err == nil {
		out.Version = parseSingboxVersion(raw)
	} else {
		problems = append(problems, fmt.Sprintf("version: %v", err))
	}

	// The process scan needs no privileges and is exactly how the fallback
	// branch already decides liveness, so it is the portable half of the
	// answer; the unit state below is the accurate half when we may ask.
	if findProcByExe(bin) > 0 {
		out.Running = true
	}
	if privileged() {
		switch service.Detect() {
		case service.KindSystemd:
			out.UnitKnown = true
			out.UnitActive = service.SingboxActive()
		case service.KindProcd:
			out.UnitKnown = true
			out.UnitActive = service.SingboxActive()
		}
	}
	if out.UnitActive {
		out.Running = true
	}

	switch raw, err := os.ReadFile(configPath); {
	case err == nil:
		if len(raw) > maxLocalConfigBytes {
			problems = append(problems, fmt.Sprintf("config %s is %d bytes (cap %d), not reported",
				configPath, len(raw), maxLocalConfigBytes))
			break
		}
		out.ConfigJSON = string(raw)
		out.ConfigSHA256 = sha256Hex(raw)
		out.AnytlsCerts = readAnytlsCerts(raw)
		out.InboundChecksKnown = true
		if out.Running {
			out.EffectiveInboundPorts = effectiveInboundPorts(raw)
		}
	case os.IsNotExist(err):
		problems = append(problems, "config file does not exist")
	default:
		problems = append(problems, fmt.Sprintf("read config: %v", err))
	}

	out.Error = strings.Join(problems, "; ")
	return out
}

// effectiveInboundPorts reports every configured listener that accepts a local
// TCP connection. The raw config only declares intent; this is the evidence the
// server needs before changing a newly added listener from pending to running.
func effectiveInboundPorts(configJSON []byte) []int {
	var cfg struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil
	}
	ports := make([]int, 0, len(cfg.Inbounds))
	seen := map[int]bool{}
	for _, inbound := range cfg.Inbounds {
		if inbound.ListenPort <= 0 || seen[inbound.ListenPort] || !tcpConnectable(inbound.ListenPort) {
			continue
		}
		seen[inbound.ListenPort] = true
		ports = append(ports, inbound.ListenPort)
	}
	return ports
}

// needWatch reports whether the desired instance lost its process (fallback
// watchdog, §5.3) or drifted from the desired version. Failed attempts back
// off so a broken download does not retry every 60s.

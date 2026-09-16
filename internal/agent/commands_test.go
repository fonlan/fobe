package agent

import (
	"encoding/json"
	"testing"

	"github.com/fonlan/fobe/internal/protocol"
)

// The envelope type is always "cmd"; the kind travels inside the payload.
// Reading env.Type answered every panel/AI command with
// "unsupported command kind: cmd".
func TestCommandFromEnvelopeReadsKindFromPayload(t *testing.T) {
	wire, err := json.Marshal(protocol.Cmd{
		ID: "c1", Kind: "run_shell", Payload: json.RawMessage(`{"command":"echo hi"}`),
	})
	if err != nil {
		t.Fatalf("marshal cmd: %v", err)
	}
	cmd, err := commandFromEnvelope(protocol.Envelope{
		V: protocol.Version, Type: protocol.TypeCmd, ID: "c1", Payload: wire,
	})
	if err != nil {
		t.Fatalf("commandFromEnvelope: %v", err)
	}
	if cmd.Kind != "run_shell" {
		t.Fatalf("kind = %q, want run_shell", cmd.Kind)
	}
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(cmd.Payload, &p); err != nil || p.Command != "echo hi" {
		t.Fatalf("payload = %s (%v), want the shell command", cmd.Payload, err)
	}
	// the envelope id is the fallback when the payload carries none
	cmd, err = commandFromEnvelope(protocol.Envelope{Type: protocol.TypeCmd, ID: "env-id", Payload: json.RawMessage(`{"kind":"tail_logs"}`)})
	if err != nil || cmd.ID != "env-id" {
		t.Fatalf("id = %q (%v), want the envelope id", cmd.ID, err)
	}
}

func TestCommandFromEnvelopeReportsBadFrames(t *testing.T) {
	if _, err := commandFromEnvelope(protocol.Envelope{
		Type: protocol.TypeCmd, ID: "c2", Payload: json.RawMessage(`{"id":"c2"}`),
	}); err == nil {
		t.Fatal("missing kind must be reported, not silently executed")
	}
	if _, err := commandFromEnvelope(protocol.Envelope{
		Type: protocol.TypeCmd, ID: "c3", Payload: json.RawMessage(`not-json`),
	}); err == nil {
		t.Fatal("undecodable payload must be reported")
	}
}

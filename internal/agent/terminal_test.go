package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/fobe-panel/fobe/internal/protocol"
)

func TestTerminalSizeDefaultsAndBounds(t *testing.T) {
	tests := []struct {
		name       string
		cols, rows int
		wantCols   int
		wantRows   int
		wantOK     bool
	}{
		{name: "defaults", wantCols: defaultTerminalCols, wantRows: defaultTerminalRows, wantOK: true},
		{name: "valid", cols: 120, rows: 40, wantCols: 120, wantRows: 40, wantOK: true},
		{name: "negative columns", cols: -1, rows: 24, wantOK: false},
		{name: "too many rows", cols: 80, rows: maxTerminalRows + 1, wantOK: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cols, rows, ok := terminalSize(test.cols, test.rows)
			if cols != test.wantCols || rows != test.wantRows || ok != test.wantOK {
				t.Fatalf("terminalSize(%d, %d) = (%d, %d, %t), want (%d, %d, %t)", test.cols, test.rows, cols, rows, ok, test.wantCols, test.wantRows, test.wantOK)
			}
		})
	}
}

func TestTerminalManagerRejectsLegacySSHMode(t *testing.T) {
	session := &agentSession{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		urgentSend: make(chan protocol.Envelope, 1),
		done:       make(chan struct{}),
	}
	manager := newTerminalManager(session)
	manager.open(protocol.TerminalOpen{SessionID: "terminal", Mode: "serial"})

	frame := <-session.urgentSend
	if frame.Type != protocol.TypeTerminalClosed {
		t.Fatalf("frame type = %q, want terminal_closed", frame.Type)
	}
	var closed protocol.TerminalClosed
	if err := json.Unmarshal(frame.Payload, &closed); err != nil {
		t.Fatal(err)
	}
	if closed.SessionID != "terminal" || closed.Reason != "terminal_unsupported_mode" {
		t.Fatalf("terminal closed = %#v", closed)
	}
}

func TestTerminalEnvironmentReplacesTERM(t *testing.T) {
	env := terminalEnvironment([]string{"PATH=/bin", "TERM=dumb", "TERM=xterm"})
	termCount := 0
	for _, value := range env {
		if value == "TERM=xterm-256color" {
			termCount++
		}
		if value == "TERM=dumb" || value == "TERM=xterm" {
			t.Fatalf("old TERM remained in environment: %q", value)
		}
	}
	if termCount != 1 {
		t.Fatalf("TERM count = %d, want 1", termCount)
	}
}

func TestTerminalPayloadSessionIDs(t *testing.T) {
	payload := protocol.TerminalInput{SessionID: "active", Data: "echo ok\n"}
	env := protocol.NewEnvelope(protocol.TypeTermInput, "request", payload)
	var decoded protocol.TerminalInput
	if err := json.Unmarshal(env.Payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SessionID != payload.SessionID || decoded.Data != payload.Data {
		t.Fatalf("decoded payload = %#v, want %#v", decoded, payload)
	}
}

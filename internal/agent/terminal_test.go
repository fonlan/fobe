package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/fonlan/fobe/internal/protocol"
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

func TestTerminalEnvironmentReplacesTERMAndSetsShell(t *testing.T) {
	env := terminalEnvironment([]string{"PATH=/bin", "TERM=dumb", "TERM=xterm", "SHELL=/bin/stale"}, "/bin/bash")
	termCount, shellCount := 0, 0
	for _, value := range env {
		switch value {
		case "TERM=xterm-256color":
			termCount++
		case "SHELL=/bin/bash":
			shellCount++
		case "TERM=dumb", "TERM=xterm", "SHELL=/bin/stale":
			t.Fatalf("stale value remained in environment: %q", value)
		}
	}
	if termCount != 1 || shellCount != 1 {
		t.Fatalf("TERM count = %d, SHELL count = %d, want 1 and 1", termCount, shellCount)
	}
}

// Regression (2026-09-20): the web terminal used to fall straight to /bin/sh
// when $SHELL was absent — which it always is under systemd/procd — so Debian
// boxes ran dash and OpenWrt ran busybox ash even where the login shell is
// bash. dash/ash have no line editor, hence no bracketed paste, which disabled
// the panel's multi-line quick commands. The passwd entry is what sshd uses.
func TestPasswdLoginShell(t *testing.T) {
	const passwd = `root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
svc:x:2:2::/home/svc:/bin/false
alice:x:1000:1000:Alice,,,:/home/alice:/usr/bin/zsh
short:x:3:3:too:few
openwrt:x:0:0:root:/root:/bin/ash
`
	cases := []struct {
		name string
		uid  int
		want string
	}{
		{"first matching entry wins", 0, "/bin/bash"},
		{"regular user", 1000, "/usr/bin/zsh"},
		{"nologin means no interactive shell", 1, ""},
		{"false means no interactive shell", 2, ""},
		{"empty shell field", 3, ""},
		{"uid absent", 2000, ""},
	}
	for _, tc := range cases {
		if got := passwdLoginShell(passwd, tc.uid); got != tc.want {
			t.Errorf("%s: passwdLoginShell(uid=%d) = %q, want %q", tc.name, tc.uid, got, tc.want)
		}
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

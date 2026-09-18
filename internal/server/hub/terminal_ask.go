package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
)

// Server-initiated terminal queries (design §12.7.1).
//
// Why this lives in the hub: the browser holding the terminal is an HTTP/WS
// connection the hub does not own, but the terminal relay is the only place
// that knows how to reach it. The relay registers a push function per session
// and hands every inbound `terminal_buffer` frame back here, so the AI's tool
// layer can do a plain ask-and-wait without knowing anything about sockets.
//
// The asymmetry worth remembering: the *server* has no screen. Raw PTY bytes
// arrive on the agent link, are forwarded to whichever browser is subscribed,
// and are **dropped when nobody is listening** (relayTerminal's `if ok` has no
// else). So "what is on the terminal" is a question only the browser can
// answer, and a keystroke sent while no browser holds the session goes nowhere.

var (
	// ErrNoTerminalBrowser means no browser currently holds that terminal
	// session — the operator closed the page, or the terminal WS reconnected
	// and minted a new session id (§12.7.5).
	ErrNoTerminalBrowser = errors.New("no browser attached to that terminal session")
	// ErrTerminalQueryTimeout means the browser did not answer in time.
	ErrTerminalQueryTimeout = errors.New("terminal query timed out")
)

// RegisterTerminalPush records how to reach the browser that owns sessionID.
// The returned function removes the registration (idempotent).
func (h *Hub) RegisterTerminalPush(sessionID string, push func(protocol.Envelope) bool) func() {
	if sessionID == "" || push == nil {
		return func() {}
	}
	h.termMu.Lock()
	h.termPush[sessionID] = push
	h.termMu.Unlock()
	return func() {
		h.termMu.Lock()
		delete(h.termPush, sessionID)
		h.termMu.Unlock()
	}
}

// DeliverTerminalBuffer routes one browser answer to the goroutine waiting for
// it. Unknown ids are dropped on purpose: the asker may have timed out and
// moved on, and that is not an error worth surfacing.
func (h *Hub) DeliverTerminalBuffer(b protocol.TerminalBuffer) bool {
	if b.ID == "" {
		return false
	}
	h.termMu.Lock()
	ch, ok := h.termWait[b.ID]
	h.termMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- b:
	default: // already answered; the second answer is stale
	}
	return true
}

// AskTerminalBuffer asks the browser holding sessionID for a window of its
// xterm buffer and waits for the answer.
//
// The wait is bounded by both the timeout and ctx: this call happens inside a
// streaming AI turn, so it must not outlive the operator's connection
// (§12.7.1). Pending waiters are deliberately not flushed when a browser
// disappears — the timeout is the bound, and callers keep it short.
func (h *Hub) AskTerminalBuffer(ctx context.Context, sessionID string, offset, lines int, timeout time.Duration) (protocol.TerminalBuffer, error) {
	reqID, err := terminalQueryID()
	if err != nil {
		return protocol.TerminalBuffer{}, err
	}

	h.termMu.Lock()
	push, ok := h.termPush[sessionID]
	if ok {
		h.termWait[reqID] = make(chan protocol.TerminalBuffer, 1)
	}
	ch := h.termWait[reqID]
	h.termMu.Unlock()
	if !ok {
		return protocol.TerminalBuffer{}, ErrNoTerminalBrowser
	}
	defer func() {
		h.termMu.Lock()
		delete(h.termWait, reqID)
		h.termMu.Unlock()
	}()

	env := protocol.NewEnvelope(protocol.TypeTermQuery, reqID, protocol.TerminalQuery{
		ID: reqID, Offset: offset, Lines: lines,
	})
	if !push(env) {
		return protocol.TerminalBuffer{}, ErrNoTerminalBrowser
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case answer := <-ch:
		if !answer.OK {
			msg := answer.Error
			if msg == "" {
				msg = "the browser could not read its terminal buffer"
			}
			return answer, errors.New(msg)
		}
		return answer, nil
	case <-timer.C:
		return protocol.TerminalBuffer{}, ErrTerminalQueryTimeout
	case <-ctx.Done():
		return protocol.TerminalBuffer{}, ctx.Err()
	}
}

// SendTerminalKeys types raw bytes into the PTY of sessionID (§12.7.3/§12.7.4).
//
// false means no browser holds that session. The caller needs this signal
// because the *agent* drops input for a session id that is not its active PTY
// **silently** (terminalManager.input just returns when the id does not match),
// so without it a tool would report "typed" for keystrokes that went nowhere.
//
// Known limitation (design §12.7.5): if a second terminal tab replaced the PTY,
// the old browser is still registered here and the agent still drops our bytes.
// Detecting that needs an ack from the agent; today the AI just sees "typed".
func (h *Hub) SendTerminalKeys(nodeID, sessionID, data string) bool {
	if nodeID == "" || sessionID == "" || data == "" {
		return false
	}
	h.termMu.Lock()
	_, live := h.termPush[sessionID]
	h.termMu.Unlock()
	if !live {
		return false
	}
	return h.Send(nodeID, protocol.NewEnvelope(protocol.TypeTermInput, sessionID, protocol.TerminalInput{
		SessionID: sessionID,
		Data:      data,
	}))
}

// terminalQueryID is a per-request correlation id. Not security material —
// only uniqueness within the process matters — but a random one avoids any
// chance of a stale answer matching a fresh request.
func terminalQueryID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "tq-" + hex.EncodeToString(buf), nil
}

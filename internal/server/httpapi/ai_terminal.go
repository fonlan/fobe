package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/fonlan/fobe/internal/server/store"
)

// The terminal tool chain (design §12.7).
//
// The AI no longer runs commands *for* the operator; it operates the terminal
// the operator is watching. Two consequences shape everything here:
//
//   - **The server has no screen.** The agent pumps raw PTY bytes and the
//     server forwards them without storing or parsing them, so every
//     observation is a round trip to the browser that holds the emulator
//     (§12.7.1). read_terminal is that round trip; run_shell will use it too.
//   - **Keystrokes can vanish silently.** The agent ignores input whose session
//     id is not its active PTY, and the hub drops terminal frames nobody is
//     subscribed to. So every path here has to turn "nobody is listening" into
//     a visible tool error instead of a cheerful "done".

const (
	// aiTerminalMaxOffset caps how far up the scrollback a read may reach.
	// Beyond the browser's `scrollback` (10000 lines) there is simply nothing.
	aiTerminalMaxOffset = 5000
	// aiTerminalMaxLines caps one requested window.
	aiTerminalMaxLines = 500
	// aiTerminalAuditBytes caps how much of a read lands in audit_logs. The
	// point of auditing reads is attribution ("what did it see"), not keeping a
	// second copy of the screen in SQLite.
	aiTerminalAuditBytes = 512
)

// aiTerminalSessionKey carries the live terminal binding from the turn request
// down to the tool handlers, which keep their §12.2 signature.
type aiTerminalSessionKey struct{}

func withAITerminalSession(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, aiTerminalSessionKey{}, sessionID)
}

// aiTerminalSessionID resolves the terminal session the tools must act on.
//
// The REQUEST wins over the session row (§12.7.5). The id is minted per browser
// WS connection, so the row's copy is a dangling pointer after a page reload;
// acting on it would type into a session the agent no longer knows, and the
// agent drops that input **silently**.
func aiTerminalSessionID(r *http.Request, session *store.AISession) string {
	if id, ok := r.Context().Value(aiTerminalSessionKey{}).(string); ok && id != "" {
		return id
	}
	return session.TerminalSessionID
}

// aiTerminalRebindNote tells the model its screen is gone.
//
// Without it the model keeps reasoning from the transcript's picture of a
// terminal that no longer exists — the worst kind of wrong answer, because it
// looks consistent. Returns "" when the binding is unchanged (the common case).
func aiTerminalRebindNote(session *store.AISession, live string) string {
	if live == "" || live == session.TerminalSessionID {
		return ""
	}
	return "Note: the operator's terminal session changed since this conversation began " +
		"(the page was reloaded, or the terminal reconnected). The previous screen is gone and " +
		"anything running in it was killed. read_terminal now shows the new session — do not rely " +
		"on anything you observed on the old one."
}

// aiRenderKeys renders key bytes for the audit row and the operator-facing
// result.
//
// Control bytes become \xNN (or the familiar escapes): an audit column holding a
// raw ESC/NUL is unreadable *and* a log-injection vector, while the operator
// reviewing "what did it press" needs to see the difference between Ctrl+C and
// the letter c. Rendering is for humans only — the bytes actually sent are the
// original data.
func aiRenderKeys(data string) string {
	var b strings.Builder
	for _, r := range data {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0x1b:
			b.WriteString(`\e`)
		default:
			switch {
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\x%02x`, r)
			case !unicode.IsPrint(r):
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// aiSendKeysPayload is the confirmation payload for send_keys. It carries the
// terminal session id because a confirmed action is executed by a LATER request
// (the continue endpoint) — by then the turn's live binding is gone.
type aiSendKeysPayload struct {
	Data              string `json:"data"`
	TerminalSessionID string `json:"terminal_session_id"`
}

func (s *Server) handleAISendKeys(r *http.Request, session *store.AISession, action *aiToolAction, toolCall map[string]any) map[string]any {
	var args struct {
		Data   string `json:"data"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(action.Args, &args); err != nil {
		toolCall["status"] = "invalid"
		return toolCall
	}
	if args.Data == "" || len(args.Data) > aiTerminalMaxKeys {
		toolCall["status"] = "invalid"
		return toolCall
	}
	terminalSessionID := aiTerminalSessionID(r, session)
	if terminalSessionID == "" {
		return aiTerminalUnavailable(toolCall, "no terminal session is bound to this conversation")
	}

	if s.aiChangeRequiresConfirmation() {
		pendingID, ok := s.requestAIConfirmation(r, session, aiToolSendKeys,
			aiSendKeysPayload{Data: args.Data, TerminalSessionID: terminalSessionID},
			args.Reason, "policy", stringField(toolCall, "id"))
		if !ok {
			toolCall["status"] = "internal"
			return toolCall
		}
		toolCall["status"] = "needs_confirmation"
		toolCall["action_id"] = pendingID
		toolCall["reason"] = args.Reason
		toolCall["risk"] = "policy"
		return toolCall
	}
	return s.deliverAIKeys(r, session, terminalSessionID, args.Data, args.Reason, "normal", toolCall)
}

// deliverAIKeys is the step that actually writes bytes, shared by the direct
// path and the confirmation resume (§12.3 `ai.default_policy=confirm`).
func (s *Server) deliverAIKeys(r *http.Request, session *store.AISession, terminalSessionID, data, reason, risk string, toolCall map[string]any) map[string]any {
	// Same brakes as run_shell (§12.3): the failure pause plus the per-minute
	// count of terminal actions. Both tools bypass the commands queue, so
	// without this call they would be the only AI actions with no rate limit.
	if allowed, gateReason := s.aiTerminalGate(session.NodeID); !allowed {
		toolCall["status"] = "blocked"
		toolCall["reason"] = gateReason
		toolCall["result"] = "refused: " + gateReason
		return toolCall
	}
	rendered := aiRenderKeys(data)
	if !s.Hub.SendTerminalKeys(session.NodeID, terminalSessionID, data) {
		return aiTerminalUnavailable(toolCall, "the terminal is not attached to a browser right now; nothing was sent")
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "ai", NodeID: session.NodeID, Action: store.AuditAITerminalKeys, Command: rendered,
		Reason: reason, Risk: risk, SourceIP: s.Trust.RealIP(r), AISessionID: session.ID,
	})
	toolCall["status"] = "ok"
	toolCall["result"] = "sent keys: " + rendered
	return toolCall
}

func (s *Server) handleAIReadTerminal(r *http.Request, session *store.AISession, action *aiToolAction, toolCall map[string]any) map[string]any {
	var args struct {
		Offset int    `json:"offset"`
		Lines  int    `json:"lines"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(action.Args, &args); err != nil {
		toolCall["status"] = "invalid"
		return toolCall
	}
	if args.Offset < 0 || args.Lines < 0 || args.Offset > aiTerminalMaxOffset || args.Lines > aiTerminalMaxLines {
		toolCall["status"] = "invalid"
		return toolCall
	}
	terminalSessionID := aiTerminalSessionID(r, session)
	if terminalSessionID == "" {
		return aiTerminalUnavailable(toolCall, "no terminal session is bound to this conversation")
	}

	buffer, err := s.Hub.AskTerminalBuffer(r.Context(), terminalSessionID, args.Offset, args.Lines, aiTerminalQueryTimeout)
	if err != nil {
		// A read that failed is still information for the model: "the page is
		// closed" and "the screen is empty" must not look alike.
		return aiTerminalUnavailable(toolCall, "could not read the terminal: "+err.Error())
	}
	// The window arrives as plain text lines; joining with \n loses nothing the
	// model needs and keeps the context cheap (no ANSI, no cursor columns).
	screen := strings.Join(buffer.Lines, "\n")
	if strings.TrimSpace(screen) == "" {
		screen = "(the terminal window is empty)"
	}
	screen = truncateAIBytes(screen, aiTerminalMaxScreenBytes)

	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "ai", NodeID: session.NodeID, Action: "ai_terminal_read",
		Command: truncateAIBytes(screen, aiTerminalAuditBytes),
		Reason:  args.Reason, Risk: "normal", SourceIP: s.Trust.RealIP(r), AISessionID: session.ID,
	})
	toolCall["status"] = "ok"
	toolCall["result"] = screen
	return toolCall
}

// aiTerminalUnavailable is the shared "the keys went nowhere / the screen could
// not be read" result. It is an error on purpose: silently reporting success
// would leave the model believing it had acted on a terminal it cannot reach.
func aiTerminalUnavailable(toolCall map[string]any, message string) map[string]any {
	toolCall["status"] = "failed"
	toolCall["reason"] = "terminal_unavailable"
	toolCall["result"] = message
	return toolCall
}

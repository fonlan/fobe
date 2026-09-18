package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// AISession is a persisted conversation associated with one node.
//
// ProviderID/ModelID/Protocol pin the conversation to one model (§12.1): a
// reasoning block is protocol-native (Anthropic thinking + signature,
// Responses reasoning items) and cannot be replayed across protocols, so the
// model picker starts a NEW session instead of mutating this one.
type AISession struct {
	ID                string `json:"id"`
	NodeID            string `json:"node_id"`
	TerminalSessionID string `json:"terminal_session_id,omitempty"`
	ProviderID        string `json:"provider_id,omitempty"`
	ModelID           string `json:"model_id,omitempty"`
	Protocol          string `json:"protocol,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	LastSeen          int64  `json:"last_seen"`
}

// AIMessage is one persisted conversation message. Content is the
// human-readable text; Blocks carries the protocol-native payload (JSON) that
// a tool loop must echo back verbatim (§12.5).
type AIMessage struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Blocks    string `json:"blocks,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// AIPendingAction is a model-requested action waiting for confirmation.
type AIPendingAction struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
	Kind      string `json:"kind"`
	Payload   string `json:"payload"`
	Reason    string `json:"reason"`
	Risk      string `json:"risk"`
	Status    string `json:"status"`
	// CallID is the model's tool_call id (empty for panel-created actions).
	CallID      string `json:"call_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	ConfirmedAt *int64 `json:"confirmed_at,omitempty"`
	CommandID   string `json:"command_id,omitempty"`
}

// CreateAISession creates a conversation that is not pinned to a model. Kept
// for callers that predate the multi-provider picker (§12.1); new conversations
// go through CreateAISessionPinned.
func (s *Store) CreateAISession(id, nodeID, terminalSessionID string) error {
	return s.CreateAISessionPinned(id, nodeID, terminalSessionID, "", "", "")
}

// CreateAISessionPinned creates a conversation bound to one (provider, model)
// pair and its wire protocol.
func (s *Store) CreateAISessionPinned(id, nodeID, terminalSessionID, providerID, modelID, protocol string) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_sessions
		   (id, node_id, terminal_session_id, provider_id, model_id, protocol, created_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, nodeID, terminalSessionID, providerID, modelID, protocol, now(), now(),
	)
	if err != nil {
		return fmt.Errorf("create ai session: %w", err)
	}
	return nil
}

func (s *Store) GetAISession(id string) (*AISession, error) {
	sess := &AISession{}
	err := s.db.QueryRow(
		`SELECT id, node_id, terminal_session_id, provider_id, model_id, protocol, created_at, last_seen
		 FROM ai_sessions WHERE id = ?`, id,
	).Scan(&sess.ID, &sess.NodeID, &sess.TerminalSessionID, &sess.ProviderID,
		&sess.ModelID, &sess.Protocol, &sess.CreatedAt, &sess.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ai session: %w", err)
	}
	return sess, nil
}

func (s *Store) TouchAISession(id string) error {
	_, err := s.db.Exec(`UPDATE ai_sessions SET last_seen = ? WHERE id = ?`, now(), id)
	return err
}

// Audit actions written by the §12.7 terminal tool chain. They are shared
// constants rather than literals because the rate counter reads them back by
// name — a typo in either place would silently stop counting.
const (
	AuditAITerminalRun  = "ai_terminal_run"
	AuditAITerminalKeys = "ai_terminal_keys"
)

// CountAITerminalActions counts AI terminal actions on a node since `since`.
//
// The per-minute cap (§12.3/§12.6) is derived from `commands` rows, but the
// terminal tools deliberately do NOT enqueue commands — they type into the
// operator's PTY (design §12.7). Without this counter the rate axis would
// silently stop applying to exactly the tools that can now act fastest, so it
// counts the record they DO write (an audit row per action).
func (s *Store) CountAITerminalActions(nodeID string, since int64) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM audit_logs
		 WHERE node_id = ? AND actor = 'ai' AND action IN (?, ?) AND ts >= ?`,
		nodeID, AuditAITerminalRun, AuditAITerminalKeys, since,
	).Scan(&n)
	return n, err
}

// SetAISessionTerminal repoints a conversation at the terminal session it is
// currently bound to (design §12.7.5).
//
// The terminal session id is minted per browser WS connection, so a page reload
// makes the stored value a dangling pointer. Rebinding keeps the "your terminal
// was replaced" note (§12.7.5) a one-time event instead of a line the model
// reads on every subsequent turn.
func (s *Store) SetAISessionTerminal(id, terminalSessionID string) error {
	_, err := s.db.Exec(`UPDATE ai_sessions SET terminal_session_id = ? WHERE id = ?`, terminalSessionID, id)
	return err
}

func (s *Store) InsertAIMessage(sessionID, role, content string) error {
	return s.InsertAIMessageBlocks(sessionID, role, content, "")
}

// InsertAIMessageBlocks persists a message together with the protocol-native
// payload the next loop step has to send back (§12.5).
func (s *Store) InsertAIMessageBlocks(sessionID, role, content, blocks string) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_messages (session_id, role, content, blocks, created_at) VALUES (?, ?, ?, ?, ?)`,
		sessionID, role, content, blocks, now(),
	)
	if err != nil {
		return fmt.Errorf("insert ai message: %w", err)
	}
	return nil
}

func (s *Store) ListAIMessages(sessionID string, limit int) ([]AIMessage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, session_id, role, content, blocks, created_at FROM ai_messages
		 WHERE session_id = ? ORDER BY id DESC LIMIT ?`, sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list ai messages: %w", err)
	}
	defer rows.Close()

	out := make([]AIMessage, 0, limit)
	for rows.Next() {
		var message AIMessage
		if err := rows.Scan(&message.ID, &message.SessionID, &message.Role, &message.Content, &message.Blocks, &message.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out, nil
}

func (s *Store) CreateAIPendingAction(action *AIPendingAction) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_pending_actions
		 (id, session_id, node_id, kind, payload, reason, risk, status, call_id, created_at, command_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, '')`,
		action.ID, action.SessionID, action.NodeID, action.Kind, action.Payload,
		action.Reason, action.Risk, action.CallID, now(),
	)
	if err != nil {
		return fmt.Errorf("create ai pending action: %w", err)
	}
	return nil
}

func (s *Store) GetAIPendingAction(id string) (*AIPendingAction, error) {
	action := &AIPendingAction{}
	err := s.db.QueryRow(
		`SELECT id, session_id, node_id, kind, payload, reason, risk, status,
		        call_id, created_at, confirmed_at, command_id
		 FROM ai_pending_actions WHERE id = ?`, id,
	).Scan(&action.ID, &action.SessionID, &action.NodeID, &action.Kind, &action.Payload,
		&action.Reason, &action.Risk, &action.Status, &action.CallID,
		&action.CreatedAt, &action.ConfirmedAt, &action.CommandID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ai pending action: %w", err)
	}
	return action, nil
}

func (s *Store) ConfirmAIPendingAction(id, commandID string, confirmedAt int64) error {
	_, err := s.db.Exec(
		`UPDATE ai_pending_actions SET status = 'confirmed', confirmed_at = ?, command_id = ?
		 WHERE id = ? AND status = 'pending'`,
		confirmedAt, commandID, id,
	)
	return err
}

func (s *Store) RejectAIPendingAction(id string, rejectedAt int64) error {
	_, err := s.db.Exec(
		`UPDATE ai_pending_actions SET status = 'rejected', confirmed_at = ?
		 WHERE id = ? AND status = 'pending'`, rejectedAt, id,
	)
	return err
}

// CheckAICommandGate enforces the minimum per-node AI command controls. It
// also records a persisted pause after three consecutive completed failures.
func (s *Store) CheckAICommandGate(nodeID string, limit int, at int64) (bool, string, error) {
	if limit <= 0 {
		limit = 10
	}
	var failures, pausedUntil int64
	err := s.db.QueryRow(
		`SELECT consecutive_failures, paused_until FROM ai_node_state WHERE node_id = ?`, nodeID,
	).Scan(&failures, &pausedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		failures, pausedUntil = 0, 0
	} else if err != nil {
		return false, "", fmt.Errorf("read ai node state: %w", err)
	}
	if int64(pausedUntil) > at {
		return false, "failure_paused", nil
	}
	if pausedUntil != 0 && pausedUntil <= at {
		failures, pausedUntil = 0, 0
	}

	// Only failures from a bounded window count. Without the window this
	// recomputation re-armed the pause on EVERY call from rows that never age
	// out, so three failures froze a node's AI execution permanently — nothing
	// could clear it, because a success cannot happen while the gate blocks
	// execution. The window (10 min) is deliberately longer than the pause
	// itself (5 min) so a genuine failure cluster still pauses, while the state
	// always terminates. A successful AI command inside the window clears it
	// immediately.
	const failureWindowSeconds = 600
	rows, err := s.db.Query(
		`SELECT status FROM commands WHERE node_id = ? AND actor = 'ai'
		 AND status IN ('ok', 'failed') AND COALESCE(finished_at, created_at) >= ?
		 ORDER BY COALESCE(finished_at, created_at) DESC, id DESC LIMIT 3`, nodeID, at-failureWindowSeconds,
	)
	if err != nil {
		return false, "", fmt.Errorf("read ai command results: %w", err)
	}
	defer rows.Close()
	consecutive := 0
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return false, "", err
		}
		if status != "failed" {
			break
		}
		consecutive++
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	if consecutive >= 3 {
		failures = int64(consecutive)
		pausedUntil = at + 300
	} else {
		failures = int64(consecutive)
	}
	if err := rows.Close(); err != nil {
		return false, "", err
	}
	if err := s.upsertAINodeState(nodeID, failures, pausedUntil, at); err != nil {
		return false, "", err
	}
	if pausedUntil > at {
		return false, "failure_paused", nil
	}

	var recent int
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM commands WHERE node_id = ? AND actor = 'ai' AND created_at >= ?`,
		nodeID, at-60,
	).Scan(&recent)
	if err != nil {
		return false, "", fmt.Errorf("count ai commands: %w", err)
	}
	if recent >= limit {
		return false, "rate_limited", nil
	}
	return true, "", nil
}

func (s *Store) upsertAINodeState(nodeID string, failures, pausedUntil, at int64) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_node_state (node_id, consecutive_failures, paused_until, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET consecutive_failures = excluded.consecutive_failures,
		 paused_until = excluded.paused_until, updated_at = excluded.updated_at`,
		nodeID, failures, pausedUntil, at,
	)
	return err
}

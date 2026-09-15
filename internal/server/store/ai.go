package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// AISession is a persisted conversation associated with one node.
type AISession struct {
	ID                string `json:"id"`
	NodeID            string `json:"node_id"`
	TerminalSessionID string `json:"terminal_session_id,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	LastSeen          int64  `json:"last_seen"`
}

// AIMessage is one persisted OpenAI-style conversation message.
type AIMessage struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

// AIPendingAction is a model-requested action waiting for confirmation.
type AIPendingAction struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	NodeID      string `json:"node_id"`
	Kind        string `json:"kind"`
	Payload     string `json:"payload"`
	Reason      string `json:"reason"`
	Risk        string `json:"risk"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
	ConfirmedAt *int64 `json:"confirmed_at,omitempty"`
	CommandID   string `json:"command_id,omitempty"`
}

// CreateAISession creates a conversation. The node foreign key ensures that
// sessions cannot outlive their selected node.
func (s *Store) CreateAISession(id, nodeID, terminalSessionID string) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_sessions (id, node_id, terminal_session_id, created_at, last_seen)
		 VALUES (?, ?, ?, ?, ?)`,
		id, nodeID, terminalSessionID, now(), now(),
	)
	if err != nil {
		return fmt.Errorf("create ai session: %w", err)
	}
	return nil
}

func (s *Store) GetAISession(id string) (*AISession, error) {
	sess := &AISession{}
	err := s.db.QueryRow(
		`SELECT id, node_id, terminal_session_id, created_at, last_seen
		 FROM ai_sessions WHERE id = ?`, id,
	).Scan(&sess.ID, &sess.NodeID, &sess.TerminalSessionID, &sess.CreatedAt, &sess.LastSeen)
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

func (s *Store) InsertAIMessage(sessionID, role, content string) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_messages (session_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		sessionID, role, content, now(),
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
		`SELECT id, session_id, role, content, created_at FROM ai_messages
		 WHERE session_id = ? ORDER BY id DESC LIMIT ?`, sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list ai messages: %w", err)
	}
	defer rows.Close()

	out := make([]AIMessage, 0, limit)
	for rows.Next() {
		var message AIMessage
		if err := rows.Scan(&message.ID, &message.SessionID, &message.Role, &message.Content, &message.CreatedAt); err != nil {
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
		 (id, session_id, node_id, kind, payload, reason, risk, status, created_at, command_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, '')`,
		action.ID, action.SessionID, action.NodeID, action.Kind, action.Payload,
		action.Reason, action.Risk, now(),
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
		        created_at, confirmed_at, command_id
		 FROM ai_pending_actions WHERE id = ?`, id,
	).Scan(&action.ID, &action.SessionID, &action.NodeID, &action.Kind, &action.Payload,
		&action.Reason, &action.Risk, &action.Status, &action.CreatedAt, &action.ConfirmedAt, &action.CommandID)
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

	rows, err := s.db.Query(
		`SELECT status FROM commands WHERE node_id = ? AND actor = 'ai'
		 AND status IN ('ok', 'failed') ORDER BY COALESCE(finished_at, created_at) DESC, id DESC LIMIT 3`, nodeID,
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

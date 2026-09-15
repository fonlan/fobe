package store

import (
	"database/sql"
	"errors"
)

// GetCommand returns one command row by ID. The AI tail_logs flow (§12.1)
// uses it to poll for the agent's cmd_result without claiming the row.
func (s *Store) GetCommand(id string) (*Command, error) {
	c := &Command{}
	err := s.db.QueryRow(
		`SELECT id, node_id, kind, payload, status, created_at, sent_at, finished_at, result,
		        ttl_seconds, actor, ai_session_id, reason, risk
		 FROM commands WHERE id = ?`, id,
	).Scan(&c.ID, &c.NodeID, &c.Kind, &c.Payload, &c.Status, &c.CreatedAt, &c.SentAt,
		&c.FinishedAt, &c.Result, &c.TTLSeconds, &c.Actor, &c.AISessionID, &c.Reason, &c.Risk)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

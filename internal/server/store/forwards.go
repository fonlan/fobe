package store

import (
	"database/sql"
	"errors"
)

// nftables port-forward snapshot (design.md §21).
//
// The probe's ruleset is the source of truth; this table only remembers the
// last report so the panel can render the list without a round trip and show
// what an external tool (nfpf.sh) did while nobody was looking.

// NodeForward is one reported DNAT rule.
type NodeForward struct {
	Handle     int
	Proto      string
	SrcPort    int
	Iface      string
	DstIP      string
	DstPort    int
	Comment    string
	ExtraMatch bool
}

// NodeForwardStatus is the reported capability half. ReportedAt==0 with a zero
// value everywhere else means "never reported" (an agent older than §21).
type NodeForwardStatus struct {
	Supported   bool
	Initialized bool
	Code        string
	Message     string
	ReportedAt  int64
}

// ReplaceNodeForwards swaps the whole snapshot of one node in a single
// transaction: a partial view would be worse than none, because a rule that
// vanished from the report would look deleted while still forwarding traffic.
func (s *Store) ReplaceNodeForwards(nodeID string, status NodeForwardStatus, rows []NodeForward) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO node_forward_status (node_id, supported, initialized, code, message, reported_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   supported = excluded.supported, initialized = excluded.initialized,
		   code = excluded.code, message = excluded.message, reported_at = excluded.reported_at`,
		nodeID, boolInt(status.Supported), boolInt(status.Initialized),
		status.Code, clip(status.Message, 512), status.ReportedAt,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM node_forwards WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(
			`INSERT INTO node_forwards
			   (node_id, handle, proto, src_port, iface, dst_ip, dst_port, comment, extra_match)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, r.Handle, r.Proto, r.SrcPort, r.Iface, r.DstIP, r.DstPort,
			r.Comment, boolInt(r.ExtraMatch),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListNodeForwards returns the last reported forwards, in rule order.
func (s *Store) ListNodeForwards(nodeID string) ([]NodeForward, error) {
	rows, err := s.db.Query(
		`SELECT handle, proto, src_port, iface, dst_ip, dst_port, comment, extra_match
		 FROM node_forwards WHERE node_id = ? ORDER BY id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeForward{}
	for rows.Next() {
		var r NodeForward
		var extra int
		if err := rows.Scan(&r.Handle, &r.Proto, &r.SrcPort, &r.Iface, &r.DstIP,
			&r.DstPort, &r.Comment, &extra); err != nil {
			return nil, err
		}
		r.ExtraMatch = extra != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetNodeForwardStatus returns the last capability report. A node that never
// reported one yields the zero value (ReportedAt 0), not ErrNotFound: "never
// reported" is a state the panel renders, not an error.
func (s *Store) GetNodeForwardStatus(nodeID string) (NodeForwardStatus, error) {
	var st NodeForwardStatus
	var supported, initialized int
	err := s.db.QueryRow(
		`SELECT supported, initialized, code, message, reported_at
		 FROM node_forward_status WHERE node_id = ?`, nodeID,
	).Scan(&supported, &initialized, &st.Code, &st.Message, &st.ReportedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeForwardStatus{}, nil
		}
		return NodeForwardStatus{}, err
	}
	st.Supported = supported != 0
	st.Initialized = initialized != 0
	return st, nil
}

// boolInt stores a Go bool in SQLite's 0/1 column shape.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// clip caps a free-form probe message before it is persisted: nft can print a
// lot, and the field is only ever rendered as a hint.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

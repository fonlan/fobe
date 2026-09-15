package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// NodeSSH is the per-node SSH credential row for the SSH-mode web terminal
// (design §11). PasswordEnc/PrivKeyEnc carry AES-GCM ciphertext produced by
// httpapi (security.Cryptor); this layer treats them as opaque strings and
// never sees plaintext.
type NodeSSH struct {
	NodeID      string
	User        string
	Port        int
	PasswordEnc string
	PrivKeyEnc  string
	UpdatedAt   int64
}

// GetNodeSSH returns ErrNotFound when the node has no stored credential row.
func (s *Store) GetNodeSSH(nodeID string) (*NodeSSH, error) {
	var n NodeSSH
	err := s.db.QueryRow(
		`SELECT node_id, ssh_user, ssh_port, password_enc, privkey_enc, updated_at
		 FROM node_ssh WHERE node_id = ?`, nodeID,
	).Scan(&n.NodeID, &n.User, &n.Port, &n.PasswordEnc, &n.PrivKeyEnc, &n.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get node ssh: %w", err)
	}
	return &n, nil
}

// UpsertNodeSSH replaces the credential row wholesale. Unchanged secrets are
// handled by the caller re-submitting the ciphertext it read earlier — the
// round-trips never expose plaintext (GET reports set/unset only).
func (s *Store) UpsertNodeSSH(n *NodeSSH) error {
	if n.UpdatedAt == 0 {
		n.UpdatedAt = now()
	}
	_, err := s.db.Exec(
		`INSERT INTO node_ssh (node_id, ssh_user, ssh_port, password_enc, privkey_enc, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   ssh_user     = excluded.ssh_user,
		   ssh_port     = excluded.ssh_port,
		   password_enc = excluded.password_enc,
		   privkey_enc  = excluded.privkey_enc,
		   updated_at   = excluded.updated_at`,
		n.NodeID, n.User, n.Port, n.PasswordEnc, n.PrivKeyEnc, n.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert node ssh: %w", err)
	}
	return nil
}

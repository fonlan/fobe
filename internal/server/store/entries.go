// Package store: §10.2 subscription entries. A subscription no longer binds
// "nodes": it binds *entries* — a target node plus the ingress it is reached
// through (” = the node's own inbound, otherwise a relay node's nftables
// DNAT). One node can therefore appear twice in the same subscription, which is
// exactly why the legacy (subscription_id, node_id) primary key could not be
// reused.
package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// SubscriptionEntry is one bound ingress of one subscription. The tuple
// (NodeID, RelayNodeID, Proto, SrcPort, Iface) is the identity; Proto/SrcPort/
// Iface mirror the §21 rule identity so "the same entry" survives a ruleset
// reload (handles are renumbered, tuples are not).
type SubscriptionEntry struct {
	SubscriptionID string
	NodeID         string
	RelayNodeID    string
	Proto          string
	SrcPort        int
	Iface          string
	// Alias is the operator's name for this entry in this subscription; empty
	// means "derive it" (nodes.sub_name → nodes.name → id for direct entries,
	// sub.relay_name_format for relayed ones).
	Alias string
	// Enabled=false is a tombstone, not a deletion: the operator unchecked the
	// entry, and the §10.2 reconciler must not re-add it.
	Enabled bool
}

// SubscriptionEntries returns every bound entry of one subscription, ordered
// the way the renderer emits them: by target node, and within a node the direct
// ingress first (” sorts before any relay id), then relays by protocol/port.
// Deterministic ordering matters because it is the order clients see in their
// node list, and a re-render must not shuffle it.
func (s *Store) SubscriptionEntries(subID string) ([]SubscriptionEntry, error) {
	rows, err := s.db.Query(`SELECT subscription_id, node_id, relay_node_id, proto, src_port, iface, alias, enabled
		FROM subscription_entries WHERE subscription_id = ?
		ORDER BY node_id, relay_node_id, proto, src_port, iface`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubscriptionEntry{}
	for rows.Next() {
		var e SubscriptionEntry
		if err := rows.Scan(&e.SubscriptionID, &e.NodeID, &e.RelayNodeID, &e.Proto, &e.SrcPort,
			&e.Iface, &e.Alias, &e.Enabled); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// entryKey renders the identity as a single string for map keys and error
// messages. '|' is not valid in any of the components (proto/iface come from
// the nftables parser, ports are numeric), so the rendering is unambiguous.
func entryKey(e SubscriptionEntry) string {
	return fmt.Sprintf("%s|%s|%s|%d|%s", e.NodeID, e.RelayNodeID, e.Proto, e.SrcPort, e.Iface)
}

// SetSubscriptionEntries replaces the full entry set of one subscription.
//
// Semantics the panel relies on (design §10.2):
//   - a listed entry with Enabled=true is upserted (alias included);
//   - a listed entry with Enabled=false writes a *tombstone* only if a row
//     already exists — an untouched candidate must stay absent, otherwise the
//     first save would silently freeze auto-enrolment forever;
//   - rows not mentioned at all are left alone (the picker always sends every
//     entry it displayed, including bound-but-unavailable ones).
//
// The direct half is mirrored into subscription_nodes in the same transaction
// so an older binary (which only knows that table) still renders what it
// understands.
func (s *Store) SetSubscriptionEntries(subID string, entries []SubscriptionEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	existing := map[string]bool{}
	rows, err := tx.Query(`SELECT node_id, relay_node_id, proto, src_port, iface FROM subscription_entries
		WHERE subscription_id = ?`, subID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var e SubscriptionEntry
		if err := rows.Scan(&e.NodeID, &e.RelayNodeID, &e.Proto, &e.SrcPort, &e.Iface); err != nil {
			rows.Close()
			return err
		}
		existing[entryKey(e)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, e := range entries {
		e.SubscriptionID = subID
		if !e.Enabled && !existing[entryKey(e)] {
			continue // candidate nobody ever bound: leave it absent
		}
		if _, err := tx.Exec(`INSERT INTO subscription_entries
				(subscription_id, node_id, relay_node_id, proto, src_port, iface, alias, enabled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (subscription_id, node_id, relay_node_id, proto, src_port, iface)
			DO UPDATE SET alias = excluded.alias, enabled = excluded.enabled`,
			subID, e.NodeID, e.RelayNodeID, e.Proto, e.SrcPort, e.Iface, e.Alias, e.Enabled); err != nil {
			return err
		}
	}
	if err := mirrorDirectNodes(tx, subID); err != nil {
		return err
	}
	return tx.Commit()
}

// mirrorDirectNodes rewrites the legacy projection (design §10.2): enabled
// direct entries only. Relay rows have no representation there — an older
// binary simply renders fewer nodes, which is the documented downgrade cost.
func mirrorDirectNodes(tx *sql.Tx, subID string) error {
	if _, err := tx.Exec(`DELETE FROM subscription_nodes WHERE subscription_id = ?`, subID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO subscription_nodes (subscription_id, node_id)
		SELECT subscription_id, node_id FROM subscription_entries
		WHERE subscription_id = ? AND relay_node_id = '' AND enabled = 1`, subID)
	return err
}

// SetSubscriptionDirectNodes is the legacy binding path (design §10.2: the
// `node_ids` API and §17 snapshots written before entries existed). It owns the
// direct half exactly as before — listed nodes are bound, previously bound
// direct nodes that are not listed are removed — and leaves relay rows alone,
// because those are governed by the reconciler, not by "which nodes did the
// operator pick".
func (s *Store) SetSubscriptionDirectNodes(subID string, nodeIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	aliases := map[string]string{}
	rows, err := tx.Query(`SELECT node_id, alias FROM subscription_entries
		WHERE subscription_id = ? AND relay_node_id = ''`, subID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, alias string
		if err := rows.Scan(&id, &alias); err != nil {
			rows.Close()
			return err
		}
		aliases[id] = alias
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM subscription_entries
		WHERE subscription_id = ? AND relay_node_id = ''`, subID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range nodeIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.Exec(`INSERT OR IGNORE INTO subscription_entries
				(subscription_id, node_id, relay_node_id, proto, src_port, iface, alias, enabled)
			VALUES (?, ?, '', '', 0, '', ?, 1)`, subID, id, aliases[id]); err != nil {
			return err
		}
	}
	if err := mirrorDirectNodes(tx, subID); err != nil {
		return err
	}
	return tx.Commit()
}

// RestoreSubscriptionEntries writes every row verbatim, tombstones included.
// It exists for §17 import, where "absent" is not an option: a snapshot that
// says "this entry is off" must arrive off, otherwise the §10.2 reconciler
// would switch it back on the first time it looked.
func (s *Store) RestoreSubscriptionEntries(subID string, entries []SubscriptionEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO subscription_entries
				(subscription_id, node_id, relay_node_id, proto, src_port, iface, alias, enabled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (subscription_id, node_id, relay_node_id, proto, src_port, iface)
			DO UPDATE SET alias = excluded.alias, enabled = excluded.enabled`,
			subID, e.NodeID, e.RelayNodeID, e.Proto, e.SrcPort, e.Iface, e.Alias, e.Enabled); err != nil {
			return err
		}
	}
	if err := mirrorDirectNodes(tx, subID); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertSubscriptionEntryIfAbsent auto-enrols one §10.2 relay candidate. It
// inserts only when no row exists in *either* state: an explicit tombstone
// (Enabled=false) is the operator's decision and must win over automation.
// Returns true when a row was actually created.
func (s *Store) InsertSubscriptionEntryIfAbsent(e SubscriptionEntry) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO subscription_entries
			(subscription_id, node_id, relay_node_id, proto, src_port, iface, alias, enabled)
		VALUES (?, ?, ?, ?, ?, ?, '', 1)
		ON CONFLICT (subscription_id, node_id, relay_node_id, proto, src_port, iface) DO NOTHING`,
		e.SubscriptionID, e.NodeID, e.RelayNodeID, e.Proto, e.SrcPort, e.Iface)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ForwardRef is one reported DNAT rule plus the node that reported it — what
// §10.2 relay detection needs, since the rule lives on the *relay* while the
// entry belongs to the target.
type ForwardRef struct {
	NodeID  string
	Forward NodeForward
}

// ListForwardsToDstPort returns every reported tcp forward whose destination
// port is the given one — the §10.2 relay candidates for a node's inbound. The
// caller still has to match dst_ip against that node's addresses: the database
// cannot know which addresses belong to the same probe.
func (s *Store) ListForwardsToDstPort(dstPort int) ([]ForwardRef, error) {
	rows, err := s.db.Query(`SELECT node_id, handle, proto, src_port, iface, dst_ip, dst_port, comment, extra_match
		FROM node_forwards WHERE proto = 'tcp' AND dst_port = ? ORDER BY node_id, src_port, iface`, dstPort)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForwardRef{}
	for rows.Next() {
		var f ForwardRef
		if err := rows.Scan(&f.NodeID, &f.Forward.Handle, &f.Forward.Proto, &f.Forward.SrcPort,
			&f.Forward.Iface, &f.Forward.DstIP, &f.Forward.DstPort, &f.Forward.Comment, &f.Forward.ExtraMatch); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ValidAlias bounds an entry alias (design §10.2). The renderers escape what
// they emit, so this is about sanity, not injection: a control character would
// make the name unreadable in a client, and an unbounded name bloats every
// config the subscription ever hands out. Empty means "derive the name".
func ValidAlias(s string) bool {
	if s == "" {
		return true
	}
	if len([]rune(s)) > 64 {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

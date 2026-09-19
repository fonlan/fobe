// Quick commands: operator-maintained command snippets for the web terminal's
// side panel (2026-09-19). Pure panel data — the server never executes them;
// the browser injects them as keyboard input into its own PTY.
package store

import "fmt"

// MaxQuickCommands caps the list. The panel is single-user and the side tab
// must stay scannable; the API refuses one more row past this.
const MaxQuickCommands = 50

type QuickCommand struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Command   string `json:"command"`
	SortOrder int64  `json:"sort_order"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// ListQuickCommands returns the commands in display order: sort_order first
// (set by ReorderQuickCommands), id second so a stale client that omitted rows
// still gets a stable order instead of shuffling between renders.
func (s *Store) ListQuickCommands() ([]QuickCommand, error) {
	rows, err := s.db.Query(`SELECT id, name, command, sort_order, created_at, updated_at FROM quick_commands ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuickCommand{}
	for rows.Next() {
		var c QuickCommand
		if err := rows.Scan(&c.ID, &c.Name, &c.Command, &c.SortOrder, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateQuickCommand appends at the end of the list (max(sort_order)+1). New
// entries land last — reordering is the operator's explicit act, not a surprise.
func (s *Store) CreateQuickCommand(name, command string) (int64, error) {
	var next int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) + 1 FROM quick_commands`).Scan(&next); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO quick_commands (name, command, sort_order, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		name, command, next, now(), now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateQuickCommand(id int64, name, command string) error {
	res, err := s.db.Exec(
		`UPDATE quick_commands SET name = ?, command = ?, updated_at = ? WHERE id = ?`,
		name, command, now(), id,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteQuickCommand(id int64) error {
	res, err := s.db.Exec(`DELETE FROM quick_commands WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderQuickCommands assigns sort_order = position for the given ids in one
// transaction. Ids not in the table update zero rows; rows left out of the list
// keep their old sort_order, which can only ever shuffle display order (the
// secondary id sort stays) — never lose data.
func (s *Store) ReorderQuickCommands(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE quick_commands SET sort_order = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, id := range ids {
		if _, err := stmt.Exec(int64(i), id); err != nil {
			return fmt.Errorf("reorder quick command %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// CountQuickCommands backs the POST-side cap (checked before insert; the cap is
// about the list staying manageable, so it counts rows rather than racing).
func (s *Store) CountQuickCommands() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM quick_commands`).Scan(&n)
	return n, err
}

package store

import (
	"path/filepath"
	"testing"
)

func nodeIDs(t *testing.T, st *Store) []string {
	t.Helper()
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func wantIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// §16 (2026-10-02): the wall order is the operator's drag order, and it must
// survive probes registering while the panel is open.
func TestNodeListFollowsDragOrder(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, id := range []string{"a", "b", "c"} {
		if err := st.CreateNode(&Node{ID: id, Name: id, MachineID: "m-" + id}, "secret"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	wantIDs(t, nodeIDs(t, st), []string{"a", "b", "c"})

	// Unknown, repeated and empty ids are absorbed instead of rejected; a row the
	// client never mentioned keeps its relative order after the mentioned ones.
	if err := st.ReorderNodes([]string{"c", "ghost", "a", "a", ""}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	wantIDs(t, nodeIDs(t, st), []string{"c", "a", "b"})

	// Reordering is idempotent — the second drag of the same order changes nothing.
	if err := st.ReorderNodes([]string{"c", "a", "b"}); err != nil {
		t.Fatalf("reorder again: %v", err)
	}
	wantIDs(t, nodeIDs(t, st), []string{"c", "a", "b"})

	// A probe that registers after a drag joins at the end of the wall, not in
	// the middle of the order the operator arranged.
	if err := st.CreateNode(&Node{ID: "d", Name: "d", MachineID: "m-d"}, "secret"); err != nil {
		t.Fatalf("create d: %v", err)
	}
	wantIDs(t, nodeIDs(t, st), []string{"c", "a", "b", "d"})
}

// Rows written before sort_order existed all carry 0 (additive migration): the
// list must then fall back to the old created_at order, with the id breaking
// ties so two probes that registered in the same second cannot swap places
// between renders.
func TestNodeListKeepsCreatedAtOrderForLegacyRows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "legacy-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, id := range []string{"a", "b", "c"} {
		if err := st.CreateNode(&Node{ID: id, Name: id, MachineID: "m-" + id}, "secret"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := st.db.Exec(`UPDATE nodes SET sort_order = 0`); err != nil {
		t.Fatal(err)
	}
	wantIDs(t, nodeIDs(t, st), []string{"a", "b", "c"})

	// max(sort_order)+1 = 1 still sorts after every migrated row.
	if err := st.CreateNode(&Node{ID: "d", Name: "d", MachineID: "m-d"}, "secret"); err != nil {
		t.Fatalf("create d: %v", err)
	}
	wantIDs(t, nodeIDs(t, st), []string{"a", "b", "c", "d"})
}

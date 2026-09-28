package store

import "testing"

// Every alerts query joins `nodes` for the name, so all three call sites must
// survive a NULL name (the LEFT JOIN side) — a plain JOIN or an unqualified
// `id` would break here (the latter as an "ambiguous column name" SQL error).
func TestAlertsResolveNodeName(t *testing.T) {
	st := newTestStore(t)
	seedNode(t, st, "n1")

	id, created, err := st.CreateAlert("node_offline", "n1", `{}`, 3600)
	if err != nil || !created {
		t.Fatalf("create alert: id=%d created=%v err=%v", id, created, err)
	}
	// An alert that is not about a probe (§15 login alerts, the cluster-wide
	// stale sweep) carries no node at all.
	if _, _, err := st.CreateAlert("login_failed", "", `{}`, 0); err != nil {
		t.Fatalf("create node-less alert: %v", err)
	}

	list, err := st.ListAlerts(10)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 alerts, got %d: %+v", len(list), list)
	}
	// Newest first: the login alert, then the probe one.
	if list[0].NodeName != "" || list[0].NodeID != "" {
		t.Fatalf("node-less alert must have no name/id: %+v", list[0])
	}
	if list[1].NodeName != "n1" {
		t.Fatalf("alert node_name = %q, want n1", list[1].NodeName)
	}

	// The name is resolved at read time, so a rename shows up immediately and
	// nothing is written back to the alerts row.
	if err := st.RenameNode("n1", "edge-hk-01"); err != nil {
		t.Fatalf("rename node: %v", err)
	}
	open, err := st.OpenAlert("node_offline", "n1")
	if err != nil {
		t.Fatalf("open alert: %v", err)
	}
	if open.NodeName != "edge-hk-01" {
		t.Fatalf("OpenAlert node_name = %q, want the current name", open.NodeName)
	}
	// UndeliveredAlerts is the delivery queue: oldest first, so the probe alert
	// leads here (ListAlerts is newest first).
	undelivered, err := st.UndeliveredAlerts()
	if err != nil {
		t.Fatalf("undelivered alerts: %v", err)
	}
	if len(undelivered) != 2 || undelivered[0].NodeName != "edge-hk-01" {
		t.Fatalf("UndeliveredAlerts = %+v, want the current name on the probe alert", undelivered)
	}

	// A deleted probe leaves the alert behind; the panel falls back to the raw
	// id, so an empty name must not turn into an error or drop the row.
	if err := st.DeleteNode("n1"); err != nil {
		t.Fatalf("delete node: %v", err)
	}
	list, err = st.ListAlerts(10)
	if err != nil {
		t.Fatalf("list alerts after delete: %v", err)
	}
	if len(list) != 2 || list[1].NodeName != "" || list[1].NodeID != "n1" {
		t.Fatalf("deleted probe alert = %+v, want empty name with the id kept", list[1])
	}
}

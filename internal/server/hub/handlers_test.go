package hub

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/store"
)

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Hub{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func mustCreateNode(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.CreateNode(&store.Node{ID: id, Name: id, MachineID: "machine-" + id}, "secret"); err != nil {
		t.Fatalf("create node %s: %v", id, err)
	}
}

func listAlerts(t *testing.T, st *store.Store) []store.Alert {
	t.Helper()
	rows, err := st.ListAlerts(100)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	return rows
}

func countAlerts(t *testing.T, st *store.Store, kind, nodeID string) int {
	t.Helper()
	n := 0
	for _, a := range listAlerts(t, st) {
		if a.Kind == kind && a.NodeID == nodeID {
			n++
		}
	}
	return n
}

func TestRecordSingboxStateAlertLifecycle(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n1")

	// healthy: no alerts
	h.recordSingboxState("n1", &protocol.SingboxState{Running: true, Version: "1.11.5", Port: 1234})
	if rows := listAlerts(t, h.store); len(rows) != 0 {
		t.Fatalf("healthy report raised alerts: %+v", rows)
	}

	// degraded: opens singbox_down carrying error+port
	h.recordSingboxState("n1", &protocol.SingboxState{
		Version: "1.11.5", Port: 1234, LastError: "port 1234 not reachable",
	})
	rows := listAlerts(t, h.store)
	if len(rows) != 1 || rows[0].Kind != "singbox_down" {
		t.Fatalf("alerts = %+v, want exactly one singbox_down", rows)
	}
	if !strings.Contains(rows[0].Payload, "1234") || !strings.Contains(rows[0].Payload, "not reachable") {
		t.Fatalf("payload = %s, want error and port", rows[0].Payload)
	}
	if rows[0].RecoveredAt != nil {
		t.Fatalf("fresh alert must be open")
	}

	// same error again: deduped, still one row
	h.recordSingboxState("n1", &protocol.SingboxState{
		Version: "1.11.5", Port: 1234, LastError: "port 1234 not reachable",
	})
	if n := countAlerts(t, h.store, "singbox_down", "n1"); n != 1 {
		t.Fatalf("identical degradation duplicated alerts (%d)", n)
	}

	// a different error inside the dedupe window: no second row
	h.recordSingboxState("n1", &protocol.SingboxState{
		Version: "1.11.5", Port: 1234, LastError: "sing-box process exited",
	})
	if n := countAlerts(t, h.store, "singbox_down", "n1"); n != 1 {
		t.Fatalf("dedupe window broken (%d rows)", n)
	}

	// recovery: the open alert is closed
	h.recordSingboxState("n1", &protocol.SingboxState{Running: true, Version: "1.11.5", Port: 1234})
	rows = listAlerts(t, h.store)
	if len(rows) != 1 || rows[0].RecoveredAt == nil {
		t.Fatalf("running state must recover the open alert: %+v", rows)
	}
}

func TestRecordSingboxStateRollbackAlert(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n2")

	// explicit agent flag opens singbox_rollback (the degraded status also
	// raises singbox_down — §15 lists both triggers independently)
	h.recordSingboxState("n2", &protocol.SingboxState{
		Version: "1.10.0", Port: 22,
		LastError: "verify: port 22 not reachable (rolled back)", RollbackHappened: true,
	})
	if n := countAlerts(t, h.store, "singbox_rollback", "n2"); n != 1 {
		t.Fatalf("singbox_rollback alerts = %d, want 1", n)
	}
	var rollback store.Alert
	for _, a := range listAlerts(t, h.store) {
		if a.Kind == "singbox_rollback" && a.NodeID == "n2" {
			rollback = a
		}
	}
	if !strings.Contains(rollback.Payload, "1.10.0") {
		t.Fatalf("payload = %s, want the rolled-back version", rollback.Payload)
	}

	// the flag is one-shot: the follow-up report must not duplicate
	h.recordSingboxState("n2", &protocol.SingboxState{
		Version: "1.10.0", Port: 22, LastError: "verify: port 22 not reachable (rolled back)",
	})
	if n := countAlerts(t, h.store, "singbox_rollback", "n2"); n != 1 {
		t.Fatalf("rollback alert duplicated (%d)", n)
	}

	// heuristic path (agents without the flag): reported version behind the
	// panel's desired_version + rollback wording in the error
	mustCreateNode(t, h.store, "n3")
	if err := h.store.UpsertNodeSingbox(&store.NodeSingbox{NodeID: "n3", DesiredVersion: "2.0.0"}); err != nil {
		t.Fatalf("seed desired_version: %v", err)
	}
	h.recordSingboxState("n3", &protocol.SingboxState{
		Version: "1.9.0", Port: 80, LastError: "config check: invalid (rolled back)",
	})
	if n := countAlerts(t, h.store, "singbox_rollback", "n3"); n != 1 {
		t.Fatalf("heuristic rollback not detected (%d)", n)
	}

	// version matching desired again: no further rollback alert
	h.recordSingboxState("n3", &protocol.SingboxState{Version: "2.0.0", Running: true, Port: 80})
	if n := countAlerts(t, h.store, "singbox_rollback", "n3"); n != 1 {
		t.Fatalf("healthy version raised rollback alerts (%d)", n)
	}
}

func TestRecordSingboxStateFirewallHint(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n4")

	// hint lands in node_singbox.firewall_hint
	h.recordSingboxState("n4", &protocol.SingboxState{
		Running: true, Version: "1.0.0", Port: 5555, FirewallHint: "ufw allow 5555/tcp",
	})
	hint, err := h.store.GetNodeSingboxFirewallHint("n4")
	if err != nil || hint != "ufw allow 5555/tcp" {
		t.Fatalf("hint = %q, %v; want the ufw command", hint, err)
	}

	// a later report without a hint clears it (agent-side resolution)
	h.recordSingboxState("n4", &protocol.SingboxState{Running: true, Version: "1.0.0", Port: 5555})
	if hint, err = h.store.GetNodeSingboxFirewallHint("n4"); err != nil || hint != "" {
		t.Fatalf("hint = %q, %v; want cleared", hint, err)
	}

	// the main upsert must never clobber the hint column
	const iptablesHint = "iptables -I INPUT -p tcp --dport 5555 -j ACCEPT"
	if err := h.store.SetNodeSingboxFirewallHint("n4", iptablesHint); err != nil {
		t.Fatalf("set hint: %v", err)
	}
	if err := h.store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: "n4", Version: "1.0.0", Status: "running", Port: 5555,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if hint, _ = h.store.GetNodeSingboxFirewallHint("n4"); hint != iptablesHint {
		t.Fatalf("upsert clobbered firewall_hint: %q", hint)
	}
}

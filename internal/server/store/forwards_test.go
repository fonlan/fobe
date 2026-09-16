package store

import "testing"

func TestReplaceAndListNodeForwards(t *testing.T) {
	st, _ := openTemp(t)
	node := mustNode(t, st, "n1")

	// Never reported yet: the zero status, not ErrNotFound.
	status, err := st.GetNodeForwardStatus(node)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.ReportedAt != 0 {
		t.Fatalf("status = %+v, want the zero value", status)
	}
	if rows, err := st.ListNodeForwards(node); err != nil || len(rows) != 0 {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}

	want := []NodeForward{
		{Handle: 3, Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Comment: "web"},
		{Handle: 4, Proto: "udp", SrcPort: 5353, DstIP: "10.0.0.2", DstPort: 5353, ExtraMatch: true},
	}
	report := NodeForwardStatus{Supported: true, Initialized: true, ReportedAt: 1730000000}
	if err := st.ReplaceNodeForwards(node, report, want); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := st.ListNodeForwards(node)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %+v", got)
	}
	if got[0] != want[0] || got[1] != want[1] {
		t.Errorf("rows = %+v, want %+v", got, want)
	}
	status, err = st.GetNodeForwardStatus(node)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != report {
		t.Errorf("status = %+v, want %+v", status, report)
	}

	// A second report replaces the snapshot wholesale: the rule nfpf.sh deleted
	// outside the panel must not linger in the panel's list.
	report.Supported, report.Code, report.ReportedAt = false, "nft_missing", 1730000100
	if err := st.ReplaceNodeForwards(node, report, nil); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	if rows, err := st.ListNodeForwards(node); err != nil || len(rows) != 0 {
		t.Fatalf("rows = %+v err = %v, want empty", rows, err)
	}
	if status, err = st.GetNodeForwardStatus(node); err != nil || status.Code != "nft_missing" {
		t.Fatalf("status = %+v err = %v", status, err)
	}
}

func TestReplaceNodeForwardsIsPerNode(t *testing.T) {
	st, _ := openTemp(t)
	a := mustNode(t, st, "a")
	b := mustNode(t, st, "b")
	rule := []NodeForward{{Handle: 3, Proto: "tcp", SrcPort: 1, DstIP: "10.0.0.1", DstPort: 2}}
	if err := st.ReplaceNodeForwards(a, NodeForwardStatus{ReportedAt: 1}, rule); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceNodeForwards(b, NodeForwardStatus{ReportedAt: 2}, rule); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceNodeForwards(a, NodeForwardStatus{ReportedAt: 3}, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListNodeForwards(b)
	if err != nil || len(rows) != 1 {
		t.Fatalf("node b rows = %+v err = %v, want its own snapshot", rows, err)
	}
}

// mustNode inserts a node row (the forwards tables cascade from it) and returns
// its id.
func mustNode(t *testing.T, st *Store, id string) string {
	t.Helper()
	if err := st.CreateNode(&Node{ID: id, Name: id, MachineID: "machine-" + id, Hostname: id, TZ: "UTC"}, "hash"); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	return id
}

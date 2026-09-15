package store

import (
	"path/filepath"
	"testing"
)

// replaceRows builds a full agent-style report (hub always reports
// IsPrimary=false; primary selection is server-side).
func replaceRows(pairs ...[2]any) []IPRow {
	out := make([]IPRow, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, IPRow{IP: p[0].(string), Family: p[1].(int), Scope: "public"})
	}
	return out
}

func TestManualPrimarySurvivesIPReplacement(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	n := &Node{ID: "n1", Name: "n1", MachineID: "m-manual-1"}
	if err := st.CreateNode(n, "hash"); err != nil {
		t.Fatal(err)
	}

	primary := func() (string, int) {
		t.Helper()
		ips, err := st.ListNodeIPs("n1")
		if err != nil {
			t.Fatalf("list ips: %v", err)
		}
		prim, manual := "", 0
		for _, ip := range ips {
			if ip.IsPrimary {
				if prim != "" {
					t.Fatalf("multiple primaries: %+v", ips)
				}
				prim = ip.IP
			}
			var m int
			if err := st.db.QueryRow(
				`SELECT manual_primary FROM node_ips WHERE node_id = ? AND ip = ?`, "n1", ip.IP,
			).Scan(&m); err != nil {
				t.Fatalf("read manual_primary: %v", err)
			}
			if m != 0 {
				manual++
			}
		}
		return prim, manual
	}

	// initial report: auto heuristic pins the first public v4
	if err := st.ReplaceNodeIPs("n1", replaceRows([2]any{"198.51.100.5", 4}, [2]any{"198.51.100.6", 4})); err != nil {
		t.Fatal(err)
	}
	if prim, manual := primary(); prim != "198.51.100.5" || manual != 0 {
		t.Fatalf("after first report primary=%q manual=%d, want 198.51.100.5/0", prim, manual)
	}

	// the panel pins the other address by hand
	if err := st.SetManualPrimary("n1", "198.51.100.6"); err != nil {
		t.Fatal(err)
	}
	if prim, manual := primary(); prim != "198.51.100.6" || manual != 1 {
		t.Fatalf("after pin primary=%q manual=%d, want 198.51.100.6/1", prim, manual)
	}
	if got, err := st.GetNode("n1"); err != nil || got.PrimaryIP != "198.51.100.6" {
		t.Fatalf("nodes.primary_ip = %q err=%v, want 198.51.100.6", got.PrimaryIP, err)
	}

	// next full agent report (same addresses, no agent-side primary): the
	// manual pick must survive the rebuild instead of the heuristic's .5
	if err := st.ReplaceNodeIPs("n1", replaceRows([2]any{"198.51.100.5", 4}, [2]any{"198.51.100.6", 4})); err != nil {
		t.Fatal(err)
	}
	if prim, manual := primary(); prim != "198.51.100.6" || manual != 1 {
		t.Fatalf("after rebuild primary=%q manual=%d, want 198.51.100.6/1", prim, manual)
	}
	if got, err := st.GetNode("n1"); err != nil || got.PrimaryIP != "198.51.100.6" {
		t.Fatalf("primary_ip after rebuild = %q err=%v, want 198.51.100.6", got.PrimaryIP, err)
	}

	// once the manual address disappears from the reports the pin is dropped
	// and the heuristic re-pins
	if err := st.ReplaceNodeIPs("n1", replaceRows([2]any{"198.51.100.7", 4}, [2]any{"198.51.100.9", 4})); err != nil {
		t.Fatal(err)
	}
	if prim, manual := primary(); prim != "198.51.100.7" || manual != 0 {
		t.Fatalf("after manual IP vanished primary=%q manual=%d, want 198.51.100.7/0", prim, manual)
	}
}

package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// §9.2 firewall_hint + §19.9 password_override: additive node_singbox
// columns accessed through the dedicated upsert-only-column methods.
func TestNodeSingboxHintAndPasswordOverride(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateNode(&Node{ID: "nx", Name: "nx", MachineID: "m-nx"}, "secret"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetNodeSingboxPasswordOverride("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent override err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetNodeSingboxFirewallHint("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent hint err = %v, want ErrNotFound", err)
	}

	if err := st.SetNodeSingboxPasswordOverride("nx", "node-secret"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if pw, err := st.GetNodeSingboxPasswordOverride("nx"); err != nil || pw != "node-secret" {
		t.Fatalf("override = %q, %v; want node-secret", pw, err)
	}

	if err := st.SetNodeSingboxFirewallHint("nx", "ufw allow 1/tcp"); err != nil {
		t.Fatalf("set hint: %v", err)
	}
	if hint, err := st.GetNodeSingboxFirewallHint("nx"); err != nil || hint != "ufw allow 1/tcp" {
		t.Fatalf("hint = %q, %v; want the ufw command", hint, err)
	}

	// empty write clears (agent reported the port as allowed)
	if err := st.SetNodeSingboxFirewallHint("nx", ""); err != nil {
		t.Fatalf("clear hint: %v", err)
	}
	if hint, _ := st.GetNodeSingboxFirewallHint("nx"); hint != "" {
		t.Fatalf("hint = %q, want cleared", hint)
	}

	// the setters must create the row when absent, leaving other columns at
	// their defaults
	if err := st.CreateNode(&Node{ID: "ny", Name: "ny", MachineID: "m-ny"}, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeSingboxPasswordOverride("ny", "pw2"); err != nil {
		t.Fatalf("set override on absent row: %v", err)
	}
	row, err := st.GetNodeSingbox("ny")
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status != "absent" || row.Version != "" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if pw, err := st.GetNodeSingboxPasswordOverride("ny"); err != nil || pw != "pw2" {
		t.Fatalf("override after row-create = %q, %v; want pw2", pw, err)
	}
}

// SingboxDesiredVersionRefs powers the §9.2 release-cache reference counts:
// only non-empty desired_version values are counted, one row per distinct
// version.
func TestSingboxDesiredVersionRefs(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "refs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if refs, err := st.SingboxDesiredVersionRefs(); err != nil || len(refs) != 0 {
		t.Fatalf("empty refs = %v, %v; want empty map", refs, err)
	}

	for _, n := range []string{"r1", "r2", "r3"} {
		if err := st.CreateNode(&Node{ID: n, Name: n, MachineID: "m-" + n}, "secret"); err != nil {
			t.Fatal(err)
		}
	}
	rows := []NodeSingbox{
		{NodeID: "r1", DesiredVersion: "1.10.0", Status: "running"},
		{NodeID: "r2", DesiredVersion: "1.10.0", Status: "installing"},
		{NodeID: "r3", Status: "absent"}, // no desired version -> not counted
	}
	for i := range rows {
		if err := st.UpsertNodeSingbox(&rows[i]); err != nil {
			t.Fatalf("upsert %s: %v", rows[i].NodeID, err)
		}
	}

	refs, err := st.SingboxDesiredVersionRefs()
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	if len(refs) != 1 || refs["1.10.0"] != 2 {
		t.Fatalf("refs = %v, want {1.10.0: 2}", refs)
	}
}

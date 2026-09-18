// Tests for design.md §10.2 entries: the binding tuple, tombstones, the legacy
// node-ids projection and the upgrade from a database that predates the table.
package store

import (
	"testing"
)

func entryFixture(nodeID, relayID string) SubscriptionEntry {
	return SubscriptionEntry{NodeID: nodeID, RelayNodeID: relayID, Proto: "tcp", SrcPort: 8080, Enabled: true}
}

// TestSubscriptionEntriesTombstoneSemantics pins the rule the panel depends on:
// listing an entry as disabled writes a tombstone only when it was bound, and
// the projection a downgrade reads contains exactly the enabled direct nodes.
func TestSubscriptionEntriesTombstoneSemantics(t *testing.T) {
	st, _ := openTemp(t)
	if err := st.CreateSubscription("sub1", "main", "hash1", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSubscription("sub2", "other", "hash2", ""); err != nil {
		t.Fatal(err)
	}
	seed := func(id string) {
		t.Helper()
		if err := st.CreateNode(&Node{ID: id, Name: id, MachineID: id}, "secret"); err != nil {
			t.Fatal(err)
		}
	}
	seed("a")
	seed("b")

	// Save "A off, B on": A was never bound, so it must stay absent — the
	// reconciler has to be able to enrol it later.
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "a", Enabled: false},
		{NodeID: "b", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.SubscriptionEntries("sub1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NodeID != "b" || !got[0].Enabled {
		t.Fatalf("entries = %+v, want only B bound and enabled", got)
	}
	ids, err := st.SubscriptionNodeIDs("sub1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("legacy projection = %v, want [b]", ids)
	}

	// Auto-enrol a relay, then switch it off: the tombstone has to survive a
	// second reconcile attempt and must not appear in the projection.
	relay := entryFixture("b", "a")
	relay.SubscriptionID = "sub1"
	created, err := st.InsertSubscriptionEntryIfAbsent(relay)
	if err != nil || !created {
		t.Fatalf("auto-enrol: created=%v err=%v", created, err)
	}
	if created, err = st.InsertSubscriptionEntryIfAbsent(relay); err != nil || created {
		t.Fatalf("auto-enrol is not idempotent: created=%v err=%v", created, err)
	}
	relay.Enabled = false
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{relay, {NodeID: "b", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if created, err = st.InsertSubscriptionEntryIfAbsent(relay); err != nil || created {
		t.Fatalf("tombstone was overwritten: created=%v err=%v", created, err)
	}
	got, _ = st.SubscriptionEntries("sub1")
	if len(got) != 2 || got[1].Enabled {
		t.Fatalf("entries = %+v, want B direct plus a disabled relay row", got)
	}
	ids, _ = st.SubscriptionNodeIDs("sub1")
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("relay leaked into the legacy projection: %v", ids)
	}
}

// TestSetSubscriptionDirectNodesKeepsRelayRowsAndAliases: the legacy API owns
// the direct half only.
func TestSetSubscriptionDirectNodesKeepsRelayRowsAndAliases(t *testing.T) {
	st, _ := openTemp(t)
	if err := st.CreateSubscription("sub1", "main", "hash1", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := st.CreateNode(&Node{ID: id, Name: id, MachineID: id}, "secret"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "a", Alias: "香港", Enabled: true},
		entryFixture("b", "a"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubscriptionNodes("sub1", []string{"b"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.SubscriptionEntries("sub1")
	var direct int
	var relayKept, aliasKept bool
	for _, e := range got {
		if e.RelayNodeID == "" {
			direct++
			continue
		}
		relayKept = e.Enabled
	}
	// A was dropped by the legacy replace; it also carried the only alias, so
	// the alias check uses a fresh save below.
	_ = aliasKept
	if direct != 1 || !relayKept {
		t.Fatalf("legacy write touched more than the direct half: %+v", got)
	}
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "b", Alias: "保留", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubscriptionNodes("sub1", []string{"b"}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.SubscriptionEntries("sub1")
	for _, e := range got {
		if e.RelayNodeID == "" {
			aliasKept = e.Alias == "保留"
		}
	}
	if !aliasKept {
		t.Fatalf("legacy write dropped the direct alias: %+v", got)
	}
}

// TestSplitSubscriptionDirectEntry: the legacy node-level direct row (src_port
// 0, "every inbound of this node") becomes one row per inbound, and the
// operator's alias/enabled state travels with it — that is what keeps an
// upgraded panel from rendering a node twice.
func TestSplitSubscriptionDirectEntry(t *testing.T) {
	st, _ := openTemp(t)
	if err := st.CreateSubscription("sub1", "main", "hash1", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateNode(&Node{ID: "b", Name: "B", MachineID: "mb"}, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "b", Alias: "东京", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	// An empty port list is a no-op: there is nothing to split into, and
	// deleting the row would drop the binding.
	if ok, err := st.SplitSubscriptionDirectEntry("sub1", "b", "东京", true, nil); err != nil || ok {
		t.Fatalf("split with no ports: ok=%v err=%v", ok, err)
	}
	if got, _ := st.SubscriptionEntries("sub1"); len(got) != 1 || got[0].SrcPort != 0 {
		t.Fatalf("row disappeared on a no-op split: %+v", got)
	}

	ok, err := st.SplitSubscriptionDirectEntry("sub1", "b", "东京", true, []int{8443, 8444})
	if err != nil || !ok {
		t.Fatalf("split: ok=%v err=%v", ok, err)
	}
	got, _ := st.SubscriptionEntries("sub1")
	if len(got) != 2 {
		t.Fatalf("entries = %+v, want two per-port rows", got)
	}
	for i, want := range []int{8443, 8444} {
		if got[i].NodeID != "b" || got[i].SrcPort != want || got[i].Alias != "东京" || !got[i].Enabled {
			t.Errorf("row %d = %+v, want node b port %d alias 东京 enabled", i, got[i], want)
		}
	}
	// Idempotent: the node-level row is gone, so a second pass has nothing to do
	// (a concurrent trigger may have got there first).
	if ok, err = st.SplitSubscriptionDirectEntry("sub1", "b", "东京", true, []int{8443, 8444}); err != nil || ok {
		t.Fatalf("second split was not a no-op: ok=%v err=%v", ok, err)
	}
	// A per-port row that already exists keeps its own alias/enabled state: the
	// operator's name for that inbound wins over the one carried by the
	// node-level row (the legacy node_ids API can have written both).
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "b", Alias: "东京", Enabled: true},
		{NodeID: "b", SrcPort: 8444, Alias: "备用", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err = st.SplitSubscriptionDirectEntry("sub1", "b", "东京", true, []int{8443, 8444}); err != nil || !ok {
		t.Fatalf("split beside an existing row: ok=%v err=%v", ok, err)
	}
	got, _ = st.SubscriptionEntries("sub1")
	byPort := map[int]SubscriptionEntry{}
	for _, e := range got {
		byPort[e.SrcPort] = e
	}
	if len(got) != 2 || byPort[8443].Alias != "东京" || byPort[8444].Alias != "备用" {
		t.Fatalf("split clobbered an existing per-port row: %+v", got)
	}
	// A tombstone splits too: the operator's "off" survives the upgrade instead
	// of coming back as an unchecked candidate in the picker. Opting out has to
	// start from a bound row — a never-bound entry stays absent by design.
	if err := st.SetSubscriptionDirectNodes("sub1", []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "b", Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.SubscriptionEntries("sub1"); len(rows) != 1 || rows[0].SrcPort != 0 || rows[0].Enabled {
		t.Fatalf("precondition: want a node-level tombstone, got %+v", rows)
	}
	if ok, err = st.SplitSubscriptionDirectEntry("sub1", "b", "", false, []int{8443, 8444}); err != nil || !ok {
		t.Fatalf("split a tombstone: ok=%v err=%v", ok, err)
	}
	got, _ = st.SubscriptionEntries("sub1")
	if len(got) != 2 || got[0].Enabled || got[1].Enabled {
		t.Fatalf("the tombstone did not survive the split: %+v", got)
	}
}

// TestMoveSubscriptionDirectEntryPorts: a port the panel itself moved carries
// the subscription bindings with it, names included.
func TestMoveSubscriptionDirectEntryPorts(t *testing.T) {
	st, _ := openTemp(t)
	if err := st.CreateSubscription("sub1", "main", "hash1", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateNode(&Node{ID: "b", Name: "B", MachineID: "mb"}, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "b", SrcPort: 8443, Alias: "东京", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.MoveSubscriptionDirectEntryPorts("b", 8443, 8443); err != nil || n != 0 {
		t.Fatalf("a move to the same port is a no-op: n=%d err=%v", n, err)
	}
	n, err := st.MoveSubscriptionDirectEntryPorts("b", 8443, 9443)
	if err != nil || n != 1 {
		t.Fatalf("move: n=%d err=%v", n, err)
	}
	got, _ := st.SubscriptionEntries("sub1")
	if len(got) != 1 || got[0].SrcPort != 9443 || got[0].Alias != "东京" {
		t.Fatalf("entries = %+v, want the alias to travel to 9443", got)
	}
	// A row that already exists at the destination wins; the stale source row is
	// dropped rather than left dangling.
	if err := st.SetSubscriptionEntries("sub1", []SubscriptionEntry{
		{NodeID: "b", SrcPort: 9443, Alias: "备用", Enabled: true},
		{NodeID: "b", SrcPort: 9444, Alias: "旧", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if n, err = st.MoveSubscriptionDirectEntryPorts("b", 9444, 9443); err != nil || n != 0 {
		t.Fatalf("move onto an existing row: n=%d err=%v", n, err)
	}
	got, _ = st.SubscriptionEntries("sub1")
	if len(got) != 1 || got[0].Alias != "备用" {
		t.Fatalf("the destination row was clobbered: %+v", got)
	}
	// The legacy projection follows too: it is rebuilt from the moved rows.
	ids, _ := st.SubscriptionNodeIDs("sub1")
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("legacy projection = %v, want [b]", ids)
	}
}

// TestEntriesMigrationFromPreEntriesDatabase: a v9 file has neither the table
// nor nodes.sub_name, and opening it must add both without touching rows.
func TestEntriesMigrationFromPreEntriesDatabase(t *testing.T) {
	st, path := openTemp(t)
	if _, err := st.db.Exec(`DROP TABLE subscription_entries`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`ALTER TABLE nodes DROP COLUMN sub_name`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 9`); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateNode(&Node{ID: "a", Name: "A", MachineID: "ma"}, "secret"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-entries database: %v", err)
	}
	defer st2.Close()
	n, err := st2.GetNode("a")
	if err != nil || n.Name != "A" || n.SubName != "" {
		t.Fatalf("node lost across migration: %+v err=%v", n, err)
	}
	// The table itself is the migration's product: reaching it means the
	// statement was created on an upgraded file, and it starts out empty.
	var rows int
	if err := st2.db.QueryRow(`SELECT count(*) FROM subscription_entries`).Scan(&rows); err != nil {
		t.Fatalf("subscription_entries missing after migration: %v", err)
	}
	if rows != 0 {
		t.Fatalf("freshly created table has %d rows", rows)
	}
	if !st2.columnExists("nodes", "sub_name") {
		t.Fatal("nodes.sub_name missing after migration")
	}
	v, _ := st2.userVersion()
	if v != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, SchemaVersion)
	}
}

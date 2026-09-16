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

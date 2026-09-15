package agentupdate

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/store"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addNode(t *testing.T, st *store.Store, id, version string) {
	t.Helper()
	if err := st.CreateNode(&store.Node{
		ID: id, Name: id, MachineID: "m-" + id, AgentVersion: version,
	}, "hash"); err != nil {
		t.Fatalf("create node: %v", err)
	}
}

// writeArtifact plants a downloadable agent build on the artifact volume.
func writeArtifact(t *testing.T, dlDir, version string) {
	t.Helper()
	dir := filepath.Join(dlDir, "agent", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "linux-amd64"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "linux-amd64.sha256"), []byte("deadbeef"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsReleaseVersion(t *testing.T) {
	cases := map[string]bool{
		"":                  false,
		"dev":               false,
		"compose":           false,
		"latest":            false,
		"unknown":           false,
		"abc":               false,
		"20260915.054229":   true,
		"v1.2.3":            true,
		"1.2.3-rc1":         true,
		"release-candidate": false,
	}
	for in, want := range cases {
		if got := IsReleaseVersion(in); got != want {
			t.Errorf("IsReleaseVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestArtifactPresent(t *testing.T) {
	dl := t.TempDir()
	if ArtifactPresent(dl, "1.0.0") {
		t.Fatal("empty volume must not report an artifact")
	}
	writeArtifact(t, dl, "1.0.0")
	if !ArtifactPresent(dl, "1.0.0") {
		t.Fatal("planted artifact not detected")
	}
	// a zero-byte file is not an artifact: it would be served as a corrupt binary
	empty := filepath.Join(dl, "agent", "2.0.0")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "linux-amd64"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "linux-amd64.sha256"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ArtifactPresent(dl, "2.0.0") {
		t.Fatal("empty artifact file must not count as present")
	}
}

func TestGatesRefuseTarget(t *testing.T) {
	dl := t.TempDir()
	writeArtifact(t, dl, "20260915.000000")

	cases := []struct {
		name     string
		cfg      Config
		want     OffReason
		wantOn   bool
		modified func(*Config)
	}{
		{name: "ok", wantOn: true},
		{name: "switch off", want: ReasonOffSwitchOff, modified: func(c *Config) {
			c.Enabled = func() bool { return false }
		}},
		{name: "kill switch", want: ReasonOffKillSwitch, modified: func(c *Config) {
			c.KillSwitch = func() bool { return true }
		}},
		{name: "no dl dir", want: ReasonOffNoDLDir, modified: func(c *Config) { c.DLDir = "" }},
		{name: "dev version", want: ReasonOffNotReleased, modified: func(c *Config) { c.ServerVersion = "dev" }},
		{name: "artifact missing", want: ReasonOffNoArtifact, modified: func(c *Config) { c.ServerVersion = "9.9.9" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			addNode(t, st, "n1", "old")
			cfg := Config{
				Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: dl,
				Enabled: func() bool { return true },
			}
			if tc.modified != nil {
				tc.modified(&cfg)
			}
			m := New(cfg)
			stt := m.Status()
			if stt.Enabled != tc.wantOn {
				t.Fatalf("enabled = %v, want %v (reason %q)", stt.Enabled, tc.wantOn, stt.Reason)
			}
			if !tc.wantOn && stt.Reason != string(tc.want) {
				t.Fatalf("reason = %q, want %q", stt.Reason, tc.want)
			}
			if tc.wantOn {
				if v, _, ok := m.Target("n1"); !ok || v != "20260915.000000" {
					t.Fatalf("Target = %q ok=%v, want the server version", v, ok)
				}
			} else if _, _, ok := m.Target("n1"); ok {
				t.Fatal("no target may be offered while a gate refuses")
			}
		})
	}
}

// The stagger anchor is written once per (node, target) and must not move on
// reconnect: the panel shows it as the plan.
func TestTargetStaggerIsStable(t *testing.T) {
	dl := t.TempDir()
	writeArtifact(t, dl, "20260915.000000")
	st := testStore(t)
	addNode(t, st, "n1", "old")
	now := int64(1_700_000_000)
	m := New(Config{
		Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: dl,
		Enabled: func() bool { return true },
		Now:     func() int64 { return now },
		Stagger: 5 * time.Minute,
	})

	v1, a1, ok := m.Target("n1")
	if !ok || v1 != "20260915.000000" {
		t.Fatalf("first Target: %q %v", v1, ok)
	}
	if a1 < now || a1 > now+int64(5*time.Minute/time.Second) {
		t.Fatalf("deadline %d outside the stagger window starting at %d", a1, now)
	}
	now += 60
	v2, a2, ok := m.Target("n1")
	if !ok || v2 != v1 || a2 != a1 {
		t.Fatalf("deadline moved on reconnect: %d → %d", a1, a2)
	}
	n, err := st.GetNode("n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.AgentTargetVersion != "20260915.000000" || n.AgentUpdatePlannedAt == 0 {
		t.Fatalf("plan not recorded: target=%q planned_at=%d", n.AgentTargetVersion, n.AgentUpdatePlannedAt)
	}
}

func TestTargetConvergedClosesThePlan(t *testing.T) {
	dl := t.TempDir()
	writeArtifact(t, dl, "20260915.000000")
	st := testStore(t)
	addNode(t, st, "n1", "20260915.000000")
	if err := st.SetAgentUpdatePlanned("n1", "20260915.000000", 12345); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: dl,
		Enabled: func() bool { return true }})
	if _, _, ok := m.Target("n1"); ok {
		t.Fatal("a converged node must not be given a target")
	}
	n, _ := st.GetNode("n1")
	if n.AgentUpdateState != StateCommitted || n.AgentUpdatePlannedAt != 0 {
		t.Fatalf("plan not closed: state=%q planned_at=%d", n.AgentUpdateState, n.AgentUpdatePlannedAt)
	}
}

func TestOnReportBookkeepingAlertsAndAudit(t *testing.T) {
	dl := t.TempDir()
	writeArtifact(t, dl, "20260915.000000")
	st := testStore(t)
	addNode(t, st, "n1", "old")
	m := New(Config{Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: dl,
		Enabled: func() bool { return true }, Now: func() int64 { return 1_700_000_000 }})

	// progress is recorded without an alert
	m.OnReport("n1", &protocol.AgentUpdate{Target: "20260915.000000", Phase: protocol.UpdateDownloading})
	n, _ := st.GetNode("n1")
	if n.AgentUpdateState != StateDownloading {
		t.Fatalf("state = %q, want downloading", n.AgentUpdateState)
	}
	if alerts, _ := st.ListAlerts(10); len(alerts) != 0 {
		t.Fatalf("progress must not raise alerts: %+v", alerts)
	}

	// terminal failure: recorded, alerted, and audited as a system action
	m.OnReport("n1", &protocol.AgentUpdate{
		Target: "20260915.000000", Phase: protocol.UpdateFailed,
		Class: protocol.ClassTerminal, Error: "sha256 mismatch", Attempts: 2,
	})
	n, _ = st.GetNode("n1")
	if n.AgentUpdateState != StateFailed || n.AgentUpdateAttempts != 2 || n.AgentUpdateError == "" {
		t.Fatalf("terminal report not recorded: %+v", n)
	}
	if _, err := st.OpenAlert(AlertFailed, "n1"); err != nil {
		t.Fatalf("terminal failure must raise %s: %v", AlertFailed, err)
	}

	// the same verdict repeated on reconnect must not duplicate the audit line
	auditsBefore := countAudit(t, st, "agent_update_failed")
	m.OnReport("n1", &protocol.AgentUpdate{
		Target: "20260915.000000", Phase: protocol.UpdateFailed,
		Class: protocol.ClassTerminal, Error: "sha256 mismatch", Attempts: 2,
	})
	if got := countAudit(t, st, "agent_update_failed"); got != auditsBefore {
		t.Fatalf("repeat report duplicated the audit line: %d → %d", auditsBefore, got)
	}

	// suppressed is its own state and keeps the high-risk alert open
	m.OnReport("n1", &protocol.AgentUpdate{
		Target: "20260915.000000", Phase: protocol.UpdateSuppressed,
		Class: protocol.ClassTerminal, Error: "gave up", Attempts: 3,
	})
	n, _ = st.GetNode("n1")
	if n.AgentUpdateState != StateSuppressed {
		t.Fatalf("state = %q, want suppressed", n.AgentUpdateState)
	}

	// committing recovers both node alerts and stamps done_at
	m.OnReport("n1", &protocol.AgentUpdate{Target: "20260915.000000", Phase: protocol.UpdateCommitted})
	n, _ = st.GetNode("n1")
	if n.AgentUpdateState != StateCommitted || n.AgentUpdateDoneAt == 0 {
		t.Fatalf("commit not recorded: %+v", n)
	}
	if _, err := st.OpenAlert(AlertFailed, "n1"); err == nil {
		t.Fatal("commit must recover the failure alert")
	}
}

func TestTransientAlertOnlyAfterThreshold(t *testing.T) {
	dl := t.TempDir()
	writeArtifact(t, dl, "20260915.000000")
	st := testStore(t)
	addNode(t, st, "n1", "old")
	m := New(Config{Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: dl,
		Enabled: func() bool { return true }})

	for i := 1; i <= 2; i++ {
		m.OnReport("n1", &protocol.AgentUpdate{
			Target: "20260915.000000", Phase: protocol.UpdateFailed,
			Class: protocol.ClassTransient, Error: "dial tcp: refused", Attempts: i,
		})
	}
	if _, err := st.OpenAlert(AlertTransient, "n1"); err == nil {
		t.Fatal("two transient failures must not alert yet")
	}
	m.OnReport("n1", &protocol.AgentUpdate{
		Target: "20260915.000000", Phase: protocol.UpdateFailed,
		Class: protocol.ClassTransient, Error: "dial tcp: refused", Attempts: 3,
	})
	if _, err := st.OpenAlert(AlertTransient, "n1"); err != nil {
		t.Fatalf("third transient failure must alert: %v", err)
	}
	n, _ := st.GetNode("n1")
	if n.AgentUpdateState != StateTransient {
		t.Fatalf("state = %q, want transient", n.AgentUpdateState)
	}
}

func TestSweepRaisesOneAggregateAlert(t *testing.T) {
	dl := t.TempDir()
	writeArtifact(t, dl, "20260915.000000")
	st := testStore(t)
	addNode(t, st, "n1", "old")
	addNode(t, st, "n2", "old")
	addNode(t, st, "n3", "20260915.000000") // converged: not stale
	now := int64(1_700_000_000)
	m := New(Config{
		Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: dl,
		Enabled: func() bool { return true }, Now: func() int64 { return now },
		ConvergeAfter: 15 * time.Minute,
	})
	// both lagging nodes were told to start just now: the window has not passed
	for _, id := range []string{"n1", "n2"} {
		if _, _, ok := m.Target(id); !ok {
			t.Fatalf("expected a target for %s", id)
		}
		if err := st.SetAgentUpdatePlanned(id, "20260915.000000", now); err != nil {
			t.Fatal(err)
		}
	}

	m.sweep()
	if alerts, _ := st.ListAlerts(10); len(alerts) != 0 {
		t.Fatalf("sweep must wait for the convergence window: %+v", alerts)
	}

	now += int64(20 * time.Minute / time.Second)
	m.sweep()
	alerts, _ := st.ListAlerts(10)
	if len(alerts) != 1 {
		t.Fatalf("want exactly one aggregate alert, got %d: %+v", len(alerts), alerts)
	}
	if alerts[0].Kind != AlertStale || alerts[0].NodeID != "" {
		t.Fatalf("aggregate alert shape: %+v", alerts[0])
	}
	// a converged node must never appear in the aggregate
	if got := countAudit(t, st, "agent_update_stale"); got != 1 {
		t.Fatalf("stale audit lines = %d, want 1", got)
	}

	// still stale, still inside the dedupe window: no second row
	m.sweep()
	if alerts, _ := st.ListAlerts(10); len(alerts) != 1 {
		t.Fatalf("stale sweep duplicated the alert: %+v", alerts)
	}
}

func TestSweepSilentWhileDisabled(t *testing.T) {
	dl := t.TempDir()
	writeArtifact(t, dl, "20260915.000000")
	st := testStore(t)
	addNode(t, st, "n1", "old")
	m := New(Config{Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: dl,
		Enabled: func() bool { return false }, Now: func() int64 { return 1_700_000_000 }})
	if err := st.SetAgentUpdatePlanned("n1", "20260915.000000", 1); err != nil {
		t.Fatal(err)
	}
	m.sweep()
	if alerts, _ := st.ListAlerts(10); len(alerts) != 0 {
		t.Fatalf("a disabled feature must not nag about convergence: %+v", alerts)
	}
}

func TestRetryClearsCounters(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "old")
	m := New(Config{Store: st, Log: testLog(), ServerVersion: "20260915.000000", DLDir: t.TempDir(),
		Enabled: func() bool { return true }})
	if err := st.RecordAgentUpdate("n1", "20260915.000000", StateSuppressed, 3, "boom", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Retry("n1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	n, _ := st.GetNode("n1")
	if n.AgentUpdateAttempts != 0 || n.AgentUpdateError != "" || n.AgentUpdatePlannedAt != 0 {
		t.Fatalf("retry did not clear the bookkeeping: %+v", n)
	}
}

func TestAutoUpdateEnabledDefaultOn(t *testing.T) {
	st := testStore(t)
	if !AutoUpdateEnabled(st) {
		t.Fatal("an unset switch must default to on")
	}
	if err := SetAutoUpdate(st, false); err != nil {
		t.Fatal(err)
	}
	if AutoUpdateEnabled(st) {
		t.Fatal("switch off must be respected")
	}
	if err := SetAutoUpdate(st, true); err != nil {
		t.Fatal(err)
	}
	if !AutoUpdateEnabled(st) {
		t.Fatal("switch on must be respected")
	}
}

func TestStartStopsWithContext(t *testing.T) {
	m := New(Config{Store: testStore(t), Log: testLog(), ServerVersion: "dev", SweepEvery: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel() // must not panic or leak; nothing else to assert here
}

func countAudit(t *testing.T, st *store.Store, action string) int {
	t.Helper()
	entries, err := st.ListAudit(200)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == action {
			n++
		}
	}
	return n
}

package scheduler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/server/notify"
	"github.com/fobe-panel/fobe/internal/server/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// decoder mimics the DecryptFunc that cmd/server/main.go builds from the
// Cryptor, backed by a settings map instead of the store.
func decoder(settings map[string]string) notify.DecryptFunc {
	return func(key string) (string, bool) {
		v, ok := settings[key]
		return v, ok
	}
}

func addNode(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	err := st.CreateNode(&store.Node{ID: id, Name: name, MachineID: "m-" + id}, "hash")
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
}

// fakeNotifier is the interface mock for Telegram-style channels.
type fakeNotifier struct {
	name       string
	configured bool
	err        error
	events     []notify.Event
}

func (f *fakeNotifier) Name() string     { return f.name }
func (f *fakeNotifier) Configured() bool { return f.configured }
func (f *fakeNotifier) Deliver(ev notify.Event) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}

func alertKinds(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	alerts, err := st.ListAlerts(100)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	kinds := map[string]int{}
	for _, a := range alerts {
		kinds[a.Kind]++
	}
	return kinds
}

// --- traffic thresholds (§15) ---

func setNetwork(t *testing.T, st *store.Store, nodeID string, quotaBytes *int64, rx, tx int64) {
	t.Helper()
	nextReset := time.Now().AddDate(0, 1, 0).Unix()
	err := st.UpsertNodeNetwork(&store.NodeNetwork{
		NodeID:      nodeID,
		Iface:       "eth0",
		Mode:        "both",
		QuotaBytes:  quotaBytes,
		CycleType:   "month",
		NextResetAt: &nextReset,
	})
	if err != nil {
		t.Fatalf("upsert node_network: %v", err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if err := st.AddTrafficDaily(nodeID, today, rx, tx); err != nil {
		t.Fatalf("add traffic: %v", err)
	}
}

func TestCheckTrafficWarnThenCritWithDedupe(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")
	quota := int64(1000)
	setNetwork(t, st, "n1", &quota, 900, 50) // 950/1000 = 95%

	s := New(st, testLogger(), "", 7)
	s.checkTraffic()
	kinds := alertKinds(t, st)
	if kinds[AlertTrafficWarn] != 1 || kinds[AlertTrafficCrit] != 0 {
		t.Fatalf("after 95%%: kinds = %v, want 1 warn / 0 crit", kinds)
	}

	// second pass within the dedupe window must not add another warn
	s.checkTraffic()
	if kinds := alertKinds(t, st); kinds[AlertTrafficWarn] != 1 {
		t.Fatalf("dedupe failed: %v", kinds)
	}

	// cross 100%: warn stays deduped, crit is created
	if err := st.AddTrafficDaily("n1", time.Now().UTC().Format("2006-01-02"), 300, 0); err != nil {
		t.Fatalf("add traffic: %v", err)
	}
	s.checkTraffic()
	kinds = alertKinds(t, st)
	if kinds[AlertTrafficWarn] != 1 || kinds[AlertTrafficCrit] != 1 {
		t.Fatalf("after 125%%: kinds = %v, want 1 warn / 1 crit", kinds)
	}

	// payload carries mode/used/quota/pct
	alerts, err := st.ListAlerts(10)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Mode       string  `json:"mode"`
		UsedBytes  int64   `json:"used_bytes"`
		QuotaBytes int64   `json:"quota_bytes"`
		Pct        float64 `json:"pct"`
	}
	for _, a := range alerts {
		if a.Kind != AlertTrafficWarn {
			continue
		}
		if err := json.Unmarshal([]byte(a.Payload), &payload); err != nil {
			t.Fatalf("decode payload %q: %v", a.Payload, err)
		}
	}
	if payload.Mode != "both" || payload.UsedBytes != 950 || payload.QuotaBytes != 1000 || payload.Pct != 95 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestCheckTrafficCustomThresholds(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")
	quota := int64(1000)
	setNetwork(t, st, "n1", &quota, 500, 50) // 55%
	if err := st.SetSetting("alert.traffic_warn_pct", "50", false); err != nil {
		t.Fatal(err)
	}

	s := New(st, testLogger(), "", 7)
	s.checkTraffic()
	if kinds := alertKinds(t, st); kinds[AlertTrafficWarn] != 1 {
		t.Fatalf("custom 50%% threshold: kinds = %v", kinds)
	}
}

func TestCheckTrafficSkipsNoQuotaAndNoNetwork(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "no-network-row")
	addNode(t, st, "n2", "no-quota")

	// n2 has a network row but quota_bytes is NULL → §8.3: no percent
	if err := st.UpsertNodeNetwork(&store.NodeNetwork{NodeID: "n2", Mode: "both", CycleType: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTrafficDaily("n2", time.Now().UTC().Format("2006-01-02"), 100000, 100000); err != nil {
		t.Fatal(err)
	}

	s := New(st, testLogger(), "", 7)
	s.checkTraffic()
	if kinds := alertKinds(t, st); len(kinds) != 0 {
		t.Fatalf("kinds = %v, want none", kinds)
	}
}

// --- billing reminders (§15) ---

func setDue(t *testing.T, st *store.Store, nodeID string, due time.Time) {
	t.Helper()
	err := st.UpsertNodeBilling(&store.NodeBilling{NodeID: nodeID, CycleType: "monthly", NextDueAt: &[]int64{due.Unix()}[0]})
	if err != nil {
		t.Fatalf("upsert billing: %v", err)
	}
}

func TestCheckBillingDueWindows(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	cases := []struct {
		node     string
		offset   time.Duration
		wantKind string
		wantDays int64
	}{
		{"far", 8 * 24 * time.Hour, "", 0},
		{"d7", 7*24*time.Hour - 2*time.Hour, AlertBillingDue, 7},
		{"d3", 3*24*time.Hour - 2*time.Hour, AlertBillingDue, 3},
		{"d1", 24*time.Hour - 2*time.Hour, AlertBillingDue, 1},
		{"mid", 5 * 24 * time.Hour, "", 0},
		{"over", -2 * time.Hour, AlertBillingOver, 0},
	}
	for _, c := range cases {
		addNode(t, st, c.node, c.node)
		setDue(t, st, c.node, now.Add(c.offset))
	}

	s := New(st, testLogger(), "", 7)
	s.checkBillingDue()

	alerts, err := st.ListAlerts(100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Alert{}
	for _, a := range alerts {
		got[a.NodeID] = a
	}
	for _, c := range cases {
		a, ok := got[c.node]
		if c.wantKind == "" {
			if ok {
				t.Fatalf("%s: unexpected alert %s", c.node, a.Kind)
			}
			continue
		}
		if !ok || a.Kind != c.wantKind {
			t.Fatalf("%s: kind = %q (found=%v), want %q", c.node, a.Kind, ok, c.wantKind)
		}
		var p struct {
			Days  int64 `json:"days"`
			DueAt int64 `json:"due_at"`
		}
		if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
			t.Fatalf("payload %q: %v", a.Payload, err)
		}
		if p.Days != c.wantDays {
			t.Fatalf("%s: days = %d, want %d", c.node, p.Days, c.wantDays)
		}
	}
}

func TestCheckBillingStageDedupeAndProgression(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")
	now := time.Now()
	setDue(t, st, "n1", now.Add(7*24*time.Hour-2*time.Hour))

	s := New(st, testLogger(), "", 7)
	s.checkBillingDue()
	s.checkBillingDue() // same stage twice on the next 6h pass
	if kinds := alertKinds(t, st); kinds[AlertBillingDue] != 1 {
		t.Fatalf("stage dedupe failed: %v", kinds)
	}

	// countdown reaches the 3-day stage: the 7-day alert closes, a new one opens
	setDue(t, st, "n1", now.Add(3*24*time.Hour-2*time.Hour))
	s.checkBillingDue()
	alerts, err := st.ListAlerts(100)
	if err != nil {
		t.Fatal(err)
	}
	openDays := map[int64]int{}
	for _, a := range alerts {
		if a.Kind != AlertBillingDue {
			t.Fatalf("unexpected kind %s", a.Kind)
		}
		if a.RecoveredAt == nil {
			var p struct {
				Days int64 `json:"days"`
			}
			if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
				t.Fatal(err)
			}
			openDays[p.Days]++
		}
	}
	if len(openDays) != 1 || openDays[3] != 1 {
		t.Fatalf("open billing alerts = %v, want exactly one stage 3", openDays)
	}

	// renewal: due pushed far out → the open reminder resolves
	setDue(t, st, "n1", now.Add(31*24*time.Hour))
	s.checkBillingDue()
	open, err := st.OpenAlert(AlertBillingDue, "n1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("OpenAlert after renewal = %v (%v), want ErrNotFound", open, err)
	}
}

// --- delivery (§15) ---

func TestDeliverAlertsMarksDelivered(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")
	fake := &fakeNotifier{name: "telegram", configured: true}
	s := New(st, testLogger(), "", 7, fake)

	if _, _, err := st.CreateAlert("node_offline", "n1", `{"reason":"90s"}`, 3600); err != nil {
		t.Fatal(err)
	}
	s.deliverAlerts()

	if len(fake.events) != 1 {
		t.Fatalf("events = %d, want 1", len(fake.events))
	}
	ev := fake.events[0]
	if ev.Kind != "node_offline" || ev.NodeID != "n1" || ev.NodeName != "edge-1" ||
		ev.Event != notify.EventAlert || ev.Payload != `{"reason":"90s"}` {
		t.Fatalf("event = %+v", ev)
	}
	if undelivered, _ := st.UndeliveredAlerts(); len(undelivered) != 0 {
		t.Fatalf("alert not marked delivered: %+v", undelivered)
	}
}

func TestDeliverAlertsSendsRecoveryForRecoveredAlerts(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")
	fake := &fakeNotifier{name: "telegram", configured: true}
	s := New(st, testLogger(), "", 7, fake)

	if _, _, err := st.CreateAlert("node_offline", "n1", "{}", 3600); err != nil {
		t.Fatal(err)
	}
	// the hub recovers the alert when the node comes back — before any delivery
	if err := st.RecoverAlert("node_offline", "n1"); err != nil {
		t.Fatal(err)
	}
	s.deliverAlerts()

	if len(fake.events) != 1 || fake.events[0].Event != notify.EventRecovery {
		t.Fatalf("events = %+v, want one recovery", fake.events)
	}
	// delivered: the recovery is not re-sent on later passes
	s.deliverAlerts()
	if len(fake.events) != 1 {
		t.Fatalf("recovery re-delivered: %d events", len(fake.events))
	}
}

func TestDeliverAlertsRetriesAfterFailure(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")
	fake := &fakeNotifier{name: "telegram", configured: true, err: errors.New("boom")}
	s := New(st, testLogger(), "", 7, fake)

	if _, _, err := st.CreateAlert("node_offline", "n1", "{}", 3600); err != nil {
		t.Fatal(err)
	}
	s.deliverAlerts()
	if undelivered, _ := st.UndeliveredAlerts(); len(undelivered) != 1 {
		t.Fatalf("failed delivery must stay undelivered, got %+v", undelivered)
	}

	fake.err = nil
	s.deliverAlerts()
	if undelivered, _ := st.UndeliveredAlerts(); len(undelivered) != 0 {
		t.Fatalf("retry should deliver, got %+v", undelivered)
	}
}

func TestDeliverAlertsSkippedWithoutConfiguredChannels(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")
	fake := &fakeNotifier{name: "telegram", configured: false}
	s := New(st, testLogger(), "", 7, fake)

	if _, _, err := st.CreateAlert("node_offline", "n1", "{}", 3600); err != nil {
		t.Fatal(err)
	}
	s.deliverAlerts()
	if undelivered, _ := st.UndeliveredAlerts(); len(undelivered) != 1 {
		t.Fatalf("unconfigured channels must not mark delivered, got %+v", undelivered)
	}
	if len(fake.events) != 0 {
		t.Fatalf("unconfigured channel delivered: %+v", fake.events)
	}
}

// End-to-end through the real Webhook notifier: HMAC signature + JSON body.
func TestDeliverAlertsThroughSignedWebhook(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "edge-1")

	type payload struct {
		Kind      string          `json:"kind"`
		NodeID    string          `json:"node_id"`
		Payload   json.RawMessage `json:"payload"`
		CreatedAt int64           `json:"created_at"`
		Event     string          `json:"event"`
	}
	var (
		got    []payload
		sigs   []string
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigs = append(sigs, r.Header.Get("X-Fobe-Signature"))
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("webhook body %s: %v", body, err)
		}
		got = append(got, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hook := notify.NewWebhook(decoder(map[string]string{
		notify.KeyWebhookURL:    srv.URL,
		notify.KeyWebhookSecret: "s3cret",
	}), srv.Client())
	s := New(st, testLogger(), "", 7, hook)

	if _, _, err := st.CreateAlert("counter_reset", "n1", `{"iface":"eth0"}`, 3600); err != nil {
		t.Fatal(err)
	}
	s.deliverAlerts()

	if len(got) != 1 {
		t.Fatalf("webhook deliveries = %d, want 1", len(got))
	}
	if got[0].Kind != "counter_reset" || got[0].NodeID != "n1" || got[0].Event != notify.EventAlert {
		t.Fatalf("payload = %+v", got[0])
	}
	if len(sigs) != 1 || !verifyHMAC(t, bodies[0], sigs[0], "s3cret") {
		t.Fatalf("signature verification failed: %q", sigs)
	}
	if undelivered, _ := st.UndeliveredAlerts(); len(undelivered) != 0 {
		t.Fatalf("not marked delivered: %+v", undelivered)
	}
}

// verifyHMAC recomputes hex(HMAC-SHA256(body, secret)) as a receiver would.
func verifyHMAC(t *testing.T, body []byte, signature, secret string) bool {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return signature == want
}

// The backup job must leave an openable SQLite file behind — the 2026-09-16
// incident had a backup loop that was "enabled" for days without ever firing
// (24h ticker vs. short-lived containers), and nobody would have noticed
// until a restore was needed.
func TestBackupJobProducesOpenableSnapshot(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	New(st, testLogger(), dir, 7).backup()

	snaps, _ := filepath.Glob(filepath.Join(dir, "fobe-*.db"))
	if len(snaps) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snaps))
	}
	snap, err := store.Open(snaps[0])
	if err != nil {
		t.Fatalf("snapshot is not an openable database: %v", err)
	}
	snap.Close()
}

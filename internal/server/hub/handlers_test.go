package hub

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
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

// §8.2 accounting is configuration-independent: a probe whose 网卡/配额 was never
// filled in still accumulates. Bailing out on a missing node_network row left
// traffic_daily empty, which is exactly what the daily-traffic chart reads.
func TestOnTrafficWithoutNodeNetwork(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n5")

	// first report only baselines the counter: no delta, no daily bytes
	h.onTraffic("n5", 1000, &protocol.Traffic{Iface: "eth0", Rx: 5000, Tx: 7000})
	if rows, err := h.store.ListTrafficDaily("n5", "1970-01-01"); err != nil || len(rows) != 0 {
		t.Fatalf("baseline report wrote daily bytes: %+v (%v)", rows, err)
	}

	h.onTraffic("n5", 1060, &protocol.Traffic{Iface: "eth0", Rx: 6000, Tx: 7500})
	rows, err := h.store.ListTrafficDaily("n5", "1970-01-01")
	if err != nil || len(rows) != 1 {
		t.Fatalf("daily rows = %+v (%v), want exactly one bucket", rows, err)
	}
	if rows[0].RxBytes != 1000 || rows[0].TxBytes != 500 {
		t.Fatalf("daily = %+v, want rx 1000 / tx 500", rows[0])
	}
	if c, err := h.store.GetTrafficCounter("n5", "eth0", "rx"); err != nil || c.PeriodUsed != 1000 {
		t.Fatalf("counter = %+v (%v), want 1000 used", c, err)
	}
}

// TestRecordSingboxStateAbsentConfirmsUninstall covers the confirmation half of
// the panel's uninstall (design §9.2 实现修订 2026-09-16): only a report that
// says "no sing-box here and nothing wrong" clears the reported half and the
// pending removal flag. It is what takes the node out of §10 subscriptions
// (they list a node only while it has a port and a pinned certificate) and what
// lets an operator reinstall on the same port.
func TestRecordSingboxStateAbsentConfirmsUninstall(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n9")
	if err := h.store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: "n9", Version: "1.10.0", DesiredUninstall: true, Port: 23456,
		Status: "running", CertPEM: "pem", CertSHA256: "sum", CertNotAfter: 123, ConfigHash: "h",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// a failed removal reports the leftovers *and* the cause: the flag stays,
	// so the manager keeps retrying and the panel keeps explaining why
	h.recordSingboxState("n9", &protocol.SingboxState{
		Version: "1.10.0", Port: 23456, LastError: "uninstall: remove /etc/one-sing: permission denied",
	})
	sb, err := h.store.GetNodeSingbox("n9")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sb.Status != "degraded" || !sb.DesiredUninstall || sb.CertPEM != "pem" {
		t.Fatalf("failed removal must not look uninstalled: %+v", sb)
	}

	// the confirmation: nothing installed, nothing wrong
	h.recordSingboxState("n9", &protocol.SingboxState{Running: false, Version: "", Port: 23456})
	sb, err = h.store.GetNodeSingbox("n9")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sb.Status != "absent" || sb.DesiredUninstall || sb.ConfigHash != "" {
		t.Fatalf("absent report not applied: %+v", sb)
	}
	if sb.CertPEM != "" || sb.CertSHA256 != "" || sb.CertNotAfter != 0 {
		t.Fatalf("certificate not cleared (node stays in subscriptions): %+v", sb)
	}
	if sb.Port != 23456 {
		t.Fatalf("port must survive so a reinstall reuses it: %+v", sb)
	}
}

// TestRecordSingboxStateAbsentNeedsAPendingRemoval pins the gate on the absent
// rule: the probe re-sends its last snapshot every 5 minutes, and an install
// that takes minutes must not be reported as "absent" by the install's own
// stale predecessor snapshot.
func TestRecordSingboxStateAbsentNeedsAPendingRemoval(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n11")
	if err := h.store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: "n11", DesiredVersion: "1.11.5", Status: "installing", Port: 23456, CertPEM: "pem",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.recordSingboxState("n11", &protocol.SingboxState{Running: false, Version: "", Port: 23456})
	sb, err := h.store.GetNodeSingbox("n11")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sb.Status != "installing" || sb.CertPEM != "pem" {
		t.Fatalf("a stale snapshot must not be read as an uninstall confirmation: %+v", sb)
	}
}

// TestBuildDesiredStateDeclaresRemoval pins the declarative carrier: an offline
// probe learns about the operator's uninstall from hello_ack, where a queued
// one-shot command would already have expired (10 min TTL).
func TestBuildDesiredStateDeclaresRemoval(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n10")
	if err := h.store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: "n10", Version: "1.10.0", DesiredUninstall: true, Port: 23456,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	desired := h.buildDesiredState("n10")
	if desired.Singbox == nil || !desired.Singbox.Uninstall || desired.Singbox.Version != "" {
		t.Fatalf("removal not declared: %+v", desired.Singbox)
	}
	// the port rides along only so the probe can hand it back in its absent
	// report (a reinstall then reuses it); it installs nothing on its own
	if desired.Singbox.Port != 23456 {
		t.Fatalf("removal declaration dropped the port: %+v", desired.Singbox)
	}

	// installed again: the declaration goes back to the managed form
	if err := h.store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: "n10", DesiredVersion: "1.11.5", Port: 23456,
	}); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	desired = h.buildDesiredState("n10")
	if desired.Singbox == nil || desired.Singbox.Uninstall || desired.Singbox.Version != "1.11.5" {
		t.Fatalf("install declaration wrong: %+v", desired.Singbox)
	}
}

// --- §14 country judgement (实现修订 2026-09-16b) ------------------------------

// stubCountry answers for the addresses it knows; everything else misses, the
// way a real GeoLite2 database has no entry for LAN ranges.
type stubCountry map[string]string

func (s stubCountry) Country(ip string) (string, bool) {
	code, ok := s[ip]
	return code, ok
}

func nodeCountry(t *testing.T, h *Hub, id string) (primary, country string) {
	t.Helper()
	n, err := h.store.GetNode(id)
	if err != nil {
		t.Fatalf("get node %s: %v", id, err)
	}
	return n.PrimaryIP, n.CountryCode
}

// TestOnHelloDerivesCountry pins the regression this revision fixes: hello used
// to store the primary address without resolving its country, and the re-pin
// guard then treated that address as "already correct" forever — a freshly
// registered probe kept an empty (gray dot) flag for its whole life.
func TestOnHelloDerivesCountry(t *testing.T) {
	h := newTestHub(t)
	h.geo = stubCountry{"203.0.113.7": "JP"}
	mustCreateNode(t, h.store, "n1")

	h.onHello(&Conn{nodeID: "n1"}, &protocol.Hello{IPs: []protocol.IPInfo{
		{IP: "203.0.113.7", Family: 4, Scope: "public", IsPrimary: true},
		{IP: "192.168.1.9", Family: 4, Scope: "private"},
	}})

	if primary, country := nodeCountry(t, h, "n1"); primary != "203.0.113.7" || country != "JP" {
		t.Fatalf("after hello: primary=%q country=%q, want 203.0.113.7/JP", primary, country)
	}

	// the same report again (state frames repeat it): nothing drifts, and the
	// country survives an idempotent re-report
	h.recordIPs("n1", []protocol.IPInfo{
		{IP: "203.0.113.7", Family: 4, Scope: "public", IsPrimary: true},
		{IP: "192.168.1.9", Family: 4, Scope: "private"},
	})
	if primary, country := nodeCountry(t, h, "n1"); primary != "203.0.113.7" || country != "JP" {
		t.Fatalf("after re-report: primary=%q country=%q, want 203.0.113.7/JP", primary, country)
	}
}

// TestRecordIPsCountryFallback covers the rest of the §14 rule: an operator's
// LAN primary pick (the reachable address, and the one they want in the list)
// can never carry a country, so the flag falls back to the node's public
// address; a manual pin outranks both, and a total miss must not wipe what is
// stored.
func TestRecordIPsCountryFallback(t *testing.T) {
	h := newTestHub(t)
	h.geo = stubCountry{"220.184.188.126": "CN", "240e:390:2c7:d0a0::1": "CN"}
	mustCreateNode(t, h.store, "n1")

	lanFirst := []protocol.IPInfo{
		{IP: "220.184.188.126", Family: 4, Scope: "public"},
		{IP: "240e:390:2c7:d0a0::1", Family: 6, Scope: "public"},
		{IP: "192.168.123.2", Family: 4, Scope: "private", IsPrimary: true},
	}
	h.recordIPs("n1", lanFirst)
	if primary, country := nodeCountry(t, h, "n1"); primary != "192.168.123.2" || country != "CN" {
		t.Fatalf("fallback: primary=%q country=%q, want 192.168.123.2/CN", primary, country)
	}

	// the operator pins a flag: re-reports must not touch it
	if err := h.store.SetNodeCountry("n1", "HK", true); err != nil {
		t.Fatalf("pin country: %v", err)
	}
	h.recordIPs("n1", lanFirst)
	if _, country := nodeCountry(t, h, "n1"); country != "HK" {
		t.Fatalf("pinned country overwritten: %q, want HK", country)
	}

	// a manual primary re-point re-derives at once (the panel calls this)
	if err := h.store.SetNodeCountry("n1", "HK", false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if err := h.store.SetManualPrimary("n1", "220.184.188.126"); err != nil {
		t.Fatalf("manual primary: %v", err)
	}
	h.RefreshCountry("n1")
	if primary, country := nodeCountry(t, h, "n1"); primary != "220.184.188.126" || country != "CN" {
		t.Fatalf("after manual re-point: primary=%q country=%q, want 220.184.188.126/CN", primary, country)
	}

	// nothing resolves any more: the primary follows the report to the only
	// address left, and the stored code survives (a missing database must never
	// blank the flag)
	h.geo = stubCountry{}
	h.recordIPs("n1", []protocol.IPInfo{{IP: "192.168.123.2", Family: 4, Scope: "private", IsPrimary: true}})
	if primary, country := nodeCountry(t, h, "n1"); primary != "192.168.123.2" || country != "CN" {
		t.Fatalf("miss wiped state: primary=%q country=%q, want 192.168.123.2/CN", primary, country)
	}
}

// --- §9.3: local listener discovery -------------------------------------------

// hubLocalConfig is one-sing.sh's shape: an anytls listener plus a VLESS one.
const hubLocalConfig = `{"inbounds":[
  {"type":"anytls","tag":"anytls-in-28711","listen_port":28711,"users":[{"password":"script-pw"}]},
  {"type":"vless","tag":"vless-in-16929","listen_port":16929,"users":[{"uuid":"b2f0a2f4-1111-2222-3333-444455556666"}]}
]}`

// A local report records all listeners without inventing a primary node port or
// creating desired state. Each listener is independently persisted for status
// acknowledgement and subscription rendering.
func TestRecordSingboxLocalKeepsListenersEqual(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n1")

	h.recordSingboxLocal("n1", &protocol.SingboxLocal{
		Present: true, ConfigJSON: hubLocalConfig, ConfigSHA256: "cafe",
	})
	sb, err := h.store.GetNodeSingbox("n1")
	if err != nil {
		t.Fatalf("get node singbox: %v", err)
	}
	if sb.Port != 0 {
		t.Fatalf("local report selected primary port %d", sb.Port)
	}
	if d := h.buildDesiredState("n1"); d.Singbox != nil {
		t.Fatalf("local report pushed desired state: %+v", d.Singbox)
	}
	targets, err := h.store.ListSingboxTargets()
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("local report made the node a sing-box target: %+v", targets)
	}
	states, err := h.store.ListNodeSingboxInbounds("n1")
	if err != nil {
		t.Fatalf("list listener states: %v", err)
	}
	if len(states) != 2 || states[0].Port != 16929 || states[1].Port != 28711 {
		t.Fatalf("stored listener states = %+v, want both reported ports", states)
	}
}

// A process-level running report is not sufficient for a listener: only the
// local TCP acknowledgement for that exact port may promote it from pending.
func TestRecordSingboxLocalPromotesOnlyAcknowledgedInbound(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n1")
	if err := h.store.SetNodeSingboxInboundPending("n1", store.NodeSingboxInbound{
		Port: 28711, Type: singbox.ProtoAnytls, Tag: "anytls-in-28711",
	}); err != nil {
		t.Fatalf("seed pending inbound: %v", err)
	}
	h.recordSingboxLocal("n1", &protocol.SingboxLocal{
		Present: true, Running: true, ConfigSHA256: "cafe", ConfigJSON: hubLocalConfig,
	})
	states, err := h.store.ListNodeSingboxInbounds("n1")
	if err != nil {
		t.Fatalf("list states without acknowledgement: %v", err)
	}
	if len(states) == 0 || states[0].Status != "pending" {
		t.Fatalf("global running promoted inbound: %+v", states)
	}

	h.recordSingboxLocal("n1", &protocol.SingboxLocal{
		Present: true, Running: true, ConfigSHA256: "beef", ConfigJSON: hubLocalConfig,
		EffectiveInboundPorts: []int{28711}, InboundChecksKnown: true,
	})
	states, err = h.store.ListNodeSingboxInbounds("n1")
	if err != nil {
		t.Fatalf("list states with acknowledgement: %v", err)
	}
	for _, state := range states {
		if state.Port == 28711 && state.Status != "running" {
			t.Fatalf("acknowledged port not running: %+v", state)
		}
		if state.Port == 16929 && state.Status != "pending" {
			t.Fatalf("unacknowledged port promoted: %+v", state)
		}
	}
}

// A pending removal reads `deleting` until the port disappears from a report —
// reports that still list it (the delete not yet applied, or applied and rolled
// back) must not flip the row back to running. The stale-report cleanup is what
// finally deletes the row. A listener the probe never acknowledged has nothing
// to delete on the probe: its row is dropped outright.
func TestReconcileKeepsDeletingInboundUntilPortVanishes(t *testing.T) {
	h := newTestHub(t)
	mustCreateNode(t, h.store, "n1")
	if err := h.store.SetNodeSingboxInboundPending("n1", store.NodeSingboxInbound{
		Port: 28711, Type: singbox.ProtoAnytls, Tag: "anytls-in-28711",
	}); err != nil {
		t.Fatalf("seed pending inbound: %v", err)
	}
	report := func(cfg string, effective []int) {
		t.Helper()
		h.recordSingboxLocal("n1", &protocol.SingboxLocal{
			Present: true, Running: true, ConfigSHA256: "hash-" + cfg, ConfigJSON: cfg,
			EffectiveInboundPorts: effective, InboundChecksKnown: true,
		})
	}
	report(hubLocalConfig, []int{28711})

	if err := h.store.SetNodeSingboxInboundDeleting("n1", store.NodeSingboxInbound{
		Port: 28711, Type: singbox.ProtoAnytls, Tag: "anytls-in-28711",
	}); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	// A report that still lists the port (the pre-apply cadence, or a rollback)
	// keeps the removal pending instead of resurrecting 运行中.
	report(hubLocalConfig, []int{28711})
	states, err := h.store.ListNodeSingboxInbounds("n1")
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	for _, state := range states {
		if state.Port == 28711 && state.Status != "deleting" {
			t.Fatalf("report resurrected the deleting row: %+v", states)
		}
	}

	// The applied delete: the port stops appearing, and the row goes with it.
	report(`{"inbounds":[
	  {"type":"vless","tag":"vless-in-16929","listen_port":16929,"users":[{"uuid":"b2f0a2f4-1111-2222-3333-444455556666"}]}
	]}`, nil)
	states, err = h.store.ListNodeSingboxInbounds("n1")
	if err != nil {
		t.Fatalf("list states after apply: %v", err)
	}
	for _, state := range states {
		if state.Port == 28711 {
			t.Fatalf("deleting row survived the confirming report: %+v", states)
		}
	}

	// An add still in flight (reported=0) never existed on the probe: deleting
	// it must not leave a `deleting` ghost behind.
	if err := h.store.SetNodeSingboxInboundPending("n1", store.NodeSingboxInbound{
		Port: 12345, Type: singbox.ProtoSocks, Tag: "socks-in-12345",
	}); err != nil {
		t.Fatalf("seed never-reported add: %v", err)
	}
	if err := h.store.SetNodeSingboxInboundDeleting("n1", store.NodeSingboxInbound{
		Port: 12345, Type: singbox.ProtoSocks, Tag: "socks-in-12345",
	}); err != nil {
		t.Fatalf("delete never-reported add: %v", err)
	}
	states, err = h.store.ListNodeSingboxInbounds("n1")
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	for _, state := range states {
		if state.Port == 12345 {
			t.Fatalf("never-reported add left a ghost row: %+v", state)
		}
	}

	// A live listener with no lifecycle row at all still gets one, so its
	// removal is visible as 删除中 from the first report on.
	if err := h.store.SetNodeSingboxInboundDeleting("n1", store.NodeSingboxInbound{
		Port: 16929, Type: singbox.ProtoVLESS, Tag: "vless-in-16929",
	}); err != nil {
		t.Fatalf("delete unrowed listener: %v", err)
	}
	states, err = h.store.ListNodeSingboxInbounds("n1")
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	if len(states) != 1 || states[0].Port != 16929 || states[0].Status != "deleting" {
		t.Fatalf("unrowed listener not marked deleting: %+v", states)
	}
}

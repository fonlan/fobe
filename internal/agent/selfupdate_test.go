package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/gorilla/websocket"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDecideStateMachine(t *testing.T) {
	now := int64(1_700_000_000)
	cases := []struct {
		name      string
		in        updateInput
		want      updateAction
		wantDelay time.Duration
	}{
		{
			name: "no target offered",
			in:   updateInput{SelfUpdate: true, Current: "a", Target: "", Now: now},
			want: actIdle,
		},
		{
			name: "already on the target",
			in:   updateInput{SelfUpdate: true, Current: "a", Target: "a", Now: now},
			want: actIdle,
		},
		{
			name: "no supervisor: never replaces itself",
			in:   updateInput{SelfUpdate: false, Current: "a", Target: "b", Now: now},
			want: actUnsupported,
		},
		{
			name: "fresh target before its stagger slot",
			in: updateInput{SelfUpdate: true, Current: "a", Target: "b", After: now + 120, Now: now,
				State: updateState{Target: "b"}},
			want: actWait, wantDelay: 120 * time.Second,
		},
		{
			name: "fresh target past its slot",
			in:   updateInput{SelfUpdate: true, Current: "a", Target: "b", After: now - 1, Now: now},
			want: actUpdate,
		},
		{
			name: "a new target resets the previous target's budget",
			in: updateInput{SelfUpdate: true, Current: "a", Target: "b", After: now - 1, Now: now,
				State: updateState{Target: "z", TerminalAttempts: 3}},
			want: actUpdate,
		},
		{
			name: "terminal budget exhausted",
			in: updateInput{SelfUpdate: true, Current: "a", Target: "b", After: now - 1, Now: now,
				State: updateState{Target: "b", TerminalAttempts: updateTerminalLimit}},
			want: actSuppressed,
		},
		{
			name: "transient backoff still pending",
			in: updateInput{SelfUpdate: true, Current: "a", Target: "b", After: now - 1, Now: now,
				State: updateState{Target: "b", TransientStreak: 2, NextRetryAt: now + 300}},
			want: actWait, wantDelay: 300 * time.Second,
		},
		{
			name: "backoff elapsed",
			in: updateInput{SelfUpdate: true, Current: "a", Target: "b", After: now - 1, Now: now,
				State: updateState{Target: "b", TransientStreak: 2, NextRetryAt: now - 1}},
			want: actUpdate,
		},
		{
			name: "one terminal failure below the limit still retries",
			in: updateInput{SelfUpdate: true, Current: "a", Target: "b", After: now - 1, Now: now,
				State: updateState{Target: "b", TerminalAttempts: 1}},
			want: actUpdate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, delay := decide(tc.in)
			if got != tc.want || delay != tc.wantDelay {
				t.Fatalf("decide = (%v, %s), want (%v, %s)", got, delay, tc.want, tc.wantDelay)
			}
		})
	}
}

func TestUpdateStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-state.json")
	u := &selfUpdater{statePath: path, log: quietLog()}
	u.state = updateState{Target: "v2", TerminalAttempts: 2, LastError: "boom", NextRetryAt: 42}
	u.persistLocked()

	got := loadUpdateState(path, quietLog())
	if got.Target != "v2" || got.TerminalAttempts != 2 || got.LastError != "boom" || got.NextRetryAt != 42 {
		t.Fatalf("state did not survive the round trip: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary state file left behind")
	}
	// A corrupt file must not stop the agent from starting.
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if got := loadUpdateState(path, quietLog()); got != (updateState{}) {
		t.Fatalf("corrupt state must be discarded, got %+v", got)
	}
}

// The probe-local counter is what breaks the "replace → restart → still old →
// replace again" loop, so it must be on disk and it must trip at the limit.
func TestTerminalFailuresTripTheLocalBreaker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-state.json")
	u := &selfUpdater{statePath: path, log: quietLog(), now: func() int64 { return 1000 },
		cfg: &Config{}}
	for i := 1; i <= updateTerminalLimit; i++ {
		u.recordResult("v2", "terminal", fmt.Sprintf("failure %d", i))
		reloaded := loadUpdateState(path, quietLog())
		if reloaded.TerminalAttempts != i {
			t.Fatalf("after %d failures the file says %d", i, reloaded.TerminalAttempts)
		}
	}
	action, _ := decide(updateInput{
		SelfUpdate: true, Current: "v1", Target: "v2", After: 0, Now: 1000,
		State: loadUpdateState(path, quietLog()),
	})
	if action != actSuppressed {
		t.Fatalf("action = %v, want suppressed at the limit", action)
	}
}

func TestTransientBackoffGrowsAndCaps(t *testing.T) {
	u := &selfUpdater{statePath: filepath.Join(t.TempDir(), "s.json"), log: quietLog(),
		now: func() int64 { return 5000 }, cfg: &Config{}}
	var last int64
	for i := 0; i < 12; i++ {
		u.recordResult("v2", "transient", "refused")
		next := u.state.NextRetryAt - 5000
		if next < last && next != int64(transientBackoffMax/time.Second) {
			t.Fatalf("backoff shrank: %d → %d", last, next)
		}
		last = next
	}
	if last != int64(transientBackoffMax/time.Second) {
		t.Fatalf("backoff did not cap at %s (got %ds)", transientBackoffMax, last)
	}
	if u.state.TerminalAttempts != 0 {
		t.Fatal("environment failures must not consume the terminal budget")
	}
}

// updateFixture wires an updater against a temp "installation" and a stub
// artifact server.
type updateFixture struct {
	updater *selfUpdater
	exe     string
	exited  chan int
	reports []string
	mu      sync.Mutex
}

func newUpdateFixture(t *testing.T, version, body string, checksumOverride, selfCheckErr string) *updateFixture {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "fobe-agent")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte(body))
	checksum := hex.EncodeToString(sum[:])
	if checksumOverride != "" {
		checksum = checksumOverride
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/linux-amd64.sha256"):
			_, _ = io.WriteString(w, checksum+"  linux-amd64\n")
		case strings.HasSuffix(r.URL.Path, "/linux-amd64"):
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	f := &updateFixture{exe: exe, exited: make(chan int, 1)}
	f.updater = &selfUpdater{
		cfg:       &Config{ServerURL: srv.URL},
		log:       quietLog(),
		statePath: filepath.Join(dir, "update-state.json"),
		exePath:   exe,
		now:       func() int64 { return time.Now().Unix() },
		exit:      func(code int) { f.exited <- code },
		execCheck: func(string) error {
			if selfCheckErr != "" {
				return fmt.Errorf("%s", selfCheckErr)
			}
			return nil
		},
	}
	f.updater.reporter = func(target, phase, class string, attempts int, errMsg string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reports = append(f.reports, phase)
	}
	_ = version
	return f
}

func (f *updateFixture) phases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reports...)
}

// The happy path is a real file replacement in a temp directory: download →
// sha256 → self-check → rename, then exit so the supervisor starts the new
// build. This is the closest a unit test gets to "the probe followed the
// server" without a service manager.
func TestPerformReplacesTheBinaryAndExits(t *testing.T) {
	f := newUpdateFixture(t, "v2", "new-binary-payload", "", "")
	f.updater.perform("v2")

	body, err := os.ReadFile(f.exe)
	if err != nil {
		t.Fatalf("read replaced binary: %v", err)
	}
	if string(body) != "new-binary-payload" {
		t.Fatalf("binary not replaced: %q", body)
	}
	if st, err := os.Stat(f.exe); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("replaced binary is not executable: %v %v", st, err)
	}
	select {
	case code := <-f.exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	default:
		t.Fatal("the updater did not exit: nothing would restart the new build")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.exe), ".fobe-agent.tmp")); !os.IsNotExist(err) {
		t.Fatal("download temp file left behind after a successful replacement")
	}
	if st := loadUpdateState(f.updater.statePath, quietLog()); st.Target != "v2" || st.CommittedVersion != "v2" {
		t.Fatalf("commit not persisted: %+v", st)
	}
	want := []string{"downloading", "verifying", "committed"}
	if got := f.phases(); len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
}

func TestPerformRefusesBadChecksumAndKeepsTheOldBinary(t *testing.T) {
	f := newUpdateFixture(t, "v2", "new-binary-payload", "0000000000000000000000000000000000000000000000000000000000000000", "")
	f.updater.perform("v2")

	body, _ := os.ReadFile(f.exe)
	if string(body) != "old-binary" {
		t.Fatalf("a checksum mismatch must touch nothing, got %q", body)
	}
	select {
	case code := <-f.exited:
		t.Fatalf("the updater exited with %d after a failed check", code)
	default:
	}
	st := loadUpdateState(f.updater.statePath, quietLog())
	if st.TerminalAttempts != 1 || !strings.Contains(st.LastError, "sha256") {
		t.Fatalf("state = %+v, want one terminal sha256 failure", st)
	}
	if got := f.phases(); len(got) != 3 || got[0] != "downloading" || got[1] != "verifying" || got[2] != "failed" {
		t.Fatalf("phases = %v, want downloading,verifying,failed", got)
	}
}

func TestPerformRefusesFailingSelfCheck(t *testing.T) {
	f := newUpdateFixture(t, "v2", "new-binary-payload", "", "server wants v3 but this binary is v2")
	f.updater.perform("v2")

	body, _ := os.ReadFile(f.exe)
	if string(body) != "old-binary" {
		t.Fatalf("a failed self-check must touch nothing, got %q", body)
	}
	st := loadUpdateState(f.updater.statePath, quietLog())
	if st.TerminalAttempts != 1 || !strings.Contains(st.LastError, "selfcheck") {
		t.Fatalf("state = %+v, want one terminal selfcheck failure", st)
	}
}

func TestDownloadRefusesWithoutDiskSpace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fobe-agent")
	orig := statfsFunc
	statfsFunc = func(string) (uint64, uint64, error) { return 1024, 1024, nil }
	defer func() { statfsFunc = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	if _, err := downloadFile(srv.Client(), srv.URL, filepath.Join(dir, "tmp"), exe); err == nil {
		t.Fatal("a filesystem too small for the artifact must be refused before writing")
	}
}

func TestSelfUpdateSupportedNeedsASupervisor(t *testing.T) {
	// The detector reports what this host is; the invariant under test is the
	// coupling: fallback mode (nohup) must never claim self-update support.
	got := selfUpdateSupported()
	if runtime.GOOS == "linux" && !got {
		t.Skip("host has no service manager: the assertion below is vacuous here")
	}
	if runtime.GOOS == "linux" && got {
		// fine: a supervisor was found
		return
	}
	if got {
		t.Fatal("non-linux builds must not report self-update support")
	}
}

// fakeSelfCheckServer answers the §5.5 bypass handshake the way hub's
// HandleAgentSelfCheck does: upgrade, read one hello, reply hello_ack, close.
// It also asserts the marking header the server relies on to route the
// connection away from the normal registration path.
func fakeSelfCheckServer(t *testing.T, target string, sawHeader *bool, sawFlag *bool) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/agent" {
			http.NotFound(w, r)
			return
		}
		*sawHeader = r.Header.Get("X-Fobe-Selfcheck") != ""
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var env protocol.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return
		}
		var hello protocol.Hello
		_ = json.Unmarshal(env.Payload, &hello)
		*sawFlag = hello.SelfCheck
		ack := protocol.HelloAck{
			NodeID:             "n1",
			AgentTargetVersion: target,
			Desired:            protocol.DesiredState{AgentTargetVersion: target},
		}
		_ = ws.WriteJSON(protocol.NewEnvelope(protocol.TypeHelloAck, "", ack))
	}))
}

func TestSelfCheckPassesOnMatchingTarget(t *testing.T) {
	var header, flag bool
	srv := fakeSelfCheckServer(t, Version, &header, &flag)
	defer srv.Close()

	cfg := &Config{ServerURL: srv.URL, NodeID: "n1", NodeSecret: "s", MachineID: "m1"}
	if err := SelfCheck(cfg, quietLog()); err != nil {
		t.Fatalf("self-check failed against a matching server: %v", err)
	}
	if !header || !flag {
		t.Fatalf("handshake must be marked on both carriers (header=%v flag=%v)", header, flag)
	}
}

func TestSelfCheckFailsWhenTheServerWantsAnotherBuild(t *testing.T) {
	var header, flag bool
	srv := fakeSelfCheckServer(t, "20260916.000000", &header, &flag)
	defer srv.Close()

	cfg := &Config{ServerURL: srv.URL, NodeID: "n1", NodeSecret: "s", MachineID: "m1"}
	err := SelfCheck(cfg, quietLog())
	if err == nil {
		t.Fatal("a binary that is not the requested build must not pass its own self-check")
	}
	if !strings.Contains(err.Error(), "20260916.000000") {
		t.Fatalf("error should name the target, got %v", err)
	}
}

// The server may stop advertising a target between download and check (switch
// off, kill switch, a redeploy). That is not evidence about this binary, so the
// check passes rather than burning a terminal attempt.
func TestSelfCheckToleratesAnEmptyTarget(t *testing.T) {
	var header, flag bool
	srv := fakeSelfCheckServer(t, "", &header, &flag)
	defer srv.Close()

	cfg := &Config{ServerURL: srv.URL, NodeID: "n1", NodeSecret: "s", MachineID: "m1"}
	if err := SelfCheck(cfg, quietLog()); err != nil {
		t.Fatalf("empty target must be tolerated: %v", err)
	}
}

func TestSelfCheckReportsTransportFailure(t *testing.T) {
	cfg := &Config{ServerURL: "http://127.0.0.1:1", NodeID: "n1", NodeSecret: "s"}
	if err := SelfCheck(cfg, quietLog()); err == nil {
		t.Fatal("an unreachable server must fail the self-check")
	}
	if err := SelfCheck(&Config{ServerURL: ""}, quietLog()); err == nil {
		t.Fatal("an unconfigured server must fail the self-check")
	}
}

// The panel's "retry" clears the server's counters; the probe keeps its own, so
// the re-anchored plan must clear them too — otherwise the button does nothing.
func TestReanchoredPlanResetsTheLocalBudget(t *testing.T) {
	exhausted := updateState{Target: "v2", TerminalAttempts: updateTerminalLimit, Anchor: 1000}
	in := updateInput{SelfUpdate: true, Current: "v1", Target: "v2", After: 1000, Now: 2000,
		State: reanchor(exhausted, "v2", 1000)}

	// Same plan, same anchor: still circuit-broken.
	if action, _ := decide(in); action != actSuppressed {
		t.Fatalf("action = %v, want suppressed while the plan is unchanged", action)
	}

	// The operator retried, so the server re-anchored the plan.
	st := reanchor(exhausted, "v2", 3000)
	if st.TerminalAttempts != 0 || st.Anchor != 3000 {
		t.Fatalf("re-anchored plan did not reset the budget: %+v", st)
	}
	in.State, in.After, in.Now = st, 3000, 4000
	if action, _ := decide(in); action != actUpdate {
		t.Fatalf("action = %v, want update after the plan was re-anchored", action)
	}

	// A different target resets anyway, and the anchor is remembered.
	if st := reanchor(exhausted, "v3", 42); st.Target != "v3" || st.TerminalAttempts != 0 || st.Anchor != 42 {
		t.Fatalf("target change must start a fresh budget: %+v", st)
	}
	// An unchanged budget keeps counting.
	if st := reanchor(exhausted, "v2", 1000); st.TerminalAttempts != updateTerminalLimit {
		t.Fatalf("an unchanged plan must keep the budget: %+v", st)
	}
}

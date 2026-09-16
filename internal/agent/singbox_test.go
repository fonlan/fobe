package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fobe-panel/fobe/internal/agent/service"
	"github.com/fobe-panel/fobe/internal/protocol"
)

func TestParseSingboxVersion(t *testing.T) {
	cases := []struct {
		output string
		want   string
	}{
		{"sing-box version 1.11.5\n\nEnvironment: go1.23.1 linux/amd64\n", "1.11.5"},
		{"sing-box version v1.12.0-alpha.1\nTags: with_gvisor\n", "1.12.0-alpha.1"},
		{"Version: 1.10.0\n", "1.10.0"},
		{"something else entirely\n", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseSingboxVersion(c.output); got != c.want {
			t.Fatalf("parseSingboxVersion(%q) = %q, want %q", c.output, got, c.want)
		}
	}
}

func TestNormalizeAndCompareVersions(t *testing.T) {
	if v := normalizeVersion(" v1.11.5 "); v != "1.11.5" {
		t.Fatalf("normalizeVersion = %q", v)
	}
	if v := normalizeVersion("V1.2.3"); v != "1.2.3" {
		t.Fatalf("normalizeVersion = %q", v)
	}

	cases := []struct {
		a, b string
		want int
	}{
		{"1.11.5", "1.11.5", 0},
		{"v1.11.5", "1.11.5", 0},
		{"1.10.0", "1.9.9", 1},    // numeric, not lexical
		{"1.2", "1.2.0", 0},       // missing part = zero
		{"2.0.0", "1.99.99", 1},   //
		{"1.11.4", "1.11.10", -1}, //
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Fatalf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSingboxKindForcesFallbackWithoutRoot(t *testing.T) {
	original := privileged
	privileged = func() bool { return false }
	t.Cleanup(func() { privileged = original })

	if got := singboxKind(); got != service.KindFallback {
		t.Fatalf("singboxKind() = %s, want fallback for an unprivileged agent", got)
	}
}

func TestRelocateConfigRemapsDefaultLayout(t *testing.T) {
	dir := t.TempDir()
	service.SetWorkDir(dir)
	t.Cleanup(func() { service.SetWorkDir(service.SingboxWorkDir) })

	config := `{"inbounds":[{"tls":{"certificate_path":"/etc/one-sing/cert/cert.crt","key_path":"/etc/one-sing/cert/private.key"}}]}`
	got := relocateConfig(config)
	if strings.Contains(got, service.SingboxWorkDir) {
		t.Fatalf("relocateConfig left the default layout behind: %s", got)
	}
	if !strings.Contains(got, filepath.Join(dir, "cert", "cert.crt")) ||
		!strings.Contains(got, filepath.Join(dir, "cert", "private.key")) {
		t.Fatalf("relocateConfig did not map certificate paths: %s", got)
	}
}

func TestConfigHashMatch(t *testing.T) {
	desired := "{\"inbounds\":[]}"
	if sha256Hex([]byte(desired)) != fmt.Sprintf("%x", sha256.Sum256([]byte(desired))) {
		t.Fatalf("sha256Hex diverges from crypto/sha256")
	}

	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	if err := os.WriteFile(cfgPath, []byte(desired), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	m := singboxActual{}
	m.configHashMatch = sha256Hex(raw) == sha256Hex([]byte(desired))
	if !m.configHashMatch {
		t.Fatalf("identical config must hash-match")
	}
	m.configHashMatch = sha256Hex(raw) == sha256Hex([]byte("{\"other\":true}"))
	if m.configHashMatch {
		t.Fatalf("different config must not hash-match")
	}
}

func TestParseListenPortAndEffectivePort(t *testing.T) {
	cfg := `{"inbounds":[
		{"type":"mixed","listen_port":1111},
		{"type":"anytls","listen_port":23456}
	]}`
	if got := parseListenPort(cfg); got != 1111 {
		t.Fatalf("parseListenPort = %d, want 1111", got)
	}
	// desired port wins over the config
	d := &protocol.SingboxDesired{Port: 34567, ConfigJSON: cfg}
	if got := effectivePort(d); got != 34567 {
		t.Fatalf("effectivePort = %d, want 34567", got)
	}
	// no desired port: fall back to the config
	d.Port = 0
	if got := effectivePort(d); got != 1111 {
		t.Fatalf("effectivePort fallback = %d, want 1111", got)
	}
	// nothing anywhere: 0 (gate ③ then checks process liveness only)
	if got := effectivePort(&protocol.SingboxDesired{}); got != 0 {
		t.Fatalf("effectivePort empty = %d, want 0", got)
	}
}

// httptest mock of the panel /dl endpoint: binary + .sha256 sidecar.
func TestInstallArtifactVerifiesSHA256(t *testing.T) {
	bin := []byte("fake-sing-box-binary\x00\x01\x02")
	sum := sha256.Sum256(bin)
	hexSum := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/singbox/1.11.5/linux-amd64":
			_, _ = w.Write(bin)
		case "/dl/singbox/1.11.5/linux-amd64.sha256":
			fmt.Fprintf(w, "%s  linux-amd64\n", hexSum)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dest := t.TempDir() + "/sing-box"
	if err := installArtifact(srv.Client(), srv.URL+"/dl/singbox/1.11.5/linux-amd64", dest, 1<<20); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(bin) {
		t.Fatalf("installed bytes differ")
	}
	st, err := os.Stat(dest)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("perm = %v err=%v, want 755", st.Mode().Perm(), err)
	}
	// no leftovers
	if _, err := os.Stat(dest + ".download"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind")
	}
}

func TestInstallArtifactRejectsWrongChecksum(t *testing.T) {
	bin := []byte("binary-with-wrong-sidecar")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/singbox/9.9.9/linux-amd64":
			_, _ = w.Write(bin)
		case "/dl/singbox/9.9.9/linux-amd64.sha256":
			fmt.Fprintf(w, "%s", strings.Repeat("0", 64)) // wrong digest
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dest := t.TempDir() + "/sing-box"
	err := installArtifact(srv.Client(), srv.URL+"/dl/singbox/9.9.9/linux-amd64", dest, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("mismatched artifact must not land at dest")
	}
	if _, err := os.Stat(dest + ".download"); !os.IsNotExist(err) {
		t.Fatalf("failed download left temp file behind")
	}
}

func TestInstallArtifactMissingSidecar(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	dest := t.TempDir() + "/sing-box"
	err := installArtifact(srv.Client(), srv.URL+"/dl/singbox/1.0.0/linux-amd64", dest, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want missing sha256", err)
	}
}

func TestInstallArtifactEnforcesSizeCap(t *testing.T) {
	big := make([]byte, 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256(big)
		switch r.URL.Path {
		case "/dl/big":
			_, _ = w.Write(big)
		case "/dl/big.sha256":
			fmt.Fprintf(w, "%s", hex.EncodeToString(sum[:]))
		}
	}))
	defer srv.Close()

	dest := t.TempDir() + "/sing-box"
	err := installArtifact(srv.Client(), srv.URL+"/dl/big", dest, 1024)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want size cap error", err)
	}
}

// The size cap is not a style choice: /dl serves the extracted binary, and
// every shipping sing-box linux-amd64-musl build is far past the 64 MiB the
// cap used to be — which rejected each release *after* downloading it and then
// rolled back, so no install could ever succeed. Guard the floor.
func TestMaxDownloadBytesCoversShippingBinaries(t *testing.T) {
	const largestSeen = 93 << 20 // 1.15.0-alpha.4 = 92,895,232 B; 1.14.1 = 91,891,552 B
	if maxDownloadBytes < largestSeen {
		t.Fatalf("maxDownloadBytes = %d MiB, below a real artifact (%d MiB): every install would roll back",
			maxDownloadBytes>>20, largestSeen>>20)
	}
}

func TestFetchExpectedSHA256Formats(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	cases := map[string]string{
		"/bare":  sum + "\n",
		"/gnu":   sum + "  filename-linux-amd64\n",
		"/upper": strings.ToUpper(sum),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, cases[r.URL.Path])
	}))
	defer srv.Close()

	for path := range cases {
		got, err := fetchExpectedSHA256(srv.Client(), srv.URL+path)
		if err != nil || got != sum {
			t.Fatalf("fetch %s = %q, %v; want %q", path, got, err, sum)
		}
	}
	if _, err := fetchExpectedSHA256(srv.Client(), srv.URL+"/none"); err == nil {
		t.Fatalf("malformed sidecar accepted")
	}
}

func TestSetDesiredEmptyVersionMeansUnmanaged(t *testing.T) {
	m := newSingboxManager(&Config{ServerURL: "http://127.0.0.1:1"}, testLogger())
	m.SetDesired(&protocol.SingboxDesired{Version: "  "})
	select {
	case <-m.kick:
	default:
		t.Fatalf("SetDesired did not nudge convergence")
	}
	// the manager goroutine is not running here; converge() itself returns
	// immediately for unmanaged — invoke it directly
	m.converge()
	if st := m.Snapshot(); st != nil {
		t.Fatalf("unmanaged manager reported state %+v", st)
	}
}

func TestSetStateDeduplicates(t *testing.T) {
	m := newSingboxManager(&Config{}, testLogger())
	st := protocol.SingboxState{Running: true, Version: "1.11.5", Port: 12345}
	m.setState(st)
	select {
	case <-m.changed: // first report notifies
	default:
		t.Fatalf("first state change did not notify")
	}
	m.setState(st) // identical: no second notification
	select {
	case <-m.changed:
		t.Fatalf("identical state must not notify")
	default:
	}
	if got := m.Snapshot(); got == nil || got.Version != "1.11.5" {
		t.Fatalf("snapshot = %+v", got)
	}
	// snapshot is a copy: mutating it must not touch the manager
	got := m.Snapshot()
	got.Version = "x"
	if m.Snapshot().Version != "1.11.5" {
		t.Fatalf("snapshot leaks internal state")
	}
}

func TestBackupAndRestorePrevConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"

	// first install: nothing to back up, no .prev appears
	backupPrevConfig(cfgPath, "{\"v\":2}")
	if _, err := os.Stat(cfgPath + ".prev"); !os.IsNotExist(err) {
		t.Fatalf(".prev created on first install")
	}

	if err := os.WriteFile(cfgPath, []byte("{\"v\":1}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// unchanged content: no pointless backup (OpenWrt flash wear, §5.4)
	backupPrevConfig(cfgPath, "{\"v\":1}")
	if _, err := os.Stat(cfgPath + ".prev"); !os.IsNotExist(err) {
		t.Fatalf(".prev created despite unchanged config")
	}
	// changed content: previous generation preserved
	backupPrevConfig(cfgPath, "{\"v\":2}")
	prev, err := os.ReadFile(cfgPath + ".prev")
	if err != nil || string(prev) != "{\"v\":1}" {
		t.Fatalf("prev = %q, %v; want old config", prev, err)
	}

	// restore swaps .prev back in place
	restoreFile(cfgPath)
	cur, err := os.ReadFile(cfgPath)
	if err != nil || string(cur) != "{\"v\":1}" {
		t.Fatalf("after restore config = %q, %v", cur, err)
	}
	if _, err := os.Stat(cfgPath + ".prev"); !os.IsNotExist(err) {
		t.Fatalf(".prev survived restore")
	}
	// restore without a .prev is a no-op
	restoreFile(cfgPath)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestVerifyWindowTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	m := newSingboxManager(&Config{}, testLogger())
	// processAlive false (no child, no service manager output) → immediate failure
	err := m.verifyWindow("nonexistent-sing-box", 1)
	if err == nil || !strings.Contains(err.Error(), "observation window") {
		t.Fatalf("err = %v, want observation window failure", err)
	}
	// port 0 skips the TCP check but the process check still gates
	m.proc = nil
	start := time.Now()
	err = m.verifyWindow("nonexistent-sing-box", 0)
	if err == nil {
		t.Fatalf("expected failure with dead process")
	}
	if time.Since(start) > observeWindow {
		t.Fatalf("verifyWindow should fail fast on a dead process")
	}
}

// --- §9.2 firewall auto-allow ---

func TestFirewallCandidatesSelection(t *testing.T) {
	cases := []struct {
		name                         string
		hasUfw, hasFirewalld, hasNft bool
		hasIptables                  bool
		want                         []string
	}{
		{"none", false, false, false, false, nil},
		{"ufw", true, false, false, false, []string{"ufw allow 23456/tcp"}},
		{"firewalld", false, true, false, false,
			[]string{"firewall-cmd --add-port=23456/tcp --permanent && firewall-cmd --reload"}},
		{"nft", false, false, true, false,
			[]string{"nft add rule inet filter input tcp dport 23456 accept"}},
		{"iptables", false, false, false, true,
			[]string{"iptables -I INPUT -p tcp --dport 23456 -j ACCEPT"}},
		{"priority order", true, true, true, true, []string{
			"ufw allow 23456/tcp",
			"firewall-cmd --add-port=23456/tcp --permanent && firewall-cmd --reload",
			"nft add rule inet filter input tcp dport 23456 accept",
			"iptables -I INPUT -p tcp --dport 23456 -j ACCEPT",
		}},
	}
	for _, c := range cases {
		got := firewallCandidates(23456, c.hasUfw, c.hasFirewalld, c.hasNft, c.hasIptables)
		hints := []string{}
		for _, a := range got {
			if len(a.cmds) == 0 {
				t.Fatalf("%s: candidate %q has no commands", c.name, a.name)
			}
			hints = append(hints, a.hint)
		}
		if fmt.Sprint(hints) != fmt.Sprint(c.want) {
			t.Fatalf("%s: hints = %v, want %v", c.name, hints, c.want)
		}
	}
}

func TestFirewallCandidateCommands(t *testing.T) {
	cases := []struct {
		name string
		c    fwAttempt
		want []string
	}{
		{
			"ufw", firewallCandidates(1, true, false, false, false)[0],
			[]string{"ufw", "allow", "1/tcp"},
		},
		{
			"nft", firewallCandidates(2, false, false, true, false)[0],
			[]string{"nft", "add", "rule", "inet", "filter", "input", "tcp", "dport", "2", "accept"},
		},
		{
			"iptables", firewallCandidates(3, false, false, false, true)[0],
			[]string{"iptables", "-I", "INPUT", "-p", "tcp", "--dport", "3", "-j", "ACCEPT"},
		},
	}
	for _, c := range cases {
		if len(c.c.cmds) != 1 || fmt.Sprint(c.c.cmds[0]) != fmt.Sprint(c.want) {
			t.Fatalf("%s: cmd = %v, want %v", c.name, c.c.cmds, c.want)
		}
	}
	// firewalld needs --permanent followed by --reload
	fw := firewallCandidates(4, false, true, false, false)[0]
	wantCmds := fmt.Sprint([][]string{
		{"firewall-cmd", "--add-port=4/tcp", "--permanent"},
		{"firewall-cmd", "--reload"},
	})
	if len(fw.cmds) != 2 || fmt.Sprint(fw.cmds) != wantCmds {
		t.Fatalf("firewalld cmds = %v, want %s", fw.cmds, wantCmds)
	}
}

func TestFirewallSelectionWithInjectedProbes(t *testing.T) {
	// OpenWrt-style host: only iptables exists and the rule applies
	var ran []string
	probe := fwProbes{
		lookPath: func(name string) (string, error) {
			if name == "iptables" {
				return "/usr/sbin/iptables", nil
			}
			return "", errors.New("not found")
		},
		fileExists: func(string) bool { return false },
		run: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return nil
		},
	}
	if hint := runFirewallCandidates(5555, probe); hint != "" {
		t.Fatalf("hint = %q, want empty (allowed)", hint)
	}
	want := []string{"iptables -I INPUT -p tcp --dport 5555 -j ACCEPT"}
	if fmt.Sprint(ran) != fmt.Sprint(want) {
		t.Fatalf("ran = %v, want %v", ran, want)
	}

	// nft binary without /etc/nftables.conf is skipped (§9.2 谨慎); with no
	// front-end at all there is nothing to run and nothing to suggest
	probe.lookPath = func(name string) (string, error) {
		if name == "nft" {
			return "/usr/sbin/nft", nil
		}
		return "", errors.New("not found")
	}
	probe.fileExists = func(path string) bool { return path == "/etc/other" }
	probe.run = func(string, ...string) error { return errors.New("must not run") }
	if hint := runFirewallCandidates(5555, probe); hint != "" {
		t.Fatalf("no firewall manager present: hint = %q, want empty", hint)
	}

	// nft with a ruleset present is attempted (then iptables); a total
	// failure suggests the highest-priority candidate's command verbatim
	probe.fileExists = func(path string) bool { return path == nftablesConfPath }
	probe.lookPath = func(name string) (string, error) {
		if name == "nft" || name == "iptables" {
			return "/usr/sbin/" + name, nil
		}
		return "", errors.New("not found")
	}
	runs := 0
	probe.run = func(string, ...string) error { runs++; return errors.New("no such chain") }
	hint := runFirewallCandidates(6666, probe)
	if hint != "nft add rule inet filter input tcp dport 6666 accept" {
		t.Fatalf("hint = %q, want the nft command", hint)
	}
	if runs != 2 {
		t.Fatalf("nft and iptables must both be attempted, got %d runs", runs)
	}

	// first success wins
	probe.run = func(string, ...string) error { return nil }
	if hint := runFirewallCandidates(6666, probe); hint != "" {
		t.Fatalf("hint = %q, want empty (allowed)", hint)
	}
}

func TestMaybeAllowFirewallOncePerPort(t *testing.T) {
	m := newSingboxManager(&Config{}, testLogger())
	calls := []int{}
	m.openFw = func(port int) string {
		calls = append(calls, port)
		return "ufw allow 1234/tcp" // pretend every front-end failed
	}

	m.maybeAllowFirewall(0) // no port: nothing to do
	if len(calls) != 0 {
		t.Fatalf("port 0 must not trigger the pass")
	}

	m.maybeAllowFirewall(1234)
	m.maybeAllowFirewall(1234) // same port: never re-run on convergence
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want a single attempt per port", calls)
	}

	// the failure hint sticks and rides every state report
	d := &protocol.SingboxDesired{Version: "1.0.0", Port: 1234}
	m.report(d, "1.0.0", true, "", certPair{})
	if st := m.Snapshot(); st == nil || st.FirewallHint != "ufw allow 1234/tcp" {
		t.Fatalf("hint not reported: %+v", m.Snapshot())
	}

	m.maybeAllowFirewall(2345) // port change: a new pass runs
	if len(calls) != 2 || calls[1] != 2345 {
		t.Fatalf("calls = %v, want a fresh attempt for 2345", calls)
	}

	// success clears the hint going forward
	m.openFw = func(int) string { return "" }
	m.maybeAllowFirewall(3456)
	m.report(d, "1.0.0", true, "", certPair{})
	if st := m.Snapshot(); st.FirewallHint != "" {
		t.Fatalf("allowed port must clear the hint, got %q", st.FirewallHint)
	}
}

func TestReportCarriesRollbackOnce(t *testing.T) {
	m := newSingboxManager(&Config{}, testLogger())
	d := &protocol.SingboxDesired{Version: "2.0.0", Port: 1}
	m.mu.Lock()
	m.rollbackSeen = true
	m.mu.Unlock()

	m.report(d, "1.9.0", false, "verify: port 1 not reachable (rolled back)", certPair{})
	st := m.Snapshot()
	if st == nil || !st.RollbackHappened || st.LastError == "" {
		t.Fatalf("rollback flag lost: %+v", st)
	}

	// one-shot: the flag drains after a single report (§15 alert dedupes)
	m.report(d, "1.9.0", false, "verify: port 1 not reachable (rolled back)", certPair{})
	if st = m.Snapshot(); st.RollbackHappened {
		t.Fatalf("one-shot flag must drain after one report")
	}
}

// --- §5.4 overlay space gate ---

func TestCheckInstallSpace(t *testing.T) {
	orig := statfsFunc
	defer func() { statfsFunc = orig }()

	statfsFunc = func(string) (uint64, uint64, error) { return 1 << 30, 10_000, nil }
	if err := checkInstallSpace("/usr/local/bin"); err != nil {
		t.Fatalf("plenty of space refused: %v", err)
	}

	statfsFunc = func(string) (uint64, uint64, error) { return 32 << 20, 10_000, nil }
	err := checkInstallSpace("/usr/local/bin")
	if err == nil || !strings.Contains(err.Error(), "insufficient disk space") {
		t.Fatalf("err = %v, want insufficient disk space", err)
	}

	statfsFunc = func(string) (uint64, uint64, error) { return 1 << 30, minInstallFreeInodes - 1, nil }
	if err := checkInstallSpace("/usr/local/bin"); err == nil {
		t.Fatalf("inode starvation must refuse the install")
	}

	// undeterminable space never blocks (fail open)
	statfsFunc = func(string) (uint64, uint64, error) { return 0, 0, errors.New("statfs failed") }
	if err := checkInstallSpace("/usr/local/bin"); err != nil {
		t.Fatalf("statfs failure must not block installs: %v", err)
	}
}

// The gate has to scale with the artifact: the temp download lives next to the
// binary, and an update additionally parks the displaced copy as .prev — so a
// flat 64 MiB let a ~90 MiB artifact start on a volume that could not hold it.
func TestCheckInstallSpaceForScalesWithArtifact(t *testing.T) {
	orig := statfsFunc
	defer func() { statfsFunc = orig }()

	const artifact = 90 << 20
	update := uint64(minInstallFreeBytes) + 2*artifact // replacing: .download + .prev
	fresh := uint64(minInstallFreeBytes) + artifact

	list := func(free uint64) {
		statfsFunc = func(string) (uint64, uint64, error) { return free, 10_000, nil }
	}

	list(update - 1)
	if err := checkInstallSpaceFor("/etc/one-sing", artifact, true); err == nil {
		t.Fatalf("free space below 2×artifact + headroom must refuse an update")
	}
	list(update)
	if err := checkInstallSpaceFor("/etc/one-sing", artifact, true); err != nil {
		t.Fatalf("exactly enough space refused: %v", err)
	}
	// A fresh install keeps no .prev, so it must not be refused for a copy that
	// does not exist.
	list(fresh - 1)
	if err := checkInstallSpaceFor("/etc/one-sing", artifact, false); err == nil {
		t.Fatalf("free space below artifact + headroom must refuse a fresh install")
	}
	list(fresh)
	if err := checkInstallSpaceFor("/etc/one-sing", artifact, false); err != nil {
		t.Fatalf("fresh install refused at the honest threshold: %v", err)
	}
	if fresh >= update {
		t.Fatalf("a fresh install must need less than an update (%d vs %d)", fresh, update)
	}
	// Without a known artifact size the old floor still applies.
	list(uint64(minInstallFreeBytes))
	if err := checkInstallSpaceFor("/etc/one-sing", 0, false); err != nil {
		t.Fatalf("floor check refused at the floor: %v", err)
	}
}

// A periodic "nothing new" report must not erase the reason the panel shows
// (§9.2): the agent keeps reporting the last failure until the node is actually
// healthy, and drops it then.
func TestReportOnlyKeepsFailureReasonOffTarget(t *testing.T) {
	dir := t.TempDir()
	service.SetWorkDir(dir)
	defer service.SetWorkDir(service.SingboxWorkDir)

	m := newSingboxManager(&Config{}, testLogger())
	d := &protocol.SingboxDesired{Version: "1.15.0-alpha.4", Port: 22039}
	m.SetDesired(d)

	const reason = "download 1.15.0-alpha.4: artifact exceeds 67108864 bytes (rolled back)"
	m.report(d, "", false, reason, certPair{})

	m.reportOnly()
	if st := m.Snapshot(); st == nil || st.LastError != reason {
		t.Fatalf("reportOnly dropped the failure reason: %+v", st)
	}

	// The binary is now on the target version but the node is not running — the
	// state a rejected `check` gate leaves behind. The reason must survive: the
	// version matching is not convergence.
	fake := "#!/bin/sh\necho \"sing-box version 1.15.0-alpha.4\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sing-box"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	m.reportOnly()
	if st := m.Snapshot(); st == nil || st.LastError != reason {
		t.Fatalf("on-target-but-dead node lost its reason: %+v", st)
	}

	// Running on the target: healthy, the error is history (the fallback branch
	// asks the process, so the test process stands in for sing-box).
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find self: %v", err)
	}
	m.mu.Lock()
	m.proc = self
	m.mu.Unlock()
	m.reportOnly()
	st := m.Snapshot()
	if st == nil || st.LastError != "" {
		t.Fatalf("healthy report must clear the error: %+v", st)
	}

	// A different target also invalidates the stored reason.
	m.report(d, "", false, reason, certPair{})
	m.SetDesired(&protocol.SingboxDesired{Version: "1.14.1", Port: 22039})
	if m.lastErr != "" {
		t.Fatalf("a new desired version must drop the previous reason, got %q", m.lastErr)
	}
}

// Gate ① must run on the "already on the target version, just not running"
// path too. Skipping it there let a config sing-box refuses to load camp on
// disk forever: the file never changed, so the change path — the only caller of
// `check` — was never entered, and every round reported gate ③'s "process
// exited during observation window" instead of the real reason.
func TestConvergeChecksConfigBeforeStartingExistingVersion(t *testing.T) {
	dir := t.TempDir()
	service.SetWorkDir(dir)
	defer service.SetWorkDir(service.SingboxWorkDir)

	// A fake binary that answers `version` with the desired version and refuses
	// `check` exactly like sing-box 1.15 does for the removed DNS format.
	fake := "#!/bin/sh\ncase \"$1\" in\n" +
		"version) echo \"sing-box version 1.15.0-alpha.4\" ;;\n" +
		"check) echo 'legacy DNS server formats are deprecated' >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "sing-box"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	cfg := []byte(`{"log":{"level":"warn"}}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	m := newSingboxManager(&Config{ServerURL: "http://127.0.0.1:1"}, testLogger())
	d := &protocol.SingboxDesired{Version: "1.15.0-alpha.4", Port: 22039, ConfigJSON: string(cfg)}
	m.SetDesired(d)
	m.converge()

	st := m.Snapshot()
	if st == nil {
		t.Fatal("no state reported")
	}
	if !strings.Contains(st.LastError, "config check") {
		t.Fatalf("last_error = %q, want the gate ① verdict", st.LastError)
	}
	if st.Running {
		t.Fatal("a config that fails check must never be reported as running")
	}
}

// checkConfig reports a missing config instead of running the binary, so gate ①
// is honest on a probe that has none.
func TestCheckConfigMissingFile(t *testing.T) {
	err := checkConfig("/nonexistent/sing-box", filepath.Join(t.TempDir(), "config.json"))
	if err == nil || !strings.Contains(err.Error(), "no config to check") {
		t.Fatalf("err = %v, want no-config error", err)
	}
}

// TestConvergeUninstallsOnDeclaredRemoval covers the probe half of the panel's
// uninstall (design §9.2 实现修订 2026-09-16): the declaration removes the
// layout, reports an "absent" state with no error (that is what the server
// treats as confirmation) and clears the flag; a re-delivered declaration is a
// silent no-op rather than a failure.
func TestConvergeUninstallsOnDeclaredRemoval(t *testing.T) {
	dir := t.TempDir()
	service.SetWorkDir(dir)
	t.Cleanup(func() { service.SetWorkDir(service.SingboxWorkDir) })

	bin, config, certDir := service.SingboxPaths()
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{bin, bin + ".prev", config, filepath.Join(certDir, service.SingboxCertFile)} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := newSingboxManager(&Config{ServerURL: "http://127.0.0.1:1"}, testLogger())
	m.SetDesired(&protocol.SingboxDesired{Uninstall: true, Port: 23456})
	select {
	case <-m.kick:
	default:
		t.Fatalf("SetDesired did not nudge convergence")
	}
	m.converge() // the manager goroutine is not running in this test

	for _, p := range []string{bin, bin + ".prev", config, certDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the uninstall", p)
		}
	}
	st := m.Snapshot()
	if st == nil || st.Running || st.Version != "" || st.LastError != "" {
		t.Fatalf("absent report wrong: %+v", st)
	}
	if st.Port != 23456 {
		t.Fatalf("absent report lost the port: %+v", st)
	}
	if m.desiredUninstall {
		t.Fatalf("removal flag not cleared after a successful uninstall")
	}

	// The declaration is re-delivered on every handshake until the server sees
	// the report: that must stay a silent no-op, not an error (a reported error
	// would keep the node 卸载中 forever).
	m.SetDesired(&protocol.SingboxDesired{Uninstall: true, Port: 23456})
	m.converge()
	if st := m.Snapshot(); st == nil || st.LastError != "" || st.Version != "" {
		t.Fatalf("re-delivered removal produced an error: %+v", st)
	}

	// Installing again cancels a pending removal: the desired state is one
	// thing at a time, and the install declaration is the later word.
	m.SetDesired(&protocol.SingboxDesired{Uninstall: true, Port: 23456})
	m.SetDesired(&protocol.SingboxDesired{Version: "1.11.5", Port: 23456, ConfigJSON: "{}"})
	if m.desiredUninstall {
		t.Fatalf("install did not cancel the pending removal")
	}
}

package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
)

// The fixtures under testdata/forwards are verbatim output of nft 1.0.6
// (Debian 12) captured inside a privileged container holding the layout and
// rules nfpf.sh creates — including the shapes the panel must NOT model
// (source-address match, counter, port range, bare `dnat to <ip>`).

type fakeCall struct {
	key   string
	stdin string
}

type fakeNftEnv struct {
	root     bool
	bins     map[string]bool
	outputs  map[string]string
	errs     map[string]error
	files    map[string]string
	calls    []fakeCall
	writeErr error
}

func newFakeEnv() *fakeNftEnv {
	return &fakeNftEnv{
		root:    true,
		bins:    map[string]bool{"nft": true},
		outputs: map[string]string{},
		errs:    map[string]error{},
		files:   map[string]string{},
	}
}

func (f *fakeNftEnv) env() nftEnv {
	return nftEnv{
		nft: func(_ time.Duration, args []string, stdin string) (string, error) {
			key := "nft " + strings.Join(args, " ")
			f.calls = append(f.calls, fakeCall{key: key, stdin: stdin})
			return f.outputs[key], f.errs[key]
		},
		run: func(name string, _ time.Duration, args ...string) (string, error) {
			key := name + " " + strings.Join(args, " ")
			f.calls = append(f.calls, fakeCall{key: key})
			return f.outputs[key], f.errs[key]
		},
		lookPath: func(name string) (string, error) {
			if f.bins[name] {
				return "/usr/sbin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		isRoot: func() bool { return f.root },
		writeFile: func(path string, data []byte, _ os.FileMode) error {
			if f.writeErr != nil {
				return f.writeErr
			}
			f.files[path] = string(data)
			return nil
		},
		readFile: func(path string) ([]byte, error) {
			v, ok := f.files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(v), nil
		},
	}
}

// lastScript returns the script handed to `nft -f -`.
func (f *fakeNftEnv) lastScript(t *testing.T) string {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].key == "nft -f -" {
			return f.calls[i].stdin
		}
	}
	t.Fatalf("no `nft -f -` call recorded: %+v", f.calls)
	return ""
}

func (f *fakeNftEnv) loadFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(data)
}

// notExist makes the fake answer like nft does for a table/chain that was never
// created: non-zero exit plus that exact message on stderr.
func notExist(f *fakeNftEnv, key string) {
	f.outputs[key] = "Error: No such file or directory\n"
	f.errs[key] = errors.New("exit status 1")
}

func tableFixture(t *testing.T, f *fakeNftEnv) {
	t.Helper()
	f.outputs["nft -j list table ip nat"] = f.loadFixture(t, "testdata/forwards/table-ip-nat.json")
}

func textFixtures(t *testing.T, f *fakeNftEnv) {
	t.Helper()
	f.outputs["nft list table ip nat"] = "table ip nat {\n}\n"
	f.outputs["nft -a list chain ip nat prerouting"] = f.loadFixture(t, "testdata/forwards/chain-prerouting.txt")
	f.outputs["nft -a list chain ip nat postrouting"] = f.loadFixture(t, "testdata/forwards/chain-postrouting.txt")
	f.outputs["nft -j list table ip nat"] = "Error: unrecognized option '-j'\n"
	f.errs["nft -j list table ip nat"] = errors.New("exit status 1")
}

func TestParseForwardsJSON(t *testing.T) {
	data, err := os.ReadFile("testdata/forwards/table-ip-nat.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseForwardsJSON(string(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.layout.ready() || got.layout.Mismatch != "" {
		t.Fatalf("layout = %+v, want fully initialized", got.layout)
	}
	want := []protocol.ForwardRule{
		{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Comment: "web 前端", Handle: 3},
		{Proto: "udp", SrcPort: 5353, DstIP: "10.0.0.2", DstPort: 5353, Handle: 4},
		// saddr match: listed, not editable
		{Proto: "tcp", SrcPort: 9000, DstIP: "10.0.0.9", DstPort: 9000, Handle: 5, ExtraMatch: true},
		// counter statement
		{Proto: "tcp", SrcPort: 7000, DstIP: "10.0.0.5", DstPort: 7000, Handle: 6, ExtraMatch: true},
		// dport range
		{Proto: "tcp", DstIP: "10.0.0.6", DstPort: 8080, Handle: 7, ExtraMatch: true},
		// bare `dnat to <ip>`: port stays 444
		{Proto: "tcp", SrcPort: 444, DstIP: "10.0.0.4", DstPort: 444, Handle: 9},
	}
	if len(got.rules) != len(want) {
		t.Fatalf("rules = %d, want %d: %+v", len(got.rules), len(want), got.rules)
	}
	for i := range want {
		if got.rules[i] != want[i] {
			t.Errorf("rule[%d] = %+v, want %+v", i, got.rules[i], want[i])
		}
	}
	// The `tcp dport 22 counter comment "ssh"` rule is not a forward at all.
	for _, r := range got.rules {
		if r.SrcPort == 22 {
			t.Errorf("non-DNAT rule leaked into the forward list: %+v", r)
		}
	}
	wantMasq := []masqRule{
		{Handle: 10, DstIP: "10.0.0.1", Proto: "tcp", DstPort: 80},
		{Handle: 11, DstIP: "10.0.0.2", Proto: "udp", DstPort: 5353},
		{Handle: 12, DstIP: "10.0.0.9", Proto: "tcp", DstPort: 9000},
	}
	if len(got.masq) != len(wantMasq) {
		t.Fatalf("masq = %+v, want %+v", got.masq, wantMasq)
	}
	for i := range wantMasq {
		if got.masq[i] != wantMasq[i] {
			t.Errorf("masq[%d] = %+v, want %+v", i, got.masq[i], wantMasq[i])
		}
	}
}

func TestParseForwardsText(t *testing.T) {
	pre := readTestdata(t, "testdata/forwards/chain-prerouting.txt")
	post := readTestdata(t, "testdata/forwards/chain-postrouting.txt")
	rules, masq := parseForwardsText(pre, post)

	// The text path recognizes exactly the nfpf.sh line shape as editable; the
	// rest is kept as extra_match with best-effort fields.
	first := rules[0]
	want := protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Comment: "web 前端", Handle: 3}
	if first != want {
		t.Errorf("rule[0] = %+v, want %+v", first, want)
	}
	second := rules[1]
	if second != (protocol.ForwardRule{Proto: "udp", SrcPort: 5353, DstIP: "10.0.0.2", DstPort: 5353, Handle: 4}) {
		t.Errorf("rule[1] = %+v", second)
	}
	if len(rules) != 6 {
		t.Fatalf("rules = %d (%+v), want 6", len(rules), rules)
	}
	for _, r := range rules[2:5] {
		if !r.ExtraMatch {
			t.Errorf("rule %+v should be extra_match on the text path", r)
		}
	}
	bare := rules[5]
	if bare.SrcPort != 444 || bare.DstPort != 444 || bare.Handle != 9 {
		t.Errorf("bare dnat rule = %+v, want src/dst 444 handle 9", bare)
	}
	if len(masq) != 3 || masq[0].Handle != 10 || masq[2].DstIP != "10.0.0.9" {
		t.Errorf("masq = %+v", masq)
	}
}

func readTestdata(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestReadForwardStateUnavailable(t *testing.T) {
	cases := []struct {
		name string
		tune func(*fakeNftEnv)
		want string
	}{
		{"not root", func(f *fakeNftEnv) { f.root = false }, "need_root"},
		{"no nft", func(f *fakeNftEnv) { f.bins["nft"] = false }, "nft_missing"},
		{"no permission", func(f *fakeNftEnv) {
			f.outputs["nft -j list table ip nat"] = "Error: Operation not permitted\n"
			f.errs["nft -j list table ip nat"] = errors.New("exit status 1")
		}, "no_permission"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEnv()
			tc.tune(f)
			got := readForwardState(f.env())
			if got.Supported {
				t.Errorf("Supported = true, want false (%+v)", got)
			}
			if got.Code != tc.want {
				t.Errorf("Code = %q, want %q", got.Code, tc.want)
			}
		})
	}
}

func TestReadForwardStateNotInitialized(t *testing.T) {
	f := newFakeEnv()
	notExist(f, "nft -j list table ip nat")
	got := readForwardState(f.env())
	if !got.Supported || got.Initialized {
		t.Fatalf("state = %+v, want supported but not initialized", got)
	}
	if len(got.Rules) != 0 || got.Code != "" {
		t.Errorf("state = %+v, want no rules and no error code", got)
	}
}

func TestReadForwardStateChainMismatch(t *testing.T) {
	f := newFakeEnv()
	f.outputs["nft -j list table ip nat"] = `{"nftables":[{"table":{"family":"ip","name":"nat"}},
		{"chain":{"family":"ip","table":"nat","name":"prerouting","handle":1}}]}`
	got := readForwardState(f.env())
	if !got.Supported || got.Initialized {
		t.Fatalf("state = %+v", got)
	}
	if got.Code != "chain_mismatch" {
		t.Errorf("Code = %q, want chain_mismatch", got.Code)
	}
}

func TestReadForwardsTextFallback(t *testing.T) {
	// nft builds without JSON: the text path is used and still finds the layout.
	f := newFakeEnv()
	textFixtures(t, f)
	cur, err := readForwards(f.env())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !cur.layout.ready() {
		t.Fatalf("layout = %+v, want ready", cur.layout)
	}
	if len(cur.rules) != 6 || len(cur.masq) != 3 {
		t.Fatalf("rules/masq = %d/%d, want 6/3", len(cur.rules), len(cur.masq))
	}
}

func TestPlanAddCreatesLayout(t *testing.T) {
	f := newFakeEnv()
	notExist(f, "nft -j list table ip nat")
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = "table ip nat {\n}\n"
	f.outputs["systemctl is-enabled nftables"] = "enabled"
	f.files["/proc/sys/net/ipv4/ip_forward"] = "0\n"
	f.files["/etc/sysctl.conf"] = "net.ipv4.ip_forward=1\n"

	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80},
	})
	if res.Error != "" {
		t.Fatalf("apply: %+v", res)
	}
	script := f.lastScript(t)
	for _, want := range []string{
		"add table ip nat",
		`add chain ip nat prerouting { type nat hook prerouting priority -100 ; }`,
		`add chain ip nat postrouting { type nat hook postrouting priority 100 ; }`,
		`add rule ip nat prerouting iifname "eth0" tcp dport 8080 dnat to 10.0.0.1:80`,
		`add rule ip nat postrouting ip daddr 10.0.0.1 tcp dport 80 masquerade`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// nfpf.sh's persistence: the live ruleset goes to /etc/nftables.conf.
	if conf, ok := f.files["/etc/nftables.conf"]; !ok || !strings.HasPrefix(conf, "#!/usr/sbin/nft -f") {
		t.Errorf("config file = %q (ok=%v)", conf, ok)
	}
}

func TestApplyForwardsConflict(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80},
	})
	if res.Error != "conflict" {
		t.Fatalf("res = %+v, want conflict (a rule without iifname collides with the eth0 one)", res)
	}
	for _, c := range f.calls {
		if c.key == "nft -f -" {
			t.Fatalf("conflicting add still ran nft: %q", c.stdin)
		}
	}
	// A different interface is allowed, exactly like nfpf.sh.
	res = applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth1", DstIP: "10.0.0.1", DstPort: 80},
	})
	if res.Error != "" {
		t.Fatalf("distinct iface should be allowed: %+v", res)
	}
}

func TestValidateForward(t *testing.T) {
	base := protocol.ForwardRule{Proto: "tcp", SrcPort: 80, DstIP: "10.0.0.1", DstPort: 80}
	cases := []struct {
		name string
		mut  func(*protocol.ForwardRule)
		want string
	}{
		{"ok", func(*protocol.ForwardRule) {}, ""},
		{"proto", func(r *protocol.ForwardRule) { r.Proto = "sctp" }, "bad_proto"},
		{"src port", func(r *protocol.ForwardRule) { r.SrcPort = 0 }, "bad_port"},
		{"dst port", func(r *protocol.ForwardRule) { r.DstPort = 70000 }, "bad_port"},
		{"ipv6", func(r *protocol.ForwardRule) { r.DstIP = "2001:db8::1" }, "bad_ip"},
		{"garbage ip", func(r *protocol.ForwardRule) { r.DstIP = "10.0.0" }, "bad_ip"},
		{"iface quote", func(r *protocol.ForwardRule) { r.Iface = `eth0" tcp dport 1 dnat to 1.1.1.1:1` }, "bad_iface"},
		{"iface newline", func(r *protocol.ForwardRule) { r.Iface = "eth0\nadd table ip x" }, "bad_iface"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mut(&r)
			if code, _ := validateForward(r); code != tc.want {
				t.Errorf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// Two source ports pointing at the same destination share nothing: nfpf.sh
// writes one masquerade rule per forward, so deleting one must leave the
// other's return path alone.
func TestMasqHandlesToDropSharedDestination(t *testing.T) {
	a := protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80, Handle: 3}
	b := protocol.ForwardRule{Proto: "tcp", SrcPort: 9090, DstIP: "10.0.0.1", DstPort: 80, Handle: 4}
	masq := []masqRule{
		{Handle: 10, DstIP: "10.0.0.1", Proto: "tcp", DstPort: 80},
		{Handle: 11, DstIP: "10.0.0.1", Proto: "tcp", DstPort: 80},
	}
	if got := masqHandlesToDrop([]protocol.ForwardRule{a}, []protocol.ForwardRule{b}, masq); len(got) != 1 || got[0] != 10 {
		t.Errorf("drop = %v, want [10]", got)
	}
	// Both go away: both masquerade rules go too.
	if got := masqHandlesToDrop([]protocol.ForwardRule{a, b}, nil, masq); len(got) != 2 {
		t.Errorf("drop = %v, want both", got)
	}
	// Someone already trimmed them by hand: we must not invent deletions.
	if got := masqHandlesToDrop([]protocol.ForwardRule{a}, []protocol.ForwardRule{b}, nil); len(got) != 0 {
		t.Errorf("drop = %v, want none", got)
	}
}

func TestApplyForwardsDelete(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = "table ip nat {\n}\n"
	f.outputs["systemctl is-enabled nftables"] = "enabled"

	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsDelete,
		Old:    &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Handle: 3},
	})
	if res.Error != "" {
		t.Fatalf("apply: %+v", res)
	}
	script := f.lastScript(t)
	if !strings.Contains(script, "delete rule ip nat prerouting handle 3") {
		t.Errorf("missing dnat deletion:\n%s", script)
	}
	if !strings.Contains(script, "delete rule ip nat postrouting handle 10") {
		t.Errorf("missing masquerade deletion:\n%s", script)
	}
	// The layout exists: a delete must not try to create it.
	if strings.Contains(script, "add table") || strings.Contains(script, "add chain") {
		t.Errorf("delete re-created the layout:\n%s", script)
	}
}

func TestApplyForwardsDeleteStaleHandle(t *testing.T) {
	// A ruleset reload renumbered the handle; the tuple still identifies the
	// rule and must win over the recycled handle.
	f := newFakeEnv()
	tableFixture(t, f)
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = ""
	f.outputs["systemctl is-enabled nftables"] = "enabled"
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsDelete,
		Old:    &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Handle: 999},
	})
	if res.Error != "" {
		t.Fatalf("apply: %+v", res)
	}
	if script := f.lastScript(t); !strings.Contains(script, "handle 3") {
		t.Errorf("stale handle was not resolved by tuple:\n%s", script)
	}
}

func TestApplyForwardsDeleteUnknown(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsDelete,
		Old:    &protocol.ForwardRule{Proto: "tcp", SrcPort: 1234, DstIP: "10.0.0.1", DstPort: 80},
	})
	if res.Error != "not_found" {
		t.Fatalf("res = %+v, want not_found", res)
	}
}

func TestApplyForwardsUpdateIsOneTransaction(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = ""
	f.outputs["systemctl is-enabled nftables"] = "enabled"
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsUpdate,
		Old:    &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Handle: 3},
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.7", DstPort: 8080},
	})
	if res.Error != "" {
		t.Fatalf("apply: %+v", res)
	}
	script := f.lastScript(t)
	if !strings.Contains(script, "delete rule ip nat prerouting handle 3") ||
		!strings.Contains(script, "dnat to 10.0.0.7:8080") {
		t.Errorf("update script = %s", script)
	}
	// Same source port + same interface as the rule being replaced: not a conflict.
	if strings.Contains(script, "9999") {
		t.Errorf("unexpected script %s", script)
	}
}

func TestApplyForwardsUpdatePortTaken(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsUpdate,
		Old:    &protocol.ForwardRule{Proto: "tcp", SrcPort: 9000, DstIP: "10.0.0.9", DstPort: 9000, Handle: 5},
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.7", DstPort: 8080},
	})
	if res.Error != "conflict" {
		t.Fatalf("res = %+v, want conflict", res)
	}
}

func TestApplyForwardsRejectsBadRule(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "not-an-ip", DstPort: 80},
	})
	if res.Error != "bad_ip" {
		t.Fatalf("res = %+v, want bad_ip", res)
	}
}

func TestApplyForwardsPersistWarnings(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = "table ip nat {\n}\n"
	// systemctl exists but the service cannot be enabled: the rule still
	// applies, and the panel gets a warning instead of a failure.
	f.bins["systemctl"] = true
	f.errs["systemctl is-enabled nftables"] = errors.New("exit status 1")
	f.errs["systemctl enable nftables"] = errors.New("exit status 1")
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 1234, DstIP: "10.0.0.1", DstPort: 80},
	})
	if res.Error != "" {
		t.Fatalf("apply: %+v", res)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, "nftables_service_disabled") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want nftables_service_disabled", res.Warnings)
	}
}

func TestEnsureIPForward(t *testing.T) {
	f := newFakeEnv()
	f.files["/proc/sys/net/ipv4/ip_forward"] = "0"
	if w := ensureIPForward(f.env()); w != "" {
		t.Fatalf("warning = %q", w)
	}
	if got := f.files["/proc/sys/net/ipv4/ip_forward"]; strings.TrimSpace(got) != "1" {
		t.Errorf("proc value = %q", got)
	}
	if got := f.files["/etc/sysctl.conf"]; !strings.Contains(got, "net.ipv4.ip_forward=1") {
		t.Errorf("sysctl.conf = %q", got)
	}

	// Already on: nothing is written.
	f2 := newFakeEnv()
	f2.files["/proc/sys/net/ipv4/ip_forward"] = "1\n"
	f2.files["/etc/sysctl.conf"] = "net.ipv4.ip_forward=1\n"
	if w := ensureIPForward(f2.env()); w != "" {
		t.Fatalf("warning = %q", w)
	}
	if got := f2.files["/etc/sysctl.conf"]; got != "net.ipv4.ip_forward=1\n" {
		t.Errorf("sysctl.conf changed: %q", got)
	}
}

// The whole §21 command kind — decode, apply, and the JSON the panel parses —
// is exercised without a live session, so a wire-shape change cannot slip
// through the command loop untested.
func TestRunForwardsCommandWire(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = "table ip nat {\n}\n"
	f.outputs["systemctl is-enabled nftables"] = "enabled"

	payload, err := json.Marshal(protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 1234, DstIP: "10.0.0.1", DstPort: 80},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, code := runForwardsCommand(f.env(), payload)
	if code != "" {
		t.Fatalf("code = %q", code)
	}
	if !out.OK || out.State.Supported != true || len(out.State.Rules) != 6 {
		t.Fatalf("result = %+v", out)
	}
	// The panel parses this exact encoding out of CmdResult.Stdout.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back protocol.ForwardsResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("panel-side decode: %v", err)
	}
	if !back.OK || len(back.State.Rules) != 6 {
		t.Fatalf("round trip = %+v", back)
	}

	// Unusable payloads are reported, never executed.
	if _, code := runForwardsCommand(f.env(), json.RawMessage(`not-json`)); code != "bad_payload" {
		t.Errorf("bad json: code = %q", code)
	}
	if _, code := runForwardsCommand(f.env(), json.RawMessage(`{}`)); code != "bad_payload" {
		t.Errorf("missing action: code = %q", code)
	}
	if res, code := runForwardsCommand(f.env(), json.RawMessage(`{"action":"nonsense"}`)); code != "" || res.Error != "bad_action" {
		t.Errorf("unknown action: code = %q res = %+v", code, res)
	}
}

// A rule without a handle can never be deleted precisely: refuse instead of
// guessing (and never answer "ok" having changed nothing).
func TestPlanDeleteRefusesHandlelessRule(t *testing.T) {
	cur := forwardsRead{rules: []protocol.ForwardRule{
		{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80},
	}}
	if _, code, _ := planDelete(cur, protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80}); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
	// No statements at all must never be reported as a successful edit.
	f := newFakeEnv()
	tableFixture(t, f)
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsDelete,
		Old:    &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80},
	})
	if res.Error != "not_found" {
		t.Fatalf("res = %+v, want not_found", res)
	}
}

// The comment rides on the DNAT rule exactly where nfpf.sh puts it (after
// `dnat to`), verbatim: nft strings have no escape sequences, so the writer must
// not "help" by escaping backslashes.
func TestPlanAddWritesComment(t *testing.T) {
	f := newFakeEnv()
	notExist(f, "nft -j list table ip nat")
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = ""
	f.files["/proc/sys/net/ipv4/ip_forward"] = "1\n"
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule: &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0",
			DstIP: "10.0.0.1", DstPort: 80, Comment: `web 前端\路径`},
	})
	if res.Error != "" {
		t.Fatalf("apply: %+v", res)
	}
	script := f.lastScript(t)
	want := `add rule ip nat prerouting iifname "eth0" tcp dport 8080 dnat to 10.0.0.1:80 comment "web 前端\路径"`
	if !strings.Contains(script, want) {
		t.Fatalf("script missing %q:\n%s", want, script)
	}
	if strings.Contains(script, `\\路径`) {
		t.Errorf("backslash was escaped; nft stores it verbatim:\n%s", script)
	}
}

func TestValidateComment(t *testing.T) {
	cases := []struct {
		comment string
		want    string
	}{
		{"", ""},
		{"web frontend", ""},
		{"中文 & ; { } # 都行", ""},
		{`back\slash`, ""},
		{`quote " inside`, "bad_comment"},
		{"new\nline", "bad_comment"},
		{"tab\there", "bad_comment"},
		{strings.Repeat("x", maxCommentLen), ""},
		{strings.Repeat("x", maxCommentLen+1), "bad_comment"},
	}
	for _, tc := range cases {
		if code, _ := validateComment(tc.comment); code != tc.want {
			t.Errorf("comment %q: code = %q, want %q", tc.comment, code, tc.want)
		}
	}
}

// An edit keeps whatever comment the operator typed, and a rule that carried a
// comment loses it when the new one has none (the whole rule is replaced).
func TestPlanUpdateReplacesComment(t *testing.T) {
	f := newFakeEnv()
	tableFixture(t, f)
	f.outputs["nft -f -"] = ""
	f.outputs["nft list ruleset"] = ""
	f.bins["systemctl"] = true
	f.outputs["systemctl is-enabled nftables"] = "enabled"
	res := applyForwards(f.env(), protocol.ForwardsRequest{
		Action: protocol.ForwardsUpdate,
		Old:    &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Comment: "web 前端", Handle: 3},
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.1", DstPort: 80, Comment: "renamed"},
	})
	if res.Error != "" {
		t.Fatalf("apply: %+v", res)
	}
	script := f.lastScript(t)
	if !strings.Contains(script, `dnat to 10.0.0.1:80 comment "renamed"`) || strings.Contains(script, "web 前端") {
		t.Fatalf("update script:\n%s", script)
	}
}

// The write path verifies its own result: a probe that applies the rule but
// drops the note must say so, because "saved, note empty" with no explanation is
// exactly the failure an operator cannot diagnose from the panel.
func TestVerifyAppliedWarnsAboutDroppedComment(t *testing.T) {
	req := protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80, Comment: "web"},
	}
	same := protocol.ForwardsState{Rules: []protocol.ForwardRule{
		{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80, Comment: "web"},
	}}
	if got := verifyApplied(req, same); len(got) != 0 {
		t.Errorf("warnings = %v, want none", got)
	}
	noComment := protocol.ForwardsState{Rules: []protocol.ForwardRule{
		{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80},
	}}
	if got := verifyApplied(req, noComment); len(got) != 1 || got[0] != "comment_not_applied" {
		t.Errorf("warnings = %v, want comment_not_applied", got)
	}
	missing := protocol.ForwardsState{}
	if got := verifyApplied(req, missing); len(got) != 1 || got[0] != "write_not_applied" {
		t.Errorf("warnings = %v, want write_not_applied", got)
	}
	// A rule without a note has nothing to verify beyond existence.
	plain := protocol.ForwardsRequest{Action: protocol.ForwardsAdd,
		Rule: &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.1", DstPort: 80}}
	if got := verifyApplied(plain, noComment); len(got) != 0 {
		t.Errorf("warnings = %v, want none", got)
	}
	// Deletes are not verified here: absence is the expected outcome.
	del := protocol.ForwardsRequest{Action: protocol.ForwardsDelete, Old: req.Rule}
	if got := verifyApplied(del, missing); len(got) != 0 {
		t.Errorf("warnings = %v, want none for delete", got)
	}
}

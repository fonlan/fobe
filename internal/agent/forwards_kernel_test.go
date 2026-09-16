package agent

// End-to-end proof that the panel's writer and github.com/fonlan/nfpf agree on
// the same ruleset — including that rules nfpf.sh wrote are recognized,
// edited, and deleted without disturbing the ones it still needs.
//
// It talks to the real kernel, so it refuses to run by accident:
//
//	FOBE_NFT_TEST=1 go test ./internal/agent -run TestForwardsNftKernel -v
//
// Run it as root inside a disposable privileged container, never on a host you
// care about (it flushes `table ip nat` on the way in and out):
//
//	docker run --rm --privileged -v "$PWD":/src -w /src golang:1.25 \
//	  sh -c 'apt-get update -qq && apt-get install -y -qq nftables >/dev/null &&
//	         FOBE_NFT_TEST=1 go test ./internal/agent -run TestForwardsNftKernel -v'

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/protocol"
)

func kernelEnv(t *testing.T) nftEnv {
	t.Helper()
	if os.Getenv("FOBE_NFT_TEST") != "1" {
		t.Skip("set FOBE_NFT_TEST=1 to run the kernel-level nftables test")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (and a disposable host): it flushes table ip nat")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	return defaultNftEnv()
}

// nftf runs one nft statement exactly as nfpf.sh's `nft -f <file>` would.
func nftf(t *testing.T, stmts ...string) {
	t.Helper()
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(strings.Join(stmts, "\n") + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nft %v: %v\n%s", stmts, err, out)
	}
}

func resetNatTable(t *testing.T) {
	t.Helper()
	_ = exec.Command("nft", "delete", "table", "ip", "nat").Run()
}

func TestForwardsNftKernel(t *testing.T) {
	env := kernelEnv(t)
	resetNatTable(t)
	t.Cleanup(func() { resetNatTable(t) })

	// 1. A host set up by nfpf.sh: same table, same chains, same rule shape,
	//    including the pair of source ports sharing one destination.
	nftf(t,
		"add table ip nat",
		"add chain ip nat prerouting { type nat hook prerouting priority -100 ; }",
		"add chain ip nat postrouting { type nat hook postrouting priority 100 ; }",
		`add rule ip nat prerouting iifname "eth0" tcp dport 8080 dnat to 10.0.0.1:80 comment "web 前端"`,
		`add rule ip nat postrouting ip daddr 10.0.0.1 tcp dport 80 masquerade`,
		`add rule ip nat prerouting udp dport 5353 dnat to 10.0.0.2:5353`,
		`add rule ip nat postrouting ip daddr 10.0.0.2 udp dport 5353 masquerade`,
	)
	st := readForwardState(env)
	if !st.Supported || !st.Initialized {
		t.Fatalf("state = %+v, want supported + initialized", st)
	}
	if len(st.Rules) != 2 {
		t.Fatalf("rules = %+v, want the two nfpf.sh rules", st.Rules)
	}
	nfpfRule := st.Rules[0]
	if nfpfRule.Proto != "tcp" || nfpfRule.SrcPort != 8080 || nfpfRule.Iface != "eth0" ||
		nfpfRule.DstIP != "10.0.0.1" || nfpfRule.DstPort != 80 || nfpfRule.ExtraMatch {
		t.Fatalf("nfpf rule misread: %+v", nfpfRule)
	}
	// nfpf.sh's comment is part of the rule and has to survive a read.
	if nfpfRule.Comment != "web 前端" {
		t.Fatalf("nfpf comment misread: %q", nfpfRule.Comment)
	}

	// 2. Add through the panel: the identical destination as the nfpf rule,
	//    different source port — allowed, and it gets its own masquerade rule.
	res := applyForwards(env, protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule: &protocol.ForwardRule{Proto: "tcp", SrcPort: 9090, DstIP: "10.0.0.1", DstPort: 80,
			Comment: `panel\note 中文`},
	})
	if res.Error != "" {
		t.Fatalf("add: %+v", res)
	}
	if len(res.State.Rules) != 3 {
		t.Fatalf("after add: %+v", res.State.Rules)
	}
	// The comment round-trips byte for byte (nft stores backslashes verbatim).
	foundComment := false
	for _, r := range res.State.Rules {
		if r.Comment != "" && r.SrcPort != 8080 {
			foundComment = true
			if r.Comment != `panel\note 中文` {
				t.Fatalf("comment round trip: %q", r.Comment)
			}
		}
	}
	if !foundComment {
		t.Fatalf("the comment we wrote was lost: %+v", res.State.Rules)
	}
	// A quote in a comment is unwritable, and must be refused before nft sees it.
	for _, bad := range []string{`a "b"`, "two\nlines"} {
		if got := applyForwards(env, protocol.ForwardsRequest{
			Action: protocol.ForwardsAdd,
			Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 9091, DstIP: "10.0.0.1", DstPort: 80, Comment: bad},
		}); got.Error != "bad_comment" {
			t.Fatalf("comment %q: %+v, want bad_comment", bad, got)
		}
	}

	// 3. The same source port on a *different* interface is legal (nfpf.sh's
	//    rule), while an any-interface duplicate is refused.
	if got := applyForwards(env, protocol.ForwardsRequest{
		Action: protocol.ForwardsAdd,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, DstIP: "10.0.0.5", DstPort: 80},
	}); got.Error != "conflict" {
		t.Fatalf("any-iface duplicate: %+v, want conflict", got)
	}

	// 4. Edit the nfpf.sh rule into a different destination.
	res = applyForwards(env, protocol.ForwardsRequest{
		Action: protocol.ForwardsUpdate,
		Old:    &nfpfRule,
		Rule:   &protocol.ForwardRule{Proto: "tcp", SrcPort: 8080, Iface: "eth0", DstIP: "10.0.0.7", DstPort: 8080},
	})
	if res.Error != "" {
		t.Fatalf("update: %+v", res)
	}
	var edited *protocol.ForwardRule
	for i := range res.State.Rules {
		if res.State.Rules[i].SrcPort == 8080 {
			edited = &res.State.Rules[i]
		}
	}
	if edited == nil || edited.DstIP != "10.0.0.7" || edited.DstPort != 8080 || edited.Iface != "eth0" {
		t.Fatalf("edited rule = %+v", edited)
	}

	// 5. Delete the panel-created 9090 forward. Its masquerade rule goes with
	//    it; the one the surviving 8080 forward needs must stay.
	var added protocol.ForwardRule
	for _, r := range res.State.Rules {
		if r.SrcPort == 9090 {
			added = r
		}
	}
	res = applyForwards(env, protocol.ForwardsRequest{Action: protocol.ForwardsDelete, Old: &added})
	if res.Error != "" {
		t.Fatalf("delete: %+v", res)
	}
	if len(res.State.Rules) != 2 {
		t.Fatalf("after delete: %+v", res.State.Rules)
	}
	out, err := exec.Command("nft", "-a", "list", "chain", "ip", "nat", "postrouting").CombinedOutput()
	if err != nil {
		t.Fatalf("list postrouting: %v\n%s", err, out)
	}
	// The eth0/8080 forward now targets 10.0.0.7:8080, the udp one uses
	// 10.0.0.2:5353: exactly two masquerade rules, no orphans.
	if got := strings.Count(string(out), "masquerade"); got != 2 {
		t.Fatalf("masquerade rules = %d, want 2:\n%s", got, out)
	}
	if !strings.Contains(string(out), "ip daddr 10.0.0.7 tcp dport 8080 masquerade") {
		t.Errorf("the edited forward lost its masquerade rule:\n%s", out)
	}

	// 6. Persistence: nfpf.sh's file gets the live ruleset, header'd but loadable.
	conf, err := os.ReadFile(nftablesConfPath)
	if err != nil {
		t.Fatalf("read %s: %v", nftablesConfPath, err)
	}
	if !strings.HasPrefix(string(conf), "#!/usr/sbin/nft -f") || !strings.Contains(string(conf), "table ip nat") {
		t.Fatalf("%s = %s", nftablesConfPath, conf)
	}
	if out, err := exec.Command("nft", "-c", "-f", nftablesConfPath).CombinedOutput(); err != nil {
		t.Fatalf("the file we wrote is not loadable: %v\n%s", err, out)
	}
}

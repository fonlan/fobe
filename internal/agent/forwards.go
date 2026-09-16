package agent

// nftables port forwarding (design.md §21), wire-compatible with
// github.com/fonlan/nfpf (`nfpf.sh`).
//
// The single source of truth is the probe's own ruleset, never the panel
// database: rules created by nfpf.sh, by hand, or by the panel must all show up
// as the same kind of row and be editable the same way. That is why this file
// only knows nfpf's layout — `table ip nat` with a `prerouting` DNAT chain and a
// `postrouting` masquerade chain — and reads it back with the `nft` binary.
//
// 实测记录（nft 0.9.3 / 1.0.6 / 1.0.9 / 1.1.6 上验证，写在这里省得再踩一遍）：
//
//   - 注释只在 `nft -f <file>`（或 `-f -` 走 stdin）里才写得对。命令行形式
//     `nft add rule ... comment "x"` 会被**调用者的 shell** 吃掉引号，nft 收到的是
//     两个裸词，报 syntax error——踩过一次的坑，别据此以为注释不可用。
//   - nft 的字符串字面量里 `\` 不是转义符：反斜杠原样存储、原样打印；只有 `"`
//     无法表示（没有转义写法），所以注释里的 `"` 必须拒绝而不是转义。
//   - 注释是 rule 对象的字段（`syntax: {"rule": {..., "comment": "x", "expr": [...]}}`），
//     不是 expr 里的一项；且 dnat 之后跟 comment 完全合法（nfpf.sh 的注释功能有效）。
//   - 删除规则用 handle（精确到一条），不用 nfpf.sh 的 `nft flush ruleset` + 重载：
//     后者是全表重建，会瞬时丢掉所有转发，还可能抹掉别的工具运行时状态。
//   - `nft -j`（JSON）自 0.9.0 起可用，解析以它为主；失败才回退文本解析
//     （文本解析只认 nfpf 生成的那一种行形状，其它一律标成 extra_match）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
)

const (
	nftBin       = "nft"
	nftTimeout   = 10 * time.Second
	nftShortTime = 5 * time.Second

	// nfpf.sh's constants, deliberately identical: same table, same chains,
	// same priorities, same config file. A probe managed by both tools has to
	// agree on where the rules live.
	nftFamily       = "ip"
	nftTable        = "nat"
	nftPreChain     = "prerouting"
	nftPostChain    = "postrouting"
	nftPrePriority  = "-100"
	nftPostPriority = "100"
	// nftablesConfPath is declared next to the sing-box firewall pass that
	// already used it (singbox.go); the same file is nfpf.sh's persistence.
	sysctlConfPath = "/etc/sysctl.conf"

	// configHeader is prepended to the ruleset dump written to
	// /etc/nftables.conf. `nft -f` and the nftables service both treat `#` as a
	// comment, so this stays loadable — and it tells the next human (or
	// nfpf.sh, which overwrites the file the same way) where the file came from.
	configHeader = "#!/usr/sbin/nft -f\n" +
		"# Written by fobe (design.md §21) from the live ruleset — do not edit by hand.\n" +
		"# nfpf.sh (github.com/fonlan/nfpf) uses this same file for persistence.\n"

	maxIfaceLen = 32
	// maxCommentLen is nfpf.sh's limit; nft itself has none, but a bound keeps
	// a pasted paragraph out of the ruleset.
	maxCommentLen = 128
)

// nftEnv bundles every side effect the forward manager needs, so the whole file
// is testable without root, nft, or a network namespace.
type nftEnv struct {
	// nft runs the nft binary; stdin (when non-empty) is fed to `nft -f -`.
	nft       func(timeout time.Duration, args []string, stdin string) (string, error)
	run       func(name string, timeout time.Duration, args ...string) (string, error)
	lookPath  func(string) (string, error)
	isRoot    func() bool
	writeFile func(path string, data []byte, perm os.FileMode) error
	readFile  func(string) ([]byte, error)
}

func defaultNftEnv() nftEnv {
	return nftEnv{
		nft: func(timeout time.Duration, args []string, stdin string) (string, error) {
			return runCmdStdin(nftBin, timeout, stdin, args...)
		},
		run:       runCmd,
		lookPath:  exec.LookPath,
		isRoot:    privileged,
		writeFile: writeFileAtomic,
		readFile:  os.ReadFile,
	}
}

// runCmdStdin is runCmd plus an optional stdin, for `nft -f -`.
func runCmdStdin(name string, timeout time.Duration, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- public surface (§21) ---

// readForwardState is the read path: what the probe's ruleset currently holds.
// It never fails — an unusable environment is reported as Supported=false with
// a Code the panel turns into a hint.
func readForwardState(env nftEnv) protocol.ForwardsState {
	st := protocol.ForwardsState{Rules: []protocol.ForwardRule{}}
	if !env.isRoot() {
		st.Code = "need_root"
		return st
	}
	if _, err := env.lookPath(nftBin); err != nil {
		st.Code = "nft_missing"
		return st
	}
	cur, err := readForwards(env)
	if err != nil {
		st.Code, st.Message = classifyNftError(err)
		return st
	}
	st.Supported = true
	st.Initialized = cur.layout.ready()
	if cur.layout.Mismatch != "" {
		st.Code = "chain_mismatch"
		st.Message = cur.layout.Mismatch
	}
	st.Rules = cur.rules
	return st
}

// forwardsApply is the write path: one nft transaction per operator edit, then
// the ruleset is persisted the way nfpf.sh persists it. The returned State is
// always the post-action truth, so callers need no second read.
func applyForwards(env nftEnv, req protocol.ForwardsRequest) protocol.ForwardsResult {
	if req.Action == protocol.ForwardsList {
		return protocol.ForwardsResult{OK: true, State: readForwardState(env)}
	}
	if !env.isRoot() {
		return forwardFail(env, "need_root", "the agent is not running as root")
	}
	if _, err := env.lookPath(nftBin); err != nil {
		return forwardFail(env, "nft_missing", "nft binary not found on the probe")
	}
	cur, err := readForwards(env)
	if err != nil {
		code, msg := classifyNftError(err)
		return forwardFail(env, code, msg)
	}
	if cur.layout.Mismatch != "" {
		return forwardFail(env, "chain_mismatch", cur.layout.Mismatch)
	}

	var plan forwardsPlan
	var code, msg string
	switch req.Action {
	case protocol.ForwardsAdd:
		if req.Rule == nil {
			return forwardFail(env, "bad_request", "missing rule")
		}
		plan, code, msg = planAdd(cur, *req.Rule)
	case protocol.ForwardsUpdate:
		if req.Rule == nil || req.Old == nil {
			return forwardFail(env, "bad_request", "update needs both old and new rule")
		}
		plan, code, msg = planUpdate(cur, *req.Old, *req.Rule)
	case protocol.ForwardsDelete:
		if req.Old == nil {
			return forwardFail(env, "bad_request", "missing rule")
		}
		plan, code, msg = planDelete(cur, *req.Old)
	default:
		return forwardFail(env, "bad_action", "unknown action "+req.Action)
	}
	if code != "" {
		return forwardFail(env, code, msg)
	}
	if len(plan.stmts) == 0 {
		// Should be unreachable (every plan branch emits statements), but a
		// silent no-op that answers "ok" would be the worst possible outcome for
		// a rule the operator believes was just removed.
		return forwardFail(env, "not_found", "nothing to change on the probe")
	}

	var warnings []string
	{
		out, err := env.nft(nftTimeout, []string{"-f", "-"}, plan.script())
		if err != nil {
			return forwardFail(env, "nft_failed", nftErrorText(out, err))
		}
		warnings = append(warnings, plan.persist(env)...)
	}
	if plan.ensureForwarding {
		if w := ensureIPForward(env); w != "" {
			warnings = append(warnings, w)
		}
	}
	st := readForwardState(env)
	warnings = append(warnings, verifyApplied(req, st)...)
	return protocol.ForwardsResult{OK: true, Warnings: warnings, State: st}
}

// verifyApplied compares what the operator asked for with what the probe now
// reports, and turns a mismatch into a warning.
//
// It exists because the failure it catches is otherwise completely silent: a
// probe whose agent predates comment support (or a panel whose backend predates
// the field) applies the rule and drops the note, so the panel would show a
// rule with an empty note and no explanation — the command itself succeeded.
// Warnings are advisory on purpose: the rule *was* written, and pretending the
// whole edit failed would be worse than saying what is missing.
func verifyApplied(req protocol.ForwardsRequest, state protocol.ForwardsState) []string {
	if req.Rule == nil ||
		(req.Action != protocol.ForwardsAdd && req.Action != protocol.ForwardsUpdate) {
		return nil
	}
	for i := range state.Rules {
		if !sameForward(state.Rules[i], *req.Rule) {
			continue
		}
		if req.Rule.Comment != "" && state.Rules[i].Comment != req.Rule.Comment {
			return []string{"comment_not_applied"}
		}
		return nil
	}
	return []string{"write_not_applied"}
}

// runForwardsCommand decodes one §21 command payload and runs it. It is split
// out of the command loop so the whole kind (decode → apply → the JSON the
// panel parses) is testable without a live agent session; a non-empty code
// means the payload itself was unusable.
func runForwardsCommand(env nftEnv, payload json.RawMessage) (protocol.ForwardsResult, string) {
	var req protocol.ForwardsRequest
	if err := json.Unmarshal(payload, &req); err != nil || req.Action == "" {
		return protocol.ForwardsResult{}, "bad_payload"
	}
	return applyForwards(env, req), ""
}

func forwardFail(env nftEnv, code, msg string) protocol.ForwardsResult {
	// A refused edit still carries the current ruleset: a conflict or a
	// vanished handle is usually visible in the list right away.
	return protocol.ForwardsResult{Error: code, Message: msg, State: readForwardState(env)}
}

// --- planning (pure) ---

// forwardsPlan is one nft transaction: optional layout creation, the rules to
// delete by handle, the rules to add. Deleting and adding in a single
// `nft -f -` is what makes an edit atomic — a mid-way failure leaves the old
// rule in place instead of a half-applied pair.
type forwardsPlan struct {
	stmts            []string
	ensureForwarding bool
}

func (p forwardsPlan) script() string { return strings.Join(p.stmts, "\n") + "\n" }

// persist writes the live ruleset to nfpf's config file and makes sure the
// nftables service will load it on boot. Both are best-effort: a failed file
// write is reported as a warning, not as an error, because the rule is already
// active — refusing the whole edit would be worse than losing it at reboot.
func (p forwardsPlan) persist(env nftEnv) []string {
	var warnings []string
	out, err := env.nft(nftTimeout, []string{"list", "ruleset"}, "")
	if err != nil {
		return []string{"config_dump_failed: " + nftErrorText(out, err)}
	}
	if err := env.writeFile(nftablesConfPath, []byte(configHeader+out), 0o644); err != nil {
		warnings = append(warnings, "config_write_failed: "+err.Error())
	}
	if _, err := env.lookPath("systemctl"); err != nil {
		return warnings // OpenWrt/procd has no nftables.service; fw4 owns the ruleset
	}
	if _, err := env.run("systemctl", nftShortTime, "is-enabled", "nftables"); err == nil {
		return warnings
	}
	if out, err := env.run("systemctl", nftTimeout, "enable", "nftables"); err != nil {
		warnings = append(warnings, "nftables_service_disabled: "+nftErrorText(out, err))
	}
	return warnings
}

func planAdd(cur forwardsRead, cand protocol.ForwardRule) (forwardsPlan, string, string) {
	if code, msg := validateForward(cand); code != "" {
		return forwardsPlan{}, code, msg
	}
	if c := conflictWith(cur.rules, cand, 0); c != nil {
		return forwardsPlan{}, "conflict", conflictMessage(c, cand)
	}
	p := forwardsPlan{ensureForwarding: true}
	p.stmts = append(p.stmts, initStmts(cur.layout)...)
	p.stmts = append(p.stmts, preRuleStmt(cand), postRuleStmt(cand))
	return p, "", ""
}

func planUpdate(cur forwardsRead, old, cand protocol.ForwardRule) (forwardsPlan, string, string) {
	if code, msg := validateForward(cand); code != "" {
		return forwardsPlan{}, code, msg
	}
	targets, code, msg := resolveTargets(cur.rules, old)
	if code != "" {
		return forwardsPlan{}, code, msg
	}
	if c := conflictWith(cur.rules, cand, old.Handle); c != nil {
		return forwardsPlan{}, "conflict", conflictMessage(c, cand)
	}
	p := forwardsPlan{ensureForwarding: true}
	p.stmts = append(p.stmts, initStmts(cur.layout)...)
	p.stmts = append(p.stmts, deleteStmts(targets, without(cur.rules, targets), cur.masq)...)
	p.stmts = append(p.stmts, preRuleStmt(cand), postRuleStmt(cand))
	return p, "", ""
}

func planDelete(cur forwardsRead, want protocol.ForwardRule) (forwardsPlan, string, string) {
	targets, code, msg := resolveTargets(cur.rules, want)
	if code != "" {
		return forwardsPlan{}, code, msg
	}
	p := forwardsPlan{}
	p.stmts = append(p.stmts, deleteStmts(targets, without(cur.rules, targets), cur.masq)...)
	return p, "", ""
}

// deleteStmts removes the DNAT rules and the masquerade rules that belonged to
// them. nfpf.sh adds one masquerade rule per forward, so a shared destination
// (two source ports → the same dst) has several; we drop exactly as many as we
// drop forwards — never one a surviving forward still needs.
func deleteStmts(targets, remaining []protocol.ForwardRule, masq []masqRule) []string {
	var stmts []string
	for _, r := range targets {
		if r.Handle != 0 {
			stmts = append(stmts, fmt.Sprintf("delete rule ip %s %s handle %d", nftTable, nftPreChain, r.Handle))
		}
	}
	for _, h := range masqHandlesToDrop(targets, remaining, masq) {
		stmts = append(stmts, fmt.Sprintf("delete rule ip %s %s handle %d", nftTable, nftPostChain, h))
	}
	return stmts
}

// masqHandlesToDrop counts, per destination, how many masquerade rules the
// deleted forwards owned: at most as many as exist, and never more than the
// surplus over what surviving forwards need. A host where someone already
// trimmed masquerade rules by hand therefore loses none of them.
func masqHandlesToDrop(targets, remaining []protocol.ForwardRule, masq []masqRule) []int {
	type destKey struct {
		ip    string
		proto string
		port  int
	}
	key := func(r protocol.ForwardRule) destKey {
		return destKey{ip: r.DstIP, proto: r.Proto, port: r.DstPort}
	}
	need := map[destKey]int{}
	for _, r := range targets {
		need[key(r)]++
	}
	keep := map[destKey]int{}
	for _, r := range remaining {
		keep[key(r)]++
	}
	avail := map[destKey][]int{}
	for _, m := range masq {
		k := destKey{ip: m.DstIP, proto: m.Proto, port: m.DstPort}
		avail[k] = append(avail[k], m.Handle)
	}
	var out []int
	for k, n := range need {
		if spare := len(avail[k]) - keep[k]; n > spare {
			n = spare
		}
		for i := 0; i < n; i++ {
			if avail[k][i] != 0 {
				out = append(out, avail[k][i])
			}
		}
	}
	sort.Ints(out)
	return out
}

// without returns rules minus the given targets (matched by handle, else by
// full tuple), i.e. what will still be on the probe after the edit.
func without(rules, targets []protocol.ForwardRule) []protocol.ForwardRule {
	out := make([]protocol.ForwardRule, 0, len(rules))
	for _, r := range rules {
		drop := false
		for _, t := range targets {
			if t.Handle != 0 && r.Handle == t.Handle {
				drop = true
				break
			}
			if sameForward(r, t) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, r)
		}
	}
	return out
}

// initStmts creates only the pieces of the nfpf layout that are missing, so the
// first add on a fresh host silently becomes the same setup nfpf.sh's
// `init_nftables` would have produced.
func initStmts(layout nftLayout) []string {
	var stmts []string
	if !layout.Table {
		stmts = append(stmts, "add table ip "+nftTable)
	}
	if !layout.Prerouting {
		stmts = append(stmts, "add chain ip "+nftTable+" "+nftPreChain+
			" { type nat hook prerouting priority "+nftPrePriority+" ; }")
	}
	if !layout.Postrouting {
		stmts = append(stmts, "add chain ip "+nftTable+" "+nftPostChain+
			" { type nat hook postrouting priority "+nftPostPriority+" ; }")
	}
	return stmts
}

// resolveTargets picks the DNAT rules an operator edit refers to. The handle
// wins when it still describes the same rule (a ruleset reload renumbers
// handles, and acting on a recycled handle would delete an unrelated forward);
// otherwise the full tuple decides — and an ambiguous tuple is refused instead
// of guessed at.
func resolveTargets(rules []protocol.ForwardRule, want protocol.ForwardRule) ([]protocol.ForwardRule, string, string) {
	if want.Handle != 0 {
		for _, r := range rules {
			if r.Handle == want.Handle && sameForward(r, want) {
				return []protocol.ForwardRule{r}, "", ""
			}
		}
	}
	// Without a handle a rule cannot be deleted precisely: nft deletes by handle,
	// and guessing from the tuple would risk removing a different rule.
	if want.Handle == 0 {
		return nil, "not_found", "no rule handle to act on; refresh the list and retry"
	}
	var matches []protocol.ForwardRule
	for _, r := range rules {
		if sameForward(r, want) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, "not_found", fmt.Sprintf("no %s/%d rule matching %s:%d on the probe",
			want.Proto, want.SrcPort, want.DstIP, want.DstPort)
	case 1:
		if matches[0].Handle == 0 {
			return nil, "not_found", "the matching rule has no nft handle"
		}
		return matches, "", ""
	default:
		return nil, "ambiguous", fmt.Sprintf("%d rules share %s/%d — refresh and retry",
			len(matches), want.Proto, want.SrcPort)
	}
}

func sameForward(a, b protocol.ForwardRule) bool {
	return a.Proto == b.Proto && a.SrcPort == b.SrcPort && a.Iface == b.Iface &&
		a.DstIP == b.DstIP && a.DstPort == b.DstPort
}

// conflictWith reproduces nfpf.sh's duplicate/port-in-use rule, because the
// panel and the script have to agree on what "this port is taken" means:
// a rule collides unless both rules name a *different* interface.
// selfHandle is the rule being replaced (0 for add).
func conflictWith(rules []protocol.ForwardRule, cand protocol.ForwardRule, selfHandle int) *protocol.ForwardRule {
	for i := range rules {
		ex := rules[i]
		if ex.Handle != 0 && ex.Handle == selfHandle {
			continue
		}
		if ex.Proto != cand.Proto || ex.SrcPort != cand.SrcPort {
			continue
		}
		if ex.Iface != "" && cand.Iface != "" && ex.Iface != cand.Iface {
			continue
		}
		return &rules[i]
	}
	return nil
}

func conflictMessage(ex *protocol.ForwardRule, cand protocol.ForwardRule) string {
	if ex.DstIP == cand.DstIP && ex.DstPort == cand.DstPort {
		return fmt.Sprintf("%s/%d already forwards to %s:%d", cand.Proto, cand.SrcPort, cand.DstIP, cand.DstPort)
	}
	return fmt.Sprintf("%s/%d is already used by %s:%d", cand.Proto, cand.SrcPort, ex.DstIP, ex.DstPort)
}

// validateForward mirrors nfpf.sh's validate_* helpers (IPv4 only — the layout
// lives in `table ip`) and tightens the interface name, which ends up inside
// quotes in the generated script.
func validateForward(r protocol.ForwardRule) (string, string) {
	if r.Proto != "tcp" && r.Proto != "udp" {
		return "bad_proto", "protocol must be tcp or udp, got " + r.Proto
	}
	if !validPort(r.SrcPort) {
		return "bad_port", fmt.Sprintf("source port %d out of range", r.SrcPort)
	}
	if !validPort(r.DstPort) {
		return "bad_port", fmt.Sprintf("destination port %d out of range", r.DstPort)
	}
	ip := net.ParseIP(strings.TrimSpace(r.DstIP))
	if ip == nil || ip.To4() == nil {
		return "bad_ip", "destination must be an IPv4 address, got " + r.DstIP
	}
	if r.Iface != "" && !ifaceRE.MatchString(r.Iface) {
		return "bad_iface", "invalid interface name " + r.Iface
	}
	if code, msg := validateComment(r.Comment); code != "" {
		return code, msg
	}
	return "", ""
}

// validateComment mirrors nfpf.sh's validate_comment and adds the one thing nft
// cannot express: a double quote. nft string literals have no escape sequence
// (a backslash is just a character), so a comment containing a quote is
// unparseable — refusing it beats writing a rule nft will reject midway.
func validateComment(c string) (string, string) {
	if c == "" {
		return "", ""
	}
	if len([]rune(c)) > maxCommentLen {
		return "bad_comment", fmt.Sprintf("comment longer than %d characters", maxCommentLen)
	}
	if strings.ContainsRune(c, '"') {
		return "bad_comment", "comment cannot contain a double quote"
	}
	for _, r := range c {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return "bad_comment", "comment cannot contain control characters"
		}
	}
	return "", ""
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// ifaceRE is deliberately stricter than the kernel's: the name is interpolated
// into an nft script, so anything that could close the quotes or span lines is
// refused rather than escaped.
var ifaceRE = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,` + strconv.Itoa(maxIfaceLen) + `}$`)

func preRuleStmt(r protocol.ForwardRule) string {
	var b strings.Builder
	b.WriteString("add rule ip " + nftTable + " " + nftPreChain + " ")
	if r.Iface != "" {
		b.WriteString(`iifname "` + r.Iface + `" `)
	}
	b.WriteString(r.Proto + " dport " + strconv.Itoa(r.SrcPort) +
		" dnat to " + r.DstIP + ":" + strconv.Itoa(r.DstPort))
	if r.Comment != "" {
		// Same position nfpf.sh uses (after `dnat to`), and safe because nft
		// only ever sees this text through `nft -f -`, never through a shell.
		b.WriteString(` comment "` + r.Comment + `"`)
	}
	return b.String()
}

func postRuleStmt(r protocol.ForwardRule) string {
	return "add rule ip " + nftTable + " " + nftPostChain + " ip daddr " + r.DstIP +
		" " + r.Proto + " dport " + strconv.Itoa(r.DstPort) + " masquerade"
}

// --- reading the ruleset ---

// forwardsRead is everything one read of the nfpf layout yields: the DNAT rules
// the panel shows, the masquerade rules an edit has to clean up alongside them,
// and which pieces of the layout exist.
type forwardsRead struct {
	rules  []protocol.ForwardRule
	masq   []masqRule
	layout nftLayout
}

// nftLayout records which pieces of the nfpf layout exist, so an add can create
// exactly what is missing, and a pre-existing chain of the wrong type is
// reported instead of silently accepting rules that will never match.
type nftLayout struct {
	Table       bool
	Prerouting  bool
	Postrouting bool
	Mismatch    string
}

func (l nftLayout) ready() bool {
	return l.Table && l.Prerouting && l.Postrouting && l.Mismatch == ""
}

func readForwards(env nftEnv) (forwardsRead, error) {
	out, err := env.nft(nftTimeout, []string{"-j", "list", "table", nftFamily, nftTable}, "")
	if err == nil {
		return parseForwardsJSON(out)
	}
	if isNotExist(out) {
		return emptyForwardsRead(), nil // nothing created yet
	}
	if strings.Contains(out, "Operation not permitted") {
		// Not a JSON problem: the text path needs the same capability and would
		// only produce a second, less specific failure.
		return forwardsRead{}, fmt.Errorf("%s: %w", strings.TrimSpace(out), err)
	}
	// Older nft builds (or a libnftables without JSON) land here. The text
	// path is nfpf.sh's own parsing problem, so it is the compatible fallback.
	return readForwardsText(env)
}

func emptyForwardsRead() forwardsRead {
	return forwardsRead{rules: []protocol.ForwardRule{}}
}

func readForwardsText(env nftEnv) (forwardsRead, error) {
	var out forwardsRead

	table, err := env.nft(nftTimeout, []string{"list", "table", nftFamily, nftTable}, "")
	if err != nil {
		if isNotExist(table) {
			return emptyForwardsRead(), nil
		}
		return out, fmt.Errorf("%s: %w", strings.TrimSpace(table), err)
	}
	out.layout.Table = true

	pre, err := env.nft(nftTimeout, []string{"-a", "list", "chain", nftFamily, nftTable, nftPreChain}, "")
	if err != nil {
		if !isNotExist(pre) {
			return out, fmt.Errorf("%s: %w", strings.TrimSpace(pre), err)
		}
	} else if strings.Contains(pre, "chain "+nftPreChain) {
		out.layout.Prerouting = true
		if !strings.Contains(pre, "type nat hook prerouting") {
			out.layout.Mismatch = "chain " + nftPreChain + " exists but is not a nat/prerouting base chain"
		}
	}
	post, err := env.nft(nftTimeout, []string{"-a", "list", "chain", nftFamily, nftTable, nftPostChain}, "")
	if err != nil {
		if !isNotExist(post) {
			return out, fmt.Errorf("%s: %w", strings.TrimSpace(post), err)
		}
	} else if strings.Contains(post, "chain "+nftPostChain) {
		out.layout.Postrouting = true
		if !strings.Contains(post, "type nat hook postrouting") {
			out.layout.Mismatch = "chain " + nftPostChain + " exists but is not a nat/postrouting base chain"
		}
	}
	out.rules, out.masq = parseForwardsText(pre, post)
	return out, nil
}

// --- JSON parsing ---

type nftDoc struct {
	Nftables []struct {
		Table *struct {
			Family string `json:"family"`
			Name   string `json:"name"`
		} `json:"table"`
		Chain *struct {
			Family string `json:"family"`
			Table  string `json:"table"`
			Name   string `json:"name"`
			Type   string `json:"type"`
			Hook   string `json:"hook"`
		} `json:"chain"`
		Rule *struct {
			Family  string            `json:"family"`
			Table   string            `json:"table"`
			Chain   string            `json:"chain"`
			Handle  int               `json:"handle"`
			Comment string            `json:"comment"`
			Expr    []json.RawMessage `json:"expr"`
		} `json:"rule"`
	} `json:"nftables"`
}

type nftExpr struct {
	Match *struct {
		Left  json.RawMessage `json:"left"`
		Right json.RawMessage `json:"right"`
	} `json:"match"`
	Dnat *struct {
		Addr string `json:"addr"`
		Port *int   `json:"port"`
	} `json:"dnat"`
	Masquerade json.RawMessage `json:"masquerade"`
	Comment    *string         `json:"comment"`
}

type nftLeft struct {
	Meta *struct {
		Key string `json:"key"`
	} `json:"meta"`
	Payload *struct {
		Protocol string `json:"protocol"`
		Field    string `json:"field"`
	} `json:"payload"`
}

func parseForwardsJSON(out string) (forwardsRead, error) {
	var doc nftDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return forwardsRead{}, fmt.Errorf("parse nft json: %w", err)
	}
	res := emptyForwardsRead()
	for _, node := range doc.Nftables {
		switch {
		case node.Table != nil:
			if node.Table.Family == nftFamily && node.Table.Name == nftTable {
				res.layout.Table = true
			}
		case node.Chain != nil:
			c := node.Chain
			if c.Family != nftFamily || c.Table != nftTable {
				continue
			}
			switch c.Name {
			case nftPreChain:
				res.layout.Prerouting = true
				if c.Type != "nat" || c.Hook != nftPreChain {
					res.layout.Mismatch = "chain " + nftPreChain + " exists but is not a nat/prerouting base chain"
				}
			case nftPostChain:
				res.layout.Postrouting = true
				if c.Type != "nat" || c.Hook != nftPostChain {
					res.layout.Mismatch = "chain " + nftPostChain + " exists but is not a nat/postrouting base chain"
				}
			}
		case node.Rule != nil:
			r := node.Rule
			if r.Family != nftFamily || r.Table != nftTable {
				continue
			}
			switch r.Chain {
			case nftPreChain:
				if rule, ok := parsePreRule(r.Handle, r.Comment, r.Expr); ok {
					res.rules = append(res.rules, rule)
				}
			case nftPostChain:
				if m, ok := parsePostRule(r.Handle, r.Expr); ok {
					res.masq = append(res.masq, m)
				}
			}
		}
	}
	return res, nil
}

// parsePreRule turns one `ip nat prerouting` rule into a forward, or reports
// that it is not a DNAT port forward at all. Anything the panel cannot
// represent sets ExtraMatch instead of being dropped (the operator still sees
// it, and can still delete it by handle).
func parsePreRule(handle int, comment string, exprs []json.RawMessage) (protocol.ForwardRule, bool) {
	// The comment is a field of the rule object, not an expression; the
	// expr-level case is kept as tolerance for other nft builds.
	r := protocol.ForwardRule{Handle: handle, Comment: comment}
	var dstIP string
	var dstPort *int
	haveDNAT := false
	for _, raw := range exprs {
		var e nftExpr
		if err := json.Unmarshal(raw, &e); err != nil {
			return r, false
		}
		switch {
		case e.Comment != nil:
			r.Comment = *e.Comment
		case e.Masquerade != nil:
			return r, false
		case e.Dnat != nil:
			if e.Dnat.Addr == "" {
				return r, false // dnat to a set/map: not a single port forward
			}
			haveDNAT = true
			dstIP = e.Dnat.Addr
			dstPort = e.Dnat.Port
		case e.Match != nil:
			var left nftLeft
			if err := json.Unmarshal(e.Match.Left, &left); err != nil {
				return r, false
			}
			num, str, scalar := nftRight(e.Match.Right)
			switch {
			case left.Payload != nil && left.Payload.Field == "dport" &&
				(left.Payload.Protocol == "tcp" || left.Payload.Protocol == "udp"):
				if r.Proto != "" {
					r.ExtraMatch = true // a second dport match
					continue
				}
				r.Proto = left.Payload.Protocol
				if !scalar || str != "" {
					r.ExtraMatch = true // set / range / named set: not one port
					continue
				}
				r.SrcPort = num
			case left.Meta != nil && left.Meta.Key == "iifname":
				if !scalar || str == "" {
					r.ExtraMatch = true
					continue
				}
				r.Iface = str
			default:
				r.ExtraMatch = true // saddr/daddr/oifname/mark/counter/limit/…
			}
		default:
			r.ExtraMatch = true
		}
	}
	if !haveDNAT {
		return r, false
	}
	r.DstIP = dstIP
	if dstPort != nil {
		r.DstPort = *dstPort
	} else {
		// `dnat to <ip>` keeps the original port; normalize it so the panel
		// always shows a concrete target port (nfpf.sh reads it the same way).
		r.DstPort = r.SrcPort
	}
	if r.Proto == "" || r.SrcPort == 0 {
		r.ExtraMatch = true // no plain tcp/udp dport match — not editable here
	}
	return r, true
}

// masqRule is one `ip nat postrouting` masquerade rule: the return-path half of
// a forward. Only the shape nfpf.sh writes is recognized.
type masqRule struct {
	Handle  int
	DstIP   string
	Proto   string
	DstPort int
}

func parsePostRule(handle int, exprs []json.RawMessage) (masqRule, bool) {
	m := masqRule{Handle: handle}
	haveMasq := false
	for _, raw := range exprs {
		var e nftExpr
		if err := json.Unmarshal(raw, &e); err != nil {
			return m, false
		}
		switch {
		case e.Masquerade != nil:
			haveMasq = true
		case e.Comment != nil:
			// comments cannot exist on a nat statement, but tolerate them
		case e.Match != nil:
			var left nftLeft
			if err := json.Unmarshal(e.Match.Left, &left); err != nil {
				return m, false
			}
			num, str, scalar := nftRight(e.Match.Right)
			switch {
			case left.Payload != nil && left.Payload.Protocol == "ip" && left.Payload.Field == "daddr":
				if !scalar || str == "" {
					return m, false
				}
				m.DstIP = str
			case left.Payload != nil && left.Payload.Field == "dport" &&
				(left.Payload.Protocol == "tcp" || left.Payload.Protocol == "udp"):
				if !scalar || str != "" {
					return m, false
				}
				m.Proto, m.DstPort = left.Payload.Protocol, num
			default:
				return m, false // anything else: not a pair we manage
			}
		default:
			return m, false
		}
	}
	if !haveMasq || m.DstIP == "" || m.Proto == "" || m.DstPort == 0 {
		return m, false
	}
	return m, true
}

// nftRight decodes a match's right-hand side: numbers and strings are scalars,
// objects (sets, ranges, maps) are not.
func nftRight(raw json.RawMessage) (num int, str string, scalar bool) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return 0, s, true
	}
	return 0, "", false
}

// --- text parsing (fallback) ---

// preLineRE is the exact line shape nfpf.sh generates, and the only shape the
// text fallback can model. Anything else is reported as extra_match.
var preLineRE = regexp.MustCompile(`^(?:iifname "([^"]*)" )?(tcp|udp) dport ([0-9]+) dnat to ([0-9.]+)(?::([0-9]+))?(?: comment "([^"]*)")?$`)
var postLineRE = regexp.MustCompile(`^ip daddr ([0-9.]+) (tcp|udp) dport ([0-9]+) masquerade$`)
var handleRE = regexp.MustCompile(`\s*#\s*handle ([0-9]+)\s*$`)
var (
	textProtoRE   = regexp.MustCompile(`\b(tcp|udp)\b`)
	textDportRE   = regexp.MustCompile(`\bdport ([0-9]+)`)
	textDnatRE    = regexp.MustCompile(`dnat to ([0-9.]+)(?::([0-9]+))?`)
	textIfaceRE   = regexp.MustCompile(`iifname "([^"]*)"`)
	textCommentRE = regexp.MustCompile(`comment "(.*)"`)
)

func parseForwardsText(pre, post string) ([]protocol.ForwardRule, []masqRule) {
	rules := []protocol.ForwardRule{}
	var masq []masqRule
	for _, line := range strings.Split(pre, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "dnat to") {
			continue
		}
		handle := 0
		if m := handleRE.FindStringSubmatch(line); m != nil {
			handle, _ = strconv.Atoi(m[1])
			line = strings.TrimSpace(handleRE.ReplaceAllString(line, ""))
		}
		if m := preLineRE.FindStringSubmatch(line); m != nil {
			srcPort, _ := strconv.Atoi(m[3])
			dstPort := srcPort
			if m[5] != "" {
				dstPort, _ = strconv.Atoi(m[5])
			}
			rules = append(rules, protocol.ForwardRule{
				Proto: m[2], SrcPort: srcPort, Iface: m[1],
				DstIP: m[4], DstPort: dstPort, Handle: handle, Comment: m[6],
			})
			continue
		}
		// Not the shape we can rewrite: keep it visible, refuse to edit it.
		r := protocol.ForwardRule{Handle: handle, ExtraMatch: true}
		if m := textProtoRE.FindStringSubmatch(line); m != nil {
			r.Proto = m[1]
		}
		if m := textDportRE.FindStringSubmatch(line); m != nil {
			r.SrcPort, _ = strconv.Atoi(m[1])
		}
		if m := textDnatRE.FindStringSubmatch(line); m != nil {
			r.DstIP = m[1]
			if m[2] != "" {
				r.DstPort, _ = strconv.Atoi(m[2])
			}
		}
		if r.DstPort == 0 {
			r.DstPort = r.SrcPort
		}
		if m := textIfaceRE.FindStringSubmatch(line); m != nil {
			r.Iface = m[1]
		}
		if m := textCommentRE.FindStringSubmatch(line); m != nil {
			r.Comment = m[1]
		}
		rules = append(rules, r)
	}
	for _, line := range strings.Split(post, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "masquerade") {
			continue
		}
		handle := 0
		if m := handleRE.FindStringSubmatch(line); m != nil {
			handle, _ = strconv.Atoi(m[1])
			line = strings.TrimSpace(handleRE.ReplaceAllString(line, ""))
		}
		m := postLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		port, _ := strconv.Atoi(m[3])
		masq = append(masq, masqRule{Handle: handle, DstIP: m[1], Proto: m[2], DstPort: port})
	}
	return rules, masq
}

// --- helpers ---

// isNotExist distinguishes "the table/chain was never created" (the normal
// first-boot state) from a real nft failure. nft has no exit-code for it, and
// its message is stable, untranslated text.
func isNotExist(out string) bool {
	return strings.Contains(out, "No such file or directory")
}

func classifyNftError(err error) (string, string) {
	msg := truncateStr(err.Error(), 400)
	if strings.Contains(err.Error(), "Operation not permitted") {
		return "no_permission", msg
	}
	return "nft_error", msg
}

func nftErrorText(out string, err error) string {
	msg := strings.TrimSpace(out)
	if err != nil {
		if msg != "" {
			msg += ": "
		}
		msg += err.Error()
	}
	return truncateStr(msg, 400)
}

// ensureIPForward mirrors nfpf.sh's enable_ip_forward: DNAT+masquerade only
// forwards anything with net.ipv4.ip_forward=1. Unlike the script it applies
// the setting immediately and appends to /etc/sysctl.conf only when no active
// line exists, so repeated calls cannot pile up duplicates.
func ensureIPForward(env nftEnv) string {
	const key = "net.ipv4.ip_forward"
	if data, err := env.readFile("/proc/sys/net/ipv4/ip_forward"); err == nil {
		if strings.TrimSpace(string(data)) == "1" {
			return ""
		}
	}
	if err := env.writeFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return "ip_forward_failed: " + err.Error()
	}
	conf, err := env.readFile(sysctlConfPath)
	if err == nil && hasActiveSysctl(string(conf), key) {
		return ""
	}
	line := ""
	if len(conf) > 0 && !strings.HasSuffix(string(conf), "\n") {
		line = "\n"
	}
	line += key + "=1\n"
	if err := env.writeFile(sysctlConfPath, append(conf, line...), 0o644); err != nil {
		return "sysctl_persist_failed: " + err.Error()
	}
	return ""
}

func hasActiveSysctl(conf, key string) bool {
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, key) {
			return true
		}
	}
	return false
}

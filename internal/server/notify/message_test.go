package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The chat body is the whole product of this file: kind slug -> readable
// headline, payload JSON -> labelled facts (§15). These tests pin the layout,
// not just the presence of a substring, because the layout is the point.
//
// enText/zhMessage are the two §15 rendering settings (2026-09-19 修订);
// keeping them explicit is what stops a test from passing because a fallback
// happened to look right.

func enText(ev Event) string {
	return MessageText(ev, TextOptions{Lang: LangEnglish, Loc: time.UTC})
}

func zhMessage(ev Event) string {
	return MessageText(ev, TextOptions{Lang: LangChinese, Loc: time.UTC})
}

func TestMessageTextAlertLayout(t *testing.T) {
	got := enText(Event{
		Kind: "node_offline", NodeID: "n1", NodeName: "edge-1",
		CreatedAt: 1730000000, Event: EventAlert,
	})
	want := "🔴 fobe · Probe offline\nNode: edge-1 (n1)\nTime: 2024-10-27 03:33:20 UTC"
	if got != want {
		t.Fatalf("MessageText = %q, want %q", got, want)
	}
}

func TestMessageTextChineseLayout(t *testing.T) {
	got := zhMessage(Event{
		Kind: "node_offline", NodeID: "n1", NodeName: "edge-1",
		CreatedAt: 1730000000, Event: EventAlert,
	})
	want := "🔴 fobe · 探针离线\n节点: edge-1 (n1)\n时间: 2024-10-27 03:33:20 UTC"
	if got != want {
		t.Fatalf("Chinese MessageText = %q, want %q", got, want)
	}
}

func TestMessageTextChineseFacts(t *testing.T) {
	traffic := zhMessage(Event{
		Kind: "traffic_warn", NodeID: "n1", NodeName: "edge-1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"mode\":\"both\",\"used_bytes\":1073741824,\"quota_bytes\":2147483648,\"pct\":50}",
	})
	for _, want := range []string{"🟠 fobe · 流量接近配额", "用量: 50%", "流量: 1.0 GB / 2.0 GB", "统计模式: 双向(IN+OUT)"} {
		if !strings.Contains(traffic, want) {
			t.Fatalf("Chinese traffic = %q, missing %q", traffic, want)
		}
	}

	due := zhMessage(Event{
		Kind: "billing_due", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"days\":3,\"due_at\":1730000000}",
	})
	if !strings.Contains(due, "🟠 fobe · 3 天后到期") || !strings.Contains(due, "到期: 2024-10-27 03:33:20 UTC") {
		t.Fatalf("Chinese billing = %q", due)
	}
	if strings.Contains(due, "days") {
		t.Fatalf("Chinese billing leaked the raw days key: %q", due)
	}
	overdue := zhMessage(Event{
		Kind: "due_overdue", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"days\":-4,\"due_at\":1730000000}",
	})
	if !strings.Contains(overdue, "🔴 fobe · 已逾期 4 天") {
		t.Fatalf("Chinese overdue = %q", overdue)
	}

	reset := zhMessage(Event{
		Kind: "counter_reset", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"iface\":\"eth0\",\"direction\":\"tx\",\"delta\":-918273645}",
	})
	for _, want := range []string{"🔵 fobe · 流量计数器重置", "网卡: eth0", "方向: 出站(tx)", "计数变化: -918273645"} {
		if !strings.Contains(reset, want) {
			t.Fatalf("Chinese counter reset = %q, missing %q", reset, want)
		}
	}
}

func TestMessageTextChineseClusterAlert(t *testing.T) {
	got := zhMessage(Event{
		Kind: "agent_update_stale", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"target_version\":\"0.1.8\",\"count\":2,\"nodes\":[{\"node_id\":\"a\"},{\"node_id\":\"b\",\"name\":\"edge-2\"}],\"checked_at\":1730000000}",
	})
	want := "🟠 fobe · 探针版本未收敛\n" +
		"时间: 2024-10-27 03:33:20 UTC\n" +
		"目标版本: 0.1.8\n" +
		"待收敛: 2 个节点\n" +
		"节点: a, edge-2\n" +
		"检查时间: 2024-10-27 03:33:20 UTC"
	if got != want {
		t.Fatalf("Chinese cluster = %q, want %q", got, want)
	}
	// The node-list cap is translated too.
	items := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		items = append(items, fmt.Sprintf("{\"node_id\":\"n%d\"}", i))
	}
	capped := zhMessage(Event{
		Kind: "singbox_update_stale", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"count\":12,\"nodes\":[" + strings.Join(items, ",") + "]}",
	})
	if !strings.Contains(capped, "另 4 台") {
		t.Fatalf("Chinese node-list cap = %q", capped)
	}
}

func TestMessageTextRecoveryAndTest(t *testing.T) {
	rec := enText(Event{
		Kind: "node_offline", NodeID: "n1", NodeName: "edge-1",
		CreatedAt: 1730000000, Event: EventRecovery,
	})
	if !strings.HasPrefix(rec, "✅ fobe · Probe offline (recovered)\n") {
		t.Fatalf("recovery = %q", rec)
	}
	recZh := zhMessage(Event{
		Kind: "node_offline", NodeID: "n1", NodeName: "edge-1",
		CreatedAt: 1730000000, Event: EventRecovery,
	})
	if !strings.HasPrefix(recZh, "✅ fobe · 探针离线(已恢复)\n") {
		t.Fatalf("Chinese recovery = %q", recZh)
	}
	test := zhMessage(Event{Kind: "notification", Event: EventTest, CreatedAt: 1730000000})
	if !strings.HasPrefix(test, "🔔 fobe · 测试消息\n时间: 2024-10-27 03:33:20 UTC") {
		t.Fatalf("Chinese test = %q", test)
	}
}

func TestMessageTextRendersTrafficFacts(t *testing.T) {
	got := enText(Event{
		Kind: "traffic_warn", NodeID: "n1", NodeName: "edge-1", CreatedAt: 1730000000,
		Event:   EventAlert,
		Payload: "{\"mode\":\"both\",\"used_bytes\":1073741824,\"quota_bytes\":2147483648,\"pct\":50}",
	})
	for _, want := range []string{
		"🟠 fobe · Traffic approaching quota",
		"Usage: 50%",
		"Traffic: 1.0 GB / 2.0 GB",
		"Mode: Both (IN+OUT)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("MessageText = %q, missing %q", got, want)
		}
	}
}

func TestMessageTextBillingTitleCarriesDays(t *testing.T) {
	due := enText(Event{
		Kind: "billing_due", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"days\":1,\"due_at\":1730000000}",
	})
	// days lives in the headline, so it must not come back as a raw fallback
	// line (the generic unknown-key pass would otherwise print "days: 1").
	wantDue := "🟠 fobe · Billing due in 1 day\n" +
		"Node: n1\n" +
		"Time: 2024-10-27 03:33:20 UTC\n" +
		"Due: 2024-10-27 03:33:20 UTC"
	if due != wantDue {
		t.Fatalf("billing due = %q, want %q", due, wantDue)
	}
	over := enText(Event{
		Kind: "due_overdue", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"days\":-4,\"due_at\":1730000000}",
	})
	if !strings.Contains(over, "🔴 fobe · Billing overdue by 4 days") {
		t.Fatalf("overdue = %q", over)
	}
}

func TestMessageTextClusterAlertListsNodes(t *testing.T) {
	got := enText(Event{
		Kind: "agent_update_stale", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"target_version\":\"0.1.8\",\"count\":2,\"nodes\":[{\"node_id\":\"a\"},{\"node_id\":\"b\",\"name\":\"edge-2\"}],\"checked_at\":1730000000}",
	})
	want := "🟠 fobe · Agent version not converged\n" +
		"Time: 2024-10-27 03:33:20 UTC\n" +
		"Target version: 0.1.8\n" +
		"Pending: 2 nodes\n" +
		"Nodes: a, edge-2\n" +
		"Checked: 2024-10-27 03:33:20 UTC"
	if got != want {
		t.Fatalf("MessageText = %q, want %q", got, want)
	}
	if strings.Contains(got, "Node: ") {
		t.Fatalf("cluster alert must not print a node line: %q", got)
	}
}

func TestMessageTextNodeListIsCapped(t *testing.T) {
	items := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		items = append(items, fmt.Sprintf("{\"node_id\":\"n%d\"}", i))
	}
	got := enText(Event{
		Kind: "singbox_update_stale", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"job_id\":7,\"count\":12,\"nodes\":[" + strings.Join(items, ",") + "]}",
	})
	if !strings.Contains(got, "Nodes: n0, n1, n2, n3, n4, n5, n6, n7, +4 more") {
		t.Fatalf("node list = %q", got)
	}
	if !strings.Contains(got, "Pending: 12 nodes") {
		t.Fatalf("count = %q", got)
	}
	// job_id is an internal handle: it stays in the panel's alert row.
	if strings.Contains(got, "job_id") {
		t.Fatalf("job_id leaked into the chat body: %q", got)
	}
}

// Login alerts (§15 实现修订 2026-09-20) are about the panel itself: no probe
// is involved, so there is no "Node:" line and the source address has to come
// out of the payload as its own labelled fact.
func TestMessageTextLoginAlerts(t *testing.T) {
	fail := enText(Event{
		Kind: "login_failed", CreatedAt: 1730000000, Event: EventAlert,
		Payload: `{"ip":"203.0.113.77","count":1,"max_fails":3}`,
	})
	want := "🟠 fobe · Panel login failed\n" +
		"Time: 2024-10-27 03:33:20 UTC\n" +
		"Source IP: 203.0.113.77\n" +
		"Failures: 1\n" +
		"Limit: 3"
	if fail != want {
		t.Fatalf("login failed = %q, want %q", fail, want)
	}
	if strings.Contains(fail, "Node:") {
		t.Fatalf("a login alert must not print a node line: %q", fail)
	}

	// A protected-network failure (§4.3) has no counter, only the flag that
	// says it was not counted — and the flag must not show when it is absent.
	protected := zhMessage(Event{
		Kind: "login_failed", CreatedAt: 1730000000, Event: EventAlert,
		Payload: `{"ip":"192.168.1.20","protected":true}`,
	})
	wantProtected := "🟠 fobe · 面板登录失败\n" +
		"时间: 2024-10-27 03:33:20 UTC\n" +
		"来源 IP: 192.168.1.20\n" +
		"受保护网段: 是"
	if protected != wantProtected {
		t.Fatalf("protected login = %q, want %q", protected, wantProtected)
	}

	blocked := zhMessage(Event{
		Kind: "login_blacklisted", CreatedAt: 1730000000, Event: EventAlert,
		Payload: `{"ip":"203.0.113.77","count":3,"block_seconds":1800}`,
	})
	wantBlocked := "🔴 fobe · 登录失败过多，来源 IP 已封禁\n" +
		"时间: 2024-10-27 03:33:20 UTC\n" +
		"来源 IP: 203.0.113.77\n" +
		"失败次数: 3\n" +
		"封禁时长: 30 分钟"
	if blocked != wantBlocked {
		t.Fatalf("blacklisted login = %q, want %q", blocked, wantBlocked)
	}
}

func TestMessageTextUnknownKindKeepsSlugAndFields(t *testing.T) {
	for _, text := range []string{
		enText(Event{
			Kind: "brand_new_alert", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
			Payload: "{\"alpha\":1,\"beta\":\"two\"}",
		}),
		zhMessage(Event{
			Kind: "brand_new_alert", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
			Payload: "{\"alpha\":1,\"beta\":\"two\"}",
		}),
	} {
		// The slug and any untranslated payload key stay verbatim in both packs:
		// a new alert type must never be silently dropped or blanked.
		for _, want := range []string{"⚪ fobe · brand_new_alert", "alpha: 1", "beta: two"} {
			if !strings.Contains(text, want) {
				t.Fatalf("MessageText = %q, missing %q", text, want)
			}
		}
	}
}

func TestMessageTextNonObjectPayloadKeepsRawText(t *testing.T) {
	got := zhMessage(Event{
		Kind: "node_offline", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "boom",
	})
	if !strings.Contains(got, "载荷: boom") {
		t.Fatalf("MessageText = %q", got)
	}
}

func TestMessageTextCapsLongValues(t *testing.T) {
	got := enText(Event{
		Kind: "singbox_down", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"error\":\"" + strings.Repeat("x", 5000) + "\"}",
	})
	if len(got) > messageCap {
		t.Fatalf("message length = %d, want <= %d", len(got), messageCap)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("truncation mark missing: %.60q", got)
	}
}

// The §15 timezone setting moves every timestamp, including the ones embedded
// in payload fields (due_at / checked_at / deadline).
func TestMessageTextTimezoneRendersConfiguredZone(t *testing.T) {
	loc, err := ParseTimezone("Asia/Shanghai")
	if err != nil {
		t.Fatalf("ParseTimezone: %v", err)
	}
	got := MessageText(Event{
		Kind: "billing_due", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert,
		Payload: "{\"days\":3,\"due_at\":1730000000}",
	}, TextOptions{Lang: LangEnglish, Loc: loc})
	want := "🟠 fobe · Billing due in 3 days\n" +
		"Node: n1\n" +
		"Time: 2024-10-27 11:33:20 CST\n" +
		"Due: 2024-10-27 11:33:20 CST"
	if got != want {
		t.Fatalf("MessageText = %q, want %q", got, want)
	}
}

func TestMessageTextTimezoneDefaultsToUTC(t *testing.T) {
	ev := Event{Kind: "node_offline", NodeID: "n1", CreatedAt: 1730000000, Event: EventAlert}
	// Both an unset option and an explicitly broken zone land on UTC: a bad
	// value must never stop a delivery.
	nilLoc := MessageText(ev, TextOptions{Lang: LangEnglish})
	badLoc := MessageText(ev, TextOptions{Lang: LangEnglish, Loc: LoadTimezone("Mars/Olympus")})
	if !strings.Contains(nilLoc, "Time: 2024-10-27 03:33:20 UTC") || nilLoc != badLoc {
		t.Fatalf("nil loc = %q, bad loc = %q", nilLoc, badLoc)
	}
}

func TestTextOptionsForResolvesSettings(t *testing.T) {
	opts := TextOptionsFor(decoder(map[string]string{
		KeyLanguage: "zh-CN",
		KeyTimezone: "Asia/Tokyo",
	}))
	if opts.Lang != LangChinese {
		t.Fatalf("Lang = %q, want %q", opts.Lang, LangChinese)
	}
	if opts.Loc == nil || opts.Loc.String() != "Asia/Tokyo" {
		t.Fatalf("Loc = %v, want Asia/Tokyo", opts.Loc)
	}

	// Unset keeps the pre-setting behaviour (English on UTC)…
	base := TextOptionsFor(decoder(map[string]string{}))
	if base.Lang != LangEnglish || base.Loc != time.UTC {
		t.Fatalf("unset options = %+v", base)
	}

	// …and an unreadable stored value degrades the same way instead of failing
	// the delivery.
	bad := TextOptionsFor(decoder(map[string]string{KeyLanguage: "fr", KeyTimezone: "Mars/Olympus"}))
	if bad.Lang != LangEnglish || bad.Loc != time.UTC {
		t.Fatalf("bad options = %+v", bad)
	}
	if nilOpts := TextOptionsFor(nil); nilOpts.Lang != LangEnglish || nilOpts.Loc != time.UTC {
		t.Fatalf("nil decrypt = %+v", nilOpts)
	}
}

func TestLanguageAndTimezoneValidation(t *testing.T) {
	for _, v := range []string{LangEnglish, LangChinese} {
		if !ValidLanguage(v) {
			t.Fatalf("ValidLanguage(%q) = false", v)
		}
	}
	for _, v := range []string{"", "en", "zh", "fr", "en_US", "EN-US"} {
		if ValidLanguage(v) {
			t.Fatalf("ValidLanguage(%q) = true, want the write path to refuse it", v)
		}
	}
	// NormalizeLanguage is the tolerant read path: anything Chinese-ish is
	// Chinese, everything else is English.
	for in, want := range map[string]string{"zh-CN": LangChinese, "zh": LangChinese, "ZH-TW": LangChinese, "": LangEnglish, "en-US": LangEnglish, "fr": LangEnglish} {
		if got := NormalizeLanguage(in); got != want {
			t.Fatalf("NormalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}

	for _, v := range []string{"", "UTC", "utc", "Asia/Shanghai", "America/New_York"} {
		if !ValidTimezone(v) {
			t.Fatalf("ValidTimezone(%q) = false", v)
		}
	}
	for _, v := range []string{"Mars/Olympus", "Europe/Nowhere", "not a zone"} {
		if ValidTimezone(v) {
			t.Fatalf("ValidTimezone(%q) = true, want false", v)
		}
	}
	if loc := LoadTimezone(""); loc != time.UTC {
		t.Fatalf("LoadTimezone(\"\") = %v, want UTC", loc)
	}
}

func TestHumanBytesMirrorsPanelFormatter(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1073741824, "1.0 GB"},
		{2147483648, "2.0 GB"},
		{1536, "1.5 KB"},
		{204800000000, "191 GB"},
	} {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

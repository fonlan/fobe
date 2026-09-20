package notify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Text rendering for the human channels (design §15).
//
// Telegram and 飞书 both send a plain-text body, and both used to get the same
// one-liner: "[fobe] ALERT node_offline - edge-1", an RFC3339 stamp, then the
// raw payload JSON. That is a debug dump, not a message — the kind is a slug to
// decode, the time is UTC with a T in it, and the facts arrive as machine JSON.
// A chat notification is read on a phone, so it is rendered here instead: a
// headline that says what happened and where, then one labelled line per fact,
// with units and times already converted.
//
// Language and clock come from the §15 settings (i18n.go, 2026-09-19 修订):
// the same structure is rendered in English or Chinese and every timestamp is
// shown in notify.timezone.
//
// The wire formats stay untouched on purpose: the generic webhook still gets
// {kind,node_id,payload,created_at,event} and alerts.payload still stores raw
// JSON (§15) — those are machine contracts, so language and timezone do not
// reach them. This file decides only what a person sees in chat.

const (
	// fieldCap bounds one rendered value: a truncated 500-byte agent error, or
	// a long node list, must not push the rest of the message out of view.
	fieldCap = 300
	// messageCap keeps the body under Telegram's 4096-character limit (and far
	// under Feishu's) even when every field sits at fieldCap.
	messageCap = 3800
)

// MessageText renders one notification as plain text. The Bot API takes no
// markup unless asked, so the body stays safe on every client (Telegram and
// Feishu alike).
func MessageText(ev Event, opts TextOptions) string {
	return opts.catalog().message(ev)
}

func (c catalog) message(ev Event) string {
	payload := decodePayload(ev.Payload)
	var b strings.Builder
	b.WriteString(c.headline(ev, payload))
	if subject := subject(ev); subject != "" {
		b.WriteString("\n")
		b.WriteString(c.tr("Node"))
		b.WriteString(": ")
		b.WriteString(clip(subject, fieldCap))
	}
	if stamp := c.stamp(ev.CreatedAt); stamp != "" {
		b.WriteString("\n")
		b.WriteString(c.tr("Time"))
		b.WriteString(": ")
		b.WriteString(stamp)
	}
	for _, f := range c.fields(ev.Kind, payload) {
		b.WriteString("\n")
		b.WriteString(f.label)
		b.WriteString(": ")
		b.WriteString(f.value)
	}
	// A payload that is not a JSON object (a legacy row, or text the agent
	// could not structure) still has to reach the operator somewhere.
	if payload == nil {
		if raw := clean(ev.Payload); raw != "" && raw != "{}" {
			b.WriteString("\n")
			b.WriteString(c.tr("Payload"))
			b.WriteString(": ")
			b.WriteString(raw)
		}
	}
	return clip(b.String(), messageCap)
}

// headline is the line that has to work on a lock screen: severity mark plus
// what happened. A recovery replaces the severity with a check mark (the alarm
// is over), and the test message gets its own shape so nobody mistakes a smoke
// test for an incident.
func (c catalog) headline(ev Event, payload map[string]any) string {
	if ev.Event == EventTest {
		return "🔔 fobe · " + c.tr("Test notification")
	}
	icon, title := c.describe(ev.Kind, payload)
	if ev.Event == EventRecovery {
		return "✅ fobe · " + fmt.Sprintf(c.tr("%s (recovered)"), title)
	}
	return icon + " fobe · " + title
}

// describe maps an alert kind to a severity mark and a title a person can read
// without the panel. Kinds are the panel's own vocabulary (i18n kind_*); an
// unknown kind keeps its slug rather than inventing a label, so a new alert
// type still arrives and stays greppable.
func (c catalog) describe(kind string, payload map[string]any) (icon, title string) {
	switch kind {
	case "node_offline":
		return "🔴", c.tr("Probe offline")
	case "login_failed":
		return "🟠", c.tr("Panel login failed")
	case "login_blacklisted":
		return "🔴", c.tr("Panel login blocked (IP blacklisted)")
	case "traffic_warn":
		return "🟠", c.tr("Traffic approaching quota")
	case "traffic_crit":
		return "🔴", c.tr("Traffic quota exceeded")
	case "billing_due":
		if days, ok := number(payload["days"]); ok && days > 0 {
			return "🟠", fmt.Sprintf(c.tr("Billing due in %s"), c.days(int64(days)))
		}
		return "🟠", c.tr("Billing due")
	case "due_overdue":
		if days, ok := number(payload["days"]); ok && days < 0 {
			return "🔴", fmt.Sprintf(c.tr("Billing overdue by %s"), c.days(int64(-days)))
		}
		return "🔴", c.tr("Billing overdue")
	case "singbox_down":
		return "🔴", c.tr("sing-box not running")
	case "singbox_rollback":
		return "🔴", c.tr("sing-box rolled back")
	case "counter_reset":
		return "🔵", c.tr("Traffic counter reset")
	case "agent_update_failed":
		return "🔴", c.tr("Agent self-update failed")
	case "agent_update_transient":
		return "🟠", c.tr("Agent self-update retrying")
	case "agent_update_stale":
		return "🟠", c.tr("Agent version not converged")
	case "singbox_update_stale":
		return "🟠", c.tr("sing-box update not converged")
	case "":
		return "⚪", c.tr("Notification")
	}
	return "⚪", kind
}

// subject names the probe as "name (id)" so the chat text and the panel's
// tables can be matched by either. Cluster-wide alerts (agent_update_stale,
// singbox_update_stale) carry no node and get no line.
func subject(ev Event) string {
	name := strings.TrimSpace(ev.NodeName)
	id := strings.TrimSpace(ev.NodeID)
	switch {
	case name != "" && id != "":
		return name + " (" + id + ")"
	case name != "":
		return name
	default:
		return id
	}
}

// field is one labelled line of the body.
type field struct{ label, value string }

// fields renders the payload for a person: known kinds get labels and units
// that match their meaning, and every key the renderer does not know about is
// still printed (raw name, plain value) so a new payload field can never be
// silently dropped from the notification.
func (c catalog) fields(kind string, p map[string]any) []field {
	if len(p) == 0 {
		return nil
	}
	b := &builder{c: c, payload: p, used: map[string]bool{}}
	switch kind {
	case "login_failed", "login_blacklisted":
		b.take(c.tr("Source IP"), "ip", textValue)
		b.take(c.tr("Failures"), "count", numberText)
		b.take(c.tr("Limit"), "max_fails", numberText)
		b.take(c.tr("Blocked for"), "block_seconds", c.duration)
		b.take(c.tr("Protected network"), "protected", c.yesNo)
	case "traffic_warn", "traffic_crit":
		b.take(c.tr("Usage"), "pct", percentText)
		b.bytesPair(c.tr("Traffic"), "used_bytes", "quota_bytes")
		b.take(c.tr("Mode"), "mode", c.mode)
	case "billing_due", "due_overdue":
		b.take(c.tr("Due"), "due_at", c.unix)
		// days is the headline ("due in 3 days" / "overdue by 4 days"), so it
		// must not come back as a raw fallback line further down.
		b.skip("days")
	case "singbox_down":
		b.take(c.tr("Port"), "port", numberText)
		b.take(c.tr("Error"), "error", textValue)
	case "singbox_rollback":
		b.take(c.tr("Version"), "version", textValue)
		b.take(c.tr("Desired version"), "desired_version", textValue)
		b.take(c.tr("Port"), "port", numberText)
		b.take(c.tr("Error"), "error", textValue)
	case "counter_reset":
		b.take(c.tr("Interface"), "iface", textValue)
		b.take(c.tr("Direction"), "direction", c.direction)
		b.take(c.tr("Delta"), "delta", numberText)
	case "agent_update_failed", "agent_update_transient":
		b.take(c.tr("Target version"), "target", textValue)
		b.take(c.tr("Phase"), "phase", textValue)
		b.take(c.tr("Class"), "class", textValue)
		b.take(c.tr("Attempts"), "attempts", numberText)
		b.take(c.tr("Error"), "reason", textValue)
		b.take(c.tr("Note"), "why", textValue)
	case "agent_update_stale", "singbox_update_stale":
		// The panel's update-job id is an internal handle; the alert row keeps
		// it for anyone who needs to correlate, but it is noise in a chat line.
		b.skip("job_id")
		b.take(c.tr("Target version"), "target_version", textValue)
		b.take(c.tr("Pending"), "count", c.count)
		b.take(c.tr("Nodes"), "nodes", c.nodeList)
		b.take(c.tr("Checked"), "checked_at", c.unix)
		b.take(c.tr("Deadline"), "deadline", c.unix)
	}
	for _, k := range sortedKeys(p) {
		if b.used[k] {
			continue
		}
		b.used[k] = true
		if v := plainText(p[k]); v != "" {
			b.out = append(b.out, field{k, v})
		}
	}
	return b.out
}

// builder collects rendered fields in a deliberate order while remembering
// which payload keys they consumed.
type builder struct {
	c       catalog
	payload map[string]any
	used    map[string]bool
	out     []field
}

func (b *builder) take(label, key string, render func(any) string) {
	v, ok := b.payload[key]
	if !ok {
		return
	}
	b.used[key] = true // consumed even when it renders empty: no duplicate below
	if s := render(v); s != "" {
		b.out = append(b.out, field{label, s})
	}
}

// skip marks payload keys already represented elsewhere (in the headline), so
// the unknown-key pass below does not repeat them as raw "key: value" lines.
func (b *builder) skip(keys ...string) {
	for _, k := range keys {
		b.used[k] = true
	}
}

// bytesPair folds used/quota into one "Traffic: 1.0 GB / 2.0 GB" line; a lone
// value (an old payload, or a quota-less cycle) still prints. Sizes keep the
// panel's units in both languages: KB/MB/GB are not translated.
func (b *builder) bytesPair(label, usedKey, quotaKey string) {
	used, okUsed := number(b.payload[usedKey])
	quota, okQuota := number(b.payload[quotaKey])
	b.used[usedKey], b.used[quotaKey] = true, true
	switch {
	case okUsed && okQuota:
		b.out = append(b.out, field{label, humanBytes(int64(used)) + " / " + humanBytes(int64(quota))})
	case okUsed:
		b.out = append(b.out, field{label, humanBytes(int64(used))})
	}
}

// decodePayload parses the stored payload; nil means "nothing renderable" and
// makes message fall back to printing the raw string.
func decodePayload(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- value renderers (language-aware ones are catalog methods) ---

func textValue(v any) string {
	s, ok := v.(string)
	if !ok {
		return plainText(v)
	}
	return clean(s)
}

func numberText(v any) string {
	n, ok := number(v)
	if !ok {
		return plainText(v)
	}
	return formatNumber(n)
}

func percentText(v any) string {
	n, ok := number(v)
	if !ok {
		return plainText(v)
	}
	return formatNumber(n) + "%"
}

func (c catalog) count(v any) string {
	n, ok := number(v)
	if !ok {
		return plainText(v)
	}
	return c.nodes(int(n))
}

func (c catalog) unix(v any) string {
	n, ok := number(v)
	if !ok {
		return ""
	}
	return c.stamp(int64(n))
}

func (c catalog) mode(v any) string {
	switch s, _ := v.(string); s {
	case "in":
		return c.tr("Inbound (IN)")
	case "out":
		return c.tr("Outbound (OUT)")
	case "both":
		return c.tr("Both (IN+OUT)")
	case "max":
		return c.tr("Max (IN/OUT)")
	default:
		return textValue(v)
	}
}

// yesNo renders a flag that is only present in the payload when it is true
// (a protected-network failure, §4.3): absent means the ordinary case, so the
// line never appears rather than printing a misleading "No".
func (c catalog) yesNo(v any) string {
	b, ok := v.(bool)
	if !ok {
		return plainText(v)
	}
	if b {
		return c.tr("Yes")
	}
	return c.tr("No")
}

// duration renders the blacklist window of a login alert ("30 min"). It is a
// catalog method because the unit is part of the translated phrase.
func (c catalog) duration(v any) string {
	n, ok := number(v)
	if !ok {
		return plainText(v)
	}
	switch secs := int64(n); {
	case secs <= 0:
		return ""
	case secs%3600 == 0:
		return fmt.Sprintf(c.tr("%d h"), secs/3600)
	case secs%60 == 0:
		return fmt.Sprintf(c.tr("%d min"), secs/60)
	default:
		return fmt.Sprintf(c.tr("%d s"), secs)
	}
}

func (c catalog) direction(v any) string {
	switch s, _ := v.(string); s {
	case "rx":
		return c.tr("Inbound (rx)")
	case "tx":
		return c.tr("Outbound (tx)")
	default:
		return textValue(v)
	}
}

// nodeList renders the stale-node arrays of the two cluster alerts. The
// per-node detail (version, state) stays in the panel: the chat line lists who
// is behind, which is what the operator acts on.
func (c catalog) nodeList(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			if s := plainText(it); s != "" {
				names = append(names, s)
			}
			continue
		}
		name, _ := obj["name"].(string)
		if strings.TrimSpace(name) == "" {
			name, _ = obj["node_id"].(string)
		}
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	const maxNames = 8
	if len(names) > maxNames {
		names = append(names[:maxNames], fmt.Sprintf(c.tr("+%d more"), len(names)-maxNames))
	}
	return clean(strings.Join(names, ", "))
}

// plainText is the fallback for values with no meaning attached: strings as
// they are, numbers without trailing zeros, nested values as compact JSON.
func plainText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return clean(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return formatNumber(t)
	default:
		enc, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return clip(clean(string(enc)), fieldCap)
	}
}

// number accepts the JSON numbers encoding/json produces (float64).
func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// formatNumber renders a number without trailing zeros: 50 stays 50, 85.5
// stays 85.5 (the scheduler already rounds percentages to one decimal).
func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// humanBytes mirrors web/src/format.ts fmtBytes so the chat and the panel never
// disagree about a size (1024-based, one decimal below 100).
func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB", "EB"}
	v := float64(n)
	neg := v < 0
	if neg {
		v = -v
	}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	text := strconv.FormatFloat(v, 'f', 0, 64)
	if i > 0 && v < 100 {
		text = strconv.FormatFloat(v, 'f', 1, 64)
	}
	if neg {
		return "-" + text + " " + units[i]
	}
	return text + " " + units[i]
}

// clean collapses whitespace (agent error strings arrive with newlines) and
// bounds the length, so a value can never break the one-line-per-fact layout.
func clean(s string) string {
	return clip(strings.Join(strings.Fields(s), " "), fieldCap)
}

// clip cuts on rune boundaries and marks the cut.
func clip(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " \t\n") + "…"
}

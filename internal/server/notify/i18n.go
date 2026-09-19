package notify

import (
	"fmt"
	"strings"
	"time"
)

// Language and timezone of the pushed text (design §15, 2026-09-19 修订).
//
// Both are settings, not compile-time choices: the panel interface language
// lives in the operator's browser, while a notification lands in a chat group
// that has no language of its own. The server therefore owns the copy language
// and the clock the timestamps are shown on, and resolves them per delivery
// through each channel's DecryptFunc (same lazy contract as the credentials),
// so a change in the panel applies to the very next alert without a restart.

// Language tags. They deliberately equal the panel's Locale values
// ('zh-CN' / 'en-US', web/src/i18n.tsx) so the notifications page can hand its
// own vocabulary to the settings API without a translation table.
const (
	LangEnglish = "en-US"
	LangChinese = "zh-CN"
)

// NormalizeLanguage maps a stored value onto one of the two packs. Anything
// that is not Chinese reads as English: the default has to be the behaviour
// that existed before this setting (English), never an empty body.
func NormalizeLanguage(v string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "zh") {
		return LangChinese
	}
	return LangEnglish
}

// ValidLanguage is the write-path check: only the canonical tags are stored, so
// the page's select and the database can never disagree about the spelling.
func ValidLanguage(v string) bool {
	switch strings.TrimSpace(v) {
	case LangEnglish, LangChinese:
		return true
	}
	return false
}

// ParseTimezone resolves the notify.timezone setting against the image's
// tzdata (deploy/Dockerfile.server installs the tzdata package, so an IANA name
// is always resolvable in production). Empty and "UTC" both mean UTC: an unset
// key must keep behaving exactly like the pre-setting default.
func ParseTimezone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "UTC") {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("notify timezone %q: %w", name, err)
	}
	return loc, nil
}

// ValidTimezone is the write-path check for ParseTimezone.
func ValidTimezone(name string) bool {
	_, err := ParseTimezone(name)
	return err == nil
}

// LoadTimezone is the delivery-path resolver: a value that cannot be read must
// never stop an alert, so it falls back to UTC (the write path refuses it, but
// an imported or hand-edited database can still hold nonsense).
func LoadTimezone(name string) *time.Location {
	loc, err := ParseTimezone(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// TextOptions is how the text channels render a message: the copy language and
// the timezone every timestamp is shown in.
type TextOptions struct {
	Lang string
	Loc  *time.Location
}

// TextOptionsFor reads the two §15 settings through a channel's DecryptFunc.
func TextOptionsFor(decrypt DecryptFunc) TextOptions {
	opts := TextOptions{Lang: LangEnglish, Loc: time.UTC}
	if decrypt == nil {
		return opts
	}
	if v, ok := decrypt(KeyLanguage); ok {
		opts.Lang = NormalizeLanguage(v)
	}
	if v, ok := decrypt(KeyTimezone); ok {
		opts.Loc = LoadTimezone(v)
	}
	return opts
}

func (o TextOptions) catalog() catalog {
	loc := o.Loc
	if loc == nil {
		loc = time.UTC
	}
	return catalog{lang: NormalizeLanguage(o.Lang), loc: loc}
}

// catalog is one language pack plus the zone its timestamps render in. The body
// is laid out once in English and translated by lookup (zhText maps a rendered
// English phrase or %s/%d template to Chinese), so a phrase nobody translated
// degrades to English instead of to an empty message.
type catalog struct {
	lang string
	loc  *time.Location
}

func (c catalog) tr(s string) string {
	if c.lang != LangChinese {
		return s
	}
	if v, ok := zhText[s]; ok {
		return v
	}
	return s
}

// days phrases a countdown inside a translated template ("3 days" / "3 天").
func (c catalog) days(n int64) string {
	if c.lang == LangChinese {
		return fmt.Sprintf("%d 天", n)
	}
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// nodes phrases a node count; the singular only exists in English.
func (c catalog) nodes(n int) string {
	if c.lang == LangChinese {
		return fmt.Sprintf("%d 个节点", n)
	}
	if n == 1 {
		return "1 node"
	}
	return fmt.Sprintf("%d nodes", n)
}

// stamp renders unix seconds on the configured clock. The zone is printed, not
// implied: a chat reader must never have to guess which clock this is.
func (c catalog) stamp(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).In(c.loc).Format("2006-01-02 15:04:05 MST")
}

// zhText is the Chinese pack, keyed by the English phrase it replaces. Entries
// carrying %s/%d are templates handed to fmt.Sprintf after translation, which
// is what lets a countdown read "3 天后到期" instead of "Billing due in 3 天".
var zhText = map[string]string{
	// headlines
	"Test notification":             "测试消息",
	"%s (recovered)":                "%s(已恢复)",
	"Probe offline":                 "探针离线",
	"Traffic approaching quota":     "流量接近配额",
	"Traffic quota exceeded":        "流量超出配额",
	"Billing due":                   "即将到期",
	"Billing due in %s":             "%s后到期",
	"Billing overdue":               "已逾期",
	"Billing overdue by %s":         "已逾期 %s",
	"sing-box not running":          "sing-box 未在运行",
	"sing-box rolled back":          "sing-box 已回滚",
	"Traffic counter reset":         "流量计数器重置",
	"Agent self-update failed":      "探针自更新失败",
	"Agent self-update retrying":    "探针自更新反复失败",
	"Agent version not converged":   "探针版本未收敛",
	"sing-box update not converged": "sing-box 更新未收敛",
	"Notification":                  "通知",

	// field labels
	"Node":            "节点",
	"Time":            "时间",
	"Payload":         "载荷",
	"Usage":           "用量",
	"Traffic":         "流量",
	"Mode":            "统计模式",
	"Due":             "到期",
	"Port":            "端口",
	"Error":           "错误",
	"Version":         "版本",
	"Desired version": "目标版本",
	"Interface":       "网卡",
	"Direction":       "方向",
	"Delta":           "计数变化",
	"Target version":  "目标版本",
	"Phase":           "阶段",
	"Class":           "类别",
	"Attempts":        "尝试次数",
	"Note":            "说明",
	"Pending":         "待收敛",
	"Nodes":           "节点",
	"Checked":         "检查时间",
	"Deadline":        "截止时间",

	// values (mode/direction labels mirror the panel's mode_* copy)
	"Inbound (IN)":   "入站(IN)",
	"Outbound (OUT)": "出站(OUT)",
	"Both (IN+OUT)":  "双向(IN+OUT)",
	"Max (IN/OUT)":   "取最大(IN/OUT)",
	"Inbound (rx)":   "入站(rx)",
	"Outbound (tx)":  "出站(tx)",
	"+%d more":       "另 %d 台",
}

package modelsdev

import (
	"slices"
	"strings"
)

// Protocol and reasoning-level mapping (design §12.5). Everything in this file
// is a pure function of its arguments: no Manager, no I/O, no globals that
// change — the panel's form logic is the part that most needs unit tests.

// Panel protocol names. These strings are the wire vocabulary of
// ai_providers.protocol; they are duplicated here on purpose rather than
// imported from store, because this package is the pure metadata layer and must
// stay free of storage dependencies. If the vocabulary ever changes, both
// packages and the adapters change together (§12.5).
const (
	ProtocolAnthropicMessages = "anthropic-messages"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolOpenAICompletions = "openai-completions"
)

// ProtocolForNPM maps a models.dev npm package name to a panel protocol. Only
// the three supported dialects are recognised; everything else returns "".
//
// 超纲的一律不猜：@ai-sdk/azure、@ai-sdk/google、@ai-sdk/google-vertex、
// @ai-sdk/groq、@ai-sdk/xai … 能不能用取决于该上游是否恰好兼容三者之一
// （Vertex/Google 就不兼容 OpenAI 方言），所以表单只预填 base_url、协议强制手选。
// 返回空串就是这个"必须手选"的信号，不是错误。
func ProtocolForNPM(npm string) string {
	switch strings.TrimSpace(npm) {
	case "@ai-sdk/anthropic":
		return ProtocolAnthropicMessages
	case "@ai-sdk/openai":
		return ProtocolOpenAIResponses
	case "@ai-sdk/openai-compatible":
		return ProtocolOpenAICompletions
	default:
		return ""
	}
}

// The panel's unified thinking levels (§12.5). Every list ReasoningLevels
// returns is drawn from these names.
const (
	LevelOff     = "off"
	LevelMinimal = "minimal"
	LevelLow     = "low"
	LevelMedium  = "medium"
	LevelHigh    = "high"
)

// reasoning_options types as published by models.dev.
const (
	ReasoningEffort       = "effort"
	ReasoningToggle       = "toggle"
	ReasoningBudgetTokens = "budget_tokens"
)

// ReasoningValueNone is the effort value models.dev publishes for "no thinking".
// It is a vendor token, not a panel level, but the caller needs it on the wire:
// for an OpenAI-side effort model whose values contain it, choosing "off" means
// sending reasoning_effort="none" — 省略字段在部分网关（默认开思考的那些）上
// 并不会真的关掉思考。Anthropic 侧相反：off 就是省略 thinking 字段。
const ReasoningValueNone = "none"

// DefaultReasoningBudgets maps a budget_tokens level to its token budget: the
// fixed 2k / 8k / 16k of §12.5. The numbers live here as a default only — the
// operator may edit them, and the caller validates ≥1024 and < max output before
// saving (Anthropic's max_tokens is the model's max output, and going over turns
// into a 400 in the middle of a conversation). ReasoningLevels itself returns
// names, never numbers.
var DefaultReasoningBudgets = map[string]int{
	LevelLow:    2048,
	LevelMedium: 8192,
	LevelHigh:   16384,
}

// ReasoningLevels returns the levels this model actually offers under the given
// protocol, in canonical order (off, minimal, low, medium, high). An unknown or
// empty protocol returns nil: if the protocol is not chosen yet, the panel must
// not offer levels it cannot translate.
//
// 档位集合 = 协议能力 ∩ 该模型 reasoning_options（类型 + values）（§12.5）：
//
//   - effort → the model's own values, intersected with the panel vocabulary
//     {minimal, low, medium, high}; the names pass straight through as
//     reasoning_effort, so what the vendor does not list must not be offered
//     (上游会 400)。values 为空时退回 low/medium/high（类型化视图里没有信息时
//     不敢猜宽，也不假装知道 minimal）。厂商的 xhigh / max / default 在词汇表
//     之外，只能丢掉：它们比 high 更高或语义未知，映射成 high 就是谎报。
//   - toggle → on/off only. "off" is LevelOff; the panel vocabulary has no word
//     for "on", so LevelHigh carries it — toggle 的语义是"尽力思考"，用 minimal/low
//     会把"开"谎报成"最省"，而 Anthropic 侧的 budget 映射会把 low 变成 2k。
//   - budget_tokens → low/medium/high; the 2k/8k/16k numbers live in
//     DefaultReasoningBudgets and are the caller's to edit.
//   - off visibility:
//   - Anthropic is opt-in (省略 thinking 字段就是关)，所以恒可关；
//   - OpenAI 侧只有 toggle 型、非推理模型（reasoning=false）、或 **effort values 里
//     带 none 的模型**能真关。最后这一类是最容易漏的一批：实测 3415 个带 values 的
//     effort 模型里 1273 个（37%）列了 none，它们能关是厂商自己写在文档里的，面板
//     按 off 处理才诚实（线上对 OpenAI 发 reasoning_effort=none）；
//   - 其余 effort / budget_tokens 型不给 off，最低档必须由面板标注为「最低档」，
//     不许谎称已关闭。
//
// A reasoning=true model with an empty reasoning_options list (1202 of the 7843
// models, mostly gateways) gets an empty list: 既不知道能传什么档位，也不能断定
// 它关得掉 —— 面板显示"该模型未提供可调档位"，由操作者手填或换模型。10 个模型的
// values 与词汇表交集为空（如只有 max），同样诚实返回空列表/仅 off。
func ReasoningLevels(m ModelMeta, protocol string) []string {
	switch protocol {
	case ProtocolAnthropicMessages:
		return levelsFor(m, true)
	case ProtocolOpenAICompletions, ProtocolOpenAIResponses:
		return levelsFor(m, openAICanTurnOff(m))
	default:
		return nil
	}
}

// openAICanTurnOff is the off rule for the two OpenAI dialects: 非推理模型本来就
// 没有思考可关；toggle 型是显式开关；effort 型的 values 里出现 none 说明厂商自己
// 提供了"关"这一档。
func openAICanTurnOff(m ModelMeta) bool {
	if !m.Reasoning {
		return true
	}
	if slices.Contains(m.ReasoningOptions, ReasoningToggle) {
		return true
	}
	return slices.Contains(m.ReasoningValues, ReasoningValueNone)
}

// levelsFor builds the ordered level list: off when the model can turn thinking
// off, then the graded names its option types actually provide.
func levelsFor(m ModelMeta, canTurnOff bool) []string {
	levels := make([]string, 0, 5)
	if canTurnOff {
		levels = append(levels, LevelOff)
	}

	graded := map[string]bool{}
	for _, opt := range m.ReasoningOptions {
		switch opt {
		case ReasoningEffort:
			for _, level := range effortLevels(m.ReasoningValues) {
				graded[level] = true
			}
		case ReasoningBudgetTokens:
			graded[LevelLow] = true
			graded[LevelMedium] = true
			graded[LevelHigh] = true
		case ReasoningToggle:
			graded[LevelHigh] = true
		}
	}
	// Fixed order; iterating the map would make the list random.
	for _, level := range []string{LevelMinimal, LevelLow, LevelMedium, LevelHigh} {
		if graded[level] {
			levels = append(levels, level)
		}
	}
	return levels
}

// effortLevels intersects the vendor's effort values with the panel vocabulary.
// An empty values list falls back to low/medium/high: 没有信息时给三档通用的，
// 不假装知道 minimal。ReasoningValueNone is not a level — it is consumed by the
// off rule, so it simply does not appear here.
func effortLevels(values []string) []string {
	if len(values) == 0 {
		return []string{LevelLow, LevelMedium, LevelHigh}
	}
	out := make([]string, 0, 4)
	for _, level := range []string{LevelMinimal, LevelLow, LevelMedium, LevelHigh} {
		if slices.Contains(values, level) {
			out = append(out, level)
		}
	}
	return out
}

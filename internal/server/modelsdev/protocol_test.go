package modelsdev

import (
	"slices"
	"testing"
)

func TestProtocolForNPM(t *testing.T) {
	cases := []struct {
		name string
		npm  string
		want string
	}{
		{"anthropic sdk", "@ai-sdk/anthropic", ProtocolAnthropicMessages},
		{"openai sdk", "@ai-sdk/openai", ProtocolOpenAIResponses},
		{"openai-compatible sdk", "@ai-sdk/openai-compatible", ProtocolOpenAICompletions},
		{"surrounding whitespace", " @ai-sdk/anthropic\n", ProtocolAnthropicMessages},
		// Everything outside the three dialects returns "": the form must force a
		// manual protocol choice instead of guessing (design §12.5).
		{"azure", "@ai-sdk/azure", ""},
		{"google", "@ai-sdk/google", ""},
		{"google vertex", "@ai-sdk/google-vertex", ""},
		{"groq", "@ai-sdk/groq", ""},
		{"openrouter", "@openrouter/ai-sdk-provider", ""},
		{"unknown", "some-provider", ""},
		{"empty", "", ""},
		{"near miss", "@ai-sdk/openai-compatible-plus", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProtocolForNPM(tc.npm); got != tc.want {
				t.Errorf("ProtocolForNPM(%q) = %q, want %q", tc.npm, got, tc.want)
			}
		})
	}
}

func TestReasoningLevels(t *testing.T) {
	// meta builds a model whose reasoning_options carry the given types and no
	// published values (the "type-only" document shape).
	meta := func(reasoning bool, options ...string) ModelMeta {
		return ModelMeta{
			ID:               "m",
			Reasoning:        reasoning,
			ReasoningOptions: options,
		}
	}
	// values builds an effort model that publishes the vendor's own names.
	values := func(reasoning bool, vals ...string) ModelMeta {
		return ModelMeta{
			ID:               "m",
			Reasoning:        reasoning,
			ReasoningOptions: []string{ReasoningEffort},
			ReasoningValues:  vals,
		}
	}

	cases := []struct {
		name     string
		model    ModelMeta
		protocol string
		want     []string
	}{
		{
			// Anthropic is opt-in: omitting the thinking field disables it, so
			// "off" is honest regardless of what the vendor publishes. Never
			// regress this case — it is the one the off rule was written for.
			name:     "anthropic is always switchable off",
			model:    meta(true, ReasoningEffort, ReasoningBudgetTokens),
			protocol: ProtocolAnthropicMessages,
			want:     []string{LevelOff, LevelLow, LevelMedium, LevelHigh},
		},
		{
			name:     "anthropic budget only",
			model:    meta(true, ReasoningBudgetTokens),
			protocol: ProtocolAnthropicMessages,
			want:     []string{LevelOff, LevelLow, LevelMedium, LevelHigh},
		},
		{
			name:     "anthropic toggle is on/off",
			model:    meta(true, ReasoningToggle),
			protocol: ProtocolAnthropicMessages,
			want:     []string{LevelOff, LevelHigh},
		},
		{
			name:     "anthropic non-reasoning model",
			model:    meta(false),
			protocol: ProtocolAnthropicMessages,
			want:     []string{LevelOff},
		},
		{
			// The vendor publishes minimal → offer it; no "none" → no off.
			name:     "openai effort values with minimal, no none",
			model:    values(true, "minimal", "low", "medium", "high"),
			protocol: ProtocolOpenAIResponses,
			want:     []string{LevelMinimal, LevelLow, LevelMedium, LevelHigh},
		},
		{
			// "none" is the vendor's own off switch: this gateway model can
			// really turn thinking off, so off must be offered.
			name:     "openai effort values with none can switch off",
			model:    values(true, ReasoningValueNone, "low", "high"),
			protocol: ProtocolOpenAICompletions,
			want:     []string{LevelOff, LevelLow, LevelHigh},
		},
		{
			// Only "none": switchable off, but the vendor names no gradation
			// inside our vocabulary.
			name:     "openai effort values with only none",
			model:    values(true, ReasoningValueNone),
			protocol: ProtocolOpenAICompletions,
			want:     []string{LevelOff},
		},
		{
			// Tiers above "high" are not panel levels and must not be mapped to
			// one; the vendor's none still shows up as off.
			name:     "openai effort values above the vocabulary",
			model:    values(true, ReasoningValueNone, "xhigh", "max"),
			protocol: ProtocolOpenAIResponses,
			want:     []string{LevelOff},
		},
		{
			// Non-empty values with no intersection at all: 不猜，也不谎报档位。
			name:     "openai effort values outside the vocabulary",
			model:    values(true, "max"),
			protocol: ProtocolOpenAIResponses,
			want:     []string{},
		},
		{
			// No published values (an effort option without values): keep the
			// generic three, and do not invent minimal.
			name:     "openai effort without values falls back to three",
			model:    meta(true, ReasoningEffort),
			protocol: ProtocolOpenAIResponses,
			want:     []string{LevelLow, LevelMedium, LevelHigh},
		},
		{
			name:     "openai completions budget cannot turn off",
			model:    meta(true, ReasoningBudgetTokens),
			protocol: ProtocolOpenAICompletions,
			want:     []string{LevelLow, LevelMedium, LevelHigh},
		},
		{
			name:     "openai responses effort+toggle can turn off",
			model:    ModelMeta{Reasoning: true, ReasoningOptions: []string{ReasoningEffort, ReasoningToggle}},
			protocol: ProtocolOpenAIResponses,
			want:     []string{LevelOff, LevelLow, LevelMedium, LevelHigh},
		},
		{
			name:     "openai completions toggle is on/off",
			model:    meta(true, ReasoningToggle),
			protocol: ProtocolOpenAICompletions,
			want:     []string{LevelOff, LevelHigh},
		},
		{
			name:     "openai non-reasoning model can turn off",
			model:    meta(false),
			protocol: ProtocolOpenAICompletions,
			want:     []string{LevelOff},
		},
		{
			// Values only matter for the effort option; a stray list on a toggle
			// model must not leak levels in.
			name: "values are ignored when effort is absent",
			model: ModelMeta{
				Reasoning:        true,
				ReasoningOptions: []string{ReasoningToggle},
				ReasoningValues:  []string{"low", "high"},
			},
			protocol: ProtocolOpenAICompletions,
			want:     []string{LevelOff, LevelHigh},
		},
		{
			// reasoning=true but no option types (1202 of the 7843 real models,
			// mostly gateways): nothing can be offered, and nothing is guessed.
			name:     "openai reasoning without option types",
			model:    meta(true),
			protocol: ProtocolOpenAICompletions,
			want:     []string{},
		},
		{
			name:     "unknown protocol is not guessed",
			model:    meta(true, ReasoningEffort),
			protocol: "",
			want:     nil,
		},
		{
			name:     "unsupported protocol is not guessed",
			model:    meta(true, ReasoningEffort),
			protocol: "gemini-generate-content",
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReasoningLevels(tc.model, tc.protocol)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ReasoningLevels(options=%v values=%v, %q) = %v, want %v",
					tc.model.ReasoningOptions, tc.model.ReasoningValues, tc.protocol, got, tc.want)
			}
			if tc.want == nil && got != nil {
				t.Errorf("ReasoningLevels returned %#v, want nil for an undecided protocol", got)
			}
		})
	}
}

// TestReasoningLevelsNeverOffersUnlistedEffortNames pins the fail-closed
// direction: every graded level returned for an effort model must be one the
// vendor actually publishes (or the documented three-name fallback when it
// publishes nothing).
func TestReasoningLevelsNeverOffersUnlistedEffortNames(t *testing.T) {
	model := ModelMeta{
		Reasoning:        true,
		ReasoningOptions: []string{ReasoningEffort},
		ReasoningValues:  []string{"low", "high"},
	}
	got := ReasoningLevels(model, ProtocolOpenAICompletions)
	if !slices.Equal(got, []string{LevelLow, LevelHigh}) {
		t.Fatalf("ReasoningLevels = %v, want [low high] for values [low high]", got)
	}
	if slices.Contains(got, LevelMedium) || slices.Contains(got, LevelMinimal) {
		t.Error("a level the vendor does not publish was offered")
	}
}

func TestReasoningLevelsDoesNotMutateModel(t *testing.T) {
	model := ModelMeta{Reasoning: true, ReasoningOptions: []string{ReasoningEffort}}
	_ = ReasoningLevels(model, ProtocolAnthropicMessages)
	if !slices.Equal(model.ReasoningOptions, []string{ReasoningEffort}) {
		t.Errorf("ReasoningOptions = %v after the call, want it untouched", model.ReasoningOptions)
	}
}

func TestDefaultReasoningBudgets(t *testing.T) {
	want := map[string]int{LevelLow: 2048, LevelMedium: 8192, LevelHigh: 16384}
	for level, tokens := range want {
		if DefaultReasoningBudgets[level] != tokens {
			t.Errorf("DefaultReasoningBudgets[%s] = %d, want %d", level, DefaultReasoningBudgets[level], tokens)
		}
	}
	if _, ok := DefaultReasoningBudgets[LevelMinimal]; ok {
		t.Error("DefaultReasoningBudgets has an entry for minimal, but budget_tokens never offers it")
	}
}

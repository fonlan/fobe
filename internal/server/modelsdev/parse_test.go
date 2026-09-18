package modelsdev

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureBytes is the small api.json the tests serve and cache.
func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "api.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func parseFixture(t *testing.T) *Index {
	t.Helper()
	ix, err := Parse(strings.NewReader(string(fixtureBytes(t))))
	if err != nil {
		t.Fatalf("Parse(fixture): %v", err)
	}
	return ix
}

func TestParseFixture(t *testing.T) {
	ix := parseFixture(t)

	if got, want := ix.ProviderCount(), 6; got != want {
		t.Errorf("ProviderCount() = %d, want %d", got, want)
	}
	if got, want := ix.ModelCount(), 9; got != want {
		t.Errorf("ModelCount() = %d, want %d", got, want)
	}
}

func TestParseProviderMeta(t *testing.T) {
	ix := parseFixture(t)

	anthropic, ok := ix.Provider("anthropic")
	if !ok {
		t.Fatal("Provider(anthropic) = miss, want hit")
	}
	if anthropic.ID != "anthropic" || anthropic.Name != "Anthropic" || anthropic.NPM != "@ai-sdk/anthropic" {
		t.Errorf("Provider(anthropic) = %+v, want id/name/npm filled", anthropic)
	}
	// 26 of the 221 real providers list no "api" at all (anthropic and openai
	// among them): an empty API must survive as empty, not as a fabricated URL.
	if anthropic.API != "" {
		t.Errorf("Provider(anthropic).API = %q, want empty (models.dev lists none)", anthropic.API)
	}

	gateway, ok := ix.Provider("tokengo")
	if !ok {
		t.Fatal("Provider(tokengo) = miss, want hit")
	}
	if gateway.API != "https://api.tokengo.dev/v1" {
		t.Errorf("Provider(tokengo).API = %q, want the document's api field", gateway.API)
	}
	if gateway.Name != "TokenGo" || gateway.NPM != "@ai-sdk/openai-compatible" {
		t.Errorf("Provider(tokengo) = %+v, want name/npm from the document", gateway)
	}

	// A provider with no models is still a provider (the slug picker lists it).
	if p, ok := ix.Provider("nometa"); !ok || p.Name != "Provider without api/npm" {
		t.Errorf("Provider(nometa) = %+v, %v; want the provider itself", p, ok)
	}
	if _, ok := ix.Provider("does-not-exist"); ok {
		t.Error("Provider(does-not-exist) = hit, want miss")
	}
	if _, ok := ix.Provider(""); ok {
		t.Error(`Provider("") = hit, want miss`)
	}
}

func TestParseModelMeta(t *testing.T) {
	ix := parseFixture(t)

	meta, ok := ix.Lookup("anthropic", "claude-sonnet-4-6")
	if !ok {
		t.Fatal("Lookup(anthropic/claude-sonnet-4-6) = miss, want hit")
	}
	if meta.ID != "claude-sonnet-4-6" || meta.Name != "Claude Sonnet 4.6" {
		t.Errorf("meta = %+v, want id and name", meta)
	}
	if !meta.Reasoning || !meta.ToolCall {
		t.Errorf("meta = %+v, want reasoning and tool_call true", meta)
	}
	if got, want := meta.ReasoningOptions, []string{ReasoningEffort, ReasoningBudgetTokens}; !slices.Equal(got, want) {
		t.Errorf("ReasoningOptions = %v, want %v", got, want)
	}
	// The vendor's own effort names are kept (including the ones outside the
	// panel vocabulary): they decide which levels may be offered.
	wantValues := []string{"low", "medium", "high", "max"}
	if !slices.Equal(meta.ReasoningValues, wantValues) {
		t.Errorf("ReasoningValues = %v, want %v", meta.ReasoningValues, wantValues)
	}
	if meta.ContextWindow != 1000000 || meta.MaxOutputTokens != 128000 {
		t.Errorf("limit = %d/%d, want 1000000/128000", meta.ContextWindow, meta.MaxOutputTokens)
	}
	if got, want := meta.InputModalities, []string{"text", "image", "pdf"}; !slices.Equal(got, want) {
		t.Errorf("InputModalities = %v, want %v", got, want)
	}
	if got, want := meta.OutputModalities, []string{"text"}; !slices.Equal(got, want) {
		t.Errorf("OutputModalities = %v, want %v", got, want)
	}

	// Non-reasoning models keep empty (never nil) lists so JSON stays [].
	haiku, ok := ix.Lookup("anthropic", "claude-3-haiku-20240307")
	if !ok {
		t.Fatal("Lookup(anthropic/claude-3-haiku-20240307) = miss, want hit")
	}
	if haiku.Reasoning {
		t.Error("claude-3-haiku Reasoning = true, want false")
	}
	if haiku.ReasoningOptions == nil || len(haiku.ReasoningOptions) != 0 {
		t.Errorf("ReasoningOptions = %#v, want an empty non-nil slice", haiku.ReasoningOptions)
	}
	if haiku.ReasoningValues == nil || len(haiku.ReasoningValues) != 0 {
		t.Errorf("ReasoningValues = %#v, want an empty non-nil slice", haiku.ReasoningValues)
	}
	if haiku.InputModalities == nil || haiku.OutputModalities == nil {
		t.Error("modalities = nil, want empty non-nil slices")
	}
}

// TestLookupDrivesReasoningLevels walks the whole path the panel uses: resolve a
// model by exact match, then ask which levels it offers.
func TestLookupDrivesReasoningLevels(t *testing.T) {
	ix := parseFixture(t)

	cases := []struct {
		slug, model string
		protocol    string
		want        []string
	}{
		// values publish minimal → minimal is offered; no "none" → no off.
		{"openai", "gpt-5", ProtocolOpenAIResponses, []string{LevelMinimal, LevelLow, LevelMedium, LevelHigh}},
		// The gateway copy lists only low/medium/high.
		{"tokengo", "gpt-5", ProtocolOpenAICompletions, []string{LevelLow, LevelMedium, LevelHigh}},
		// values contain none → this gateway really can switch thinking off.
		{"tokengo", "deepseek/deepseek-v3.2", ProtocolOpenAICompletions, []string{LevelOff, LevelLow, LevelHigh}},
		// Anthropic is opt-in, so off is always there; "max" is not a panel level.
		{"anthropic", "claude-sonnet-4-6", ProtocolAnthropicMessages, []string{LevelOff, LevelLow, LevelMedium, LevelHigh}},
		// An effort option with no values falls back to the three generic names.
		{"azure", "gpt-5", ProtocolOpenAIResponses, []string{LevelLow, LevelMedium, LevelHigh}},
		// budget_tokens only.
		{"google-vertex", "gemini-3-pro", ProtocolOpenAICompletions, []string{LevelLow, LevelMedium, LevelHigh}},
		// Non-reasoning model: off is the only honest choice.
		{"anthropic", "claude-3-haiku-20240307", ProtocolAnthropicMessages, []string{LevelOff}},
	}
	for _, tc := range cases {
		meta, ok := ix.Lookup(tc.slug, tc.model)
		if !ok {
			t.Fatalf("Lookup(%s/%s) = miss, want hit", tc.slug, tc.model)
		}
		if got := ReasoningLevels(meta, tc.protocol); !slices.Equal(got, tc.want) {
			t.Errorf("ReasoningLevels(%s/%s, %s) = %v, want %v", tc.slug, tc.model, tc.protocol, got, tc.want)
		}
	}
}

func TestLookupIsExactPerProvider(t *testing.T) {
	ix := parseFixture(t)

	// The same id exists in three providers with different limits: matching must
	// follow the slug, never a global id search.
	openai, ok := ix.Lookup("openai", "gpt-5")
	if !ok {
		t.Fatal("Lookup(openai/gpt-5) = miss, want hit")
	}
	gateway, ok := ix.Lookup("tokengo", "gpt-5")
	if !ok {
		t.Fatal("Lookup(tokengo/gpt-5) = miss, want hit")
	}
	azure, ok := ix.Lookup("azure", "gpt-5")
	if !ok {
		t.Fatal("Lookup(azure/gpt-5) = miss, want hit")
	}
	if openai.ContextWindow != 400000 || gateway.ContextWindow != 128000 || azure.ContextWindow != 400000 {
		t.Errorf("context windows = %d/%d/%d, want 400000/128000/400000",
			openai.ContextWindow, gateway.ContextWindow, azure.ContextWindow)
	}
	if gateway.ToolCall {
		t.Error("tokengo/gpt-5 ToolCall = true, want the gateway's own (false) value")
	}

	// Model ids containing a slash are ordinary ids here (4503 of the real ones
	// do) and must match as-is.
	if _, ok := ix.Lookup("tokengo", "qwen/qwen3.5-397b-a17b"); !ok {
		t.Error("Lookup(tokengo/qwen/qwen3.5-397b-a17b) = miss, want hit")
	}
	if _, ok := ix.Lookup("tokengo", "qwen3.5-397b-a17b"); ok {
		t.Error("Lookup with a shortened id = hit, want miss (no fuzzy matching)")
	}

	// Missing id field: fall back to the JSON key so the entry stays reachable.
	orphan, ok := ix.Lookup("tokengo", "no-id-model")
	if !ok {
		t.Fatal("Lookup(tokengo/no-id-model) = miss, want hit")
	}
	if orphan.ID != "no-id-model" {
		t.Errorf("orphan.ID = %q, want the JSON key", orphan.ID)
	}

	if _, ok := ix.Lookup("openai", "claude-sonnet-4-6"); ok {
		t.Error("cross-provider lookup = hit, want miss")
	}
	if _, ok := ix.Lookup("nope", "gpt-5"); ok {
		t.Error("unknown provider = hit, want miss")
	}
	if _, ok := ix.Lookup("", ""); ok {
		t.Error(`Lookup("", "") = hit, want miss`)
	}
}

func TestLookupTrimsFormInput(t *testing.T) {
	ix := parseFixture(t)
	if _, ok := ix.Lookup(" anthropic ", " claude-sonnet-4-6\n"); !ok {
		t.Error("Lookup with surrounding whitespace = miss, want hit")
	}
}

func TestIndexMethodsAreNilSafe(t *testing.T) {
	var ix *Index
	if _, ok := ix.Lookup("anthropic", "claude-sonnet-4-6"); ok {
		t.Error("nil Index Lookup = hit, want miss")
	}
	if _, ok := ix.Provider("anthropic"); ok {
		t.Error("nil Index Provider = hit, want miss")
	}
	if ix.ProviderCount() != 0 || ix.ModelCount() != 0 {
		t.Error("nil Index counts = non-zero, want 0")
	}
	if got := ix.Providers(); len(got) != 0 {
		t.Errorf("nil Index Providers() = %v, want empty", got)
	}
	if got := ix.Models("anthropic"); len(got) != 0 {
		t.Errorf("nil Index Models() = %v, want empty", got)
	}
}

func TestIndexEnumeration(t *testing.T) {
	ix := parseFixture(t)

	providers := ix.Providers()
	if len(providers) != ix.ProviderCount() {
		t.Fatalf("Providers() = %d entries, want %d", len(providers), ix.ProviderCount())
	}
	for i := 1; i < len(providers); i++ {
		if providers[i-1].ID >= providers[i].ID {
			t.Fatalf("Providers() is not sorted by slug: %q before %q", providers[i-1].ID, providers[i].ID)
		}
	}
	if providers[0].ID != "anthropic" {
		t.Errorf("first provider = %q, want anthropic (sorted)", providers[0].ID)
	}

	models := ix.Models("anthropic")
	if len(models) != 2 {
		t.Fatalf("Models(anthropic) = %d entries, want 2", len(models))
	}
	if models[0].ID != "claude-3-haiku-20240307" || models[1].ID != "claude-sonnet-4-6" {
		t.Errorf("Models(anthropic) = %q/%q, want them sorted by id", models[0].ID, models[1].ID)
	}

	// A provider with no models, and an unknown slug, both give an empty list
	// rather than nil.
	if got := ix.Models("nometa"); got == nil || len(got) != 0 {
		t.Errorf("Models(nometa) = %#v, want an empty non-nil slice", got)
	}
	if got := ix.Models("does-not-exist"); got == nil || len(got) != 0 {
		t.Errorf("Models(does-not-exist) = %#v, want an empty non-nil slice", got)
	}

	// The returned slices are copies: mutating them must not touch the snapshot.
	models[0].ID = "mutated"
	if again := ix.Models("anthropic"); again[0].ID != "claude-3-haiku-20240307" {
		t.Error("Models() handed out the snapshot's own storage")
	}
	providers[0].Name = "mutated"
	if p, _ := ix.Provider("anthropic"); p.Name != "Anthropic" {
		t.Error("Providers() handed out the snapshot's own storage")
	}
}

func TestParseRejectsUnusableDocuments(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"html error page", "<!doctype html><html><body>404</body></html>"},
		{"top-level array", `[]`},
		{"top-level string", `"nope"`},
		{"empty body", ``},
		{"truncated document", `{"anthropic":{"id":"anthropic","models":{"m":{"id":"m"}}}`},
		{"provider is not an object", `{"anthropic":42}`},
		{"wrong field type", `{"anthropic":{"name":"Anthropic","models":{"m":{"limit":{"context":"many"}}}}}`},
		{"trailing html", `{"anthropic":{"id":"anthropic","models":{"m":{"id":"m"}}}}<html>oops</html>`},
		{"two documents concatenated", `{"a":{"id":"a","models":{"m":{"id":"m"}}}}{"b":{"id":"b"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(tc.body)); err == nil {
				t.Fatalf("Parse(%s) = nil error, want failure", tc.name)
			}
		})
	}
}

func TestParseRejectsEmptyProviderSet(t *testing.T) {
	// A mirror answering "{}" must not displace a working cache.
	if _, err := Parse(strings.NewReader(`{}`)); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Parse({}) = %v, want ErrEmpty", err)
	}
}

func TestParseAcceptsDocumentWithOnlyUnknownFields(t *testing.T) {
	// Every field the panel does not use is skipped, not rejected.
	const doc = `{"p":{"id":"p","name":"P","doc":"x",` +
		`"models":{"m":{"id":"m","cost":{"input":1}}}}}`
	ix, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	meta, ok := ix.Lookup("p", "m")
	if !ok {
		t.Fatal("Lookup(p/m) = miss, want hit")
	}
	if meta.Reasoning || meta.ContextWindow != 0 {
		t.Errorf("meta = %+v, want zero values for absent fields", meta)
	}
}

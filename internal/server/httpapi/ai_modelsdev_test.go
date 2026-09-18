package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/modelsdev"
	"github.com/fonlan/fobe/internal/server/store"
)

// newModelsDevTestManager points a manager at the modelsdev package's fixture,
// laid out exactly like the real cache (<dir>/models/api.json), with the network
// switched off: these tests must not depend on models.dev being reachable, nor
// on what it currently publishes.
func newModelsDevTestManager(t *testing.T) *modelsdev.Manager {
	t.Helper()
	dir := t.TempDir()
	target := modelsdev.CachePath(dir)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "modelsdev", "testdata", "api.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	m := modelsdev.New(modelsdev.Config{Dir: dir, DisableAutoUpdate: true})
	if err := m.LoadCache(); err != nil {
		t.Fatal(err)
	}
	if !m.Loaded() {
		t.Fatal("models.dev fixture did not load")
	}
	return m
}

func TestModelsDevStatusListsSlugsWithProtocolHints(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	api.ModelsDev = newModelsDevTestManager(t)
	cookie := loginCookie(t, server.URL)

	resp, raw := doAuthed(t, "GET", server.URL+"/api/ai/modelsdev", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", resp.StatusCode, raw)
	}
	var view modelsDevStatusView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if !view.Loaded || view.Providers == 0 {
		t.Fatalf("cache not reported as loaded: %+v", view)
	}
	hints := map[string]string{}
	for _, slug := range view.Slugs {
		hints[slug.Slug] = slug.Protocol
	}
	if hints["anthropic"] != store.ProtocolAnthropicMessages {
		t.Fatalf("anthropic npm hint = %q", hints["anthropic"])
	}
	if hints["tokengo"] != store.ProtocolOpenAICompletions {
		t.Fatalf("openai-compatible npm hint = %q", hints["tokengo"])
	}
	// Azure has an npm hint we deliberately do not map: pre-filling a protocol
	// we cannot speak would produce a provider that fails on first use.
	if hints["azure"] != "" {
		t.Fatalf("azure should force a manual protocol choice, got %q", hints["azure"])
	}
}

func TestMatchAIModelsFillsMetadataAndFreezesEditedFields(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	api.ModelsDev = newModelsDevTestManager(t)
	cookie := loginCookie(t, server.URL)

	// A gateway provider with a slug. tokengo serves its OWN copy of "gpt-5"
	// with a 128k context while openai's is 400k — using the fixture's two
	// divergent copies is what makes an accidental fuzzy match fail this test.
	if err := api.Store.UpsertAIProvider(&store.AIProvider{
		ID: "aip-gw", Name: "gateway", Protocol: store.ProtocolOpenAICompletions,
		BaseURL: "https://gw.example/v1", ModelsDevSlug: "tokengo",
		Enabled: true, CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}

	resp, raw := doAuthed(t, "POST", server.URL+"/api/ai/models/match", cookie,
		[]byte(`{"provider_id":"aip-gw","model_ids":["gpt-5","definitely-not-a-model"]}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("match: %d %s", resp.StatusCode, raw)
	}
	var result aiMatchResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "gpt-5" {
		t.Fatalf("applied = %+v", result.Applied)
	}
	if len(result.Unmatched) != 1 || result.Unmatched[0] != "definitely-not-a-model" {
		t.Fatalf("unmatched = %+v (an id the slug does not serve is a normal outcome)", result.Unmatched)
	}

	model, err := api.Store.GetAIModel("gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if model.ContextWindow != 128000 || model.MaxOutputTokens != 16384 {
		t.Fatalf("metadata came from the wrong provider copy: %+v", model)
	}
	if model.DisplayName != "GPT-5 (gateway copy)" || model.Source != "models_dev" {
		t.Fatalf("display name/source not applied: %+v", model)
	}
	if len(model.ReasoningLevels) == 0 {
		t.Fatalf("reasoning levels not derived: %+v", model)
	}

	// The operator corrects the context window by hand (their gateway clips it
	// further) and freezes that one field.
	model.ContextWindow = 65536
	model.OverriddenFields = []string{"context_window"}
	if err := api.Store.UpsertAIModel(model); err != nil {
		t.Fatal(err)
	}

	resp, raw = doAuthed(t, "POST", server.URL+"/api/ai/models/match", cookie,
		[]byte(`{"provider_id":"aip-gw","model_ids":["gpt-5"]}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-match: %d %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Frozen) != 1 || result.Frozen[0] != "gpt-5:context_window" {
		t.Fatalf("frozen fields not reported: %+v", result.Frozen)
	}
	after, err := api.Store.GetAIModel("gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if after.ContextWindow != 65536 {
		t.Fatalf("hand-edited context window was overwritten: %d", after.ContextWindow)
	}
	// Unfrozen fields still follow the metadata: freezing one field must not
	// freeze the row.
	if after.MaxOutputTokens != 16384 || after.DisplayName != "GPT-5 (gateway copy)" {
		t.Fatalf("a frozen field froze the whole row: %+v", after)
	}
}

func TestMatchAIModelsWithoutSlugIsRefused(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	api.ModelsDev = newModelsDevTestManager(t)
	cookie := loginCookie(t, server.URL)

	if err := api.Store.UpsertAIProvider(&store.AIProvider{
		ID: "aip-noslug", Name: "self-hosted", Protocol: store.ProtocolOpenAICompletions,
		BaseURL: "https://self.example/v1", Enabled: true,
		CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}
	resp, raw := doAuthed(t, "POST", server.URL+"/api/ai/models/match", cookie,
		[]byte(`{"provider_id":"aip-noslug","model_ids":["gpt-5"]}`))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "provider_has_no_slug") {
		t.Fatalf("got %d %s, want 400 provider_has_no_slug", resp.StatusCode, raw)
	}
}

// TestAIEffectiveLevelsAppliesProtocolRule pins the §12.5 rule that makes the
// global model row workable: levels are stored protocol-independently, and
// Anthropic (where omitting `thinking` IS the off switch) always gains `off`,
// while the OpenAI dialects keep exactly what the model documents — calling the
// lowest effort "off" would be a lie the operator cannot see.
func TestAIEffectiveLevelsAppliesProtocolRule(t *testing.T) {
	stored := []string{"low", "medium", "high"}
	got := aiEffectiveLevels(stored, store.ProtocolAnthropicMessages)
	want := []string{"off", "low", "medium", "high"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("anthropic levels = %v, want %v", got, want)
	}
	got = aiEffectiveLevels(stored, store.ProtocolOpenAIResponses)
	if strings.Join(got, ",") != "low,medium,high" {
		t.Fatalf("openai levels = %v, want no off", got)
	}
	// A toggle-style model already documents off; the Anthropic rule must not
	// duplicate it, and ordering stays off-first.
	got = aiEffectiveLevels([]string{"off", "high"}, store.ProtocolAnthropicMessages)
	if strings.Join(got, ",") != "off,high" {
		t.Fatalf("toggle model levels = %v", got)
	}
}

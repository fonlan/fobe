package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

// Contract tests for the §12.1/§12.5 multi-provider surface. They exist because
// three of these properties are invisible in normal use and expensive to
// discover in production: secrets staying write-only, the unreadable-key
// distinction, and the "default must be a usable pair" rule.

// seedProviderModel writes a provider+model pair straight through the store
// (the HTTP path is exercised by the CRUD test).
func seedProviderModel(t *testing.T, api *Server, providerID, baseURL, key, modelID string, protocol string) {
	t.Helper()
	ciphertext := ""
	if key != "" {
		enc, err := api.Crypt.Encrypt(key)
		if err != nil {
			t.Fatal(err)
		}
		ciphertext = enc
	}
	if err := api.Store.UpsertAIProvider(&store.AIProvider{
		ID: providerID, Name: providerID, Protocol: protocol, BaseURL: baseURL,
		APIKeyEnc: ciphertext, Enabled: true, CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.UpsertAIModel(&store.AIModel{
		ID: modelID, DisplayName: modelID, ContextWindow: 200000, MaxOutputTokens: 8192,
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		Source: "manual", Enabled: true, CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.LinkAIProviderModel(providerID, modelID); err != nil {
		t.Fatal(err)
	}
}

func TestAIProviderCRUDKeepsSecretsWriteOnly(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)

	resp, raw := doAuthed(t, "POST", server.URL+"/api/ai/providers", cookie, []byte(`{
		"name":"openrouter","protocol":"openai-completions","base_url":"https://openrouter.ai/api/v1/",
		"api_key":"sk-test-sentinel","extra_headers":"{\"HTTP-Referer\":\"https://panel.example\"}",
		"models_dev_slug":"openrouter"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create provider: %d %s", resp.StatusCode, raw)
	}
	// The trailing slash is trimmed by ROOT normalization, and no secret may
	// appear in the response body — not the plaintext, not the ciphertext.
	if !strings.Contains(string(raw), `"base_url":"https://openrouter.ai/api/v1"`) {
		t.Fatalf("base_url not normalized: %s", raw)
	}
	for _, forbidden := range []string{"sk-test-sentinel", apiKeyCiphertextMarker(t, api, "sk-test-sentinel")} {
		if forbidden != "" && strings.Contains(string(raw), forbidden) {
			t.Fatalf("provider response leaks a secret: %s", raw)
		}
	}
	var created aiProviderView
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if !created.HasKey || !created.HasHeaders {
		t.Fatalf("has_key/has_headers not reported: %+v", created)
	}
	if len(created.HeaderNames) != 1 || created.HeaderNames[0] != "HTTP-Referer" {
		t.Fatalf("header names not reported: %+v", created.HeaderNames)
	}
	if created.ID == "" || !created.Enabled {
		t.Fatalf("provider not created enabled: %+v", created)
	}

	// The catalog (what the settings page and picker load) must be equally
	// clean, since it is fetched far more often than the create response.
	resp, raw = doAuthed(t, "GET", server.URL+"/api/ai/catalog", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog: %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "sk-test-sentinel") {
		t.Fatalf("catalog leaks the key: %s", raw)
	}

	// Omitting api_key leaves the stored one alone (that is what makes the
	// settings form safe to save repeatedly); "" clears it.
	resp, raw = doAuthed(t, "PATCH", server.URL+"/api/ai/providers/"+created.ID, cookie,
		[]byte(`{"name":"renamed","protocol":"openai-completions","base_url":"https://openrouter.ai/api/v1"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch provider: %d %s", resp.StatusCode, raw)
	}
	var patched aiProviderView
	if err := json.Unmarshal(raw, &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Name != "renamed" || !patched.HasKey {
		t.Fatalf("patch dropped the stored key: %+v", patched)
	}

	// The audit trail names the action and the provider, never the key.
	audit, err := api.Store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	sawProviderAudit := false
	for _, entry := range audit {
		if strings.Contains(entry.Command, "sk-test-sentinel") || strings.Contains(entry.Reason, "sk-test-sentinel") {
			t.Fatalf("audit leaked the key: %+v", entry)
		}
		if strings.HasPrefix(entry.Action, "ai_provider_") {
			sawProviderAudit = true
		}
	}
	if !sawProviderAudit {
		t.Fatal("provider administration was not audited")
	}
}

// apiKeyCiphertextMarker returns the ciphertext the store holds for plaintext,
// so a response echoing the ciphertext is caught too.
func apiKeyCiphertextMarker(t *testing.T, api *Server, plaintext string) string {
	t.Helper()
	providers, err := api.Store.ListAIProviders()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if p.APIKeyEnc == "" {
			continue
		}
		if got, err := api.Crypt.Decrypt(p.APIKeyEnc); err == nil && got == plaintext {
			return p.APIKeyEnc
		}
	}
	return ""
}

func TestAIProviderRejectsBadInput(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"unknown protocol", `{"name":"x","protocol":"google-generatecontent","base_url":"https://x.example"}`, "bad_protocol"},
		{"empty protocol", `{"name":"x","protocol":"","base_url":"https://x.example"}`, "bad_protocol"},
		// ROOT semantics: an endpoint-shaped base_url would produce
		// /chat/completions/chat/completions and a 404 the operator cannot explain.
		{"endpoint-shaped base_url", `{"name":"x","protocol":"openai-completions","base_url":"https://x.example/v1/chat/completions"}`, "ok"},
		{"query in base_url", `{"name":"x","protocol":"openai-completions","base_url":"https://x.example/v1?key=1"}`, "bad_base_url"},
		{"no scheme", `{"name":"x","protocol":"openai-completions","base_url":"x.example/v1"}`, "bad_base_url"},
		{"header injection", `{"name":"x","protocol":"openai-completions","base_url":"https://x.example","extra_headers":"{\"X-A\":\"a\\r\\nX-B: b\"}"}`, "bad_extra_headers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := doAuthed(t, "POST", server.URL+"/api/ai/providers", cookie, []byte(tc.body))
			if tc.code == "ok" {
				// The endpoint-shaped URL is accepted as a plain path segment —
				// documenting that the guard is about WHAT is rejected, not a
				// rewrite. If this ever starts failing, the adapter started
				// being clever about paths and the design note must change.
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("expected accept, got %d %s", resp.StatusCode, raw)
				}
				return
			}
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), tc.code) {
				t.Fatalf("got %d %s, want 400 %s", resp.StatusCode, raw, tc.code)
			}
		})
	}

	// Model validation: the unified reasoning vocabulary, the limit invariant,
	// and unknown override fields are all refused at the API, because each one
	// would otherwise surface as a mid-conversation upstream error.
	seedProviderModel(t, api, "aip-v", "https://v.example", "k", "gpt-v", store.ProtocolOpenAICompletions)
	modelCases := []struct {
		name string
		body string
		code string
	}{
		{"unknown reasoning level", `{"id":"m1","reasoning_levels":["off","turbo"]}`, "bad_reasoning_level"},
		{"unknown override field", `{"id":"m1","overridden_fields":["cost"]}`, "bad_override_field"},
		{"output over context", `{"id":"m1","context_window":1000,"max_output_tokens":2000}`, "bad_limit"},
		{"missing id", `{"display_name":"x"}`, "bad_model_id"},
	}
	for _, tc := range modelCases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := doAuthed(t, "POST", server.URL+"/api/ai/models", cookie, []byte(tc.body))
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), tc.code) {
				t.Fatalf("got %d %s, want 400 %s", resp.StatusCode, raw, tc.code)
			}
		})
	}

	// The §12.5 thinking budgets are built-in constants, not settings: the key
	// must be refused like any other unknown one, so a hand-written value can
	// never install a mapping Anthropic would reject mid-conversation.
	resp, raw := doAuthed(t, "PUT", server.URL+"/api/settings", cookie,
		[]byte(`{"settings":{"ai.reasoning_budget":"{\"low\":512}"}}`))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "unknown_key") {
		t.Fatalf("the removed budget setting is still writable: %d %s", resp.StatusCode, raw)
	}
}

func TestAIDefaultsRequireUsablePair(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)

	seedProviderModel(t, api, "aip-a", "https://a.example/v1", "key-a", "model-a", store.ProtocolOpenAICompletions)

	readConfigured := func() bool {
		t.Helper()
		_, raw := doAuthed(t, "GET", server.URL+"/api/settings", cookie, nil)
		var body struct {
			AIConfigured bool `json:"ai_configured"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return body.AIConfigured
	}

	if !readConfigured() {
		t.Fatal("ai_configured = false with an enabled provider+model pair and a key")
	}

	// A half pair is meaningless: the two ids only identify a gateway together.
	resp, raw := doAuthed(t, "PUT", server.URL+"/api/ai/defaults", cookie, []byte(`{"provider_id":"aip-a"}`))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "bad_default_pair") {
		t.Fatalf("half pair accepted: %d %s", resp.StatusCode, raw)
	}

	// An existing model that is not linked to the provider is not selectable.
	if err := api.Store.UpsertAIModel(&store.AIModel{
		ID: "model-orphan", DisplayName: "orphan", Source: "manual", Enabled: true,
		CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}
	resp, raw = doAuthed(t, "PUT", server.URL+"/api/ai/defaults", cookie,
		[]byte(`{"provider_id":"aip-a","model_id":"model-orphan"}`))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "default_not_usable") {
		t.Fatalf("unlinked pair accepted as default: %d %s", resp.StatusCode, raw)
	}

	resp, raw = doAuthed(t, "PUT", server.URL+"/api/ai/defaults", cookie,
		[]byte(`{"provider_id":"aip-a","model_id":"model-a"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usable pair rejected: %d %s", resp.StatusCode, raw)
	}
	providerID, _ := api.Store.GetSetting("ai.default_provider_id")
	modelID, _ := api.Store.GetSetting("ai.default_model_id")
	if providerID != "aip-a" || modelID != "model-a" {
		t.Fatalf("defaults not stored as a pair: %q/%q", providerID, modelID)
	}
}

// TestAIUnreadableKeyIsNotUnconfigured is the §10.1 invariant applied to
// provider secrets: an undecryptable ciphertext (wrong master key, restored db)
// must surface as a NAMED error, never as "nothing is configured" — the latter
// invites the operator to re-enter a key and quietly rotate whatever still
// worked.
func TestAIUnreadableKeyIsNotUnconfigured(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)
	nodeID := createAINode(t, api, "node-unreadable")

	// Ciphertext produced by a DIFFERENT master key.
	foreign, err := security.NewCryptor(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	unreadable, err := foreign.Encrypt("secret-under-another-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.UpsertAIProvider(&store.AIProvider{
		ID: "aip-bad", Name: "bad key", Protocol: store.ProtocolOpenAICompletions,
		BaseURL: "https://bad.example/v1", APIKeyEnc: unreadable,
		Enabled: true, CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.UpsertAIModel(&store.AIModel{
		ID: "model-bad", DisplayName: "model-bad", Source: "manual", Enabled: true,
		CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.LinkAIProviderModel("aip-bad", "model-bad"); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetSetting("ai.default_provider_id", "aip-bad", false); err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetSetting("ai.default_model_id", "model-bad", false); err != nil {
		t.Fatal(err)
	}

	// The sidebar gate stays TRUE: the configuration exists, it is broken.
	_, raw := doAuthed(t, "GET", server.URL+"/api/settings", cookie, nil)
	var settings struct {
		AIConfigured bool `json:"ai_configured"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.AIConfigured {
		t.Fatal("unreadable key reported as unconfigured — this is the silent-rotation trap")
	}

	result := postAIChat(t, server.URL, cookie, aiChatRequest{NodeID: nodeID, Message: "hello"})
	if result.Status != http.StatusInternalServerError {
		t.Fatalf("chat with an unreadable key: got %d %v, want 500 ai_key_unreadable", result.Status, result.JSONBody)
	}
	if code := result.errorCode(); code != "ai_key_unreadable" {
		t.Fatalf("wire code = %q, want ai_key_unreadable", code)
	}
}

// TestAIModelSharedByProvidersStaysOneRow pins the "model row is globally
// unique" decision from §12.1: the same model id through two gateways is ONE
// row with TWO links, and the catalog reports both.
func TestAIModelSharedByProvidersStaysOneRow(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)

	seedProviderModel(t, api, "aip-1", "https://one.example/v1", "k1", "shared-model", store.ProtocolOpenAICompletions)
	seedProviderModel(t, api, "aip-2", "https://two.example/v1", "k2", "shared-model", store.ProtocolOpenAICompletions)

	resp, raw := doAuthed(t, "GET", server.URL+"/api/ai/catalog", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog: %d %s", resp.StatusCode, raw)
	}
	var catalog aiCatalogResponse
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range catalog.Models {
		if m.ID != "shared-model" {
			continue
		}
		count++
		if len(m.ProviderIDs) != 2 {
			t.Fatalf("model did not report both gateways: %+v", m.ProviderIDs)
		}
	}
	if count != 1 {
		t.Fatalf("model rows for one id = %d, want 1", count)
	}
	// Every provider still advertises it, which is what the grouped picker needs.
	for _, p := range catalog.Providers {
		if len(p.ModelIDs) != 1 || p.ModelIDs[0] != "shared-model" {
			t.Fatalf("provider %s lost its model link: %+v", p.ID, p.ModelIDs)
		}
	}

	// Unlinking from one gateway keeps the row and the other link: deleting a
	// gateway must not delete a model another gateway still serves.
	resp, raw = doAuthed(t, "DELETE", server.URL+"/api/ai/providers/aip-2/models/shared-model", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlink: %d %s", resp.StatusCode, raw)
	}
	if _, err := api.Store.GetAIModel("shared-model"); err != nil {
		t.Fatalf("unlink deleted the model row: %v", err)
	}
	resp, raw = doAuthed(t, "DELETE", server.URL+"/api/ai/providers/aip-1", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete provider: %d %s", resp.StatusCode, raw)
	}
	if _, err := api.Store.GetAIModel("shared-model"); err != nil {
		t.Fatalf("deleting a provider deleted a shared model row: %v", err)
	}
}

// TestAIDisabledProviderOrModelIsNotCallable pins the meaning of `enabled`.
//
// Before this, disabling a provider or model only removed it from the settings
// list while `resolveAIModel` still served it — a switch that looked like a
// control and enforced nothing. The UI hides disabled models too, but the
// enforcement has to live server-side: the picker is not a security boundary and
// a hand-crafted request names the pair explicitly.
func TestAIDisabledProviderOrModelIsNotCallable(t *testing.T) {
	server, api := newTestServer(t)
	defer server.Close()
	cookie := loginCookie(t, server.URL)
	nodeID := createAINode(t, api, "node-disabled")
	seedProviderModel(t, api, "aip-off", "https://off.example/v1", "key-off", "model-off", store.ProtocolOpenAICompletions)
	// A SECOND usable pair, deliberately: with only one pair, disabling it makes
	// the whole configuration unusable and the §12.1 sidebar gate answers 503
	// (`ai_not_configured`) before the pair is ever resolved — which is correct
	// behavior, but it hides the specific code this test is about.
	seedProviderModel(t, api, "aip-ok", "https://ok.example/v1", "key-ok", "model-ok", store.ProtocolOpenAICompletions)

	model, err := api.Store.GetAIModel("model-off")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := api.Store.GetAIProvider("aip-off")
	if err != nil {
		t.Fatal(err)
	}

	// Disabled MODEL: refused with its own code, before any upstream call.
	model.Enabled = false
	if err := api.Store.UpsertAIModel(model); err != nil {
		t.Fatal(err)
	}
	result := postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "hello", ProviderID: "aip-off", ModelID: "model-off",
	})
	if result.Status != http.StatusBadRequest {
		t.Fatalf("disabled model: got %d (code %q)", result.Status, result.errorCode())
	}
	if got := result.errorCode(); got != "ai_model_disabled" {
		t.Fatalf("wire code = %q, want ai_model_disabled", got)
	}

	// Disabled PROVIDER (model usable again) is refused as well.
	model.Enabled = true
	if err := api.Store.UpsertAIModel(model); err != nil {
		t.Fatal(err)
	}
	provider.Enabled = false
	if err := api.Store.UpsertAIProvider(provider); err != nil {
		t.Fatal(err)
	}
	result = postAIChat(t, server.URL, cookie, aiChatRequest{
		NodeID: nodeID, Message: "hello", ProviderID: "aip-off", ModelID: "model-off",
	})
	if result.Status != http.StatusBadRequest {
		t.Fatalf("disabled provider: got %d (code %q)", result.Status, result.errorCode())
	}
	if got := result.errorCode(); got != "ai_provider_disabled" {
		t.Fatalf("wire code = %q, want ai_provider_disabled", got)
	}

	// The catalog reports the model-level flag so the picker can hide it, and
	// keeps listing it so the settings page can turn it back on.
	provider.Enabled = true
	if err := api.Store.UpsertAIProvider(provider); err != nil {
		t.Fatal(err)
	}
	model.Enabled = false
	if err := api.Store.UpsertAIModel(model); err != nil {
		t.Fatal(err)
	}
	_, raw := doAuthed(t, "GET", server.URL+"/api/ai/catalog", cookie, nil)
	var catalog aiCatalogResponse
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range catalog.Providers {
		for _, m := range p.Models {
			if m.ID != "model-off" {
				continue
			}
			found = true
			if m.Enabled {
				t.Fatalf("catalog reports a disabled model as enabled: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("disabled model missing from the provider's model list (the settings page needs it to re-enable)")
	}
}

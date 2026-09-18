package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/server/aiprotocol"
	"github.com/fonlan/fobe/internal/server/store"
)

// AI provider / model endpoints (design.md §12.1/§12.5, 2026-09-18).
//
// Wire contract notes that the frontend depends on:
//
//   - Provider secrets are WRITE-ONLY. Responses carry has_key / has_headers
//     and the header NAMES, never the ciphertext or the plaintext — the same
//     stance §4.4 takes for settings, and the reason a stolen GET response is
//     not a credential dump.
//   - api_key / extra_headers are *string: omitted (null) means "leave what is
//     stored", "" means "clear it". Without that distinction the settings form
//     could not express "I did not touch the key" and every save would wipe it.
//   - A provider whose key cannot be DECRYPTED is reported as an error, never
//     as "unconfigured" — turning an undecryptable ciphertext into a fresh
//     blank state is exactly the silent-rotation trap §10.1 warns about.

// aiProviderModelView is one model as seen THROUGH one provider.
//
// The picker needs the effective reasoning levels for the selected pair, and
// that set depends on the provider's protocol (Anthropic can always disable
// thinking because the field is opt-in; the OpenAI dialects cannot claim "off"
// for a model that only reasons). Computing it here keeps the rule in one place
// — re-implementing it in the frontend is exactly the duplication that drifts
// (§12.1 takes the same stance on the ai_configured gate).
type aiProviderModelView struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name"`
	ContextWindow   int      `json:"context_window"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	EffectiveLevels []string `json:"effective_levels"`
	// Enabled is carried so the MODEL PICKER can hide a model the operator
	// switched off, while the settings page (same payload) still lists it —
	// otherwise "disabled" would be an unenforceable label.
	Enabled bool `json:"enabled"`
}

// aiProviderView is the provider shape the panel renders.
type aiProviderView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Protocol      string   `json:"protocol"`
	BaseURL       string   `json:"base_url"`
	ModelsDevSlug string   `json:"models_dev_slug"`
	Enabled       bool     `json:"enabled"`
	HasKey        bool     `json:"has_key"`
	HasHeaders    bool     `json:"has_headers"`
	HeaderNames   []string `json:"header_names"`
	ModelIDs      []string `json:"model_ids"`
	// Models carries the same links with the per-protocol view the picker needs.
	Models    []aiProviderModelView `json:"models"`
	CreatedAt int64                 `json:"created_at"`
	UpdatedAt int64                 `json:"updated_at"`
	// Endpoint is the URL the protocol adapter will actually call. The form
	// shows it so a wrong base_url is visible before the first request instead
	// of surfacing as an opaque upstream 404.
	Endpoint string `json:"endpoint,omitempty"`
}

// aiProviderRequest is the write shape. Pointers distinguish "absent" from
// "cleared" for the secret fields (see the file comment).
type aiProviderRequest struct {
	Name          string  `json:"name"`
	Protocol      string  `json:"protocol"`
	BaseURL       string  `json:"base_url"`
	ModelsDevSlug string  `json:"models_dev_slug"`
	APIKey        *string `json:"api_key"`
	ExtraHeaders  *string `json:"extra_headers"`
	Enabled       *bool   `json:"enabled"`
}

// aiModelView is the model shape. A model is global: ModelIDs on the provider
// side and ProviderIDs here are the two halves of the many-to-many link.
type aiModelView struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	ContextWindow    int      `json:"context_window"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	ReasoningLevels  []string `json:"reasoning_levels"`
	// ReasoningOffStyle: how "off" is spelled on the wire for this model
	// (omit|none|disabled, §12.5). The panel cannot derive it from the level
	// list, and guessing sends a body the upstream rejects.
	ReasoningOffStyle string   `json:"reasoning_off_style"`
	OverriddenFields  []string `json:"overridden_fields"`
	Source            string   `json:"source"`
	Enabled           bool     `json:"enabled"`
	ProviderIDs       []string `json:"provider_ids"`
}

type aiModelRequest struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	ContextWindow     int      `json:"context_window"`
	MaxOutputTokens   int      `json:"max_output_tokens"`
	InputModalities   []string `json:"input_modalities"`
	OutputModalities  []string `json:"output_modalities"`
	ReasoningLevels   []string `json:"reasoning_levels"`
	ReasoningOffStyle string   `json:"reasoning_off_style"`
	OverriddenFields  []string `json:"overridden_fields"`
	Source            string   `json:"source"`
	Enabled           *bool    `json:"enabled"`
}

// aiCatalogResponse is what /api/ai/catalog returns: everything the settings
// page and the model picker need in ONE round trip. The picker is rendered on
// every terminal page mount, so three separate list calls would be three
// chances to render a half-loaded selector.
type aiCatalogResponse struct {
	Providers       []aiProviderView `json:"providers"`
	Models          []aiModelView    `json:"models"`
	DefaultProvider string           `json:"default_provider_id"`
	DefaultModel    string           `json:"default_model_id"`
}

// Unified reasoning level vocabulary (§12.5). The set a given model exposes is
// the intersection of what its protocol can express and its reasoning_options;
// the adapters translate these names, so a level stored here must be one the
// translation layer knows.
var aiReasoningLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true, "high": true,
}

// AI protocol endpoints, per §12.5. Kept next to the adapters' expectations:
// the settings form shows the final URL so a wrong base_url is visible before
// the first request instead of surfacing as a 404 from the upstream.
var aiProtocolPaths = map[string]string{
	store.ProtocolOpenAICompletions: "/chat/completions",
	store.ProtocolOpenAIResponses:   "/responses",
	store.ProtocolAnthropicMessages: "/messages",
}

// normalizeProviderBaseURL validates and canonicalizes a provider base_url.
// ROOT semantics (§12.5): the adapter appends the protocol path, so a base URL
// that already ends in /chat/completions is rejected rather than silently
// producing /chat/completions/chat/completions. Query and fragment are refused
// because they cannot survive path concatenation meaningfully.
func normalizeProviderBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme %q", u.Scheme)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("host/credentials/query/fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// decodeExtraHeaders validates the optional custom-header JSON. The value is
// stored as Cryptor ciphertext (§12.5): these headers routinely carry a second
// credential (OpenRouter's HTTP-Referer sits next to a real key), so treating
// them as non-secret would be a guess in the wrong direction.
func decodeExtraHeaders(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(raw), &headers); err != nil {
		return nil, err
	}
	for name, value := range headers {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("empty header name")
		}
		if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			// Header injection: a newline in either half lets a settings form
			// forge additional headers on the upstream request.
			return nil, fmt.Errorf("header %q contains a newline", name)
		}
	}
	return headers, nil
}

func headerNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// aiResolved is a provider+model pair ready to be called: secrets decrypted,
// protocol validated. The caller (adapter layer) owns the wire format.
type aiResolved struct {
	Provider store.AIProvider
	Model    store.AIModel
	APIKey   string
	Headers  map[string]string
}

// resolveAIModel loads one provider+model pair and decrypts its secrets.
// Empty providerID/modelID fall back to the panel default (§12.1: the default
// is a PAIR, because the same model id may be reachable through several
// gateways and only the pair identifies which one).
func (s *Server) resolveAIModel(providerID, modelID string) (*aiResolved, error) {
	if providerID == "" {
		providerID, _ = s.Store.GetSetting("ai.default_provider_id")
	}
	if modelID == "" {
		modelID, _ = s.Store.GetSetting("ai.default_model_id")
	}
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(modelID) == "" {
		return nil, errAINotConfigured
	}

	provider, apiKey, headers, err := s.resolveProvider(providerID)
	if err != nil {
		return nil, err
	}
	if !provider.Enabled {
		return nil, errAIProviderDisabled
	}
	model, err := s.Store.GetAIModel(modelID)
	if err != nil {
		return nil, err
	}
	if !model.Enabled {
		return nil, errAIModelDisabled
	}

	// Verify the pair is actually linked: a model that exists but is not
	// attached to this provider would otherwise be sent to a gateway that
	// never advertised it (the picker only offers linked pairs, so this is a
	// hand-crafted-request guard).
	linked, err := s.Store.ListAIModelsForProvider(providerID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, m := range linked {
		if m.ID == modelID {
			found = true
			break
		}
	}
	if !found {
		return nil, errAIModelNotLinked
	}

	return &aiResolved{Provider: *provider, Model: *model, APIKey: apiKey, Headers: headers}, nil
}

// resolveProvider loads one provider and decrypts its secrets. It exists apart
// from resolveAIModel because listing the upstream's models happens BEFORE any
// model row exists — that is the point of the call.
//
// An undecryptable secret is an OPERATOR problem (wrong master key, restored
// database), never a reason to behave as if nothing were configured: turning
// ciphertext we cannot read into a fresh blank state is the silent-rotation trap
// §10.1 forbids.
func (s *Server) resolveProvider(providerID string) (*store.AIProvider, string, map[string]string, error) {
	provider, err := s.Store.GetAIProvider(providerID)
	if err != nil {
		return nil, "", nil, err
	}
	if !store.ValidAIProtocol(provider.Protocol) {
		return nil, "", nil, errAIProtocol
	}

	apiKey := ""
	if provider.APIKeyEnc != "" {
		plain, err := s.Crypt.Decrypt(provider.APIKeyEnc)
		if err != nil {
			return nil, "", nil, fmt.Errorf("%w: provider %s key", errAIKeyUnreadable, provider.ID)
		}
		apiKey = plain
	}
	var headers map[string]string
	if provider.ExtraHeadersEnc != "" {
		plain, err := s.Crypt.Decrypt(provider.ExtraHeadersEnc)
		if err != nil {
			return nil, "", nil, fmt.Errorf("%w: provider %s headers", errAIKeyUnreadable, provider.ID)
		}
		headers, err = decodeExtraHeaders(plain)
		if err != nil {
			return nil, "", nil, fmt.Errorf("%w: provider %s headers", errAIKeyUnreadable, provider.ID)
		}
	}
	return provider, apiKey, headers, nil
}

// Sentinel errors the chat handler maps onto distinct wire codes: collapsing
// them into one "ai_upstream_error" is what makes an unreadable key
// indistinguishable from a network hiccup (§10.1's invariant, applied here).
var (
	errAINotConfigured  = errors.New("ai not configured")
	errAIProtocol       = errors.New("ai protocol unsupported")
	errAIModelNotLinked = errors.New("ai model not linked to provider")
	errAIKeyUnreadable  = errors.New("ai provider secret unreadable")
	// "enabled" is a real switch: without these two the flag only hid rows from
	// the settings page while a hand-crafted request could still use them.
	errAIProviderDisabled = errors.New("ai provider disabled")
	errAIModelDisabled    = errors.New("ai model disabled")
)

// providerView renders one provider. modelsByID supplies the linked model rows
// so the response can carry the per-protocol view (effective levels) the picker
// needs; a nil map is fine and just omits them.
func (s *Server) providerView(p store.AIProvider, modelIDs []string, modelsByID map[string]store.AIModel) aiProviderView {
	view := aiProviderView{
		ID: p.ID, Name: p.Name, Protocol: p.Protocol, BaseURL: p.BaseURL,
		ModelsDevSlug: p.ModelsDevSlug, Enabled: p.Enabled,
		HasKey: p.APIKeyEnc != "", HasHeaders: p.ExtraHeadersEnc != "",
		HeaderNames: []string{}, ModelIDs: modelIDs,
		Models:    []aiProviderModelView{},
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if view.ModelIDs == nil {
		view.ModelIDs = []string{}
	}
	if path, ok := aiProtocolPaths[p.Protocol]; ok && p.BaseURL != "" {
		view.Endpoint = p.BaseURL + path
	}
	for _, modelID := range modelIDs {
		m, ok := modelsByID[modelID]
		if !ok {
			continue
		}
		view.Models = append(view.Models, aiProviderModelView{
			ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens, Enabled: m.Enabled,
			EffectiveLevels: aiEffectiveLevels(m.ReasoningLevels, p.Protocol),
		})
	}
	if p.ExtraHeadersEnc != "" {
		// Names only: the settings form renders "which headers are set" and
		// lets the operator replace the block wholesale.
		if plain, err := s.Crypt.Decrypt(p.ExtraHeadersEnc); err == nil {
			if headers, err := decodeExtraHeaders(plain); err == nil {
				view.HeaderNames = headerNames(headers)
			}
		}
	}
	return view
}

func (s *Server) modelView(m store.AIModel, providerIDs []string) aiModelView {
	if providerIDs == nil {
		providerIDs = []string{}
	}
	return aiModelView{
		ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow,
		MaxOutputTokens: m.MaxOutputTokens, InputModalities: m.InputModalities,
		OutputModalities: m.OutputModalities, ReasoningLevels: m.ReasoningLevels,
		OverriddenFields: m.OverriddenFields, Source: m.Source, Enabled: m.Enabled,
		ProviderIDs: providerIDs,
	}
}

// aiCatalog assembles the full picture. It also inverts the provider→model
// links so a model can report every gateway that serves it.
func (s *Server) aiCatalog() (*aiCatalogResponse, error) {
	providers, err := s.Store.ListAIProviders()
	if err != nil {
		return nil, err
	}
	models, err := s.Store.ListAIModels()
	if err != nil {
		return nil, err
	}
	links, err := s.Store.AIProviderModelIDs()
	if err != nil {
		return nil, err
	}

	modelsByID := make(map[string]store.AIModel, len(models))
	for _, m := range models {
		modelsByID[m.ID] = m
	}

	modelProviders := map[string][]string{}
	for providerID, modelIDs := range links {
		for _, modelID := range modelIDs {
			modelProviders[modelID] = append(modelProviders[modelID], providerID)
		}
	}

	out := &aiCatalogResponse{Providers: []aiProviderView{}, Models: []aiModelView{}}
	for _, p := range providers {
		out.Providers = append(out.Providers, s.providerView(p, links[p.ID], modelsByID))
	}
	for _, m := range models {
		out.Models = append(out.Models, s.modelView(m, modelProviders[m.ID]))
	}
	out.DefaultProvider, _ = s.Store.GetSetting("ai.default_provider_id")
	out.DefaultModel, _ = s.Store.GetSetting("ai.default_model_id")
	return out, nil
}

func (s *Server) handleGetAICatalog(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.aiCatalog()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}

// applyProviderRequest validates a create/update body against the stored row
// and produces the row to write. existing is nil on create.
func (s *Server) applyProviderRequest(req aiProviderRequest, existing *store.AIProvider) (*store.AIProvider, error) {
	if !store.ValidAIProtocol(req.Protocol) {
		return nil, errors.New("bad_protocol")
	}
	baseURL, err := normalizeProviderBaseURL(req.BaseURL)
	if err != nil {
		return nil, errors.New("bad_base_url")
	}

	row := &store.AIProvider{
		Name: strings.TrimSpace(req.Name), Protocol: req.Protocol,
		BaseURL: baseURL, ModelsDevSlug: strings.TrimSpace(req.ModelsDevSlug),
		Enabled: true,
	}
	if row.Name == "" {
		// A nameless provider would be unselectable in the picker; derive one
		// from the host instead of rejecting the save.
		if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
			row.Name = u.Host
		} else {
			row.Name = baseURL
		}
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}

	if existing != nil {
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		row.APIKeyEnc = existing.APIKeyEnc
		row.ExtraHeadersEnc = existing.ExtraHeadersEnc
	}
	if req.APIKey != nil {
		if *req.APIKey == "" {
			row.APIKeyEnc = ""
		} else {
			ciphertext, err := s.Crypt.Encrypt(strings.TrimSpace(*req.APIKey))
			if err != nil {
				return nil, fmt.Errorf("encrypt api key: %w", err)
			}
			row.APIKeyEnc = ciphertext
		}
	}
	if req.ExtraHeaders != nil {
		if strings.TrimSpace(*req.ExtraHeaders) == "" {
			row.ExtraHeadersEnc = ""
		} else {
			if _, err := decodeExtraHeaders(*req.ExtraHeaders); err != nil {
				return nil, errors.New("bad_extra_headers")
			}
			ciphertext, err := s.Crypt.Encrypt(*req.ExtraHeaders)
			if err != nil {
				return nil, fmt.Errorf("encrypt extra headers: %w", err)
			}
			row.ExtraHeadersEnc = ciphertext
		}
	}
	return row, nil
}

func (s *Server) handleCreateAIProvider(w http.ResponseWriter, r *http.Request) {
	var req aiProviderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	row, err := s.applyProviderRequest(req, nil)
	if err != nil {
		code := err.Error()
		if code == "bad_protocol" || code == "bad_base_url" || code == "bad_extra_headers" {
			writeErr(w, http.StatusBadRequest, code)
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	id, err := randomID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	row.ID = "aip-" + id
	if err := s.Store.UpsertAIProvider(row); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.auditAIProvider(r, "ai_provider_create", row)
	writeJSON(w, http.StatusOK, s.providerView(*row, nil, nil))
}

func (s *Server) handleUpdateAIProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.Store.GetAIProvider(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req aiProviderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	row, err := s.applyProviderRequest(req, existing)
	if err != nil {
		code := err.Error()
		if code == "bad_protocol" || code == "bad_base_url" || code == "bad_extra_headers" {
			writeErr(w, http.StatusBadRequest, code)
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.UpsertAIProvider(row); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	links, _ := s.Store.AIProviderModelIDs()
	models, _ := s.Store.ListAIModels()
	modelsByID := make(map[string]store.AIModel, len(models))
	for _, m := range models {
		modelsByID[m.ID] = m
	}
	s.auditAIProvider(r, "ai_provider_update", row)
	writeJSON(w, http.StatusOK, s.providerView(*row, links[row.ID], modelsByID))
}

func (s *Server) handleDeleteAIProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	provider, err := s.Store.GetAIProvider(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.DeleteAIProvider(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.clearAIDefaultIfProvider(id)
	s.auditAIProvider(r, "ai_provider_delete", provider)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clearAIDefaultIfProvider drops the default pair when its provider vanishes.
// Leaving a dangling default is not fatal (the picker falls back), but it makes
// the panel show a default that cannot be resolved — and §12.6's fallback
// notice would then fire on every mount for no reason.
func (s *Server) clearAIDefaultIfProvider(providerID string) {
	current, _ := s.Store.GetSetting("ai.default_provider_id")
	if current != providerID {
		return
	}
	_ = s.Store.SetSetting("ai.default_provider_id", "", false)
	_ = s.Store.SetSetting("ai.default_model_id", "", false)
}

func (s *Server) handleSaveAIModel(w http.ResponseWriter, r *http.Request) {
	var req aiModelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "bad_model_id")
		return
	}
	for _, level := range req.ReasoningLevels {
		if !aiReasoningLevels[level] {
			writeErr(w, http.StatusBadRequest, "bad_reasoning_level")
			return
		}
	}
	if !aiReasoningOffStyles[req.ReasoningOffStyle] {
		writeErr(w, http.StatusBadRequest, "bad_reasoning_off_style")
		return
	}
	for _, field := range req.OverriddenFields {
		if !aiModelOverrideFields[field] {
			writeErr(w, http.StatusBadRequest, "bad_override_field")
			return
		}
	}
	if req.ContextWindow < 0 || req.MaxOutputTokens < 0 {
		writeErr(w, http.StatusBadRequest, "bad_limit")
		return
	}
	// §12.5: an Anthropic-shaped budget must stay under max_tokens, and a
	// model that cannot express its limits must not pretend it can. The budget
	// table itself lives in settings; only the invariant is enforced here.
	if req.MaxOutputTokens > 0 && req.ContextWindow > 0 && req.MaxOutputTokens > req.ContextWindow {
		writeErr(w, http.StatusBadRequest, "bad_limit")
		return
	}

	row := &store.AIModel{
		ID: req.ID, DisplayName: strings.TrimSpace(req.DisplayName),
		ContextWindow: req.ContextWindow, MaxOutputTokens: req.MaxOutputTokens,
		InputModalities: req.InputModalities, OutputModalities: req.OutputModalities,
		ReasoningLevels: req.ReasoningLevels, ReasoningOffStyle: req.ReasoningOffStyle,
		OverriddenFields: req.OverriddenFields, Source: req.Source, Enabled: true,
	}
	if row.DisplayName == "" {
		row.DisplayName = row.ID
	}
	if row.Source == "" {
		row.Source = "manual"
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	if existing, err := s.Store.GetAIModel(req.ID); err == nil {
		row.CreatedAt = existing.CreatedAt
	}
	if err := s.Store.UpsertAIModel(row); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.auditAIProvider(r, "ai_model_save", nil)
	writeJSON(w, http.StatusOK, s.modelView(*row, nil))
}

// aiModelOverrideFields is the vocabulary a manual edit may freeze (§12.5:
// "首次匹配即冻结、逐字段标记"). Anything outside this set would be a field
// the refresh never writes, i.e. a no-op that only looks like protection.
var aiModelOverrideFields = map[string]bool{
	"display_name": true, "context_window": true, "max_output_tokens": true,
	"input_modalities": true, "output_modalities": true, "reasoning_levels": true,
	"reasoning_off_style": true,
}

// aiReasoningOffStyles is the §12.5 vocabulary for "how off is spelled".
// Validated so a typo cannot reach the adapter as an unknown style, where it
// would silently degrade to "omit" — the one spelling that does NOT turn
// thinking off on gateways that think by default.
var aiReasoningOffStyles = map[string]bool{
	"": true, "omit": true, "none": true, "disabled": true,
}

func (s *Server) handleDeleteAIModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetAIModel(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.DeleteAIModel(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if current, _ := s.Store.GetSetting("ai.default_model_id"); current == id {
		_ = s.Store.SetSetting("ai.default_provider_id", "", false)
		_ = s.Store.SetSetting("ai.default_model_id", "", false)
	}
	s.auditAIProvider(r, "ai_model_delete", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type aiLinkRequest struct {
	ModelIDs []string `json:"model_ids"`
}

// handleLinkAIProviderModels attaches models to a provider. Batch-shaped
// because the settings page adds a whole fetched selection at once, and one
// request per model would let a mid-list failure leave a half-linked state the
// operator cannot see.
func (s *Server) handleLinkAIProviderModels(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if _, err := s.Store.GetAIProvider(providerID); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req aiLinkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	for _, modelID := range req.ModelIDs {
		if _, err := s.Store.GetAIModel(modelID); errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "model_not_found")
			return
		} else if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		if err := s.Store.LinkAIProviderModel(providerID, modelID); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	s.auditAIProvider(r, "ai_provider_models_link", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "linked": len(req.ModelIDs)})
}

func (s *Server) handleUnlinkAIProviderModel(w http.ResponseWriter, r *http.Request) {
	providerID, modelID := r.PathValue("id"), r.PathValue("modelID")
	if err := s.Store.UnlinkAIProviderModel(providerID, modelID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.auditAIProvider(r, "ai_provider_model_unlink", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type aiDefaultsRequest struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

// handleSetAIDefaults pins the default (provider, model) pair. An empty pair is
// allowed (it just means "no default yet"); a HALF pair is not — the two ids
// only mean something together (§12.1).
func (s *Server) handleSetAIDefaults(w http.ResponseWriter, r *http.Request) {
	var req aiDefaultsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	req.ProviderID, req.ModelID = strings.TrimSpace(req.ProviderID), strings.TrimSpace(req.ModelID)
	if (req.ProviderID == "") != (req.ModelID == "") {
		writeErr(w, http.StatusBadRequest, "bad_default_pair")
		return
	}
	if req.ProviderID != "" {
		if _, err := s.Store.GetAIProvider(req.ProviderID); errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "provider_not_found")
			return
		} else if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		if _, err := s.resolveAIModel(req.ProviderID, req.ModelID); err != nil {
			// The default must be callable: storing a half-linked pair would
			// only move the failure to the first chat submit.
			writeErr(w, http.StatusBadRequest, "default_not_usable")
			return
		}
	}
	if err := s.Store.SetSetting("ai.default_provider_id", req.ProviderID, false); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.SetSetting("ai.default_model_id", req.ModelID, false); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.auditAIProvider(r, "ai_default_set", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// auditAIProvider records provider/model administration. Values are never
// included: same "no credentials in the audit trail" rule as anytls rotation
// (§10.1), and the reason an operator can safely paste an audit row into a bug
// report.
func (s *Server) auditAIProvider(r *http.Request, action string, provider *store.AIProvider) {
	target := ""
	if provider != nil {
		target = provider.Name + " (" + provider.ID + ")"
	}
	_ = s.Store.InsertAudit(&store.AuditEntry{
		Actor:    "panel",
		Action:   action,
		Command:  target,
		Reason:   "ai provider/model settings",
		SourceIP: s.Trust.RealIP(r),
	})
}

// aiFetchModelsTimeout bounds the upstream model-list call. Shorter than a chat
// request on purpose: this is an interactive "list what you have" button, and a
// gateway that needs longer is better reported as a failure than as a spinner.
const aiFetchModelsTimeout = 20 * time.Second

// aiFetchedModelView is one entry of the upstream's model list, annotated with
// what the panel already knows about it so the picker can render
// "matched / not matched" and "already added" without a second call.
type aiFetchedModelView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	// Matched: models.dev has metadata for this id (this provider's slug when it
	// has one, the bare id otherwise — see lookupModelMeta), so adding it fills
	// the limits automatically.
	Matched bool `json:"matched"`
	// Linked: already attached to this provider.
	Linked bool `json:"linked"`
}

// handleFetchAIModels asks the upstream what it serves (GET {base}/models).
//
// A provider that does not implement /models is a normal case, not a bug: the
// error code tells the frontend to say "type model ids by hand" rather than
// implying the key or the URL is wrong.
func (s *Server) handleFetchAIModels(w http.ResponseWriter, r *http.Request) {
	provider, apiKey, headers, err := s.resolveProvider(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if errors.Is(err, errAIProtocol) {
		writeErr(w, http.StatusBadRequest, "ai_protocol_unsupported")
		return
	}
	if errors.Is(err, errAIKeyUnreadable) {
		writeErr(w, http.StatusInternalServerError, "ai_key_unreadable")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), aiFetchModelsTimeout)
	defer cancel()
	client := s.AIHTTPClient
	if client == nil {
		client = &http.Client{}
	}
	fetched, err := aiprotocol.FetchModels(ctx, client, aiprotocol.Request{
		BaseURL:  provider.BaseURL,
		APIKey:   apiKey,
		Headers:  headers,
		Protocol: provider.Protocol,
	})
	if err != nil {
		// The upstream error never contains the key (the adapter redacts), so
		// logging it is safe and is the only way to explain a 404 endpoint.
		s.Log.Warn("fetch upstream models", "provider", provider.ID, "err", err)
		writeErr(w, http.StatusBadGateway, "ai_fetch_models_failed")
		return
	}

	linked := map[string]bool{}
	if all, err := s.Store.AIProviderModelIDs(); err == nil {
		for _, id := range all[provider.ID] {
			linked[id] = true
		}
	}
	out := make([]aiFetchedModelView, 0, len(fetched))
	for _, m := range fetched {
		// Same resolution the import will use (§12.5): a slugless provider
		// (relay) matches by model id, so the "has metadata" mark must not be
		// stricter than the import itself.
		_, _, matched := s.lookupModelMeta(provider, m.ID)
		out = append(out, aiFetchedModelView{
			ID: m.ID, DisplayName: m.DisplayName, Matched: matched, Linked: linked[m.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

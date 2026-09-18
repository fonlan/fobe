package httpapi

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/fonlan/fobe/internal/server/aiprotocol"
	"github.com/fonlan/fobe/internal/server/modelsdev"
	"github.com/fonlan/fobe/internal/server/store"
)

// models.dev metadata surface (design.md §12.5).
//
// Three properties decide the shape of everything here:
//
//   - Matching is EXACT (`<slug>/<id>`). The same model id appears under dozens
//     of gateways with different limits, so a fuzzy match would silently attach
//     gateway A's context window to gateway B's model.
//   - A model row is global while the protocol is per provider, so the level set
//     is computed in two steps: `reasoning_levels` stores what the MODEL
//     documents (protocol-independent), and `aiEffectiveLevels` applies the
//     protocol's own rule on the way out — Anthropic can always disable
//     thinking (the field is opt-in), the OpenAI dialects only when the model
//     says so.
//   - First match freezes: a field the operator edited is listed in
//     `overridden_fields` and a later refresh never touches it again.

// modelsDevStatusView is what the settings page shows about the cache.
type modelsDevStatusView struct {
	Loaded     bool                `json:"loaded"`
	AutoUpdate bool                `json:"auto_update"`
	Providers  int                 `json:"providers"`
	Models     int                 `json:"models"`
	UpdatedAt  int64               `json:"updated_at,omitempty"`
	URL        string              `json:"url,omitempty"`
	LastError  string              `json:"last_error,omitempty"`
	Slugs      []modelsDevSlugView `json:"slugs"`
}

type modelsDevSlugView struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	BaseURL  string `json:"base_url,omitempty"`
	Protocol string `json:"protocol,omitempty"` // guessed from npm; "" = operator must choose
}

// handleGetModelsDev reports the cache state plus the provider slugs the
// settings form offers. The slug list is part of the same response because
// picking a slug and seeing whether the cache loaded are one decision for the
// operator ("why is my provider not in the list").
func (s *Server) handleGetModelsDev(w http.ResponseWriter, r *http.Request) {
	view := modelsDevStatusView{Slugs: []modelsDevSlugView{}}
	if s.ModelsDev == nil {
		// Not wired (dev / API-only): report it instead of failing, mirroring
		// the §14.1 GeoIP section's "not wired" stance.
		view.LastError = "models_dev_not_wired"
		writeJSON(w, http.StatusOK, view)
		return
	}
	status := s.ModelsDev.Status()
	view.Loaded = status.Loaded
	view.AutoUpdate = status.AutoUpdate
	view.Providers = status.Providers
	view.Models = status.Models
	view.URL = status.URL
	view.LastError = status.LastError
	if !status.UpdatedAt.IsZero() {
		view.UpdatedAt = status.UpdatedAt.Unix()
	}
	for _, p := range s.ModelsDev.Providers() {
		view.Slugs = append(view.Slugs, modelsDevSlugView{
			Slug: p.ID, Name: p.Name, BaseURL: p.API,
			Protocol: modelsdev.ProtocolForNPM(p.NPM),
		})
	}
	writeJSON(w, http.StatusOK, view)
}

// handleRefreshModelsDev forces a fetch. Manual only: the daily ticker lives in
// the manager, and a panel that re-downloaded 4.7 MB on every page open would
// be its own problem.
func (s *Server) handleRefreshModelsDev(w http.ResponseWriter, r *http.Request) {
	if s.ModelsDev == nil {
		writeErr(w, http.StatusServiceUnavailable, "models_dev_not_wired")
		return
	}
	ctx := s.Background
	if ctx == nil {
		ctx = r.Context()
	}
	if err := s.ModelsDev.Ensure(ctx, true); err != nil {
		s.Log.Warn("models.dev refresh", "err", err)
		writeErr(w, http.StatusBadGateway, "models_dev_fetch_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type aiMatchRequest struct {
	ProviderID string   `json:"provider_id"`
	ModelIDs   []string `json:"model_ids"`
}

type aiMatchResult struct {
	// Applied lists models that were created or updated from metadata.
	Applied []string `json:"applied"`
	// Unmatched lists ids the chosen models.dev provider does not serve. This
	// is a normal outcome for a self-hosted gateway, not an error.
	Unmatched []string `json:"unmatched"`
	// Frozen lists fields that were left alone because the operator edited them
	// (§12.5 "首次匹配即冻结") — naming them is what makes a "why didn't it
	// update my context window" question answerable from the UI.
	Frozen []string `json:"frozen"`
}

// handleMatchAIModels fills model rows from models.dev.
//
// Batch-shaped for the same reason the link endpoint is: the settings page adds
// a whole fetched selection at once, and a per-model request would leave a
// half-applied state on failure.
func (s *Server) handleMatchAIModels(w http.ResponseWriter, r *http.Request) {
	if s.ModelsDev == nil {
		writeErr(w, http.StatusServiceUnavailable, "models_dev_not_wired")
		return
	}
	var req aiMatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	provider, err := s.Store.GetAIProvider(strings.TrimSpace(req.ProviderID))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if provider.ModelsDevSlug == "" {
		// No slug = no matching. Saying so beats silently creating empty rows
		// the operator then has to fill by hand without knowing why.
		writeErr(w, http.StatusBadRequest, "provider_has_no_slug")
		return
	}

	result := aiMatchResult{Applied: []string{}, Unmatched: []string{}, Frozen: []string{}}
	for _, modelID := range req.ModelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		meta, ok := s.ModelsDev.Lookup(provider.ModelsDevSlug, modelID)
		if !ok {
			result.Unmatched = append(result.Unmatched, modelID)
			continue
		}
		frozen, err := s.applyModelMeta(meta, modelID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		for _, field := range frozen {
			result.Frozen = append(result.Frozen, modelID+":"+field)
		}
		result.Applied = append(result.Applied, modelID)
	}
	sort.Strings(result.Frozen)
	s.auditAIProvider(r, "ai_models_matched", provider)
	writeJSON(w, http.StatusOK, result)
}

// applyModelMeta merges metadata into a model row, per-field.
//
// Existing rows are updated FIELD BY FIELD against overridden_fields rather than
// wholesale: overwriting the whole row would erase the operator's corrections
// on every refresh, and skipping the row would freeze the match forever at
// whatever models.dev said the first time (transposed limits do get fixed
// upstream). It returns the fields it deliberately left alone.
func (s *Server) applyModelMeta(meta modelsdev.ModelMeta, modelID string) ([]string, error) {
	existing, err := s.Store.GetAIModel(modelID)
	if errors.Is(err, store.ErrNotFound) {
		existing = nil
	} else if err != nil {
		return nil, err
	}

	row := &store.AIModel{
		ID: modelID, DisplayName: meta.Name, ContextWindow: meta.ContextWindow,
		MaxOutputTokens: meta.MaxOutputTokens, InputModalities: meta.InputModalities,
		OutputModalities: meta.OutputModalities, Source: "models_dev", Enabled: true,
		// Stored levels are the model's own documented capability; the protocol
		// rule is applied on the way out (see aiEffectiveLevels).
		ReasoningLevels: modelsdev.ReasoningLevels(meta, store.ProtocolOpenAIResponses),
		// How "off" must be spelled for THIS model (§12.5): a toggle-style model
		// needs its own close shape, an effort-style model whose values include
		// "none" needs an explicit none (omitting does not switch thinking off
		// on those gateways), and Anthropic ignores the field entirely.
		ReasoningOffStyle: aiOffStyleFromMeta(meta),
	}
	if row.DisplayName == "" {
		row.DisplayName = modelID
	}
	if existing == nil {
		row.CreatedAt, row.UpdatedAt = nowUnix(), nowUnix()
		return nil, s.Store.UpsertAIModel(row)
	}

	row.CreatedAt, row.UpdatedAt = existing.CreatedAt, nowUnix()
	row.Enabled = existing.Enabled
	row.OverriddenFields = existing.OverriddenFields

	frozenSet := map[string]bool{}
	for _, field := range existing.OverriddenFields {
		frozenSet[field] = true
	}
	frozen := []string{}

	// Each branch: keep the stored value when the operator froze that field,
	// otherwise take the metadata.
	if frozenSet["display_name"] {
		row.DisplayName = existing.DisplayName
		frozen = append(frozen, "display_name")
	}
	if frozenSet["context_window"] {
		row.ContextWindow = existing.ContextWindow
		frozen = append(frozen, "context_window")
	}
	if frozenSet["max_output_tokens"] {
		row.MaxOutputTokens = existing.MaxOutputTokens
		frozen = append(frozen, "max_output_tokens")
	}
	if frozenSet["input_modalities"] {
		row.InputModalities = existing.InputModalities
		frozen = append(frozen, "input_modalities")
	}
	if frozenSet["output_modalities"] {
		row.OutputModalities = existing.OutputModalities
		frozen = append(frozen, "output_modalities")
	}
	if frozenSet["reasoning_levels"] {
		row.ReasoningLevels = existing.ReasoningLevels
		frozen = append(frozen, "reasoning_levels")
	}
	if frozenSet["reasoning_off_style"] {
		row.ReasoningOffStyle = existing.ReasoningOffStyle
		frozen = append(frozen, "reasoning_off_style")
	}
	return frozen, s.Store.UpsertAIModel(row)
}

// aiImportRequest is the "add the models I selected" call: the frontend has
// just listed what the upstream advertises and the operator ticked a few.
type aiImportRequest struct {
	ModelIDs []string `json:"model_ids"`
}

type aiImportResult struct {
	Added     []string `json:"added"`
	Unmatched []string `json:"unmatched"`
	Frozen    []string `json:"frozen"`
}

// handleImportAIModels creates and links the selected models in ONE call.
//
// Deliberately not "match, then link" as two client calls: a model the upstream
// advertises but models.dev does not know must still become a row (the operator
// may well want it, with hand-filled limits), and splitting the work across two
// requests leaves a half-added selection whenever the second one fails.
func (s *Server) handleImportAIModels(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	provider, err := s.Store.GetAIProvider(providerID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req aiImportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}

	result := aiImportResult{Added: []string{}, Unmatched: []string{}, Frozen: []string{}}
	for _, modelID := range req.ModelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		// Metadata when we can get it, a hand-entered row otherwise. Note this
		// is the ONLY place a model id becomes a row without the operator
		// typing limits, so the "unmatched" list is what tells them which
		// entries still need attention.
		matched := false
		if s.ModelsDev != nil && provider.ModelsDevSlug != "" {
			if meta, ok := s.ModelsDev.Lookup(provider.ModelsDevSlug, modelID); ok {
				frozen, err := s.applyModelMeta(meta, modelID)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, "internal")
					return
				}
				matched = true
				for _, field := range frozen {
					result.Frozen = append(result.Frozen, modelID+":"+field)
				}
			}
		}
		if !matched {
			if _, err := s.Store.GetAIModel(modelID); errors.Is(err, store.ErrNotFound) {
				row := &store.AIModel{
					ID: modelID, DisplayName: modelID, Source: "manual", Enabled: true,
					InputModalities: []string{"text"}, OutputModalities: []string{"text"},
					CreatedAt: nowUnix(), UpdatedAt: nowUnix(),
				}
				if err := s.Store.UpsertAIModel(row); err != nil {
					writeErr(w, http.StatusInternalServerError, "internal")
					return
				}
			} else if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			result.Unmatched = append(result.Unmatched, modelID)
		}
		if err := s.Store.LinkAIProviderModel(providerID, modelID); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		result.Added = append(result.Added, modelID)
	}
	sort.Strings(result.Frozen)
	s.auditAIProvider(r, "ai_models_imported", provider)
	writeJSON(w, http.StatusOK, result)
}

// aiOffStyleFromMeta maps models.dev's reasoning options onto the wire spelling
// of "off" (§12.5). Order matters: a toggle option means the model has its own
// close shape, which is more specific than a "none" among the effort values.
func aiOffStyleFromMeta(meta modelsdev.ModelMeta) string {
	for _, option := range meta.ReasoningOptions {
		if option == modelsdev.ReasoningToggle {
			return aiprotocol.OffStyleDisabled
		}
	}
	for _, value := range meta.ReasoningValues {
		if value == modelsdev.ReasoningValueNone {
			return aiprotocol.OffStyleNone
		}
	}
	// Nothing documented: omit, the only spelling that cannot contradict the
	// upstream (it is also what Anthropic requires).
	return aiprotocol.OffStyleOmit
}

// aiEffectiveLevels applies the per-protocol rule to a model's stored levels.
//
// Anthropic's extended thinking is opt-in: NOT sending `thinking` is how you
// disable it, so `off` is always expressible there (design §12.5). The OpenAI
// dialects keep exactly what the model documents, because for a reasoning-only
// model the lowest effort is not "off" and calling it off would be a lie.
func aiEffectiveLevels(stored []string, protocol string) []string {
	levels := make([]string, 0, len(stored)+1)
	seen := map[string]bool{}
	for _, level := range stored {
		if !seen[level] {
			seen[level] = true
			levels = append(levels, level)
		}
	}
	if protocol == store.ProtocolAnthropicMessages && !seen["off"] {
		levels = append([]string{"off"}, levels...)
	}
	sort.SliceStable(levels, func(i, j int) bool {
		return aiLevelOrder(levels[i]) < aiLevelOrder(levels[j])
	})
	return levels
}

// aiLevelOrder keeps the picker's level list in a fixed, meaningful order
// instead of alphabetical (which would render "high, low, medium, off").
func aiLevelOrder(level string) int {
	switch level {
	case "off":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	}
	return 9
}

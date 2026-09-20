package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// AI provider/model storage (design.md §12.1/§12.5, 2026-09-18).
//
// Two invariants live here rather than in the handlers:
//
//  1. A model row is GLOBALLY unique — `ai_models.id` is the model id string
//     itself (e.g. "gpt-5"), and `ai_provider_models` says through which
//     providers it can be reached. The accepted cost (§12.1) is that a gateway
//     clipping the context differently cannot be expressed.
//  2. Secrets are ciphertext in this layer. `api_key_enc` / `extra_headers_enc`
//     are Cryptor output; the httpapi layer encrypts before writing and
//     decrypts after reading, exactly like `settings.encrypted` (§4.4). The
//     store therefore never has to know whether a key is readable, which is
//     what lets the handlers distinguish "unset" from "unreadable" — the
//     distinction §10.1's invariant depends on.

// Protocol values accepted by ai_providers.protocol (§12.5). Three wire
// dialects only: the adapters in internal/server/httpapi/ai_proto_*.go exist
// for exactly these, and anything else (azure, vertex, google) is refused at
// the API layer instead of being silently coerced into one of them.
const (
	ProtocolOpenAICompletions = "openai-completions"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
)

// ValidAIProtocol reports whether protocol is one of the three supported
// dialects. Used by the provider handlers to reject bad input early.
func ValidAIProtocol(protocol string) bool {
	switch protocol {
	case ProtocolOpenAICompletions, ProtocolOpenAIResponses, ProtocolAnthropicMessages:
		return true
	}
	return false
}

// AIProvider is one upstream endpoint.
type AIProvider struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Protocol        string `json:"protocol"`
	BaseURL         string `json:"base_url"`
	APIKeyEnc       string `json:"-"` // never serialized: ciphertext must not leak into an export or a response
	ExtraHeadersEnc string `json:"-"` // same
	ModelsDevSlug   string `json:"models_dev_slug,omitempty"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// AIModel is one model, shared by every provider that serves it.
type AIModel struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	ContextWindow    int      `json:"context_window"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	ReasoningLevels  []string `json:"reasoning_levels"`
	// ReasoningOffStyle is how "off" must be sent for this model (omit|none|
	// disabled, §12.5). Empty means "omit" — the safe default.
	ReasoningOffStyle string   `json:"reasoning_off_style"`
	OverriddenFields  []string `json:"overridden_fields"`
	Source            string   `json:"source"` // models_dev|manual
	Enabled           bool     `json:"enabled"`
	CreatedAt         int64    `json:"created_at"`
	UpdatedAt         int64    `json:"updated_at"`
}

// csvList/csvJoin keep the multi-valued columns as plain comma-separated text.
// No element of these lists can contain a comma (model modalities and the
// unified reasoning level names are fixed vocabularies), so a JSON array would
// only add parsing failure modes.
func csvJoin(values []string) string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, ",")
}

func csvSplit(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ListAIProviders returns every provider, enabled or not, ordered by name so
// the settings page and the model picker render a stable list.
func (s *Store) ListAIProviders() ([]AIProvider, error) {
	rows, err := s.db.Query(
		`SELECT id, name, protocol, base_url, api_key_enc, extra_headers_enc,
		        models_dev_slug, enabled, created_at, updated_at
		 FROM ai_providers ORDER BY name, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list ai providers: %w", err)
	}
	defer rows.Close()

	out := []AIProvider{}
	for rows.Next() {
		var p AIProvider
		if err := rows.Scan(&p.ID, &p.Name, &p.Protocol, &p.BaseURL, &p.APIKeyEnc,
			&p.ExtraHeadersEnc, &p.ModelsDevSlug, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetAIProvider(id string) (*AIProvider, error) {
	p := &AIProvider{}
	err := s.db.QueryRow(
		`SELECT id, name, protocol, base_url, api_key_enc, extra_headers_enc,
		        models_dev_slug, enabled, created_at, updated_at
		 FROM ai_providers WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &p.Protocol, &p.BaseURL, &p.APIKeyEnc,
		&p.ExtraHeadersEnc, &p.ModelsDevSlug, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ai provider: %w", err)
	}
	return p, nil
}

// UpsertAIProvider writes the row, creating it when absent. Callers keep
// CreatedAt stable by reading the existing row first; this method preserves
// created_at on conflict so an ordinary edit cannot reorder the list.
func (s *Store) UpsertAIProvider(p *AIProvider) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_providers
		   (id, name, protocol, base_url, api_key_enc, extra_headers_enc,
		    models_dev_slug, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   name = excluded.name,
		   protocol = excluded.protocol,
		   base_url = excluded.base_url,
		   api_key_enc = excluded.api_key_enc,
		   extra_headers_enc = excluded.extra_headers_enc,
		   models_dev_slug = excluded.models_dev_slug,
		   enabled = excluded.enabled,
		   updated_at = excluded.updated_at`,
		p.ID, p.Name, p.Protocol, p.BaseURL, p.APIKeyEnc, p.ExtraHeadersEnc,
		p.ModelsDevSlug, p.Enabled, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert ai provider: %w", err)
	}
	return nil
}

// DeleteAIProvider removes the provider and its links. Models survive: they are
// global, and deleting one gateway must not silently delete a model that
// another gateway still serves.
func (s *Store) DeleteAIProvider(id string) error {
	if _, err := s.db.Exec(`DELETE FROM ai_providers WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete ai provider: %w", err)
	}
	return nil
}

// aiModelCols is the single column list every simple ai_models query shares
// (same contract as nodeColumns); scanAIModel is its one scan target and also
// decodes the csv-packed columns.
const aiModelCols = `id, display_name, context_window, max_output_tokens,
		       input_modalities, output_modalities, reasoning_levels, reasoning_off_style,
		       overridden_fields, source, enabled, created_at, updated_at`

func scanAIModel(rs rowScanner) (*AIModel, error) {
	m := &AIModel{}
	var in, out2, levels, overridden string
	err := rs.Scan(&m.ID, &m.DisplayName, &m.ContextWindow, &m.MaxOutputTokens,
		&in, &out2, &levels, &m.ReasoningOffStyle, &overridden, &m.Source, &m.Enabled, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan ai model: %w", err)
	}
	m.InputModalities = csvSplit(in)
	m.OutputModalities = csvSplit(out2)
	m.ReasoningLevels = csvSplit(levels)
	m.OverriddenFields = csvSplit(overridden)
	return m, nil
}

// ListAIModels returns every model, enabled or not, ordered by display name for
// a stable picker.
func (s *Store) ListAIModels() ([]AIModel, error) {
	rows, err := s.db.Query(`SELECT ` + aiModelCols + ` FROM ai_models ORDER BY display_name, id`)
	if err != nil {
		return nil, fmt.Errorf("list ai models: %w", err)
	}
	defer rows.Close()

	out := []AIModel{}
	for rows.Next() {
		m, err := scanAIModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) GetAIModel(id string) (*AIModel, error) {
	return scanAIModel(s.db.QueryRow(
		`SELECT `+aiModelCols+` FROM ai_models WHERE id = ?`, id))
}

// UpsertAIModel writes the row, preserving created_at on conflict.
func (s *Store) UpsertAIModel(m *AIModel) error {
	if m.Source == "" {
		m.Source = "manual"
	}
	_, err := s.db.Exec(
		`INSERT INTO ai_models
		   (id, display_name, context_window, max_output_tokens, input_modalities,
		    output_modalities, reasoning_levels, reasoning_off_style, overridden_fields,
		    source, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   display_name = excluded.display_name,
		   context_window = excluded.context_window,
		   max_output_tokens = excluded.max_output_tokens,
		   input_modalities = excluded.input_modalities,
		   output_modalities = excluded.output_modalities,
		   reasoning_levels = excluded.reasoning_levels,
		   reasoning_off_style = excluded.reasoning_off_style,
		   overridden_fields = excluded.overridden_fields,
		   source = excluded.source,
		   enabled = excluded.enabled,
		   updated_at = excluded.updated_at`,
		m.ID, m.DisplayName, m.ContextWindow, m.MaxOutputTokens,
		csvJoin(m.InputModalities), csvJoin(m.OutputModalities),
		csvJoin(m.ReasoningLevels), m.ReasoningOffStyle, csvJoin(m.OverriddenFields),
		m.Source, m.Enabled, m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert ai model: %w", err)
	}
	return nil
}

// DeleteAIModel removes the model and its links (the sessions that used it keep
// their recorded model_id — audit history must not be rewritten by a delete).
func (s *Store) DeleteAIModel(id string) error {
	if _, err := s.db.Exec(`DELETE FROM ai_models WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete ai model: %w", err)
	}
	return nil
}

// LinkAIProviderModel attaches a model to a provider. Idempotent: re-adding a
// model the operator already sees in a fetched list must not error.
func (s *Store) LinkAIProviderModel(providerID, modelID string) error {
	_, err := s.db.Exec(
		`INSERT INTO ai_provider_models (provider_id, model_id, created_at)
		 VALUES (?, ?, ?) ON CONFLICT(provider_id, model_id) DO NOTHING`,
		providerID, modelID, now(),
	)
	if err != nil {
		return fmt.Errorf("link ai provider model: %w", err)
	}
	return nil
}

func (s *Store) UnlinkAIProviderModel(providerID, modelID string) error {
	_, err := s.db.Exec(
		`DELETE FROM ai_provider_models WHERE provider_id = ? AND model_id = ?`,
		providerID, modelID,
	)
	if err != nil {
		return fmt.Errorf("unlink ai provider model: %w", err)
	}
	return nil
}

// ListAIModelsForProvider returns the enabled-or-not models reachable through
// one provider, ordered for the picker.
func (s *Store) ListAIModelsForProvider(providerID string) ([]AIModel, error) {
	rows, err := s.db.Query(
		`SELECT m.id, m.display_name, m.context_window, m.max_output_tokens,
		        m.input_modalities, m.output_modalities, m.reasoning_levels, m.reasoning_off_style,
		        m.overridden_fields, m.source, m.enabled, m.created_at, m.updated_at
		 FROM ai_models m
		 JOIN ai_provider_models pm ON pm.model_id = m.id
		 WHERE pm.provider_id = ?
		 ORDER BY m.display_name, m.id`, providerID,
	)
	if err != nil {
		return nil, fmt.Errorf("list ai models for provider: %w", err)
	}
	defer rows.Close()

	out := []AIModel{}
	for rows.Next() {
		m, err := scanAIModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// AIProviderModelIDs maps provider id -> model ids, for rendering the grouped
// model picker without a query per provider.
func (s *Store) AIProviderModelIDs() (map[string][]string, error) {
	rows, err := s.db.Query(
		`SELECT provider_id, model_id FROM ai_provider_models ORDER BY provider_id, model_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list ai provider model ids: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var providerID, modelID string
		if err := rows.Scan(&providerID, &modelID); err != nil {
			return nil, err
		}
		out[providerID] = append(out[providerID], modelID)
	}
	return out, rows.Err()
}

// HasUsableAIModel is the §12.1 sidebar gate: at least one ENABLED provider
// that has a base_url and a key (ciphertext present — readability is the
// handler's problem, and an unreadable key must NOT read as "unconfigured",
// §10.1) linked to at least one ENABLED model.
func (s *Store) HasUsableAIModel() (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS (
		   SELECT 1 FROM ai_providers p
		   JOIN ai_provider_models pm ON pm.provider_id = p.id
		   JOIN ai_models m ON m.id = pm.model_id
		   WHERE p.enabled = 1 AND m.enabled = 1
		     AND p.base_url <> '' AND p.api_key_enc <> '' AND p.protocol <> ''
		 )`,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read ai configured gate: %w", err)
	}
	return exists, nil
}

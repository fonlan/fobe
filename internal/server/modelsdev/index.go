package modelsdev

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// This file holds the trimmed wire types, the streaming parser and the
// immutable index.

// ModelMeta is one model, reduced to the fields the panel actually uses
// (§12.5's field list). Everything else in api.json (description, cost, family,
// knowledge, open_weights …) is skipped by the decoder and never allocated.
type ModelMeta struct {
	// ID is the models.dev model id — the modelID side of Lookup.
	ID string
	// Name is the human-readable name.
	Name string
	// Reasoning is the boolean "this model can think" flag. It is the ONLY
	// thing models.json offers, which is why that document is unusable here.
	Reasoning bool
	// ReasoningOptions is the set of reasoning_options types (ReasoningEffort /
	// ReasoningToggle / ReasoningBudgetTokens). ReasoningLevels turns it, plus
	// ReasoningValues and a protocol, into the panel's level list. Never nil, so
	// the panel's JSON gets [] and not null.
	ReasoningOptions []string
	// ReasoningValues is the union of the "values" the document publishes for
	// those options — in practice only the effort option carries them (实测 221
	// providers 里没有任何 toggle / budget_tokens 项带 values). They are the
	// vendor's own effort names, including placeholders that are NOT panel
	// levels: "none" means the model can really switch thinking off, and
	// "xhigh"/"max"/"default" are tiers above or beside our vocabulary.
	// ReasoningLevels intersects this with the panel vocabulary. Never nil.
	ReasoningValues []string
	// ContextWindow and MaxOutputTokens come from limit.context / limit.output.
	// The panel uses them for context trimming and the max_tokens field (§12.5).
	ContextWindow   int
	MaxOutputTokens int
	// InputModalities and OutputModalities come from modalities.input/output.
	// Never nil, same reason as above.
	InputModalities  []string
	OutputModalities []string
	// ToolCall reports whether the model supports tool use.
	ToolCall bool
}

// ProviderMeta is one provider, reduced to the fields the form prefills.
type ProviderMeta struct {
	// ID is the models.dev provider slug — the same string stored in
	// ai_providers.models_dev_slug and the left half of Lookup.
	ID string
	// Name is the human-readable provider name.
	Name string
	// API is the prefill for ai_providers.base_url. Watch out: 26 of the 221
	// providers have no "api" at all — anthropic, openai, google, google-vertex,
	// azure, groq and xai are all among them — while gateways usually do. An
	// empty value therefore means "models.dev does not say", not "this provider
	// has no endpoint"; the form has to fall back on its own defaults.
	API string
	// NPM is the protocol hint (see ProtocolForNPM).
	NPM string
}

// Index is one immutable snapshot of the parsed document. A refresh builds a
// new one and swaps it in wholesale, so readers never see a half-updated map.
type Index struct {
	providers map[string]ProviderMeta
	models    map[string]map[string]ModelMeta
}

func newIndex() *Index {
	return &Index{
		providers: map[string]ProviderMeta{},
		models:    map[string]map[string]ModelMeta{},
	}
}

// Lookup resolves "<provider slug>/<model id>" by exact match. Both sides are
// whitespace-trimmed (form input hygiene — still an exact match, not a search).
func (ix *Index) Lookup(providerSlug, modelID string) (ModelMeta, bool) {
	if ix == nil {
		return ModelMeta{}, false
	}
	byID, ok := ix.models[strings.TrimSpace(providerSlug)]
	if !ok {
		return ModelMeta{}, false
	}
	meta, ok := byID[strings.TrimSpace(modelID)]
	return meta, ok
}

// Provider resolves a provider slug.
func (ix *Index) Provider(slug string) (ProviderMeta, bool) {
	if ix == nil {
		return ProviderMeta{}, false
	}
	p, ok := ix.providers[strings.TrimSpace(slug)]
	return p, ok
}

// ProviderCount is the number of providers in the snapshot.
func (ix *Index) ProviderCount() int {
	if ix == nil {
		return 0
	}
	return len(ix.providers)
}

// Providers lists every provider, sorted by slug. The form's slug picker is the
// reason this exists: §12.5 prefills protocol and base_url from the chosen slug's
// npm/api, and choosing one of 221 slugs needs the list, not a lookup. The
// returned slice is a copy, so callers cannot reach into the snapshot.
func (ix *Index) Providers() []ProviderMeta {
	if ix == nil {
		return []ProviderMeta{}
	}
	out := make([]ProviderMeta, 0, len(ix.providers))
	for _, p := range ix.providers {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b ProviderMeta) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Models lists the models a slug publishes, sorted by id; an unknown slug gives
// an empty list. Enumeration is for browsing a catalogue — attaching metadata to
// a configured model still goes through the exact-match Lookup.
func (ix *Index) Models(providerSlug string) []ModelMeta {
	if ix == nil {
		return []ModelMeta{}
	}
	byID := ix.models[strings.TrimSpace(providerSlug)]
	out := make([]ModelMeta, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b ModelMeta) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// ModelCount is the number of models in the snapshot.
func (ix *Index) ModelCount() int {
	if ix == nil {
		return 0
	}
	n := 0
	for _, byID := range ix.models {
		n += len(byID)
	}
	return n
}

// addProvider folds one decoded provider into the index. The JSON key is the
// slug (it always equals the entry's "id" in today's document; the key wins
// because that is what the slug picker in the panel shows).
func (ix *Index) addProvider(slug string, p apiProvider) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = slug
	}
	ix.providers[slug] = ProviderMeta{
		ID:   slug,
		Name: name,
		API:  strings.TrimSpace(p.API),
		NPM:  strings.TrimSpace(p.NPM),
	}
	if len(p.Models) == 0 {
		return
	}

	byID := make(map[string]ModelMeta, len(p.Models))
	for key, m := range p.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			id = key // defensive: the id field is what Lookup uses
		}
		byID[id] = ModelMeta{
			ID:               id,
			Name:             m.Name,
			Reasoning:        m.Reasoning,
			ReasoningOptions: reasoningTypes(m.ReasoningOptions),
			ReasoningValues:  reasoningValues(m.ReasoningOptions),
			ContextWindow:    m.Limit.Context,
			MaxOutputTokens:  m.Limit.Output,
			InputModalities:  orEmpty(m.Modalities.Input),
			OutputModalities: orEmpty(m.Modalities.Output),
			ToolCall:         m.ToolCall,
		}
	}
	ix.models[slug] = byID
}

// reasoningTypes keeps the option types in document order without duplicates.
func reasoningTypes(opts []apiReasoningOpt) []string {
	out := make([]string, 0, len(opts))
	seen := make(map[string]bool, len(opts))
	for _, o := range opts {
		t := strings.TrimSpace(o.Type)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// reasoningValues flattens the values published by the option entries, in
// document order and without duplicates. Today only the effort entry has them,
// but flattening means a future variant that puts them elsewhere still works.
func reasoningValues(opts []apiReasoningOpt) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, o := range opts {
		for _, v := range o.Values {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// orEmpty keeps JSON output stable: a missing array must serialize as [] rather
// than null, or the panel has to special-case every list field.
func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// Top-level shape violations.
var (
	// errNotObject: api.json is a JSON object of slug → provider.
	errNotObject = errors.New("top-level value is not an object")
	// errBadKey: every member of that object is keyed by the provider slug.
	errBadKey = errors.New("provider key is not a string")
	// errTrailingData: nothing may follow the document.
	errTrailingData = errors.New("trailing data after the document")
)

// apiProvider / apiModel / … are the TRIMMED views of api.json. Fields that are
// not listed here are not "discarded after parsing" — they are never allocated,
// because encoding/json only skips over keys it does not know. That is where the
// 22–27 MB → ~3 MB difference comes from（§12.5 实测）。
type apiProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	API    string              `json:"api"`
	NPM    string              `json:"npm"`
	Models map[string]apiModel `json:"models"`
}

type apiModel struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Reasoning        bool              `json:"reasoning"`
	ReasoningOptions []apiReasoningOpt `json:"reasoning_options"`
	Limit            apiLimit          `json:"limit"`
	Modalities       apiModalities     `json:"modalities"`
	ToolCall         bool              `json:"tool_call"`
}

// apiReasoningOpt keeps the option type and its published values. §12.5's field
// list asked only for the type; the values are what make "which thinking levels
// does this model support" answerable, and 1273 of the 3415 effort models list
// "none" there — 不看 values 就会把「真能关」的那批网关模型误判成「关不掉」。
//
// "min" (budget_tokens) is deliberately NOT kept: 实测它的取值只有缺省、1024、以及
// 1024 以下的噪声（1、128、0、512、256、-1），而最低档预算默认 2048 已经大于它们
// 中的任何一个，所以 min 永远藏不掉一个档位，留着只会让档位可见性多一个假开关。
type apiReasoningOpt struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type apiLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type apiModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// Parse reads api.json (or a cached copy of it) into an index.
//
// It streams: the top level is an object of slug → provider, so each provider is
// decoded on its own and folded into the index immediately. At no point does the
// whole document exist as a second in-memory structure, which is what keeps the
// steady footprint near the size of the index itself. A document that parses but
// contains no provider is rejected (ErrEmpty) so a mirror answering "{}" cannot
// displace a working cache.
func Parse(r io.Reader) (*Index, error) {
	// A buffered reader matters here: the decoder does a great many small reads.
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<16))

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decode models document: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("decode models document: %w", errNotObject)
	}

	ix := newIndex()
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decode provider key: %w", err)
		}
		slug, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("decode provider key: %w", errBadKey)
		}

		var p apiProvider
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("decode provider %q: %w", slug, err)
		}
		if slug == "" {
			slug = strings.TrimSpace(p.ID)
		}
		if slug == "" {
			continue // no slug: nothing can ever look it up
		}
		ix.addProvider(slug, p)
	}

	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, fmt.Errorf("decode models document: %w", err)
	}
	// Reject trailing junk (a mirror that appends an error page, two documents
	// concatenated): io.EOF is the only acceptable continuation.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode models document: %w", errTrailingData)
	}
	if ix.ProviderCount() == 0 {
		return nil, ErrEmpty
	}
	return ix, nil
}

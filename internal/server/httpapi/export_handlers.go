package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- panel export / import (design §17) ---

// exportFormatVersion bumps when the snapshot shape changes; import accepts
// exactly this version.
//
// 2 (2026-09-18): the snapshot now CARRIES CIPHERTEXT CREDENTIALS
// (SensitiveSettings + the AI provider blocks). Version 1 files are refused
// rather than half-imported: a v1 file has no secrets at all, so accepting it
// would produce a restore that quietly drops every key and still reports
// success.
const exportFormatVersion = 2

// The snapshot still carries no PLAINTEXT credential material (§17): no
// node_secret_hash, no certificate PEM, no subscription token, and reg_tokens
// are skipped entirely. What changed on 2026-09-18 is that Cryptor ciphertext
// travels with the file — sensitive settings and AI provider
// keys/extra-headers — so a restore does not silently lose every credential.
//
// Two consequences the operator owns (design §20.15):
//   - the backup file is now equivalent to a credential file and must be kept
//     beside .master_key;
//   - ciphertext only opens under the SAME master key, so a restore onto a
//     panel with a different key must NAME what it could not read instead of
//     reporting "unconfigured" (see importSensitiveSettings).
type exportFile struct {
	Version        int               `json:"version"`
	ExportedAt     int64             `json:"exported_at"`
	Nodes          []exportNode      `json:"nodes"`
	LatencyTargets []exportTarget    `json:"latency_targets"`
	Subscriptions  []exportSub       `json:"subscriptions"`
	Templates      []exportTemplate  `json:"templates"`
	Settings       map[string]string `json:"settings"`
	// SensitiveSettings holds Cryptor ciphertext keyed by setting key, and the
	// AI block below carries the provider secrets. All three are omitted when
	// empty so a snapshot with no credentials still looks like one.
	SensitiveSettings map[string]string  `json:"sensitive_settings,omitempty"`
	AIProviders       []exportAIProvider `json:"ai_providers,omitempty"`
	AIModels          []exportAIModel    `json:"ai_models,omitempty"`
	AIProviderModels  []exportAILink     `json:"ai_provider_models,omitempty"`
}

// exportAIProvider mirrors store.AIProvider with its secrets in ciphertext.
type exportAIProvider struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Protocol        string `json:"protocol"`
	BaseURL         string `json:"base_url"`
	APIKeyEnc       string `json:"api_key_enc,omitempty"`
	ExtraHeadersEnc string `json:"extra_headers_enc,omitempty"`
	ModelsDevSlug   string `json:"models_dev_slug,omitempty"`
	Enabled         bool   `json:"enabled"`
}

type exportAIModel struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	ContextWindow    int      `json:"context_window"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	ReasoningLevels  []string `json:"reasoning_levels,omitempty"`
	// How "off" is spelled for this model (§12.5). Carried because losing it
	// silently degrades to "omit", which does not switch thinking off on
	// gateways that think by default.
	ReasoningOffStyle string   `json:"reasoning_off_style,omitempty"`
	OverriddenFields  []string `json:"overridden_fields,omitempty"`
	Source            string   `json:"source"`
	Enabled           bool     `json:"enabled"`
}

type exportAILink struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

type exportNode struct {
	MachineID    string              `json:"machine_id"`
	Name         string              `json:"name"`
	Note         string              `json:"note"`
	CreatedAt    int64               `json:"created_at"`
	Network      *exportNetwork      `json:"network,omitempty"`
	TrafficCycle *exportTrafficCycle `json:"traffic_cycle,omitempty"`
	Billing      *exportBilling      `json:"billing,omitempty"`
	IPs          []exportIP          `json:"ips,omitempty"`
	Singbox      *exportSingbox      `json:"singbox,omitempty"` // metadata only, never cert_pem
}

type exportNetwork struct {
	Iface      string `json:"iface"`
	Mode       string `json:"mode"`
	QuotaBytes *int64 `json:"quota_bytes"`
	// Legacy fields preserve imports created before traffic cycles became a
	// distinct object. Current exports leave them empty.
	AnchorAt *int64 `json:"anchor_at,omitempty"`
}

type exportTrafficCycle struct {
	CycleType   string `json:"cycle_type"`
	NextResetAt *int64 `json:"next_reset_at"`
}

type exportBilling struct {
	CycleType string `json:"cycle_type"`
	CycleDays *int64 `json:"cycle_days"`
	NextDueAt *int64 `json:"next_due_at"`
	Note      string `json:"note"`
}

type exportIP struct {
	IP        string `json:"ip"`
	Family    int    `json:"family"`
	Scope     string `json:"scope"`
	IsPrimary bool   `json:"is_primary"`
}

type exportSingbox struct {
	Version        string `json:"version"`
	DesiredVersion string `json:"desired_version"`
	Status         string `json:"status"`
	Port           int    `json:"port"`
	CertSHA256     string `json:"cert_sha256"`
	CertNotAfter   int64  `json:"cert_not_after"`
}

type exportTarget struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// exportSub binds nodes by machine_id (stable across panels; node ids are
// random) and templates by name (ids are random too).
type exportSub struct {
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Template       string   `json:"template,omitempty"`
	Format         string   `json:"format,omitempty"` // '' = auto (§10 修订)
	NodeMachineIDs []string `json:"node_machine_ids,omitempty"`
	// Entries is the §10.2 binding (target + ingress + alias). NodeMachineIDs
	// stays for snapshots and callers that only ever knew about nodes; when
	// both are present, entries win.
	Entries []exportEntry `json:"entries,omitempty"`
}

// exportEntry binds both ends by machine_id (node ids are random) and carries
// the alias, because a lost alias silently renames nodes for every client.
type exportEntry struct {
	Node    string `json:"node"`
	Relay   string `json:"relay,omitempty"`
	Proto   string `json:"proto,omitempty"`
	SrcPort int    `json:"src_port,omitempty"`
	Iface   string `json:"iface,omitempty"`
	Alias   string `json:"alias,omitempty"`
	Enabled bool   `json:"enabled"`
}

type exportTemplate struct {
	Name    string `json:"name"`
	Format  string `json:"format"`
	Content string `json:"content"`
}

// handleExport streams the snapshot as a download attachment.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ef, err := s.buildExport()
	if err != nil {
		s.Log.Error("export", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="fobe-export.json"`)
	writeJSON(w, http.StatusOK, ef)
	// data is leaving the panel — worth an audit entry even though §17 only
	// requires auditing imports
	s.audit("data_exported", exportSummary(ef), s.Trust.RealIP(r))
}

func (s *Server) buildExport() (*exportFile, error) {
	ef := &exportFile{
		Version:           exportFormatVersion,
		ExportedAt:        nowUnix(),
		Nodes:             []exportNode{},
		LatencyTargets:    []exportTarget{},
		Subscriptions:     []exportSub{},
		Templates:         []exportTemplate{},
		Settings:          map[string]string{},
		SensitiveSettings: map[string]string{},
	}

	nodes, err := s.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	nodeMachine := map[string]string{} // node id → machine id, for subscription bindings
	for i := range nodes {
		n := &nodes[i]
		nodeMachine[n.ID] = n.MachineID
		en := exportNode{
			MachineID: n.MachineID, Name: n.Name, Note: n.Note, CreatedAt: n.CreatedAt,
		}
		if net, err := s.Store.GetNodeNetwork(n.ID); err == nil {
			en.Network = &exportNetwork{
				Iface: net.Iface, Mode: net.Mode, QuotaBytes: net.QuotaBytes,
			}
			en.TrafficCycle = &exportTrafficCycle{CycleType: net.CycleType, NextResetAt: net.NextResetAt}
		}
		if b, err := s.Store.GetNodeBilling(n.ID); err == nil {
			en.Billing = &exportBilling{
				CycleType: b.CycleType, CycleDays: b.CycleDays, NextDueAt: b.NextDueAt, Note: b.Note,
			}
		}
		if ips, err := s.Store.ListNodeIPs(n.ID); err == nil && len(ips) > 0 {
			en.IPs = make([]exportIP, 0, len(ips))
			for _, ip := range ips {
				en.IPs = append(en.IPs, exportIP{IP: ip.IP, Family: ip.Family, Scope: ip.Scope, IsPrimary: ip.IsPrimary})
			}
		}
		if sb, err := s.Store.GetNodeSingbox(n.ID); err == nil {
			en.Singbox = &exportSingbox{
				Version: sb.Version, DesiredVersion: sb.DesiredVersion, Status: sb.Status,
				Port: sb.Port, CertSHA256: sb.CertSHA256, CertNotAfter: sb.CertNotAfter,
			}
		}
		ef.Nodes = append(ef.Nodes, en)
	}

	targets, err := s.Store.ListLatencyTargets()
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		ef.LatencyTargets = append(ef.LatencyTargets, exportTarget{Name: t.Name, Kind: t.Kind, Host: t.Host, Port: t.Port})
	}

	subs, err := s.Store.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	for i := range subs {
		sub := &subs[i]
		es := exportSub{Name: sub.Name, Enabled: sub.Enabled, Format: sub.Format}
		if sub.TemplateID.Valid && sub.TemplateID.String != "" {
			if t, err := s.Store.GetTemplate(sub.TemplateID.String); err == nil {
				es.Template = t.Name
			}
		}
		if ids, err := s.Store.SubscriptionNodeIDs(sub.ID); err == nil {
			for _, id := range ids {
				if mid, ok := nodeMachine[id]; ok {
					es.NodeMachineIDs = append(es.NodeMachineIDs, mid)
				}
			}
		}
		// §10.2: entries are the real binding; the direct projection above is
		// kept in the file so an older panel importing this snapshot still
		// binds the nodes it can represent.
		if entries, err := s.Store.SubscriptionEntries(sub.ID); err == nil {
			for _, e := range entries {
				mid, ok := nodeMachine[e.NodeID]
				if !ok {
					continue
				}
				ex := exportEntry{
					Node: mid, Proto: e.Proto, SrcPort: e.SrcPort, Iface: e.Iface,
					Alias: e.Alias, Enabled: e.Enabled,
				}
				if e.RelayNodeID != "" {
					rmid, ok := nodeMachine[e.RelayNodeID]
					if !ok {
						continue // the relay is gone: the entry is not portable
					}
					ex.Relay = rmid
				}
				es.Entries = append(es.Entries, ex)
			}
		}
		ef.Subscriptions = append(ef.Subscriptions, es)
	}

	tpls, err := s.Store.ListTemplates()
	if err != nil {
		return nil, err
	}
	for _, t := range tpls {
		ef.Templates = append(ef.Templates, exportTemplate{Name: t.Name, Format: t.Format, Content: t.Content})
	}

	// Settings: plaintext into Settings, ciphertext into SensitiveSettings
	// (§17 2026-09-18 修订). Exporting the ciphertext is what makes a restore
	// complete; the cost is that the file is now a credential file (§20.15).
	for key := range allowedKeys {
		val, err := s.Store.GetSetting(key)
		if err != nil || val == "" {
			continue
		}
		if sensitiveKeys[key] {
			ef.SensitiveSettings[key] = val
			continue
		}
		ef.Settings[key] = val
	}

	// §12.5 AI providers/models: the keys and extra headers travel as
	// ciphertext; the model rows and links carry no secrets and are exported
	// so a restored panel does not have to re-fetch every model list.
	providers, err := s.Store.ListAIProviders()
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		ef.AIProviders = append(ef.AIProviders, exportAIProvider{
			ID: p.ID, Name: p.Name, Protocol: p.Protocol, BaseURL: p.BaseURL,
			APIKeyEnc: p.APIKeyEnc, ExtraHeadersEnc: p.ExtraHeadersEnc,
			ModelsDevSlug: p.ModelsDevSlug, Enabled: p.Enabled,
		})
	}
	models, err := s.Store.ListAIModels()
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		ef.AIModels = append(ef.AIModels, exportAIModel{
			ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens, InputModalities: m.InputModalities,
			OutputModalities: m.OutputModalities, ReasoningLevels: m.ReasoningLevels,
			ReasoningOffStyle: m.ReasoningOffStyle,
			OverriddenFields:  m.OverriddenFields, Source: m.Source, Enabled: m.Enabled,
		})
	}
	links, err := s.Store.AIProviderModelIDs()
	if err != nil {
		return nil, err
	}
	for providerID, modelIDs := range links {
		for _, modelID := range modelIDs {
			ef.AIProviderModels = append(ef.AIProviderModels, exportAILink{ProviderID: providerID, ModelID: modelID})
		}
	}
	return ef, nil
}

func exportSummary(ef *exportFile) string {
	return fmt.Sprintf("nodes=%d targets=%d subs=%d templates=%d settings=%d",
		len(ef.Nodes), len(ef.LatencyTargets), len(ef.Subscriptions), len(ef.Templates), len(ef.Settings))
}

// --- import ---

type importStats struct {
	NodesCreated           int `json:"nodes_created"`
	NodesUpdated           int `json:"nodes_updated"`
	LatencyTargetsCreated  int `json:"latency_targets_created"`
	SubscriptionsCreated   int `json:"subscriptions_created"`
	SubscriptionsUpdated   int `json:"subscriptions_updated"`
	TemplatesCreated       int `json:"templates_created"`
	TemplatesUpdated       int `json:"templates_updated"`
	SettingsImported       int `json:"settings_imported"`
	SensitiveImported      int `json:"sensitive_imported"`
	AIProvidersImported    int `json:"ai_providers_imported"`
	AIModelsImported       int `json:"ai_models_imported"`
	AIProviderModelsLinked int `json:"ai_provider_models_linked"`
	// SkippedSecrets names every credential the restore could NOT read (wrong
	// master key) or had to drop. It is part of the response on purpose: a
	// silent skip is indistinguishable from "you never configured it", which
	// is the failure mode §10.1's invariant exists to prevent.
	SkippedSecrets []string `json:"skipped_secrets,omitempty"`
}

// handleImport merges a previously exported snapshot into this panel
// (idempotent by machine_id / name; the counts are returned and audited).
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var ef exportFile
	if err := decodeJSON(r, &ef); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if ef.Version != exportFormatVersion {
		writeErr(w, http.StatusBadRequest, "bad_version")
		return
	}
	stats, err := s.runImport(&ef)
	if err != nil {
		s.Log.Error("import", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// A restored snapshot may carry per-inbound entries (§10.2 实现修订
	// 2026-09-18) next to rows this panel has not split yet (a node that never
	// reported its inbounds); without a reconcile both would render and the node
	// would appear twice. The same pass also enrols relay entries, which is what
	// a restart would have done anyway.
	s.ReconcileSubscriptionEntries()
	detail, _ := json.Marshal(stats)
	s.audit("data_imported", string(detail), s.Trust.RealIP(r))
	s.publishEvent("data_imported", "")
	writeJSON(w, http.StatusOK, stats)
}

// runImport merges the snapshot sections: nodes first (subscriptions need
// their ids), then templates (subscriptions may bind one by name).
func (s *Server) runImport(ef *exportFile) (*importStats, error) {
	stats := &importStats{}

	nodeByMachine, err := s.importNodes(ef.Nodes, stats)
	if err != nil {
		return nil, err
	}
	if err := s.importTemplates(ef.Templates, stats); err != nil {
		return nil, err
	}
	if err := s.importLatencyTargets(ef.LatencyTargets, stats); err != nil {
		return nil, err
	}
	if err := s.importSubscriptions(ef.Subscriptions, nodeByMachine, stats); err != nil {
		return nil, err
	}
	s.importSettings(ef.Settings, stats)
	// Secrets and AI configuration last: a restored provider list is useless
	// without its default pair, and both depend on nothing else in the file.
	s.importSensitiveSettings(ef.SensitiveSettings, stats)
	s.importAISettings(ef, stats)
	return stats, nil
}

// importSensitiveSettings restores write-only settings from ciphertext.
//
// The decrypt check is the whole point. Ciphertext only opens under the SAME
// master key, so without the check a snapshot restored onto a panel with a
// different key would install values no code path can read: every notification
// channel silently dead, every login to the bot failing, and nothing in the
// panel saying why. A value that does not decrypt is skipped and NAMED in the
// result instead (§10.1's invariant for anytls passwords, applied to every
// secret).
func (s *Server) importSensitiveSettings(settings map[string]string, stats *importStats) {
	for key, value := range settings {
		if !sensitiveKeys[key] || strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := s.Crypt.Decrypt(value); err != nil {
			stats.SkippedSecrets = append(stats.SkippedSecrets, key)
			continue
		}
		if err := s.Store.SetSetting(key, value, true); err != nil {
			s.Log.Warn("import sensitive setting", "key", key, "err", err)
			continue
		}
		stats.SensitiveImported++
	}
	sort.Strings(stats.SkippedSecrets) // deterministic result for tests and operators
}

// importAISettings restores providers, models and links (§12.5).
//
// A provider whose key cannot be decrypted is still imported — the row, its
// base_url and its protocol are useful, and the operator only has to re-enter
// the key — but the unreadable ciphertext is DROPPED rather than stored. Keeping
// it would leave the panel looking configured while every call failed with
// ai_key_unreadable, and the id is already named in SkippedSecrets.
func (s *Server) importAISettings(ef *exportFile, stats *importStats) {
	for _, p := range ef.AIProviders {
		if p.ID == "" || !store.ValidAIProtocol(p.Protocol) {
			continue
		}
		baseURL, err := normalizeProviderBaseURL(p.BaseURL)
		if err != nil {
			continue
		}
		row := &store.AIProvider{
			ID: p.ID, Name: p.Name, Protocol: p.Protocol, BaseURL: baseURL,
			ModelsDevSlug: p.ModelsDevSlug, Enabled: p.Enabled,
		}
		if existing, err := s.Store.GetAIProvider(p.ID); err == nil {
			row.CreatedAt = existing.CreatedAt
		}
		if row.CreatedAt == 0 {
			row.CreatedAt = nowUnix()
		}
		row.UpdatedAt = nowUnix()

		if p.APIKeyEnc != "" {
			if _, err := s.Crypt.Decrypt(p.APIKeyEnc); err != nil {
				stats.SkippedSecrets = append(stats.SkippedSecrets, "ai_provider:"+p.ID+":api_key")
			} else {
				row.APIKeyEnc = p.APIKeyEnc
			}
		}
		if p.ExtraHeadersEnc != "" {
			plain, err := s.Crypt.Decrypt(p.ExtraHeadersEnc)
			if err == nil {
				_, err = decodeExtraHeaders(plain)
			}
			if err != nil {
				stats.SkippedSecrets = append(stats.SkippedSecrets, "ai_provider:"+p.ID+":extra_headers")
			} else {
				row.ExtraHeadersEnc = p.ExtraHeadersEnc
			}
		}
		if err := s.Store.UpsertAIProvider(row); err != nil {
			s.Log.Warn("import ai provider", "id", p.ID, "err", err)
			continue
		}
		stats.AIProvidersImported++
	}

	for _, m := range ef.AIModels {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		model := &store.AIModel{
			ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens, Source: m.Source, Enabled: m.Enabled,
		}
		if model.DisplayName == "" {
			model.DisplayName = model.ID
		}
		if model.Source == "" {
			model.Source = "manual"
		}
		// Drop vocabulary we do not know instead of importing a level the
		// adapter layer cannot translate (§12.5).
		for _, level := range m.ReasoningLevels {
			if aiReasoningLevels[level] {
				model.ReasoningLevels = append(model.ReasoningLevels, level)
			}
		}
		for _, field := range m.OverriddenFields {
			if aiModelOverrideFields[field] {
				model.OverriddenFields = append(model.OverriddenFields, field)
			}
		}
		if aiReasoningOffStyles[m.ReasoningOffStyle] {
			model.ReasoningOffStyle = m.ReasoningOffStyle
		}
		model.InputModalities, model.OutputModalities = m.InputModalities, m.OutputModalities
		if existing, err := s.Store.GetAIModel(m.ID); err == nil {
			model.CreatedAt = existing.CreatedAt
		}
		if model.CreatedAt == 0 {
			model.CreatedAt = nowUnix()
		}
		model.UpdatedAt = nowUnix()
		if err := s.Store.UpsertAIModel(model); err != nil {
			s.Log.Warn("import ai model", "id", m.ID, "err", err)
			continue
		}
		stats.AIModelsImported++
	}

	for _, l := range ef.AIProviderModels {
		// Link only pairs whose two halves actually landed: a dangling link
		// cannot exist (foreign keys) and would abort the whole import.
		if _, err := s.Store.GetAIProvider(l.ProviderID); err != nil {
			continue
		}
		if _, err := s.Store.GetAIModel(l.ModelID); err != nil {
			continue
		}
		if err := s.Store.LinkAIProviderModel(l.ProviderID, l.ModelID); err != nil {
			continue
		}
		stats.AIProviderModelsLinked++
	}
	sort.Strings(stats.SkippedSecrets)
}

func (s *Server) importNodes(nodes []exportNode, stats *importStats) (map[string]string, error) {
	existing, err := s.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	byMachine := make(map[string]string, len(existing)) // machine_id → node id
	for _, n := range existing {
		byMachine[n.MachineID] = n.ID
	}

	for i := range nodes {
		en := &nodes[i]
		if en.MachineID == "" {
			continue
		}
		if id, ok := byMachine[en.MachineID]; ok {
			s.mergeNode(id, en)
			stats.NodesUpdated++
			continue
		}
		id, err := s.createImportedNode(en)
		if err != nil {
			return nil, err
		}
		byMachine[en.MachineID] = id
		stats.NodesCreated++
	}
	return byMachine, nil
}

// mergeNode updates an existing node from the snapshot. Identity data stays:
// the probe owns its secret and reports its own addresses.
func (s *Server) mergeNode(id string, en *exportNode) {
	if en.Name != "" {
		_ = s.Store.RenameNode(id, en.Name)
	}
	_ = s.Store.SetNodeNote(id, en.Note)
	s.mergeNodeConfig(id, en)
}

// mergeNodeConfig applies the snapshot's network / billing rows.
func (s *Server) mergeNodeConfig(id string, en *exportNode) {
	if en.Network != nil {
		net := &store.NodeNetwork{
			NodeID: id, Iface: en.Network.Iface, Mode: en.Network.Mode,
			QuotaBytes: en.Network.QuotaBytes, CycleType: "none",
		}
		if en.TrafficCycle != nil {
			net.CycleType = en.TrafficCycle.CycleType
			net.NextResetAt = en.TrafficCycle.NextResetAt
		} else if en.Network.AnchorAt != nil {
			// Older v1 exports used a monthly traffic anchor in the network
			// object. Interpret it as the new cycle's reset anchor.
			net.CycleType = "month"
			net.NextResetAt = en.Network.AnchorAt
		}
		_ = s.Store.UpsertNodeNetwork(net)
	}
	if en.Billing != nil {
		_ = s.Store.UpsertNodeBilling(&store.NodeBilling{
			NodeID: id, CycleType: en.Billing.CycleType, CycleDays: en.Billing.CycleDays,
			NextDueAt: en.Billing.NextDueAt, Note: en.Billing.Note,
		})
	}
}

// createImportedNode materializes a placeholder node with a fresh id and a
// random secret hash, so subscriptions and billing can reference it even
// before the probe itself ever registers on this panel.
func (s *Server) createImportedNode(en *exportNode) (string, error) {
	id, err := randomID()
	if err != nil {
		return "", err
	}
	_, hash, err := newNodeSecret()
	if err != nil {
		return "", err
	}
	n := &store.Node{
		ID: id, Name: defaultNodeName(en.Name, id), MachineID: en.MachineID, Note: en.Note,
	}
	if err := s.Store.CreateNode(n, hash); err != nil {
		return "", err
	}
	s.mergeNodeConfig(id, en)
	if len(en.IPs) > 0 {
		rows := make([]store.IPRow, 0, len(en.IPs))
		for _, ip := range en.IPs {
			rows = append(rows, store.IPRow{IP: ip.IP, Family: ip.Family, Scope: ip.Scope, IsPrimary: ip.IsPrimary})
		}
		_ = s.Store.ReplaceNodeIPs(id, rows)
	}
	return id, nil
}

func (s *Server) importTemplates(tpls []exportTemplate, stats *importStats) error {
	existing, err := s.Store.ListTemplates()
	if err != nil {
		return err
	}
	byName := make(map[string]store.Template, len(existing))
	for _, t := range existing {
		byName[t.Name] = t
	}
	// A snapshot written before {{rules}} was removed still carries the
	// placeholder in its templates (the snippets themselves are no longer an
	// importable setting). Inline them exactly like the startup migration does,
	// so an old backup restores a renderable template instead of one with a
	// literal "{{rules}}" in it (§10 实现修订 2026-09-16b).
	sbSnippet, clashSnippet := s.legacyRuleSnippets()
	for _, et := range tpls {
		name := strings.TrimSpace(et.Name)
		content := inlineLegacyRules(et.Content, et.Format, sbSnippet, clashSnippet)
		// skip entries the template editor would reject anyway
		if name == "" || !validFormat(et.Format) || !templatePlaceholdersOK(content) {
			continue
		}
		if t, ok := byName[name]; ok {
			t.Format, t.Content = et.Format, content
			if err := s.Store.UpdateTemplate(&t); err != nil {
				return err
			}
			stats.TemplatesUpdated++
			continue
		}
		id, err := randomID()
		if err != nil {
			return err
		}
		t := store.Template{ID: id, Name: name, Format: et.Format, Content: content}
		if err := s.Store.InsertTemplate(&t); err != nil {
			return err
		}
		byName[name] = t
		stats.TemplatesCreated++
	}
	return nil
}

func targetKey(name, host string, port int) string {
	return name + "\x00" + host + "\x00" + strconv.Itoa(port)
}

func (s *Server) importLatencyTargets(targets []exportTarget, stats *importStats) error {
	existing, err := s.Store.ListLatencyTargets()
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	for _, t := range existing {
		have[targetKey(t.Name, t.Host, t.Port)] = true
	}
	for _, et := range targets {
		if et.Host == "" {
			continue
		}
		kind := et.Kind
		if kind != "icmp" && kind != "tcp" {
			continue
		}
		port := et.Port
		if kind == "tcp" && (port <= 0 || port > 65535) {
			continue
		}
		if kind == "icmp" {
			port = 0
		}
		key := targetKey(et.Name, et.Host, port)
		if have[key] {
			continue // idempotent by (name, host, port)
		}
		if _, err := s.Store.CreateLatencyTarget(et.Name, kind, et.Host, port); err != nil {
			return err
		}
		have[key] = true
		stats.LatencyTargetsCreated++
	}
	return nil
}

func (s *Server) importSubscriptions(subs []exportSub, nodeByMachine map[string]string, stats *importStats) error {
	existing, err := s.Store.ListSubscriptions()
	if err != nil {
		return err
	}
	byName := make(map[string]store.Subscription, len(existing))
	for _, sub := range existing {
		byName[sub.Name] = sub
	}
	tplByName := map[string]string{} // template name → id (imported ones included)
	if tpls, err := s.Store.ListTemplates(); err == nil {
		for _, t := range tpls {
			tplByName[t.Name] = t.ID
		}
	}

	for _, es := range subs {
		name := strings.TrimSpace(es.Name)
		if name == "" {
			continue
		}
		nodeIDs := resolveImportedNodes(es.NodeMachineIDs, nodeByMachine)
		var tplID *string
		if es.Template != "" {
			if id, ok := tplByName[es.Template]; ok {
				tplID = &id
			}
		}
		if sub, ok := byName[name]; ok {
			if err := s.Store.SetSubscriptionEnabled(sub.ID, es.Enabled); err != nil {
				return err
			}
			if es.NodeMachineIDs != nil {
				if err := s.Store.SetSubscriptionNodes(sub.ID, nodeIDs); err != nil {
					return err
				}
			}
			// §10.2: entries win over the legacy direct projection, which is
			// why they are applied second (a newer snapshot carries both).
			if es.Entries != nil {
				if err := s.Store.RestoreSubscriptionEntries(sub.ID, importEntries(es.Entries, nodeByMachine)); err != nil {
					return err
				}
			}
			if tplID != nil {
				if err := s.Store.SetSubscriptionMeta(sub.ID, sub.Name, tplID); err != nil {
					return err
				}
			}
			// An imported pin is applied only when the snapshot carries one, so
			// re-importing an older file cannot silently unpin a subscription.
			if validFormat(es.Format) {
				if err := s.Store.SetSubscriptionFormat(sub.ID, es.Format); err != nil {
					return err
				}
			}
			stats.SubscriptionsUpdated++
			continue
		}
		id, err := randomID()
		if err != nil {
			return err
		}
		token, err := security.RandomToken(24)
		if err != nil {
			return err
		}
		// The import mints a fresh token (the snapshot carries none, §17) and
		// stores it encrypted too, so the URL of an imported subscription is
		// copyable from the panel like any other (§10 实现修订 2026-09-16).
		enc, err := s.Crypt.Encrypt(token)
		if err != nil {
			return err
		}
		if err := s.Store.CreateSubscription(id, name, tokenHash(token), enc); err != nil {
			return err
		}
		if len(nodeIDs) > 0 {
			if err := s.Store.SetSubscriptionNodes(id, nodeIDs); err != nil {
				return err
			}
		}
		if es.Entries != nil {
			if err := s.Store.RestoreSubscriptionEntries(id, importEntries(es.Entries, nodeByMachine)); err != nil {
				return err
			}
		}
		if tplID != nil {
			if err := s.Store.SetSubscriptionMeta(id, name, tplID); err != nil {
				return err
			}
		}
		if validFormat(es.Format) {
			if err := s.Store.SetSubscriptionFormat(id, es.Format); err != nil {
				return err
			}
		}
		byName[name] = store.Subscription{ID: id, Name: name, Enabled: es.Enabled}
		stats.SubscriptionsCreated++
	}
	return nil
}

// resolveImportedNodes maps snapshot machine_ids to live node ids, skipping
// machines this panel does not know.
func resolveImportedNodes(machineIDs []string, nodeByMachine map[string]string) []string {
	if len(machineIDs) == 0 {
		return nil
	}
	out := make([]string, 0, len(machineIDs))
	for _, mid := range machineIDs {
		if id, ok := nodeByMachine[mid]; ok {
			out = append(out, id)
		}
	}
	return out
}

// importEntries maps a snapshot's §10.2 rows back onto this panel's node ids.
// An entry whose target node is unknown cannot be rendered, and one whose relay
// is unknown would silently degrade into "directly dial B" — neither is what
// the file said, so both are dropped instead of guessed.
func importEntries(entries []exportEntry, nodeByMachine map[string]string) []store.SubscriptionEntry {
	out := make([]store.SubscriptionEntry, 0, len(entries))
	for _, e := range entries {
		nodeID, ok := nodeByMachine[e.Node]
		if !ok {
			continue
		}
		entry := store.SubscriptionEntry{
			NodeID: nodeID, Proto: e.Proto, SrcPort: e.SrcPort, Iface: e.Iface,
			Alias: e.Alias, Enabled: e.Enabled,
		}
		if e.Relay != "" {
			relayID, ok := nodeByMachine[e.Relay]
			if !ok || relayID == nodeID {
				continue
			}
			entry.RelayNodeID = relayID
		}
		out = append(out, entry)
	}
	return out
}

// importSettings applies plaintext settings only. Unknown keys and
// sensitiveKeys are dropped here — secrets come in through
// importSensitiveSettings, which can TELL whether a value is readable, and a
// hand-crafted snapshot must not be able to write a key this endpoint cannot
// validate.
func (s *Server) importSettings(settings map[string]string, stats *importStats) {
	for key, value := range settings {
		if !allowedKeys[key] || sensitiveKeys[key] {
			continue
		}
		if key == "server.public_url" {
			if _, err := normalizePublicURL(value); err != nil {
				continue
			}
		}
		// A hand-edited snapshot must not install a §14.1 policy the API would
		// have rejected (a nonsense threshold or a non-URL source).
		if geoIPSettingKey(key) && !validGeoIPSetting(key, value) {
			continue
		}
		if err := s.Store.SetSetting(key, value, false); err != nil {
			s.Log.Warn("import setting", "key", key, "err", err)
			continue
		}
		stats.SettingsImported++
	}
}

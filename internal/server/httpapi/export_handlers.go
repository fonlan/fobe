package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// --- panel export / import (design §17: JSON snapshot without credentials) ---

// exportFormatVersion bumps when the snapshot shape changes; import accepts
// exactly this version.
const exportFormatVersion = 1

// The snapshot deliberately carries no credential material (§17): no
// node_secret_hash, no certificate PEM, no subscription token (hash or
// plaintext), no sensitive settings, and reg_tokens are skipped entirely.
type exportFile struct {
	Version        int               `json:"version"`
	ExportedAt     int64             `json:"exported_at"`
	Nodes          []exportNode      `json:"nodes"`
	LatencyTargets []exportTarget    `json:"latency_targets"`
	Subscriptions  []exportSub       `json:"subscriptions"`
	Templates      []exportTemplate  `json:"templates"`
	Settings       map[string]string `json:"settings"`
}

type exportNode struct {
	MachineID string         `json:"machine_id"`
	Name      string         `json:"name"`
	Note      string         `json:"note"`
	CreatedAt int64          `json:"created_at"`
	Network   *exportNetwork `json:"network,omitempty"`
	Billing   *exportBilling `json:"billing,omitempty"`
	IPs       []exportIP     `json:"ips,omitempty"`
	Singbox   *exportSingbox `json:"singbox,omitempty"` // metadata only, never cert_pem
}

type exportNetwork struct {
	Iface      string `json:"iface"`
	Mode       string `json:"mode"`
	QuotaBytes *int64 `json:"quota_bytes"`
	CycleDays  *int64 `json:"cycle_days"`
	AnchorAt   *int64 `json:"anchor_at"`
	TZ         string `json:"tz"`
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
	NodeMachineIDs []string `json:"node_machine_ids,omitempty"`
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
		Version:        exportFormatVersion,
		ExportedAt:     nowUnix(),
		Nodes:          []exportNode{},
		LatencyTargets: []exportTarget{},
		Subscriptions:  []exportSub{},
		Templates:      []exportTemplate{},
		Settings:       map[string]string{},
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
				CycleDays: net.CycleDays, AnchorAt: net.AnchorAt, TZ: net.TZ,
			}
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
		es := exportSub{Name: sub.Name, Enabled: sub.Enabled}
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
		ef.Subscriptions = append(ef.Subscriptions, es)
	}

	tpls, err := s.Store.ListTemplates()
	if err != nil {
		return nil, err
	}
	for _, t := range tpls {
		ef.Templates = append(ef.Templates, exportTemplate{Name: t.Name, Format: t.Format, Content: t.Content})
	}

	// plaintext keys only: sensitiveKeys stay write-only (§4.4)
	for key := range allowedKeys {
		if sensitiveKeys[key] {
			continue
		}
		val, err := s.Store.GetSetting(key)
		if err != nil || val == "" {
			continue
		}
		ef.Settings[key] = val
	}
	return ef, nil
}

func exportSummary(ef *exportFile) string {
	return fmt.Sprintf("nodes=%d targets=%d subs=%d templates=%d settings=%d",
		len(ef.Nodes), len(ef.LatencyTargets), len(ef.Subscriptions), len(ef.Templates), len(ef.Settings))
}

// --- import ---

type importStats struct {
	NodesCreated          int `json:"nodes_created"`
	NodesUpdated          int `json:"nodes_updated"`
	LatencyTargetsCreated int `json:"latency_targets_created"`
	SubscriptionsCreated  int `json:"subscriptions_created"`
	SubscriptionsUpdated  int `json:"subscriptions_updated"`
	TemplatesCreated      int `json:"templates_created"`
	TemplatesUpdated      int `json:"templates_updated"`
	SettingsImported      int `json:"settings_imported"`
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
	return stats, nil
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
		_ = s.Store.UpsertNodeNetwork(&store.NodeNetwork{
			NodeID: id, Iface: en.Network.Iface, Mode: en.Network.Mode,
			QuotaBytes: en.Network.QuotaBytes, CycleDays: en.Network.CycleDays,
			AnchorAt: en.Network.AnchorAt, TZ: en.Network.TZ,
		})
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
	for _, et := range tpls {
		name := strings.TrimSpace(et.Name)
		// skip entries the template editor would reject anyway
		if name == "" || !validFormat(et.Format) || !templatePlaceholdersOK(et.Content) {
			continue
		}
		if t, ok := byName[name]; ok {
			t.Format, t.Content = et.Format, et.Content
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
		t := store.Template{ID: id, Name: name, Format: et.Format, Content: et.Content}
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
			if tplID != nil {
				if err := s.Store.SetSubscriptionMeta(sub.ID, sub.Name, tplID); err != nil {
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
		if err := s.Store.CreateSubscription(id, name, tokenHash(token)); err != nil {
			return err
		}
		if len(nodeIDs) > 0 {
			if err := s.Store.SetSubscriptionNodes(id, nodeIDs); err != nil {
				return err
			}
		}
		if tplID != nil {
			if err := s.Store.SetSubscriptionMeta(id, name, tplID); err != nil {
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

// importSettings applies plaintext settings only: unknown keys and
// sensitiveKeys are dropped — a snapshot never carries credentials, and an
// uploaded one must not be able to write them either.
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
		if err := s.Store.SetSetting(key, value, false); err != nil {
			s.Log.Warn("import setting", "key", key, "err", err)
			continue
		}
		stats.SettingsImported++
	}
}

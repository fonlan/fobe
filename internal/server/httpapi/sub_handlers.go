package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// --- subscriptions & templates (design §10) ---

// subscription formats. Output format choice: ?format= beats UA sniffing,
// sniff failure defaults to singbox.
const (
	FormatSingbox = "singbox"
	FormatClash   = "clash"
)

func validFormat(f string) bool { return f == FormatSingbox || f == FormatClash }

// Placeholder every stored template must contain (§10): {{nodes}} is where the
// renderer injects the anytls outbound list. Routing rules are written straight
// into the template — the {{rules}} placeholder and the two settings that used
// to fill it were removed (§10 实现修订 2026-09-16b).
const placeholderNodes = "{{nodes}}"

func templatePlaceholdersOK(content string) bool {
	return strings.Contains(content, placeholderNodes)
}

// --- legacy {{rules}} upgrade (§10 实现修订 2026-09-16b) ---
//
// legacyRulesPlaceholder is the deleted placeholder. It survives in code only to
// upgrade data written before the removal: a stored template that still carries
// it gets the snippet from the (equally deleted) settings inlined once at
// startup, so an upgrade cannot turn a working subscription into a config with a
// literal "{{rules}}" in it. New and edited templates are *refused* instead —
// an operator who pastes an old template deserves an error, not a broken client.
const legacyRulesPlaceholder = "{{rules}}"

// Dead keys: removed from allowedKeys, readable/writable nowhere, kept only as
// the source text of the one-time inlining and dropped once it has run.
const (
	legacySettingRulesSingbox = "sub.rules_singbox"
	legacySettingRulesClash   = "sub.rules_clash"
)

// legacyRuleSnippets returns the two snippets the deleted settings used to
// hold (both empty once the migration below has consumed them).
func (s *Server) legacyRuleSnippets() (singbox, clash string) {
	sb, _ := s.Store.GetSetting(legacySettingRulesSingbox)
	cl, _ := s.Store.GetSetting(legacySettingRulesClash)
	return sb, cl
}

// inlineLegacyRules replaces the removed placeholder with the snippet the old
// settings held for that format — the exact substitution the renderer used to
// do, just performed once against the stored template instead of on every
// fetch. An empty snippet (or no placeholder at all) leaves the text alone.
func inlineLegacyRules(content, format, sbSnippet, clashSnippet string) string {
	if !strings.Contains(content, legacyRulesPlaceholder) {
		return content
	}
	snippet := sbSnippet
	if format == FormatClash {
		snippet = clashSnippet
	}
	return strings.ReplaceAll(content, legacyRulesPlaceholder, snippet)
}

// MigrateLegacyRules is the one-time upgrade: every stored template still
// carrying {{rules}} gets the old snippet inlined, then the two dead settings
// are dropped. Returns the number of templates rewritten.
//
// The settings are deleted only when every rewrite succeeded: a failed one keeps
// the source snippet around so the next startup can retry instead of
// irretrievably replacing rules with nothing.
func (s *Server) MigrateLegacyRules() int {
	sb, cl := s.legacyRuleSnippets()
	tpls, err := s.Store.ListTemplates()
	if err != nil {
		s.Log.Warn("legacy rules migration: list templates", "err", err)
		return 0
	}
	migrated, failed := 0, 0
	for i := range tpls {
		t := &tpls[i]
		next := inlineLegacyRules(t.Content, t.Format, sb, cl)
		if next == t.Content {
			continue
		}
		t.Content = next
		if err := s.Store.UpdateTemplate(t); err != nil {
			s.Log.Warn("legacy rules migration: update template", "template", t.Name, "err", err)
			failed++
			continue
		}
		migrated++
	}
	if failed == 0 {
		_ = s.Store.DeleteSetting(legacySettingRulesSingbox)
		_ = s.Store.DeleteSetting(legacySettingRulesClash)
	}
	return migrated
}

// --- subscriptions API ---

type subscriptionView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	CreatedAt     int64    `json:"created_at"`
	TemplateID    *string  `json:"template_id,omitempty"`
	UAFilter      string   `json:"ua_filter"`
	NodeIDs       []string `json:"node_ids"`
	LinkAvailable bool     `json:"link_available"`
	// Format is '' (auto) or a pinned output format (§10 实现修订 2026-09-16).
	Format string `json:"format"`
	// EntryCount is the number of *enabled* §10.2 entries, which is what the
	// subscription actually emits — NodeIDs (the legacy direct projection)
	// cannot express a node appearing twice, once directly and once via a relay.
	EntryCount int `json:"entry_count"`
}

func (s *Server) subscriptionView(sub *store.Subscription) subscriptionView {
	nodeIDs, _ := s.Store.SubscriptionNodeIDs(sub.ID)
	entries, _ := s.Store.SubscriptionEntries(sub.ID)
	entryCount := 0
	for _, e := range entries {
		if e.Enabled {
			entryCount++
		}
	}
	v := subscriptionView{
		ID: sub.ID, Name: sub.Name, Enabled: sub.Enabled, CreatedAt: sub.CreatedAt,
		UAFilter: sub.UAFilter,
		NodeIDs:  nodeIDs,
		Format:   sub.Format,
		// §10 实现修订 2026-09-16: the URL can be re-shown while the ciphertext
		// is present; legacy rows (created before the column existed) can only
		// get a working link by rotating it.
		LinkAvailable: sub.TokenEnc != "",
		EntryCount:    entryCount,
	}
	if sub.TemplateID.Valid && sub.TemplateID.String != "" {
		v.TemplateID = &sub.TemplateID.String
	}
	return v
}

func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.Store.ListSubscriptions()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]subscriptionView, 0, len(subs))
	for i := range subs {
		out = append(out, s.subscriptionView(&subs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

type createSubscriptionReq struct {
	Name string `json:"name"`
}

// handleCreateSubscription mints a subscription and returns its URL. The token
// is stored twice (§10 实现修订 2026-09-16): hashed for /sub/<token> lookups,
// and as Cryptor ciphertext so the panel can re-show the link at any time.
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var req createSubscriptionReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name_required")
		return
	}
	id, err := security.RandomToken(8)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	token, err := security.RandomToken(24)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	enc, err := s.Crypt.Encrypt(token)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.CreateSubscription(id, name, tokenHash(token), enc); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "subscription_created", Command: name, SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("subscriptions_changed", id)

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "token": token, "url": s.subscriptionURL(r, token)})
}

// subscriptionURL builds the client-facing URL for one token. An unconfigured
// server.public_url yields an empty string (the panel then shows the bare
// path instead of a wrong origin).
func (s *Server) subscriptionURL(r *http.Request, token string) string {
	base, err := s.publicBaseURL(r)
	if err != nil {
		return ""
	}
	return base + "/sub/" + token
}

// subscriptionToken decrypts the stored token ciphertext. A row created before
// the token_enc column (or one whose ciphertext no longer decrypts, e.g. after
// a FOBE_MASTER_KEY change) has no recoverable plaintext — only rotation can
// mint a usable link again.
func (s *Server) subscriptionToken(sub *store.Subscription) (string, error) {
	if sub.TokenEnc == "" {
		return "", errTokenUnrecoverable
	}
	token, err := s.Crypt.Decrypt(sub.TokenEnc)
	if err != nil || token == "" {
		return "", errTokenUnrecoverable
	}
	return token, nil
}

var errTokenUnrecoverable = errors.New("subscription token is not recoverable")

// handleSubscriptionLink re-reveals an existing subscription's URL (§10 实现修订
// 2026-09-16: copy it whenever you like, not only at creation time).
func (s *Server) handleSubscriptionLink(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sub, err := s.Store.GetSubscription(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	token, err := s.subscriptionToken(sub)
	if err != nil {
		// 409, not 404: the subscription exists, its URL just cannot be
		// reconstructed — the panel turns this into "rotate to get a new link".
		writeErr(w, http.StatusConflict, "link_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "url": s.subscriptionURL(r, token)})
}

// handleUpdateSubscription toggles enabled / renames / rebinds the template.
// A disabled subscription 404s at /sub/<token> (§10 吊销 URL).
func (s *Server) handleUpdateSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sub, err := s.Store.GetSubscription(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req struct {
		Enabled    *bool    `json:"enabled,omitempty"`
		Name       *string  `json:"name,omitempty"`
		TemplateID **string `json:"template_id,omitempty"` // null → clear, string → set
		UAFilter   *string  `json:"ua_filter,omitempty"`
		Format     *string  `json:"format,omitempty"` // '' → auto, else singbox/clash
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Format != nil {
		f := strings.TrimSpace(*req.Format)
		if f != "" && !validFormat(f) {
			writeErr(w, http.StatusBadRequest, "bad_format")
			return
		}
		if err := s.Store.SetSubscriptionFormat(id, f); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if req.UAFilter != nil {
		if err := s.Store.SetSubscriptionUAFilter(id, normalizeUAFilter(*req.UAFilter)); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if req.Enabled != nil {
		if err := s.Store.SetSubscriptionEnabled(id, *req.Enabled); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if req.Name != nil || req.TemplateID != nil {
		name := sub.Name
		if req.Name != nil {
			name = strings.TrimSpace(*req.Name)
			if name == "" {
				writeErr(w, http.StatusBadRequest, "name_required")
				return
			}
		}
		var tpl *string
		if req.TemplateID != nil {
			tpl = *req.TemplateID
			if tpl != nil && *tpl != "" {
				t, err := s.Store.GetTemplate(*tpl)
				if errors.Is(err, store.ErrNotFound) {
					writeErr(w, http.StatusBadRequest, "template_not_found")
					return
				}
				if err != nil {
					writeErr(w, http.StatusInternalServerError, "internal")
					return
				}
				tid := t.ID
				tpl = &tid
			}
		} else {
			tpl = nil
			if sub.TemplateID.Valid {
				t := sub.TemplateID.String
				tpl = &t
			}
		}
		if err := s.Store.SetSubscriptionMeta(id, name, tpl); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	s.publishEvent("subscriptions_changed", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSubscription(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.DeleteSubscription(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "subscription_deleted", SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("subscriptions_changed", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// subEntryInput is one picker row (§10.2). Selected=false only ever writes a
// *tombstone* for an entry that is already bound: an untouched candidate listed
// as "off" must stay absent from the table, otherwise the first save would
// freeze auto-enrolment forever (store.SetSubscriptionEntries owns that rule).
type subEntryInput struct {
	NodeID      string `json:"node_id"`
	RelayNodeID string `json:"relay_node_id"`
	Proto       string `json:"proto"`
	SrcPort     int    `json:"src_port"`
	Iface       string `json:"iface"`
	Alias       string `json:"alias"`
	Selected    bool   `json:"selected"`
}

type setSubNodesReq struct {
	// NodeIDs is the pre-§10.2 shape: "these nodes, direct entry only". Still
	// accepted so an old frontend (or a §17 snapshot) keeps working; it owns
	// the direct half and leaves relay rows to the reconciler.
	NodeIDs []string        `json:"node_ids"`
	Entries []subEntryInput `json:"entries"`
}

func (s *Server) handleSetSubscriptionNodes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSubscription(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req setSubNodesReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Entries == nil {
		// foreign keys would reject unknown ids mid-transaction; validate first
		for _, nid := range req.NodeIDs {
			if _, err := s.Store.GetNode(nid); err != nil {
				writeErr(w, http.StatusBadRequest, "bad_request")
				return
			}
		}
		if err := s.Store.SetSubscriptionNodes(id, req.NodeIDs); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		s.publishEvent("subscriptions_changed", id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	entries, code := s.validateSubEntries(req.Entries)
	if code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	if err := s.Store.SetSubscriptionEntries(id, entries); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", Action: "subscription_entries_updated",
		Command: fmt.Sprintf("%d entries", len(entries)), SourceIP: s.Trust.RealIP(r),
	})
	s.publishEvent("subscriptions_changed", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// validateSubEntries turns the picker payload into store rows. It returns a
// snake_case error code (§16) rather than an error: the panel maps codes to
// i18n text, so "why did my save not stick" stays answerable in both languages.
func (s *Server) validateSubEntries(in []subEntryInput) ([]store.SubscriptionEntry, string) {
	seenIdentity := map[string]bool{}
	seenAlias := map[string]bool{}
	out := make([]store.SubscriptionEntry, 0, len(in))
	for _, e := range in {
		if _, err := s.Store.GetNode(e.NodeID); err != nil {
			return nil, "bad_request"
		}
		if e.RelayNodeID != "" {
			if e.RelayNodeID == e.NodeID {
				return nil, "bad_request" // a self-loop is not a relay (§10.2)
			}
			if _, err := s.Store.GetNode(e.RelayNodeID); err != nil {
				return nil, "bad_request"
			}
		}
		switch e.Proto {
		case "", "tcp", "udp":
		default:
			return nil, "bad_request"
		}
		if e.SrcPort < 0 || e.SrcPort > 65535 {
			return nil, "bad_request"
		}
		// Trimmed here, not only in the panel: a name of spaces would render as
		// a blank node in every client, and "" already means "derive the name".
		e.Alias = strings.TrimSpace(e.Alias)
		if !store.ValidAlias(e.Alias) {
			return nil, "bad_alias"
		}
		if e.Alias != "" {
			// An explicit duplicate would silently become "-2"/"#2" at render
			// time, which is exactly what a template author cannot predict.
			if seenAlias[e.Alias] {
				return nil, "alias_conflict"
			}
			seenAlias[e.Alias] = true
		}
		entry := store.SubscriptionEntry{
			NodeID: e.NodeID, RelayNodeID: e.RelayNodeID,
			Proto: e.Proto, SrcPort: e.SrcPort, Iface: e.Iface,
			Alias: e.Alias, Enabled: e.Selected,
		}
		if seenIdentity[entryKey(entry)] {
			continue
		}
		seenIdentity[entryKey(entry)] = true
		out = append(out, entry)
	}
	return out, ""
}

// handleSubscriptionEntries serves the §10.2 picker: every candidate entry of
// this subscription (direct entries for all nodes, plus the relay entries
// derived from the fleet's forwards) unioned with what is bound. Reading is
// also a reconcile trigger — it is the one moment the operator is definitely
// looking, and auto-enrolment is meant to be visible, not to happen at a
// random later time.
func (s *Server) handleSubscriptionEntries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sub, err := s.Store.GetSubscription(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.reconcileSubscriptionEntries(sub)
	views, err := s.subscriptionEntryViews(sub)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": views,
		// The picker shows the auto-name it will get and lets the operator turn
		// auto-enrolment off from here, so both travel with the data.
		"relay_auto_include": s.relayAutoInclude(),
		"relay_name_format":  s.relayNameFormat(),
	})
}

// handleRotateSubscription mints a new token; the old URL stops resolving and
// the new one stays copyable from the panel afterwards.
func (s *Server) handleRotateSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSubscription(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	token, err := security.RandomToken(24)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	enc, err := s.Crypt.Encrypt(token)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.RotateSubscriptionToken(id, tokenHash(token), enc); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "subscription_rotated", SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("subscriptions_changed", id)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "url": s.subscriptionURL(r, token)})
}

func (s *Server) handleSubscriptionAccess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetSubscription(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	logs, err := s.Store.ListSubAccess(id, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// --- templates API ---

type templateView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Format    string `json:"format"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	tpls, err := s.Store.ListTemplates()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]templateView, 0, len(tpls))
	for _, t := range tpls {
		out = append(out, templateView{t.ID, t.Name, t.Format, t.Content, t.CreatedAt, t.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

type saveTemplateReq struct {
	Name    string `json:"name"`
	Format  string `json:"format"`
	Content string `json:"content"`
}

func (s *Server) validateTemplateReq(req *saveTemplateReq) (string, bool) {
	if strings.TrimSpace(req.Name) == "" {
		return "name_required", false
	}
	if !validFormat(req.Format) {
		return "bad_format", false
	}
	if !templatePlaceholdersOK(req.Content) {
		return "missing_placeholder", false
	}
	// The removed placeholder is refused rather than silently preserved: rules
	// flagged this way would reach every client as a literal "{{rules}}".
	if strings.Contains(req.Content, legacyRulesPlaceholder) {
		return "obsolete_placeholder", false
	}
	return "", true
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req saveTemplateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if code, ok := s.validateTemplateReq(&req); !ok {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	id, err := security.RandomToken(8)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	t := &store.Template{ID: id, Name: strings.TrimSpace(req.Name), Format: req.Format, Content: req.Content}
	if err := s.Store.InsertTemplate(t); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "template_created", Command: t.Name, SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("subscriptions_changed", id)
	writeJSON(w, http.StatusOK, templateView{t.ID, t.Name, t.Format, t.Content, t.CreatedAt, t.UpdatedAt})
}

func (s *Server) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.Store.GetTemplate(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var req saveTemplateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Name == "" && req.Format == "" && req.Content == "" {
		req.Name, req.Format, req.Content = t.Name, t.Format, t.Content
	}
	if code, ok := s.validateTemplateReq(&req); !ok {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	t.Name, t.Format, t.Content = strings.TrimSpace(req.Name), req.Format, req.Content
	if err := s.Store.UpdateTemplate(t); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{Actor: "panel", Action: "template_updated", Command: t.Name, SourceIP: s.Trust.RealIP(r)})
	s.publishEvent("subscriptions_changed", id)
	writeJSON(w, http.StatusOK, templateView{t.ID, t.Name, t.Format, t.Content, t.CreatedAt, t.UpdatedAt})
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetTemplate(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.Store.DeleteTemplate(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.publishEvent("subscriptions_changed", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- /sub/<token> rendering (design §10) ---

// uaAllowed enforces the §10 UA filter: an empty filter lets every client
// through; otherwise the User-Agent must contain at least one of the
// comma-separated substrings (case-insensitive). Mismatches 404 exactly like
// unknown tokens so the URL's existence is never revealed.
func uaAllowed(filter, ua string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	ua = strings.ToLower(ua)
	for _, part := range strings.Split(filter, ",") {
		if needle := strings.ToLower(strings.TrimSpace(part)); needle != "" && strings.Contains(ua, needle) {
			return true
		}
	}
	return false
}

// normalizeUAFilter trims whitespace around each comma-separated entry and
// drops empties, so stored filters compare cleanly at fetch time.
func normalizeUAFilter(filter string) string {
	parts := strings.Split(filter, ",")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ",")
}

// sniffFormat is the last resort for the output format: the User-Agent
// (clash/mihomo → clash, sing-box → singbox), default singbox. The Surge UA
// that §10 once listed is deliberately absent — there is no Surge output
// format, and handing a Surge client a Clash config would be worse than the
// JSON default it can at least be told about.
func sniffFormat(r *http.Request) string {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	switch {
	case strings.Contains(ua, "clash"), strings.Contains(ua, "mihomo"):
		return FormatClash
	case strings.Contains(ua, "sing-box"), strings.Contains(ua, "singbox"):
		return FormatSingbox
	default:
		return FormatSingbox
	}
}

// resolveFormat decides which format this fetch produces (§10 实现修订
// 2026-09-16). In order:
//
//  1. ?format= — an explicit request always wins, it is how one subscription
//     can still serve a second client type;
//  2. the subscription's own pinned format;
//  3. auto (” format): the bound template's format — binding a Clash template
//     is a statement about what this subscription is for, so a client whose UA
//     we do not recognise must not silently get the sing-box default with the
//     operator's template unused;
//  4. the User-Agent, then sing-box.
func (s *Server) resolveFormat(sub *store.Subscription, r *http.Request) string {
	if f := r.URL.Query().Get("format"); validFormat(f) {
		return f
	}
	if validFormat(sub.Format) {
		return sub.Format
	}
	if sub.TemplateID.Valid && sub.TemplateID.String != "" {
		if t, err := s.Store.GetTemplate(sub.TemplateID.String); err == nil && validFormat(t.Format) {
			return t.Format
		}
	}
	return sniffFormat(r)
}

// subRenderable is the single predicate behind "this node shows up in a
// subscription" (§10): a primary IP, an inbound port and a reported
// certificate. The certificate is what makes pinning possible, so a node
// without one is skipped instead of rendered with insecure=true (§9.3).
// The node list exposes it as `singbox_ready` and the renderer below applies
// it — the subscription page's node picker must offer exactly this set,
// otherwise checking a node could have no effect on the output at all.
func subRenderable(node *store.Node, sb *store.NodeSingbox) bool {
	// nil sb is the common "never installed sing-box" case, not an error to
	// handle at every call site (§10.2 asks this question for every node).
	if node == nil || sb == nil || node.PrimaryIP == "" {
		return false
	}
	if sb.Port > 0 && sb.CertPEM != "" {
		return true
	}
	// §9.3 实现修订 2026-09-17: a node can be renderable through its *adopted*
	// inbounds alone — an operator who adopted only a VLESS inbound never
	// installed fobe's own anytls, so there is no certificate, yet his clients
	// must still get the node.
	return sb.ExtrasPresent
}

// subscriptionNodes assembles the renderable proxies for a subscription
// (design §10.2): every *enabled entry* becomes one anytls outbound. A direct
// entry dials the node's own inbound; a relayed entry dials the relay's primary
// IP and the src port of its DNAT rule — while the pinned certificate still
// belongs to the *target*, because DNAT is layer 4 and TLS terminates on B
// exactly as it would if the client dialled B directly.
func (s *Server) subscriptionNodes(sub *store.Subscription) []singbox.ProxyNode {
	entries, err := s.Store.SubscriptionEntries(sub.ID)
	if err != nil {
		return nil
	}
	format := s.relayNameFormat()
	nodes := make([]singbox.ProxyNode, 0, len(entries))
	for _, e := range entries {
		if !e.Enabled {
			continue // tombstone (§10.2): unbound, kept so reconcile cannot re-add it
		}
		target, err := s.Store.GetNode(e.NodeID)
		if err != nil {
			continue
		}
		sb, err := s.Store.GetNodeSingbox(e.NodeID)
		if err != nil {
			continue
		}
		name := entryBaseName(target)

		// §9.3 实现修订 2026-09-17b (editor model): the probe's config.json is
		// the source of truth, so the payload is rendered from the file the
		// probe last reported. A `jq`-appended inbound therefore shows up here
		// without any adoption step, and the panel's own inbound keeps its
		// reported certificate for pinning.
		live, _ := s.liveNodesFor(e.NodeID, name, target.PrimaryIP)
		if len(live) == 0 {
			// No report yet (an install in flight, an agent that predates the
			// field, or a row written before the editor model): fall back to
			// the managed pair so a fresh node still appears as soon as it has
			// a port and a certificate. The credential still comes from the
			// node's own config (§10.1 实现修订 2026-09-17e) — rendering is not a
			// place that mints, and a node with no readable credential is
			// skipped instead of served with an empty password (a client would
			// import an outbound that can never authenticate).
			password := s.nodeProxyPassword(e.NodeID, sb.Port)
			if !subRenderable(target, sb) || password == "" {
				continue
			}
			live = []singbox.ProxyNode{{
				ID: e.NodeID, Name: name, Server: target.PrimaryIP, Port: sb.Port,
				Password: password, CertPEM: sb.CertPEM,
			}}
		}

		if e.RelayNodeID == "" {
			if e.Alias != "" {
				// One entry can carry several inbounds: the first takes the
				// alias verbatim, the rest keep their suffix so names stay
				// unique inside a client's proxy group.
				for i := range live {
					if i == 0 {
						live[i].Name = e.Alias
					} else {
						live[i].Name = e.Alias + " · " + live[i].NameSuffix
					}
				}
			}
			nodes = append(nodes, live...)
			continue
		}

		// A relayed entry dials the relay's primary IP and the src port of its
		// DNAT rule, while the pinned certificate still belongs to the target
		// (DNAT is layer 4; TLS terminates on B). It lands on the node's anytls
		// inbound, so only that one inbound is relayed — an adopted or
		// hand-written inbound is reachable directly, never through the rule.
		relay, err := s.Store.GetNode(e.RelayNodeID)
		if err != nil || relay.PrimaryIP == "" {
			continue // the relay leg is gone (rule deleted / node removed)
		}
		var relayed *singbox.ProxyNode
		for i := range live {
			if (live[i].Protocol == "" || live[i].Protocol == singbox.ProtoAnytls) && live[i].CertPEM != "" {
				relayed = &live[i]
				break
			}
		}
		if relayed == nil {
			continue // no anytls inbound on the target: nothing to reach through A
		}
		name = relayAutoName(format, target, relay, e)
		if e.Alias != "" {
			name = e.Alias
		}
		nodes = append(nodes, singbox.ProxyNode{
			ID: e.NodeID, Name: name, Server: relay.PrimaryIP, Port: e.SrcPort,
			Password: relayed.Password, CertPEM: relayed.CertPEM,
		})
	}
	return nodes
}

// renderSubscription produces the final config body for the chosen format:
// the subscription's template when it matches the format, otherwise the
// built-in default; {{nodes}} is substituted with the rendered outbounds.
// Routing rules are part of the template itself (§10 实现修订 2026-09-16b) — the
// renderer substitutes nothing else, so what the operator wrote is what the
// client gets, byte for byte.
func (s *Server) renderSubscription(sub *store.Subscription, format string, nodes []singbox.ProxyNode) (string, error) {
	tmpl := ""
	if sub.TemplateID.Valid && sub.TemplateID.String != "" {
		t, err := s.Store.GetTemplate(sub.TemplateID.String)
		if err == nil && t.Format == format {
			tmpl = t.Content
		}
	}
	if tmpl == "" {
		if format == FormatClash {
			tmpl = singbox.DefaultTemplateClash
		} else {
			tmpl = singbox.DefaultTemplateSingbox
		}
	}
	var injected string
	var err error
	if format == FormatClash {
		injected = singbox.RenderNodesYAML(nodes)
	} else {
		injected, err = singbox.RenderNodesJSON(nodes)
		if err != nil {
			return "", err
		}
	}
	return strings.ReplaceAll(tmpl, placeholderNodes, injected), nil
}

// handleSubscription serves GET /sub/<token> (design §10): token in URL,
// only its hash in the DB; disabled or unknown subscriptions 404 without
// leaking which one it was. A subscription UA filter (§10) that does not
// match the client 404s identically — before any format sniffing, so an
// explicit ?format= cannot bypass it. Every allowed hit is logged for the
// panel's access log.
// Refusal codes stored in sub_access_logs.reason (§10 实现修订 2026-09-16).
// ” means the fetch was served. Every refusal answers the client with the same
// 404 as an unknown token, so this column is the *only* place the operator can
// see why a client was turned away.
const (
	subAccessServed     = ""
	subAccessDisabled   = "disabled"
	subAccessUAMismatch = "ua_mismatch"
	subAccessRenderErr  = "render_error"
)

func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sub, err := s.Store.GetSubscriptionByToken(tokenHash(token))
	if errors.Is(err, store.ErrNotFound) {
		// An unknown token belongs to no subscription, so there is nowhere in
		// the panel to show it (the access log is per subscription). Logging it
		// would also mean inventing a row for a subscription that may not
		// exist; §10's "never reveal whether the URL exists" is unaffected.
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !sub.Enabled {
		s.logSubAccess(sub.ID, r, subAccessDisabled)
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if !uaAllowed(sub.UAFilter, r.Header.Get("User-Agent")) {
		s.logSubAccess(sub.ID, r, subAccessUAMismatch)
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}

	format := s.resolveFormat(sub, r)
	nodes := s.subscriptionNodes(sub)
	body, err := s.renderSubscription(sub, format, nodes)
	if err != nil {
		s.Log.Error("render subscription", "sub", sub.ID, "err", err)
		s.logSubAccess(sub.ID, r, subAccessRenderErr)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	s.logSubAccess(sub.ID, r, subAccessServed)

	w.Header().Set("Cache-Control", "no-store")
	if format == FormatClash {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// logSubAccess writes one access-log row (fire-and-forget: a logging failure
// must never break a subscription fetch). reason is subAccessServed for a
// served fetch, else the refusal code.
func (s *Server) logSubAccess(subID string, r *http.Request, reason string) {
	s.Store.InsertSubAccess(subID, s.Trust.RealIP(r), r.Header.Get("User-Agent"), reason)
}

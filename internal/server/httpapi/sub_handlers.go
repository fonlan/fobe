package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/singbox"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// --- subscriptions & templates (design §10) ---

// subscription formats. Output format choice: ?format= beats UA sniffing,
// sniff failure defaults to singbox.
const (
	FormatSingbox = "singbox"
	FormatClash   = "clash"
)

func validFormat(f string) bool { return f == FormatSingbox || f == FormatClash }

// Placeholders every stored template must contain (§10): {{nodes}} is where
// the renderer injects the anytls outbound list; {{rules}} is where it injects
// the operator's routing rules (see the rules settings below).
const (
	placeholderNodes = "{{nodes}}"
	placeholderRules = "{{rules}}"
)

func templatePlaceholdersOK(content string) bool {
	return strings.Contains(content, placeholderNodes) && strings.Contains(content, placeholderRules)
}

// --- routing rules (§10 实现修订 2026-09-16) ---
//
// Rules for {{rules}} are two panel settings instead of free text inside every
// template: one snippet per output format, because a subscription's format is
// chosen per request (?format= / UA sniffing) and sing-box rules are JSON
// while Clash rules are YAML list items. The renderer splices the snippet in
// verbatim, so — exactly like {{nodes}} — the snippet owns its own list
// markers and indentation and the template owns only the surrounding key.
const (
	SettingRulesSingbox = "sub.rules_singbox"
	SettingRulesClash   = "sub.rules_clash"
)

func rulesSettingKey(format string) string {
	if format == FormatClash {
		return SettingRulesClash
	}
	return SettingRulesSingbox
}

// validateRulesFragment rejects snippets that cannot possibly render: an empty
// value is fine (the placeholder expands to nothing), otherwise the text must
// parse as the array elements it will be spliced between.
func validateRulesFragment(format, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if format == FormatClash {
		// YAML sequence entries: "  - MATCH,PROXY". Comments and blank lines
		// are allowed so an operator can annotate the fragment.
		for _, line := range strings.Split(value, "\n") {
			l := strings.TrimSpace(line)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if !strings.HasPrefix(l, "-") {
				return false
			}
		}
		return true
	}
	// sing-box: the elements of a route.rules array. Wrapping them in [] has to
	// yield valid JSON of objects — this is the check that catches a half-typed
	// rule before it reaches every client.
	var rules []map[string]any
	return json.Unmarshal([]byte("["+value+"]"), &rules) == nil
}

// subscriptionRules returns the configured snippet for one format (” when the
// operator never configured rules for it).
func (s *Server) subscriptionRules(format string) string {
	val, err := s.Store.GetSetting(rulesSettingKey(format))
	if err != nil {
		return ""
	}
	return val
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
}

func (s *Server) subscriptionView(sub *store.Subscription) subscriptionView {
	nodeIDs, _ := s.Store.SubscriptionNodeIDs(sub.ID)
	v := subscriptionView{
		ID: sub.ID, Name: sub.Name, Enabled: sub.Enabled, CreatedAt: sub.CreatedAt,
		UAFilter: sub.UAFilter,
		NodeIDs:  nodeIDs,
		// §10 实现修订 2026-09-16: the URL can be re-shown while the ciphertext
		// is present; legacy rows (created before the column existed) can only
		// get a working link by rotating it.
		LinkAvailable: sub.TokenEnc != "",
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
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
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

type setSubNodesReq struct {
	NodeIDs []string `json:"node_ids"`
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

// sniffFormat decides the output format: explicit ?format= wins, then the
// User-Agent (clash/mihomo → clash, sing-box → singbox), default singbox.
func sniffFormat(r *http.Request) string {
	if f := r.URL.Query().Get("format"); validFormat(f) {
		return f
	}
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

// subscriptionNodes assembles the renderable proxies for a subscription:
// primary IP + sing-box inbound port + reported certificate. Probes without
// a port/certificate yet are skipped — a pinning subscription must never
// fall back to insecure=true (§9.3).
func (s *Server) subscriptionNodes(sub *store.Subscription) []singbox.ProxyNode {
	ids, err := s.Store.SubscriptionNodeIDs(sub.ID)
	if err != nil {
		return nil
	}
	password, _ := s.GetDecryptedSetting("anytls_password")
	nodes := make([]singbox.ProxyNode, 0, len(ids))
	for _, id := range ids {
		node, err := s.Store.GetNode(id)
		if err != nil || node.PrimaryIP == "" {
			continue
		}
		sb, err := s.Store.GetNodeSingbox(id)
		if err != nil || sb.Port <= 0 || sb.CertPEM == "" {
			continue
		}
		nodes = append(nodes, singbox.ProxyNode{
			ID: node.ID, Name: node.Name, Server: node.PrimaryIP,
			Port: sb.Port, Password: password, CertPEM: sb.CertPEM,
		})
	}
	return nodes
}

// renderSubscription produces the final config body for the chosen format:
// the subscription's template when it matches the format, otherwise the
// built-in default; {{nodes}} and {{rules}} are substituted with the rendered
// outbounds and the format's configured routing rules.
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
	// {{nodes}} first, then {{rules}}: a rules fragment is operator text and
	// must never be re-scanned for the node placeholder.
	body := strings.ReplaceAll(tmpl, placeholderNodes, injected)
	return strings.ReplaceAll(body, placeholderRules, s.subscriptionRules(format)), nil
}

// handleSubscription serves GET /sub/<token> (design §10): token in URL,
// only its hash in the DB; disabled or unknown subscriptions 404 without
// leaking which one it was. A subscription UA filter (§10) that does not
// match the client 404s identically — before any format sniffing, so an
// explicit ?format= cannot bypass it. Every allowed hit is logged for the
// panel's access log.
func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	sub, err := s.Store.GetSubscriptionByToken(tokenHash(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !sub.Enabled) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !uaAllowed(sub.UAFilter, r.Header.Get("User-Agent")) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}

	format := sniffFormat(r)
	nodes := s.subscriptionNodes(sub)
	body, err := s.renderSubscription(sub, format, nodes)
	if err != nil {
		s.Log.Error("render subscription", "sub", sub.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	s.InsertSubAccessSafe(sub.ID, s.Trust.RealIP(r), r.Header.Get("User-Agent"))

	w.Header().Set("Cache-Control", "no-store")
	if format == FormatClash {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// InsertSubAccessSafe wraps the fire-and-forget access log write so a logging
// failure can never break a subscription fetch.
func (s *Server) InsertSubAccessSafe(subID, ip, ua string) {
	s.Store.InsertSubAccess(subID, ip, ua)
}

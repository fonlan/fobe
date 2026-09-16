// HTTP tests for the §14 MMDB upload and the §17 panel export/import.
package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/store"
)

// loginSession logs in with the seeded test password and returns the
// session cookie header value.
func loginSession(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, out := postJSON(t, &http.Client{}, srv.URL+"/api/login", map[string]string{"password": "test-password-123"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d %v", resp.StatusCode, out)
	}
	cookie := ""
	for _, c := range readCookies(resp) {
		if c.Name == security.SessionCookieName {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	return cookie
}

// doAuthed performs a cookie-authenticated request; the response body is
// drained and closed, resp must only be used for status/headers.
func doAuthed(t *testing.T, method, url, cookie string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

// reloadSpy stands in for *geoip.MMDB: resolver + Reload, counting reloads.
type reloadSpy struct {
	reloads int
}

func (f *reloadSpy) Country(string) (string, bool) { return "", false }
func (f *reloadSpy) Reload() bool                  { f.reloads++; return true }

func TestMMDBUpload(t *testing.T) {
	srv, api := newTestServer(t)
	mmdbPath := filepath.Join(t.TempDir(), "geoip", "nested", "GeoLite2-Country.mmdb")
	api.GeoIPMMDBPath = mmdbPath
	spy := &reloadSpy{}
	api.GeoIPResolver = spy

	// no session → 401
	resp, err := http.Post(srv.URL+"/api/geoip/mmdb", "application/octet-stream", bytes.NewReader([]byte("MMDB")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload: got %d, want 401", resp.StatusCode)
	}

	cookie := loginSession(t, srv)

	// An HTML error page (or any payload that is not an MMDB) → 400 bad_mmdb,
	// nothing written. This is the case a truncated mirror response produces.
	resp, raw := doAuthed(t, "POST", srv.URL+"/api/geoip/mmdb", cookie, []byte("<html>404 not found</html>"))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "bad_mmdb") {
		t.Fatalf("junk upload: got %d %s, want 400 bad_mmdb", resp.StatusCode, raw)
	}
	// Regression guard: a leading "MMDB" byte string is not the file format's
	// magic — real databases start with the search tree — so it must be
	// rejected too (an earlier version accepted exactly this and rejected every
	// genuine upload).
	resp, raw = doAuthed(t, "POST", srv.URL+"/api/geoip/mmdb", cookie, append([]byte("MMDB"), bytes.Repeat([]byte{0}, 128)...))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "bad_mmdb") {
		t.Fatalf("fake MMDB header: got %d %s, want 400 bad_mmdb", resp.StatusCode, raw)
	}
	if _, err := os.Stat(mmdbPath); !os.IsNotExist(err) {
		t.Fatalf("rejected upload must not create the file: %v", err)
	}
	if spy.reloads != 0 {
		t.Fatalf("reload after rejected upload: got %d, want 0", spy.reloads)
	}

	// A real database → 200, bytes land at the path (missing dirs created),
	// resolver reloaded.
	body := mmdbFixture(t)
	resp, raw = doAuthed(t, "POST", srv.URL+"/api/geoip/mmdb", cookie, body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("valid upload: got %d %s, want 200 ok", resp.StatusCode, raw)
	}
	got, err := os.ReadFile(mmdbPath)
	if err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("uploaded content mismatch")
	}
	if spy.reloads != 1 {
		t.Fatalf("resolver Reload calls: got %d, want 1", spy.reloads)
	}

	// the upload is audited
	resp, raw = doAuthed(t, "GET", srv.URL+"/api/audit?limit=100", cookie, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "geoip_mmdb_uploaded") {
		t.Fatalf("upload audit entry missing: %d %s", resp.StatusCode, raw)
	}

	// server without a configured path → 503
	srv2, api2 := newTestServer(t)
	cookie2 := loginSession(t, srv2)
	if api2.GeoIPMMDBPath != "" {
		t.Fatal("test server should start unconfigured")
	}
	resp, raw = doAuthed(t, "POST", srv2.URL+"/api/geoip/mmdb", cookie2, body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "mmdb_not_configured") {
		t.Fatalf("unconfigured upload: got %d %s, want 503 mmdb_not_configured", resp.StatusCode, raw)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	srv1, api1 := newTestServer(t)
	cookie1 := loginSession(t, srv1)

	// register a node through the agent flow
	token := freshToken(t, srv1, cookie1, "exported-node")
	_, reg := postJSON(t, &http.Client{}, srv1.URL+"/api/agent/register", map[string]any{
		"token": token, "machine_id": "m-export-1", "hostname": "exporthost",
		"os": "linux", "arch": "amd64", "version": "dev", "tz": "UTC", "cpu_cores": 2,
	})
	nodeID, _ := reg["node_id"].(string)
	if nodeID == "" {
		t.Fatalf("register failed: %v", reg)
	}

	// panel-side data: rename/note, network, billing, singbox metadata (incl. a
	// certificate that must NOT travel), latency target, template, subscription
	resp, raw := doAuthed(t, "PATCH", srv1.URL+"/api/nodes/"+nodeID, cookie1, []byte(
		`{"name":"renamed-export","note":"keep me","network":{"iface":"eth0","mode":"both","quota_bytes":1000,"tz":"UTC"},"billing":{"cycle_type":"monthly","next_due_at":1900000000,"note":"bill"}}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node patch: %d %s", resp.StatusCode, raw)
	}
	if err := api1.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: nodeID, Version: "1.9.0", Status: "running", Port: 8443,
		CertPEM:    "-----BEGIN CERTIFICATE-----LEAKME-----END CERTIFICATE-----",
		CertSHA256: "abc123", UpdatedAt: nowUnix(),
	}); err != nil {
		t.Fatal(err)
	}

	resp, raw = doAuthed(t, "POST", srv1.URL+"/api/latency-targets", cookie1, []byte(`{"name":"google","kind":"tcp","host":"8.8.8.8","port":443}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("latency target create: %d %s", resp.StatusCode, raw)
	}
	resp, raw = doAuthed(t, "POST", srv1.URL+"/api/templates", cookie1, []byte(`{"name":"base","format":"singbox","content":"{{nodes}}\n{{rules}}"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("template create: %d %s", resp.StatusCode, raw)
	}
	var tpl struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &tpl)
	resp, raw = doAuthed(t, "POST", srv1.URL+"/api/subscriptions", cookie1, []byte(`{"name":"family"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscription create: %d %s", resp.StatusCode, raw)
	}
	var sub struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &sub)
	resp, raw = doAuthed(t, "PUT", srv1.URL+"/api/subscriptions/"+sub.ID, cookie1, []byte(`{"template_id":"`+tpl.ID+`"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscription template bind: %d %s", resp.StatusCode, raw)
	}
	resp, raw = doAuthed(t, "PUT", srv1.URL+"/api/subscriptions/"+sub.ID+"/nodes", cookie1, []byte(`{"node_ids":["`+nodeID+`"]}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscription node bind: %d %s", resp.StatusCode, raw)
	}
	resp, raw = doAuthed(t, "PUT", srv1.URL+"/api/settings", cookie1, []byte(
		`{"settings":{"server.public_url":"https://panel.example.com","ai.api_key":"super-secret-key"}}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings put: %d %s", resp.StatusCode, raw)
	}

	// --- export: shape + no credential material ---
	resp, raw = doAuthed(t, "GET", srv1.URL+"/api/export", cookie1, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="fobe-export.json"`) {
		t.Fatalf("content-disposition: %q", cd)
	}
	exported := string(raw)
	for _, leaked := range []string{"super-secret-key", "LEAKME", "cert_pem", "node_secret_hash", "token_hash", "reg_token"} {
		if strings.Contains(exported, leaked) {
			t.Fatalf("export leaks %q", leaked)
		}
	}
	var ef exportFile
	if err := json.Unmarshal(raw, &ef); err != nil {
		t.Fatal(err)
	}
	if ef.Version != exportFormatVersion || len(ef.Nodes) != 1 {
		t.Fatalf("unexpected export header/nodes: version=%d nodes=%d", ef.Version, len(ef.Nodes))
	}
	en := ef.Nodes[0]
	if en.MachineID != "m-export-1" || en.Name != "renamed-export" || en.Note != "keep me" {
		t.Fatalf("node identity not exported: %+v", en)
	}
	if en.Network == nil || en.Network.QuotaBytes == nil || *en.Network.QuotaBytes != 1000 {
		t.Fatalf("network not exported: %+v", en.Network)
	}
	if en.Billing == nil || en.Billing.CycleType != "monthly" {
		t.Fatalf("billing not exported: %+v", en.Billing)
	}
	if en.Singbox == nil || en.Singbox.Port != 8443 {
		t.Fatalf("singbox metadata not exported: %+v", en.Singbox)
	}
	if len(ef.LatencyTargets) != 1 || ef.LatencyTargets[0].Host != "8.8.8.8" {
		t.Fatalf("latency targets not exported: %+v", ef.LatencyTargets)
	}
	if len(ef.Templates) != 1 || ef.Templates[0].Name != "base" {
		t.Fatalf("templates not exported: %+v", ef.Templates)
	}
	if len(ef.Subscriptions) != 1 || ef.Subscriptions[0].Name != "family" ||
		!ef.Subscriptions[0].Enabled || ef.Subscriptions[0].Template != "base" ||
		len(ef.Subscriptions[0].NodeMachineIDs) != 1 || ef.Subscriptions[0].NodeMachineIDs[0] != "m-export-1" {
		t.Fatalf("subscription not exported: %+v", ef.Subscriptions)
	}
	if ef.Settings["server.public_url"] != "https://panel.example.com" {
		t.Fatalf("plaintext settings not exported: %+v", ef.Settings)
	}
	if _, ok := ef.Settings["ai.api_key"]; ok {
		t.Fatal("sensitive setting exported")
	}

	// --- import into a fresh panel ---
	srv2, api2 := newTestServer(t)
	cookie2 := loginSession(t, srv2)
	resp, raw = doAuthed(t, "POST", srv2.URL+"/api/import", cookie2, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import: %d %s", resp.StatusCode, raw)
	}
	var first importStats
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	want1 := importStats{NodesCreated: 1, LatencyTargetsCreated: 1, SubscriptionsCreated: 1, TemplatesCreated: 1, SettingsImported: 1}
	if first != want1 {
		t.Fatalf("first import stats: %+v, want %+v", first, want1)
	}

	// imported node is queryable over HTTP and by machine_id
	resp, raw = doAuthed(t, "GET", srv2.URL+"/api/nodes", cookie2, nil)
	var list struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Nodes) != 1 || list.Nodes[0].Name != "renamed-export" {
		t.Fatalf("imported node not queryable: %s", raw)
	}
	n, err := api2.Store.GetNodeByMachineID("m-export-1")
	if err != nil || n.Name != "renamed-export" || n.Note != "keep me" {
		t.Fatalf("imported node lookup by machine_id: %v %+v", err, n)
	}
	if net, err := api2.Store.GetNodeNetwork(n.ID); err != nil || net.QuotaBytes == nil || *net.QuotaBytes != 1000 {
		t.Fatalf("imported network missing: %v %+v", err, net)
	}

	// subscription rebinds to the imported node + template (by name)
	resp, raw = doAuthed(t, "GET", srv2.URL+"/api/subscriptions", cookie2, nil)
	var subs struct {
		Subscriptions []struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Enabled    bool     `json:"enabled"`
			TemplateID *string  `json:"template_id"`
			NodeIDs    []string `json:"node_ids"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(raw, &subs); err != nil {
		t.Fatal(err)
	}
	resp, raw = doAuthed(t, "GET", srv2.URL+"/api/templates", cookie2, nil)
	var tpls struct {
		Templates []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(raw, &tpls); err != nil {
		t.Fatal(err)
	}
	if len(subs.Subscriptions) != 1 || len(tpls.Templates) != 1 {
		t.Fatalf("imported subs/templates: %d subs / %d templates", len(subs.Subscriptions), len(tpls.Templates))
	}
	importedSub := subs.Subscriptions[0]
	if !importedSub.Enabled || len(importedSub.NodeIDs) != 1 || importedSub.NodeIDs[0] != n.ID {
		t.Fatalf("subscription binding not remapped: %+v", importedSub)
	}
	if importedSub.TemplateID == nil || *importedSub.TemplateID != tpls.Templates[0].ID {
		t.Fatalf("subscription template not remapped: %+v vs %+v", importedSub, tpls.Templates)
	}

	// sensitive settings never travel; plaintext ones do
	resp, raw = doAuthed(t, "GET", srv2.URL+"/api/settings", cookie2, nil)
	var settingsResp struct {
		Settings []settingView `json:"settings"`
	}
	if err := json.Unmarshal(raw, &settingsResp); err != nil {
		t.Fatal(err)
	}
	for _, sv := range settingsResp.Settings {
		if sv.Key == "ai.api_key" && sv.Set {
			t.Fatal("ai.api_key was imported")
		}
		if sv.Key == "server.public_url" && (!sv.Set || sv.Value != "https://panel.example.com") {
			t.Fatalf("server.public_url not imported: %+v", sv)
		}
	}

	// --- second import is a no-op for entity counts ---
	resp, raw = doAuthed(t, "POST", srv2.URL+"/api/import", cookie2, []byte(exported))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second import: %d %s", resp.StatusCode, raw)
	}
	var second importStats
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatal(err)
	}
	want2 := importStats{NodesUpdated: 1, SubscriptionsUpdated: 1, TemplatesUpdated: 1, SettingsImported: 1}
	if second != want2 {
		t.Fatalf("second import stats: %+v, want %+v", second, want2)
	}

	// counts unchanged after the second import
	resp, raw = doAuthed(t, "GET", srv2.URL+"/api/nodes", cookie2, nil)
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Nodes) != 1 {
		t.Fatalf("node count changed after re-import: %s", raw)
	}
	resp, raw = doAuthed(t, "GET", srv2.URL+"/api/latency-targets", cookie2, nil)
	if strings.Count(string(raw), `"host"`) != 1 {
		t.Fatalf("latency target count changed after re-import: %s", raw)
	}

	// the import is audited
	resp, raw = doAuthed(t, "GET", srv2.URL+"/api/audit?limit=100", cookie2, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "data_imported") {
		t.Fatalf("import audit entry missing: %d %s", resp.StatusCode, raw)
	}

	// version mismatch is rejected
	resp, raw = doAuthed(t, "POST", srv2.URL+"/api/import", cookie2, []byte(`{"version":99}`))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "bad_version") {
		t.Fatalf("version check: got %d %s, want 400 bad_version", resp.StatusCode, raw)
	}
}

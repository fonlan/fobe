// Tests for design §10.1 实现修订 2026-09-16: the global anytls password is
// machine-generated on first use. It used to be an operator-supplied setting,
// and everything that installs sing-box refused to run without it
// (`anytls_password_unset`) — these tests pin the replacements: installation
// mints it, subscriptions carry it, and the one case that must still fail is a
// ciphertext that cannot be decrypted.
package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/singbox"
)

var anytlsPasswordRE = regexp.MustCompile(`^[A-Za-z0-9]{16}$`)

// storedAnytlsPassword decrypts the setting the way the server does.
func storedAnytlsPassword(t *testing.T, api *Server) string {
	t.Helper()
	raw, err := api.Store.GetSetting("anytls_password")
	if err != nil {
		t.Fatalf("anytls_password was not generated: %v", err)
	}
	plain, err := api.Crypt.Decrypt(raw)
	if err != nil {
		t.Fatalf("decrypt anytls_password: %v", err)
	}
	return plain
}

func setPort(t *testing.T, srv string, cookie string, nodeID string, port int) httpResult {
	t.Helper()
	return doReq(t, &http.Client{}, "PUT", srv+"/api/nodes/"+nodeID+"/singbox/port", cookie, map[string]any{"port": port})
}

func TestInstallMintsAnytlsPassword(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "fresh", "m-mint-1", "198.51.100.20")

	// Before the revision this was 400 anytls_password_unset: nobody had supplied
	// a password yet, so the panel refused to manage the node at all.
	if r := setPort(t, srv.URL, cookie, nodeID, 34567); r.Status != http.StatusOK {
		t.Fatalf("set port without a password = %d %s", r.Status, r.Body)
	}

	password := storedAnytlsPassword(t, api)
	if !anytlsPasswordRE.MatchString(password) {
		t.Fatalf("generated password %q is not %d alnum chars", password, singbox.AnytlsPasswordLen)
	}
	// At rest it is ciphertext (§4.4): the row must not contain the plaintext.
	if raw, err := api.Store.GetSetting("anytls_password"); err != nil || strings.Contains(raw, password) {
		t.Fatalf("password not encrypted at rest: %q (%v)", raw, err)
	}
	// And it is what the agent is told to serve.
	cfg, err := api.Store.GetSetting("singbox_config:" + nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, password) {
		t.Fatalf("node config does not carry the generated password:\n%s", cfg)
	}

	// A second node joins the same credential instead of rotating it.
	node2, _ := seedNode(t, api, "fresh-2", "m-mint-2", "198.51.100.21")
	if r := setPort(t, srv.URL, cookie, node2, 34568); r.Status != http.StatusOK {
		t.Fatalf("set port on a second node = %d %s", r.Status, r.Body)
	}
	if again := storedAnytlsPassword(t, api); again != password {
		t.Fatalf("second use rotated the password: %q -> %q", password, again)
	}

	// Exactly one generation event, and never the value (§4.4).
	entries, err := api.Store.ListAudit(200)
	if err != nil {
		t.Fatal(err)
	}
	generated := 0
	for _, e := range entries {
		if strings.Contains(e.Command, password) {
			t.Fatalf("password leaked into the audit log: %+v", e)
		}
		if e.Action != "anytls_password_generated" {
			continue
		}
		generated++
		if e.Actor != "system" || e.Command != "[redacted]" {
			t.Fatalf("generation audit entry = %+v", e)
		}
	}
	if generated != 1 {
		t.Fatalf("generation audited %d times, want 1", generated)
	}
}

// A subscription is the other "use it" moment (§10.1 实现修订 2026-09-16), and it
// has to hand out the *same* credential the node config carries.
func TestSubscriptionCarriesGeneratedAnytlsPassword(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "probe-1", "m-mint-sub", "203.0.113.30")
	seedSingbox(t, api, nodeID, 23456)
	subID, token := createSubscription(t, srv, cookie, "main")
	if r := doReq(t, &http.Client{}, "PUT", srv.URL+"/api/subscriptions/"+subID+"/nodes", cookie,
		map[string]any{"node_ids": []string{nodeID}}); r.Status != 200 {
		t.Fatalf("set sub nodes: %d %s", r.Status, r.Body)
	}

	r := fetchSub(t, srv, token, "?format=singbox", "")
	if r.Status != 200 {
		t.Fatalf("singbox render: %d %s", r.Status, r.Body)
	}
	var cfg struct {
		Outbounds []struct {
			Password string `json:"password"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(r.Body, &cfg); err != nil {
		t.Fatalf("render is not JSON: %v\n%s", err, r.Body)
	}
	if len(cfg.Outbounds) != 1 {
		t.Fatalf("outbounds = %d, want 1", len(cfg.Outbounds))
	}
	password := storedAnytlsPassword(t, api)
	if cfg.Outbounds[0].Password != password {
		t.Fatalf("subscription password %q != stored %q", cfg.Outbounds[0].Password, password)
	}
	if !anytlsPasswordRE.MatchString(password) {
		t.Fatalf("generated password %q is not %d alnum chars", password, singbox.AnytlsPasswordLen)
	}
}

// A stored value that cannot be decrypted is the wrong-master-key incident
// (§4.4), not "no password configured". Falling through to generation would
// rotate the credential and cut off every client still on the previous one,
// which is the worst possible response to an operator already restoring keys.
func TestUndecryptableAnytlsPasswordIsNotRotated(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "wrongkey", "m-mint-3", "198.51.100.23")

	other, err := security.NewCryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.Encrypt("encrypted-with-another-master-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetSetting("anytls_password", foreign, true); err != nil {
		t.Fatal(err)
	}

	if r := setPort(t, srv.URL, cookie, nodeID, 34569); r.Status != http.StatusInternalServerError {
		t.Fatalf("undecryptable password = %d %s, want 500", r.Status, r.Body)
	}
	if raw, _ := api.Store.GetSetting("anytls_password"); raw != foreign {
		t.Fatalf("the unreadable value was overwritten: %q -> %q", foreign, raw)
	}
}

// Tests for design §10.1 实现修订 2026-09-17e: the credential belongs to the
// node's *inbound*, not to one global setting shared by the fleet.
//
// What these tests pin down:
//   - installing (or adding) an inbound mints a fresh 16-char alnum password
//     for that node, and no global `anytls_password` is ever created;
//   - every later regeneration — a port change, a reinstall, a subscription
//     render — reads the same password back from the node's own configuration;
//   - an install over a listener one-sing.sh already serves keeps *that*
//     listener's password, so its clients are not cut off;
//   - an unreadable (wrong-master-key) report is never minted over.
package httpapi

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

var anytlsPasswordRE = regexp.MustCompile(`^[A-Za-z0-9]{16}$`)

// configPassword reads the panel inbound's credential out of the node's stored
// document — the bytes the agent is told to serve.
func configPassword(t *testing.T, api *Server, nodeID string, port int) string {
	t.Helper()
	cfg, err := api.Store.GetSetting("singbox_config:" + nodeID)
	if err != nil {
		t.Fatalf("stored config for %s: %v", nodeID, err)
	}
	pw := singbox.InboundPasswordAtPort(cfg, port)
	if pw == "" {
		t.Fatalf("no credential on port %d of the stored config:\n%s", port, cfg)
	}
	return pw
}

// generationAudits counts the credential-generation audit entries and fails if
// the value leaked into any audit line (§4.4).
func generationAudits(t *testing.T, api *Server, secret string) int {
	t.Helper()
	entries, err := api.Store.ListAudit(200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Command, secret) {
			t.Fatalf("password leaked into the audit log: %+v", e)
		}
		if e.Action != "anytls_password_generated" {
			continue
		}
		n++
		if e.Actor != "system" || e.Command != "[redacted]" {
			t.Fatalf("generation audit entry = %+v", e)
		}
	}
	return n
}

func installSingboxAtPort(t *testing.T, srv, cookie, nodeID string, port int) httpResult {
	t.Helper()
	return doReq(t, &http.Client{}, "POST", srv+"/api/nodes/"+nodeID+"/singbox/install", cookie, map[string]any{
		"version": "1.13.0-beta.7", "port": port,
	})
}

func installSingbox(t *testing.T, srv, cookie, nodeID, version string) httpResult {
	t.Helper()
	return doReq(t, &http.Client{}, "POST", srv+"/api/nodes/"+nodeID+"/singbox/install", cookie, map[string]any{"version": version})
}

func TestInstallMintsAnInboundCredential(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "fresh", "m-mint-1", "198.51.100.20")

	if r := installSingboxAtPort(t, srv.URL, cookie, nodeID, 34567); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	password := configPassword(t, api, nodeID, 34567)
	if !anytlsPasswordRE.MatchString(password) {
		t.Fatalf("generated password %q is not %d alnum chars", password, singbox.AnytlsPasswordLen)
	}
	// The whole point of the revision: nothing is shared any more.
	if _, err := api.Store.GetSetting("anytls_password"); err == nil {
		t.Fatal("a global anytls_password setting was created")
	}
	if n := generationAudits(t, api, password); n != 1 {
		t.Fatalf("generation audited %d times, want 1", n)
	}
}

// Two nodes must not end up on one credential — that was the property the
// global password could not have.
func TestEachNodeGetsItsOwnCredential(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	first, _ := seedNode(t, api, "one", "m-own-1", "198.51.100.21")
	second, _ := seedNode(t, api, "two", "m-own-2", "198.51.100.22")

	if r := installSingboxAtPort(t, srv.URL, cookie, first, 34567); r.Status != http.StatusOK {
		t.Fatalf("first install = %d %s", r.Status, r.Body)
	}
	if r := installSingboxAtPort(t, srv.URL, cookie, second, 34568); r.Status != http.StatusOK {
		t.Fatalf("second install = %d %s", r.Status, r.Body)
	}
	one := configPassword(t, api, first, 34567)
	two := configPassword(t, api, second, 34568)
	if one == two {
		t.Fatalf("both nodes share the credential %q", one)
	}
	for _, pw := range []string{one, two} {
		if !anytlsPasswordRE.MatchString(pw) {
			t.Fatalf("password %q is not %d alnum chars", pw, singbox.AnytlsPasswordLen)
		}
	}
}

// Regeneration is not a rotation: reinstalling the version keeps the
// credential the clients already hold. Later per-rule port edits go through the
// config editor instead of regenerating the whole document.
func TestReinstallKeepsTheCredential(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "keep", "m-keep", "198.51.100.23")

	if r := installSingboxAtPort(t, srv.URL, cookie, nodeID, 34567); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	password := configPassword(t, api, nodeID, 34567)

	if r := installSingbox(t, srv.URL, cookie, nodeID, "1.13.0-beta.7"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	if got := configPassword(t, api, nodeID, 34567); got != password {
		t.Fatalf("reinstall rotated the credential: %q -> %q", password, got)
	}
	if n := generationAudits(t, api, password); n != 1 {
		t.Fatalf("generation audited %d times, want 1 across two writes", n)
	}
}

// Installing over a listener one-sing.sh already runs keeps that listener's
// password: the clients holding the script's URI keep working, and the panel
// does not invent a second credential for the same service.
func TestInstallOverAScriptListenerKeepsItsPassword(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "HK-Sharon", "m-script-pw", "203.0.113.31")
	seedLocalSnapshot(t, api, nodeID, true)
	// The legacy managed port identifies the initial install target.
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{NodeID: nodeID, Port: 28711, Status: "absent"}); err != nil {
		t.Fatalf("seed recognised port: %v", err)
	}

	if r := installSingbox(t, srv.URL, cookie, nodeID, "1.13.0-beta.7"); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	if got := configPassword(t, api, nodeID, 28711); got != "AnyTlsScriptPw1" {
		t.Fatalf("install rotated the script listener's credential: %q", got)
	}
	if n := generationAudits(t, api, "AnyTlsScriptPw1"); n != 0 {
		t.Fatalf("minted a credential for a listener that already had one (%d audits)", n)
	}
}

// The subscription hands out the node's own credential — the one the probe
// serves — not a fleet-wide value.
func TestSubscriptionCarriesTheNodesOwnCredential(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "probe-1", "m-mint-sub", "203.0.113.30")
	if r := installSingboxAtPort(t, srv.URL, cookie, nodeID, 23456); r.Status != http.StatusOK {
		t.Fatalf("install = %d %s", r.Status, r.Body)
	}
	// The reported managed pair: no config report yet, so the subscription uses
	// the fallback render path, which must read the same credential.
	if err := api.Store.UpsertNodeSingbox(&store.NodeSingbox{
		NodeID: nodeID, Version: "1.10.0", DesiredVersion: "1.13.0-beta.7",
		Status: "running", Port: 23456, CertPEM: testCertPEM, CertSHA256: "f00d",
	}); err != nil {
		t.Fatalf("seed singbox: %v", err)
	}
	subID, token := createSubscription(t, srv, cookie, "main")
	bindEntries(t, srv, cookie, subID, []subEntryInput{{NodeID: nodeID, Selected: true}})

	body := string(fetchSub(t, srv, token, "?format=singbox", "").Body)
	password := configPassword(t, api, nodeID, 23456)
	if !strings.Contains(body, password) {
		t.Fatalf("subscription does not carry the node's own credential %q:\n%s", password, body)
	}
	if _, err := api.Store.GetSetting("anytls_password"); err == nil {
		t.Fatal("rendering a subscription minted a global credential")
	}
}

// A report that exists but cannot be decrypted is the wrong-master-key incident
// (§4.4), not "no credential configured". Minting over it would rotate a live
// credential while the operator is restoring keys.
func TestUnreadableReportIsNotMintedOver(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)
	nodeID, _ := seedNode(t, api, "wrongkey", "m-mint-3", "198.51.100.24")

	other, err := security.NewCryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Store.SetNodeSingboxLocal(nodeID, store.NodeSingboxLocal{
		LocalHash: "cafe", LocalPresent: true, ConfigJSON: oneSingLocalConfig,
	}, other); err != nil {
		t.Fatal(err)
	}

	if r := installSingboxAtPort(t, srv.URL, cookie, nodeID, 34569); r.Status != http.StatusInternalServerError {
		t.Fatalf("unreadable report = %d %s, want 500", r.Status, r.Body)
	}
	if _, err := api.Store.GetSetting("singbox_config:" + nodeID); err == nil {
		t.Fatal("a config was written despite the unreadable report")
	}
}

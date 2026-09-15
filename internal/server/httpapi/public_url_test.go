package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicURLOverridesRequestHostForInstallCommand(t *testing.T) {
	srv, api := newTestServer(t)
	defer srv.Close()

	if err := api.Store.SetSetting("server.public_url", "https://panel.example.com/control/", false); err != nil {
		t.Fatal(err)
	}
	cookie := testSessionCookie(t, api)
	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+"/api/reg-tokens",
		bytes.NewBufferString(`{"name":"edge-router"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	req.Host = "127.0.0.1:8080"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		InstallCommand string `json:"install_command"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.InstallCommand, "'https://panel.example.com/control/install.sh'") {
		t.Fatalf("install command does not use configured URL: %q", body.InstallCommand)
	}
	if !strings.Contains(body.InstallCommand, "--server 'https://panel.example.com/control'") {
		t.Fatalf("server argument does not use configured URL: %q", body.InstallCommand)
	}
	if strings.Contains(body.InstallCommand, "127.0.0.1") {
		t.Fatalf("install command leaked request host: %q", body.InstallCommand)
	}
	// The generated command must not require sudo: most VPS root users have
	// no sudo installed. The script elevates by itself when necessary.
	if strings.Contains(body.InstallCommand, "sudo") {
		t.Fatalf("install command must not embed sudo: %q", body.InstallCommand)
	}
}

func TestInstallScriptUsesConfiguredPublicURL(t *testing.T) {
	srv, api := newTestServer(t)
	defer srv.Close()

	if err := api.Store.SetSetting("server.public_url", "https://panel.example.com", false); err != nil {
		t.Fatal(err)
	}
	tmplPath := filepath.Join(t.TempDir(), "install.sh.tmpl")
	if err := os.WriteFile(tmplPath, []byte("#!/bin/sh\nSERVER={{.DefaultServer}}\nTOKEN={{.DefaultToken}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api.InstallTmplPath = tmplPath

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/install.sh?token=token-123", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:8080"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	bodyText := string(got)
	if !strings.Contains(bodyText, "SERVER=https://panel.example.com") {
		t.Fatalf("script did not use configured URL: %q", bodyText)
	}
	if !strings.Contains(bodyText, "TOKEN=token-123") {
		t.Fatalf("script did not render token: %q", bodyText)
	}
}

func TestInvalidPublicURLDoesNotCreateRegistrationToken(t *testing.T) {
	srv, api := newTestServer(t)
	defer srv.Close()

	if err := api.Store.SetSetting("server.public_url", "127.0.0.1:8080", false); err != nil {
		t.Fatal(err)
	}
	cookie := testSessionCookie(t, api)
	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+"/api/reg-tokens",
		bytes.NewBufferString(`{"name":"edge-router"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "invalid_public_url" {
		t.Fatalf("error code = %q, want invalid_public_url", body.Error.Code)
	}
	tokens, err := api.Store.ListRegTokens(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 0 {
		t.Fatalf("created %d token(s) despite invalid URL", len(tokens))
	}
}

func TestPublicURLFallsBackToForwardedRequestOrigin(t *testing.T) {
	srv, api := newTestServer(t)
	defer srv.Close()

	cookie := testSessionCookie(t, api)
	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+"/api/reg-tokens",
		bytes.NewBufferString(`{"name":"fallback-node"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "panel.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		InstallCommand string `json:"install_command"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.InstallCommand, "'https://panel.example.com/install.sh'") {
		t.Fatalf("fallback origin missing from command: %q", body.InstallCommand)
	}
}

func testSessionCookie(t *testing.T, api *Server) string {
	t.Helper()
	const sessionID = "test-session-public-url"
	if err := api.Store.CreateSession(sessionID, "test", "127.0.0.1", nowUnix()); err != nil {
		t.Fatal(err)
	}
	return "fobe_session=" + sessionID
}

package httpapi

import (
	"bytes"
	"net/http"
	"testing"
)

// Request-surface guards added 2026-09-20: a cross-site guard for state-changing
// requests, and a body cap in front of every handler.

func TestCrossSiteGuardBlocksStateChangingRequests(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := loginSession(t, srv)

	// No Sec-Fetch-Site at all (curl, the agent, and these tests) must pass: the
	// header is a browser signal, not a requirement.
	resp, _ := doAuthed(t, http.MethodPost, srv.URL+"/api/quick-commands", cookie, []byte(`{"name":"x"}`))
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("a POST without Sec-Fetch-Site must not be treated as cross-site")
	}

	crossSite := func(method, url string) int {
		t.Helper()
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// A state-changing method from another site is refused.
	if code := crossSite(http.MethodPost, srv.URL+"/api/quick-commands"); code != http.StatusForbidden {
		t.Fatalf("cross-site POST: status %d, want 403", code)
	}
	// A plain read is still allowed: following an external link into the SPA
	// must keep working, and a cross-site GET cannot read the response anyway.
	if code := crossSite(http.MethodGet, srv.URL+"/api/me"); code != http.StatusOK {
		t.Fatalf("cross-site GET /api/me: status %d, want 200", code)
	}
	// The two GETs with side effects are the exception.
	if code := crossSite(http.MethodGet, srv.URL+"/api/export"); code != http.StatusForbidden {
		t.Fatalf("cross-site GET /api/export: status %d, want 403", code)
	}
}

func TestRequestBodyIsCapped(t *testing.T) {
	srv, _ := newTestServer(t)

	// An unauthenticated endpoint with a body over the cap: the reader refuses
	// before the JSON decoder buffers it.
	payload := append([]byte(`{"password":"`), bytes.Repeat([]byte("a"), maxJSONBody+1024)...)
	payload = append(payload, []byte(`"}`)...)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/login", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body: status %d, want 400", resp.StatusCode)
	}
}

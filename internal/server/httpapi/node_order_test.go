// §16 (2026-10-02): the server list's drag order via PUT /api/nodes/order.
package httpapi

import (
	"net/http"
	"testing"
)

// nodeNames reads the order the list endpoint hands the panel.
func nodeNames(t *testing.T, srvURL, cookie string) []string {
	t.Helper()
	r := doReq(t, &http.Client{}, http.MethodGet, srvURL+"/api/nodes", cookie, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("list nodes: %d %s", r.Status, r.Body)
	}
	raw, _ := r.JSONMap(t)["nodes"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		name, _ := item.(map[string]any)["name"].(string)
		out = append(out, name)
	}
	return out
}

func wantNames(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestReorderNodesPersistsWallOrder(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	ids := map[string]string{}
	for _, name := range []string{"first", "second", "third"} {
		id, _ := seedNode(t, api, name, "m-"+name, "")
		ids[name] = id
	}
	wantNames(t, nodeNames(t, srv.URL, cookie), []string{"first", "second", "third"})

	// third, first, second — the wall is what the operator dropped, not created_at.
	r := doReq(t, &http.Client{}, http.MethodPut, srv.URL+"/api/nodes/order", cookie,
		map[string]any{"ids": []string{ids["third"], ids["first"], ids["second"]}})
	if r.Status != http.StatusOK {
		t.Fatalf("reorder: %d %s", r.Status, r.Body)
	}
	wantNames(t, nodeNames(t, srv.URL, cookie), []string{"third", "first", "second"})

	// An id the server has never seen (or one deleted meanwhile) is absorbed: it
	// drops out of the list and the rows it mentioned still land in that order,
	// with the unmentioned "third" last.
	r = doReq(t, &http.Client{}, http.MethodPut, srv.URL+"/api/nodes/order", cookie,
		map[string]any{"ids": []string{"ghost", ids["second"]}})
	if r.Status != http.StatusOK {
		t.Fatalf("reorder with unknown id: %d %s", r.Status, r.Body)
	}
	wantNames(t, nodeNames(t, srv.URL, cookie), []string{"second", "third", "first"})

	// An empty list is a client bug, not an intent.
	r = doReq(t, &http.Client{}, http.MethodPut, srv.URL+"/api/nodes/order", cookie,
		map[string]any{"ids": []string{}})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "invalid_ids" {
		t.Fatalf("empty order: got %d %s, want 400 invalid_ids", r.Status, r.Body)
	}
	// The rejected call must not have touched the stored order.
	wantNames(t, nodeNames(t, srv.URL, cookie), []string{"second", "third", "first"})

	// Anonymous callers cannot reorder the wall.
	r = doReq(t, &http.Client{}, http.MethodPut, srv.URL+"/api/nodes/order", "",
		map[string]any{"ids": []string{ids["first"]}})
	if r.Status != http.StatusUnauthorized {
		t.Fatalf("anonymous reorder: got %d, want 401", r.Status)
	}
}

package httpapi

// The audit page's numbered pagination.
//
// Numbered pages and "jump to page" need OFFSET plus a total, which the panel
// deliberately traded against window stability (design §16): audit rows keep
// arriving while the trail is read, so a page can shift by a row or two. These
// tests pin the contract the page control depends on — total, echoed page,
// clamping, and page-size validation.
//
// The harness writes to the trail itself (login is audited), so every
// expectation is relative to the count taken before seeding.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/store"
)

type auditPageResp struct {
	Entries []store.AuditEntry `json:"entries"`
	Total   int                `json:"total"`
	Page    int                `json:"page"`
	Limit   int                `json:"limit"`
}

func getAuditPage(t *testing.T, srv *httptest.Server, cookie, query string) auditPageResp {
	t.Helper()
	resp, raw := doAuthed(t, "GET", srv.URL+"/api/audit"+query, cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/audit%s = %d: %s", query, resp.StatusCode, raw)
	}
	var page auditPageResp
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode /api/audit%s: %v (%s)", query, err, raw)
	}
	return page
}

// insertAuditAction appends one recognisable row. Actions are prefixed so a
// test never depends on what else the harness wrote to the trail.
func insertAuditAction(t *testing.T, api *Server, action string) {
	t.Helper()
	if err := api.Store.InsertAudit(&store.AuditEntry{Actor: "cli", Action: action}); err != nil {
		t.Fatalf("insert audit %s: %v", action, err)
	}
}

func TestAuditPaginationNumberedPages(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	base, err := api.Store.CountAudit()
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	const rows = 25
	for i := 1; i <= rows; i++ {
		insertAuditAction(t, api, fmt.Sprintf("pg_%02d", i))
	}
	total := base + rows

	// Defaults: 20 per page, page 1, newest first.
	first := getAuditPage(t, srv, cookie, "")
	if first.Limit != 20 || first.Page != 1 || first.Total != total {
		t.Fatalf("defaults = limit %d page %d total %d, want 20/1/%d",
			first.Limit, first.Page, first.Total, total)
	}
	if len(first.Entries) != 20 {
		t.Fatalf("first page carries %d rows, want 20", len(first.Entries))
	}
	if first.Entries[0].Action != "pg_25" {
		t.Fatalf("newest row is %q, want pg_25 (newest first)", first.Entries[0].Action)
	}

	second := getAuditPage(t, srv, cookie, "?limit=20&page=2")
	if want := total - 20; len(second.Entries) != want {
		t.Fatalf("page 2 carries %d rows, want %d", len(second.Entries), want)
	}
	if second.Total != total {
		t.Fatalf("total changed between pages: %d then %d", first.Total, second.Total)
	}
	if second.Entries[0].ID >= first.Entries[len(first.Entries)-1].ID {
		t.Fatalf("page 2 overlaps page 1: first id %d >= page 1 last id %d",
			second.Entries[0].ID, first.Entries[len(first.Entries)-1].ID)
	}

	// A smaller page size re-cuts the same trail. Walking every page of it
	// must visit all 25 seeded rows exactly once, newest first.
	seen := map[string]int{}
	var ids []int64
	lastPage := (total + 9) / 10
	for p := 1; p <= lastPage; p++ {
		page := getAuditPage(t, srv, cookie, fmt.Sprintf("?limit=10&page=%d", p))
		if len(page.Entries) == 0 {
			t.Fatalf("page %d is empty before the trail ran out", p)
		}
		for _, e := range page.Entries {
			ids = append(ids, e.ID)
			if strings.HasPrefix(e.Action, "pg_") {
				seen[e.Action]++
			}
		}
	}
	if len(ids) != total {
		t.Fatalf("walking %d pages of 10 returned %d rows, want %d", lastPage, len(ids), total)
	}
	for i := 1; i <= rows; i++ {
		action := fmt.Sprintf("pg_%02d", i)
		if got := seen[action]; got != 1 {
			t.Fatalf("%s appeared %d times across the walk, want exactly once", action, got)
		}
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatalf("ids are not strictly descending at %d: %v", i, ids)
		}
	}
}

func TestAuditPaginationClampsAndValidates(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := loginSession(t, srv)
	for i := 1; i <= 5; i++ {
		insertAuditAction(t, api, fmt.Sprintf("pg_%d", i))
	}

	// A junk page falls back to page 1, the same way a junk limit falls back
	// to the default instead of failing the request.
	one := getAuditPage(t, srv, cookie, "?limit=2")
	for _, query := range []string{"?limit=2&page=0", "?limit=2&page=-3", "?limit=2&page=abc"} {
		got := getAuditPage(t, srv, cookie, query)
		if got.Page != 1 || len(got.Entries) == 0 || got.Entries[0].ID != one.Entries[0].ID {
			t.Fatalf("%s served page %d with %d rows; want page 1 starting at id %d",
				query, got.Page, len(got.Entries), one.Entries[0].ID)
		}
	}

	// An out-of-range page is clamped to the last page rather than served
	// empty: the control can hold a stale number after the trail shrinks.
	lastPage := (one.Total + 1) / 2 // limit=2
	last := getAuditPage(t, srv, cookie, fmt.Sprintf("?limit=2&page=%d", lastPage))
	clamped := getAuditPage(t, srv, cookie, "?limit=2&page=99999")
	if clamped.Page != last.Page || len(clamped.Entries) != len(last.Entries) {
		t.Fatalf("page 99999 = page %d with %d rows; last page is %d with %d rows",
			clamped.Page, len(clamped.Entries), last.Page, len(last.Entries))
	}
	if clamped.Entries[0].ID != last.Entries[0].ID {
		t.Fatalf("clamped page starts at id %d, last page at %d", clamped.Entries[0].ID, last.Entries[0].ID)
	}

	// The response reports the limit actually used.
	if def := getAuditPage(t, srv, cookie, "?limit=abc"); def.Limit != 20 {
		t.Fatalf("limit=abc served limit %d, want the 20 default", def.Limit)
	}
	if big := getAuditPage(t, srv, cookie, "?limit=1000"); big.Limit != 1000 {
		t.Fatalf("limit=1000 served limit %d", big.Limit)
	}
}

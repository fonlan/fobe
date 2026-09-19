package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/fonlan/fobe/internal/server/store"
)

func quickCmdJSON(t *testing.T, srvURL, method, path, cookie, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srvURL+path, bytes.NewReader([]byte(body)))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode response: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func TestQuickCommandsCrudAndReorder(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := panelCookie(t, srv)

	// Create three; each appends at the end of the list.
	ids := map[string]float64{}
	for _, name := range []string{"first", "second", "third"} {
		code, out := quickCmdJSON(t, srv.URL, http.MethodPost, "/api/quick-commands", cookie,
			fmt.Sprintf(`{"name":%q,"command":"echo %s"}`, name, name))
		if code != http.StatusOK {
			t.Fatalf("create %s: status %d out %v", name, code, out)
		}
		ids[name] = out["id"].(float64)
	}

	code, out := quickCmdJSON(t, srv.URL, http.MethodGet, "/api/quick-commands", cookie, "")
	if code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	list := out["commands"].([]any)
	if len(list) != 3 {
		t.Fatalf("list len = %d, want 3", len(list))
	}
	if got := list[0].(map[string]any)["name"]; got != "first" {
		t.Fatalf("append order broken: first item = %v", got)
	}

	// Reorder: third, first, second. The order endpoint must win over sort by id.
	orderIDs := []float64{ids["third"], ids["first"], ids["second"]}
	raw, _ := json.Marshal(map[string]any{"ids": orderIDs})
	code, _ = quickCmdJSON(t, srv.URL, http.MethodPut, "/api/quick-commands/order", cookie, string(raw))
	if code != http.StatusOK {
		t.Fatalf("reorder: status %d", code)
	}

	_, out = quickCmdJSON(t, srv.URL, http.MethodGet, "/api/quick-commands", cookie, "")
	list = out["commands"].([]any)
	want := []string{"third", "first", "second"}
	for i, w := range want {
		if got := list[i].(map[string]any)["name"]; got != w {
			t.Fatalf("after reorder item %d = %v, want %s", i, got, w)
		}
	}

	// Update rewrites name and command in place, keeping the position.
	raw, _ = json.Marshal(map[string]any{"name": "first renamed", "command": "uptime"})
	code, _ = quickCmdJSON(t, srv.URL, http.MethodPut, fmt.Sprintf("/api/quick-commands/%d", int64(ids["first"])), cookie, string(raw))
	if code != http.StatusOK {
		t.Fatalf("update: status %d", code)
	}
	_, out = quickCmdJSON(t, srv.URL, http.MethodGet, "/api/quick-commands", cookie, "")
	list = out["commands"].([]any)
	if got := list[1].(map[string]any)["name"]; got != "first renamed" {
		t.Fatalf("updated row drifted: item 1 = %v", got)
	}

	// Delete removes the row; a second delete is 404.
	code, _ = quickCmdJSON(t, srv.URL, http.MethodDelete, fmt.Sprintf("/api/quick-commands/%d", int64(ids["second"])), cookie, "")
	if code != http.StatusOK {
		t.Fatalf("delete: status %d", code)
	}
	code, _ = quickCmdJSON(t, srv.URL, http.MethodDelete, fmt.Sprintf("/api/quick-commands/%d", int64(ids["second"])), cookie, "")
	if code != http.StatusNotFound {
		t.Fatalf("double delete: status %d, want 404", code)
	}
}

func TestQuickCommandsValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := panelCookie(t, srv)

	cases := []struct {
		name, body, code string
	}{
		{"empty name", `{"name":"  ","command":"ls"}`, "invalid_name"},
		{"long name", fmt.Sprintf(`{"name":%q,"command":"ls"}`, strings.Repeat("名", 101)), "invalid_name"},
		{"blank command", `{"name":"x","command":"   "}`, "invalid_command"},
		{"oversized command", fmt.Sprintf(`{"name":"x","command":%q}`, strings.Repeat("a", 17*1024)), "invalid_command"},
		// One line past the tty canonical limit must be refused: the line
		// discipline would truncate it and the operator would run a mangled
		// command (grill-me 定稿 2026-09-19).
		{"long single line", fmt.Sprintf(`{"name":"x","command":%q}`, strings.Repeat("a", 4001)), "command_line_too_long"},
		{"long line inside multi-line", fmt.Sprintf(`{"name":"x","command":"ok\n%s"}`, strings.Repeat("a", 4001)), "command_line_too_long"},
	}
	for _, tc := range cases {
		code, out := quickCmdJSON(t, srv.URL, http.MethodPost, "/api/quick-commands", cookie, tc.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", tc.name, code)
		}
		if got := out["error"].(map[string]any)["code"]; got != tc.code {
			t.Fatalf("%s: code %v, want %s", tc.name, got, tc.code)
		}
	}

	// 4000 bytes is the accepted ceiling (MAX_CANON is 4095).
	code, _ := quickCmdJSON(t, srv.URL, http.MethodPost, "/api/quick-commands", cookie,
		fmt.Sprintf(`{"name":"edge","command":%q}`, strings.Repeat("a", 4000)))
	if code != http.StatusOK {
		t.Fatalf("4000-byte line rejected: status %d", code)
	}

	// CRLF is folded and trailing newlines are stripped — a Windows paste must
	// not bake \r into the PTY input or auto-execute an empty extra line.
	code, out := quickCmdJSON(t, srv.URL, http.MethodPost, "/api/quick-commands", cookie,
		`{"name":"crlf","command":"line1\r\nline2\r\n"}`)
	if code != http.StatusOK {
		t.Fatalf("crlf create: status %d", code)
	}
	_, out = quickCmdJSON(t, srv.URL, http.MethodGet, "/api/quick-commands", cookie, "")
	list := out["commands"].([]any)
	for _, item := range list {
		row := item.(map[string]any)
		if row["name"] == "crlf" {
			if got := row["command"]; got != "line1\nline2" {
				t.Fatalf("crlf normalization: command = %q", got)
			}
		}
	}
}

func TestQuickCommandsCap(t *testing.T) {
	srv, api := newTestServer(t)
	cookie := panelCookie(t, srv)

	// Fill the store directly: the cap is about the list staying manageable,
	// not about the HTTP path being slow.
	for i := 0; i < store.MaxQuickCommands; i++ {
		if _, err := api.Store.CreateQuickCommand(fmt.Sprintf("c%d", i), "true"); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	code, out := quickCmdJSON(t, srv.URL, http.MethodPost, "/api/quick-commands", cookie, `{"name":"over","command":"true"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("over cap: status %d, want 400", code)
	}
	if got := out["error"].(map[string]any)["code"]; got != "too_many_commands" {
		t.Fatalf("over cap: code %v, want too_many_commands", got)
	}
}

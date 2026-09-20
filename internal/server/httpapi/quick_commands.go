// Quick commands HTTP surface (2026-09-19): CRUD + reorder for the terminal
// side panel's command snippets. Session-guarded panel data; nothing here
// executes anything — the browser sends the text into its own PTY as keyboard
// input, which is why there is no audit trail (it is the operator's own typing).
package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fonlan/fobe/internal/server/store"
)

// Validation limits (grill-me 定稿 2026-09-19):
//   - name ≤ 100 runes — a list button shows the name as its main text.
//   - command ≤ 16 KiB total — a multi-line snippet, not a script file.
//   - each line ≤ 4000 bytes — the tty line discipline (MAX_CANON 4095) drops
//     or truncates longer lines, which would execute a silently mangled command.
//   - ≤ 50 rows (store.MaxQuickCommands).
const (
	quickNameLimit      = 100
	quickCommandLimit   = 16 * 1024
	quickCommandLineMax = 4000
)

type quickCommandReq struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// normalizeQuickCommand validates and canonicalizes one entry; it returns a
// snake_case error code or "" when the entry is acceptable. CRLF is folded to
// LF (a Windows-originated paste would otherwise bake stray \r into the PTY
// input) and trailing newlines are stripped — they would only auto-execute an
// empty extra line.
func normalizeQuickCommand(req *quickCommandReq) string {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > quickNameLimit {
		return "invalid_name"
	}
	command := strings.ReplaceAll(req.Command, "\r\n", "\n")
	command = strings.TrimRight(command, "\n")
	if strings.TrimSpace(command) == "" {
		return "invalid_command"
	}
	if len(command) > quickCommandLimit {
		return "invalid_command"
	}
	for _, line := range strings.Split(command, "\n") {
		if len(line) > quickCommandLineMax {
			return "command_line_too_long"
		}
	}
	req.Command = command
	return ""
}

func (s *Server) handleListQuickCommands(w http.ResponseWriter, r *http.Request) {
	commands, err := s.Store.ListQuickCommands()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": commands})
}

func (s *Server) handleCreateQuickCommand(w http.ResponseWriter, r *http.Request) {
	var req quickCommandReq
	if !decodeReq(w, r, &req) {
		return
	}
	if code := normalizeQuickCommand(&req); code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	n, err := s.Store.CountQuickCommands()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if n >= store.MaxQuickCommands {
		writeErr(w, http.StatusBadRequest, "too_many_commands")
		return
	}
	id, err := s.Store.CreateQuickCommand(req.Name, req.Command)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleUpdateQuickCommand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	var req quickCommandReq
	if !decodeReq(w, r, &req) {
		return
	}
	if code := normalizeQuickCommand(&req); code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	if err := s.Store.UpdateQuickCommand(id, req.Name, req.Command); !writeStoreErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteQuickCommand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if err := s.Store.DeleteQuickCommand(id); !writeStoreErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type quickCommandOrderReq struct {
	IDs []int64 `json:"ids"`
}

func (s *Server) handleReorderQuickCommands(w http.ResponseWriter, r *http.Request) {
	var req quickCommandOrderReq
	if !decodeReq(w, r, &req) {
		return
	}
	if err := s.Store.ReorderQuickCommands(req.IDs); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

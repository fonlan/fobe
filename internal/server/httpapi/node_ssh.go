package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fobe-panel/fobe/internal/protocol"
	"github.com/fobe-panel/fobe/internal/server/security"
	"github.com/fobe-panel/fobe/internal/server/store"
)

// Per-node SSH credentials for the SSH-mode web terminal (design §11).
// Secrets are AES-GCM encrypted at rest (security.Cryptor) and are write-only
// over the API: GET reports whether a password / private key is set, never
// the value. The plaintext exists in memory only for the moment it is copied
// into a terminal_open frame on its way to the agent.

const (
	defaultSSHUser = "root"
	defaultSSHPort = 22

	// clearSecretPrefix marks "delete the stored value" in PUT requests.
	// Chosen over an explicit JSON null because encoding/json cannot
	// distinguish an absent field from `null` on a plain string field, so the
	// prefix is the one unambiguous clear marker. A missing field or an empty
	// string keeps the stored value.
	clearSecretPrefix = "!"

	// Reason sent to the browser (terminal_closed) when an ssh-mode
	// terminal_open cannot be served because the node has no usable stored
	// credentials. The frontend localizes it.
	reasonSSHCredentialsMissing = "ssh_credentials_missing"
)

// errSSHCredentialsMissing marks a node without usable stored credentials.
var errSSHCredentialsMissing = errors.New("ssh credentials not configured")

type nodeSSHView struct {
	User        string `json:"user"`
	Port        int    `json:"port"`
	PasswordSet bool   `json:"password_set"`
	PrivKeySet  bool   `json:"privkey_set"`
}

func (s *Server) handleGetNodeSSH(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	if _, err := s.Store.GetNode(nodeID); err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	row, err := s.Store.GetNodeSSH(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, nodeSSHView{User: defaultSSHUser, Port: defaultSSHPort})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	user := row.User
	if user == "" {
		user = defaultSSHUser
	}
	port := row.Port
	if port <= 0 {
		port = defaultSSHPort
	}
	writeJSON(w, http.StatusOK, nodeSSHView{
		User:        user,
		Port:        port,
		PasswordSet: row.PasswordEnc != "",
		PrivKeySet:  row.PrivKeyEnc != "",
	})
}

// putNodeSSHReq — see clearSecretPrefix for the keep/set/clear semantics.
type putNodeSSHReq struct {
	User       string `json:"user"`
	Port       int    `json:"port"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
}

func (s *Server) handlePutNodeSSH(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	if _, err := s.Store.GetNode(nodeID); err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	var req putNodeSSHReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Port != 0 && (req.Port < 1 || req.Port > 65535) {
		writeErr(w, http.StatusBadRequest, "bad_port")
		return
	}

	row, err := s.Store.GetNodeSSH(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		row = &store.NodeSSH{NodeID: nodeID, User: defaultSSHUser, Port: defaultSSHPort}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	if user := strings.TrimSpace(req.User); user != "" {
		row.User = user
	}
	if req.Port != 0 {
		row.Port = req.Port
	}
	if err := s.applySecretChange(&row.PasswordEnc, req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := s.applySecretChange(&row.PrivKeyEnc, req.PrivateKey); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	if err := s.Store.UpsertNodeSSH(row); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "panel", NodeID: nodeID, Action: "node_ssh_updated", SourceIP: s.Trust.RealIP(r),
	})
	writeJSON(w, http.StatusOK, nodeSSHView{
		User:        row.User,
		Port:        row.Port,
		PasswordSet: row.PasswordEnc != "",
		PrivKeySet:  row.PrivKeyEnc != "",
	})
}

// applySecretChange implements the keep / set / clear semantics documented on
// clearSecretPrefix; new values are encrypted before they touch the store.
func (s *Server) applySecretChange(dst *string, value string) error {
	switch {
	case value == "":
		return nil // keep
	case strings.HasPrefix(value, clearSecretPrefix):
		*dst = "" // clear
		return nil
	default:
		enc, err := s.Crypt.Encrypt(value)
		if err != nil {
			return fmt.Errorf("encrypt ssh secret: %w", err)
		}
		*dst = enc
		return nil
	}
}

// injectSSH fills an ssh-mode terminal_open with the node's stored
// credentials (design §11). The host is pinned to loopback — the agent dials
// its own sshd, so a panel user cannot turn the agent into an arbitrary SSH
// client. Returns errSSHCredentialsMissing when the node has nothing usable
// stored; the caller answers the browser without bothering the agent.
func (s *Server) injectSSH(nodeID string, open *protocol.TerminalOpen) error {
	row, err := s.Store.GetNodeSSH(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return errSSHCredentialsMissing
	}
	if err != nil {
		return err
	}
	return applyNodeSSH(open, row, s.Crypt)
}

// applyNodeSSH is the testable core of injectSSH: it maps a stored (still
// encrypted) credential row onto a TerminalOpen, decrypting in place.
func applyNodeSSH(open *protocol.TerminalOpen, row *store.NodeSSH, crypt *security.Cryptor) error {
	open.Host = "127.0.0.1"
	if row.Port > 0 {
		open.Port = row.Port
	} else {
		open.Port = defaultSSHPort
	}
	if row.User != "" {
		open.User = row.User
	} else {
		open.User = defaultSSHUser
	}
	if row.PasswordEnc != "" {
		pw, err := crypt.Decrypt(row.PasswordEnc)
		if err != nil {
			return fmt.Errorf("decrypt ssh password: %w", err)
		}
		open.Password = pw
	}
	if row.PrivKeyEnc != "" {
		key, err := crypt.Decrypt(row.PrivKeyEnc)
		if err != nil {
			return fmt.Errorf("decrypt ssh private key: %w", err)
		}
		open.PrivKey = key
	}
	if open.Password == "" && open.PrivKey == "" {
		// Row exists (user/port choices preserved) but no usable secret.
		return errSSHCredentialsMissing
	}
	return nil
}

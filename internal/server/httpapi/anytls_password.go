package httpapi

import (
	"fmt"
	"strings"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// ensureAnytlsPassword returns the global shared anytls password, generating and
// persisting one the first time anything needs it (design §10.1 实现修订
// 2026-09-16).
//
// The credential used to be operator-supplied: the panel refused to install
// sing-box anywhere until `anytls_password` was set (errAnytlsPasswordUnset →
// 400 `anytls_password_unset`), and the Settings page was the only way to
// provide or rotate it. That asked the operator to invent, remember and re-type
// a secret whose only consumers are machine-generated artifacts — the node
// config pushed to the agent and the subscription the client fetches. Nobody
// ever needs to read it, so the server now owns it end to end.
//
// Being reachable from a read path (subscription rendering) is deliberate: the
// moment of use *is* the moment to create it, which keeps a fresh install from
// minting a credential it never uses.
//
// The mutex is not decoration. The value is read-then-written, and SQLite's
// single writer makes each statement atomic, not the pair — two concurrent
// installs would otherwise bake two different passwords into two node configs,
// and only one of them would be the one subscriptions hand out.
func (s *Server) ensureAnytlsPassword() (string, error) {
	s.anytlsMu.Lock()
	defer s.anytlsMu.Unlock()

	raw, err := s.Store.GetSetting("anytls_password")
	if err == nil && strings.TrimSpace(raw) != "" {
		// Present means hand it back — including the "stored but unreadable"
		// case, which must fail loudly instead of falling through to
		// generation. An undecryptable ciphertext is the wrong-master-key
		// incident (§4.4), and quietly rotating over it would cut off every
		// client that is still working off the previous password while the
		// operator is already staring at a much bigger problem.
		plain, derr := s.Crypt.Decrypt(raw)
		if derr != nil {
			return "", fmt.Errorf("decrypt anytls_password: %w", derr)
		}
		if plain != "" {
			return plain, nil
		}
	}

	password, err := singbox.GenerateAnytlsPassword()
	if err != nil {
		return "", err
	}
	encrypted, err := s.Crypt.Encrypt(password)
	if err != nil {
		return "", fmt.Errorf("encrypt anytls_password: %w", err)
	}
	if err := s.Store.SetSetting("anytls_password", encrypted, true); err != nil {
		return "", fmt.Errorf("store anytls_password: %w", err)
	}
	// Audit the event, never the value (§4.4): the plaintext appears only inside
	// the agent's config and the rendered subscription.
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "system", Action: "anytls_password_generated", Command: "[redacted]",
	})
	s.Log.Info("generated the global anytls password", "length", len(password))
	return password, nil
}

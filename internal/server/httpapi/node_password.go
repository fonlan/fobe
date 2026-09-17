// The credential of a node's own anytls inbound (design §10.1 实现修订
// 2026-09-17e).
//
// It used to be one global `settings.anytls_password` shared by the whole
// fleet: every node the panel installed baked the same value in, rotating it
// meant re-pushing every node (and every client re-subscribing), and the
// setting's consumers were machine artifacts nobody ever reads. A single
// leaked credential therefore forced a fleet-wide rotation, while a node's
// password had nothing to do with the node.
//
// The credential belongs to the **inbound**. The server mints a fresh 16-char
// alnum one at the moment it *creates* an inbound, and reads it back from the
// node's own configuration whenever it regenerates that config — so an install
// that finds an existing credential keeps it, and nodes installed by older
// builds (whose credential was the old global value, baked into the config
// they already carry) keep serving it with no migration step at all.
package httpapi

import (
	"errors"
	"fmt"
	"strings"

	"github.com/fonlan/fobe/internal/server/singbox"
	"github.com/fonlan/fobe/internal/server/store"
)

// nodeProxyPassword returns the credential of the explicitly chosen inbound,
// resolved from the node's configuration in the order that answers "what is the
// probe serving right now":
//
//  1. the document the panel last wrote (`singbox_config:<node_id>`);
//  2. the file the probe last reported (a node the panel never installed —
//     a listener recognised from its report — has only this one);
//  3. §19.9's per-node override column: nothing writes it since the editor
//     model, but a value set by hand must still win over minting a new one.
//
// `port` identifies the listener. Empty means there is no credential at that
// port; callers creating an entry may then mint one.
func (s *Server) nodeProxyPassword(nodeID string, port int) string {
	if doc, err := s.Store.GetSetting("singbox_config:" + nodeID); err == nil && strings.TrimSpace(doc) != "" {
		if pw := singbox.InboundPasswordAtPort(doc, port); pw != "" {
			return pw
		}
	}
	if local, err := s.Store.GetNodeSingboxLocal(nodeID, s.Crypt); err == nil {
		if pw := singbox.InboundPasswordAtPort(local.ConfigJSON, port); pw != "" {
			return pw
		}
	}
	if pw, err := s.Store.GetNodeSingboxPasswordOverride(nodeID); err == nil && pw != "" {
		return pw
	}
	return ""
}

// ensureNodeProxyPassword is the "the panel is creating this inbound" path: the
// node's existing credential when it has one, otherwise a freshly generated
// 16-char alnum password (the charset is alnum on purpose: the same string
// travels as a JSON field, as a URI auth component in third-party clients and
// as a Clash YAML scalar, and only letters and digits need no escaping in all
// three, §10.1).
//
// Only this path mints. Every other caller (the startup sync, subscription
// rendering) must reuse what it finds: minting during a regeneration would
// silently rotate a working credential and cut off every client holding the
// old URI.
func (s *Server) ensureNodeProxyPassword(nodeID string, port int) (string, error) {
	if pw := s.nodeProxyPassword(nodeID, port); pw != "" {
		return pw, nil
	}
	// Both sources are checked for *readability* before minting: a value that
	// exists but cannot be read is the wrong-master-key incident (§4.4), not
	// "this node has no credential". Minting over it would rotate a credential
	// whose clients are still connected while the operator is already restoring
	// keys — report and touch nothing instead.
	if _, err := s.Store.GetSetting("singbox_config:" + nodeID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("read node %s config before minting a credential: %w", nodeID, err)
	}
	if _, err := s.Store.GetNodeSingboxLocal(nodeID, s.Crypt); err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("tell whether node %s already has a credential: %w", nodeID, err)
	}
	pw, err := singbox.GenerateAnytlsPassword()
	if err != nil {
		return "", err
	}
	// Audit the event, never the value (§4.4): the plaintext appears only inside
	// the config pushed to the probe and in the subscription it renders.
	s.Store.InsertAudit(&store.AuditEntry{
		Actor: "system", NodeID: nodeID, Action: "anytls_password_generated", Command: "[redacted]",
	})
	s.Log.Info("generated the node's inbound credential", "node", nodeID, "port", port)
	return pw, nil
}

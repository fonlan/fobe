package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/fonlan/fobe/internal/server/security"
	"github.com/fonlan/fobe/internal/server/singbox"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedNode gives the FK-constrained node_singbox rows something to point at.
func seedNode(t *testing.T, st *Store, id string) {
	t.Helper()
	if err := st.CreateNode(&Node{ID: id, Name: id, MachineID: "m-" + id}, "hash"); err != nil {
		t.Fatalf("create node: %v", err)
	}
}

func newTestCryptor(t *testing.T, fill byte) *security.Cryptor {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	c, err := security.NewCryptor(key)
	if err != nil {
		t.Fatalf("cryptor: %v", err)
	}
	return c
}

// The local snapshot is the operator's own config.json: it may carry
// credentials, so it is ciphertext at rest (§9.3 实现修订 2026-09-17) and must
// survive the round trip byte for byte.
func TestLocalSnapshotEncryptedRoundTrip(t *testing.T) {
	st := newTestStore(t)
	seedNode(t, st, "n1")
	crypt := newTestCryptor(t, 3)
	const cfg = `{"inbounds":[{"type":"anytls","users":[{"password":"AnyTlsScriptPw1"}]}]}`
	in := NodeSingboxLocal{
		LocalHash: "abc", ConfigPath: "/etc/one-sing/config.json",
		LocalPresent: true, LocalVersion: "1.13.0-beta.7", LocalRunning: true,
		LocalUnit: true, LocalUnitOK: true, ConfigJSON: cfg,
	}
	if err := st.SetNodeSingboxLocal("n1", in, crypt); err != nil {
		t.Fatalf("set: %v", err)
	}

	var raw string
	if err := st.db.QueryRow(`SELECT local_config FROM node_singbox WHERE node_id = ?`, "n1").Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if bytes.Contains([]byte(raw), []byte("AnyTlsScriptPw1")) {
		t.Fatalf("stored snapshot is plaintext: %s", raw)
	}

	got, err := st.GetNodeSingboxLocal("n1", crypt)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != in {
		t.Fatalf("round trip changed the snapshot:\n got %+v\nwant %+v", got, in)
	}

	// A different master key must fail loudly. Silently returning "" would look
	// like "the probe never reported" and invite the operator to adopt nothing.
	if _, err := st.GetNodeSingboxLocal("n1", newTestCryptor(t, 9)); err == nil {
		t.Fatal("a wrong master key must not decrypt")
	}
}

// Adopted inbounds carry UUIDs, passwords and a REALITY private key: same
// treatment, and the same "must not silently empty out" rule.
func TestExtraInboundsEncryptedRoundTrip(t *testing.T) {
	st := newTestStore(t)
	seedNode(t, st, "n1")
	crypt := newTestCryptor(t, 5)
	extras := []singbox.ExtraInbound{{
		Type: "vless", Tag: "vless-in", Listen: "::", ListenPort: 16929,
		Users: []singbox.ExtraUser{{UUID: "uuid-1", Flow: "xtls-rprx-vision"}},
		TLS: &singbox.ExtraTLS{
			Enabled: true, ServerName: "www.microsoft.com",
			Reality: &singbox.ExtraReality{Enabled: true, PrivateKey: "priv", PublicKey: "pub", ShortID: []string{""}},
		},
	}}
	if err := st.SetNodeSingboxExtraInbounds("n1", extras, crypt); err != nil {
		t.Fatalf("set: %v", err)
	}
	var raw string
	if err := st.db.QueryRow(`SELECT extra_inbounds FROM node_singbox WHERE node_id = ?`, "n1").Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if bytes.Contains([]byte(raw), []byte("uuid-1")) {
		t.Fatalf("stored inbounds are plaintext: %s", raw)
	}

	got, err := st.GetNodeSingboxExtraInbounds("n1", crypt)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || got[0].ListenPort != 16929 || got[0].Users[0].UUID != "uuid-1" {
		t.Fatalf("round trip = %+v", got)
	}
	if got[0].TLS == nil || got[0].TLS.Reality == nil || got[0].TLS.Reality.PublicKey != "pub" {
		t.Fatalf("reality half lost: %+v", got[0].TLS)
	}

	// An empty set clears the column instead of storing "null".
	if err := st.SetNodeSingboxExtraInbounds("n1", nil, crypt); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, err := st.GetNodeSingboxExtraInbounds("n1", crypt); err != nil || len(got) != 0 {
		t.Fatalf("after clear: %+v (%v)", got, err)
	}
}

// The list read carries "has adopted inbounds" without decrypting anything —
// the subscription picker decides renderability from it.
func TestNodeSingboxReadReportsExtrasPresent(t *testing.T) {
	st := newTestStore(t)
	seedNode(t, st, "n1")
	crypt := newTestCryptor(t, 7)
	if err := st.UpsertNodeSingbox(&NodeSingbox{NodeID: "n1", Port: 22039}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sb, err := st.GetNodeSingbox("n1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sb.ExtrasPresent {
		t.Fatal("ExtrasPresent = true with no adopted inbounds")
	}
	if err := st.SetNodeSingboxExtraInbounds("n1", []singbox.ExtraInbound{{Type: "socks", ListenPort: 1080}}, crypt); err != nil {
		t.Fatalf("set: %v", err)
	}
	sb, err = st.GetNodeSingbox("n1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !sb.ExtrasPresent {
		t.Fatal("ExtrasPresent = false after adoption")
	}
}

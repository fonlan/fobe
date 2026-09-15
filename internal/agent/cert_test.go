package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureSelfSignedCertGeneratesAndReadsBack(t *testing.T) {
	dir := t.TempDir()

	pair, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if pair.PEM == "" || pair.SHA256 == "" {
		t.Fatalf("empty report: %+v", pair)
	}
	if pair.SHA256 != sha256Hex([]byte(pair.PEM)) {
		t.Fatalf("sha256 mismatch with PEM")
	}

	// files exist with the required modes (§9.3: dir 0700, files 0600)
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)
	for _, p := range []string{certPath, keyPath} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s perm = %o, want 600", p, st.Mode().Perm())
		}
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("cert dir perm = %v err=%v, want 700", st.Mode().Perm(), err)
	}

	// the certificate itself: CN, validity, key type
	block, _ := pem.Decode([]byte(pair.PEM))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("no CERTIFICATE pem block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if leaf.Subject.CommonName != certCommonName {
		t.Fatalf("CN = %q, want %q", leaf.Subject.CommonName, certCommonName)
	}
	if leaf.NotAfter.Unix() != pair.NotAfter {
		t.Fatalf("NotAfter mismatch: %d vs %d", leaf.NotAfter.Unix(), pair.NotAfter)
	}
	if validity := leaf.NotAfter.Sub(leaf.NotBefore); validity < 9*365*24*time.Hour {
		t.Fatalf("validity = %s, want ~10 years", validity)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("public key = %T, want ECDSA P-256", leaf.PublicKey)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		t.Fatalf("no key pem block")
	}
	if _, err := x509.ParseECPrivateKey(kb.Bytes); err != nil {
		t.Fatalf("parse key: %v", err)
	}
}

func TestEnsureSelfSignedCertIdempotent(t *testing.T) {
	dir := t.TempDir()

	first, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.SHA256 != second.SHA256 || first.PEM != second.PEM {
		t.Fatalf("existing pair was regenerated")
	}
}

func TestEnsureSelfSignedCertRegeneratesCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, certFileName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	pair, err := ensureSelfSignedCert(dir)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if _, err := x509.ParseCertificate(pemBlockBytes(t, pair.PEM)); err != nil {
		t.Fatalf("corrupt file not replaced: %v", err)
	}
}

func pemBlockBytes(t *testing.T, pemStr string) []byte {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		t.Fatalf("no pem block in %q", pemStr)
	}
	return block.Bytes
}

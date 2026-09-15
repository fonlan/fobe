package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/fobe-panel/fobe/internal/agent/service"
)

// Self-signed certificate for the anytls inbound (design §9.3): the private
// key never leaves the probe, the agent reports cert PEM + SHA256 + NotAfter
// so the panel can pin it in subscriptions instead of `insecure: true`.
//
// The file names come from the service package because the server's generated
// config.json names them (§9.3 实现修订: cert.crt/private.key, the one-sing.sh
// layout). ECDSA P-256 is kept over one-sing.sh's RSA-4096: probes are small
// boxes, the key is generated on-device, and clients pin the certificate by
// SHA256, so the algorithm is invisible to them.
const (
	certCommonName = "www.bing.com"
	certOrg        = "fobe"
	certValidity   = 10 * 365 * 24 * time.Hour
	certFileName   = service.SingboxCertFile
	keyFileName    = service.SingboxKeyFile
)

// certPair is what convergence needs to report after ensuring the files.
type certPair struct {
	PEM      string
	SHA256   string
	NotAfter int64 // unix seconds
}

// ensureSelfSignedCert returns the certificate pair in dir, generating an
// ECDSA P-256 self-signed cert (CN=www.bing.com, 10 years) when missing or
// unreadable. dir gets 0700, the files 0600.
func ensureSelfSignedCert(dir string) (certPair, error) {
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	if pair, err := readCertPair(certPath, keyPath); err == nil {
		return pair, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return certPair{}, fmt.Errorf("create cert dir: %w", err)
	}
	// MkdirAll leaves an existing too-open dir untouched; §9.3 demands 0700.
	if err := os.Chmod(dir, 0o700); err != nil {
		return certPair{}, fmt.Errorf("chmod cert dir: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return certPair{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return certPair{}, fmt.Errorf("generate serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: certCommonName, Organization: []string{certOrg}},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate small clock skew
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{certCommonName},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return certPair{}, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return certPair{}, fmt.Errorf("marshal key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// 0600: both files hold private material; sing-box runs as root (§9.2 unit).
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return certPair{}, fmt.Errorf("write key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return certPair{}, fmt.Errorf("write certificate: %w", err)
	}
	return summarizeCert(certPEM, der)
}

// readCertPair loads an existing pair; any parse failure triggers regeneration.
func readCertPair(certPath, keyPath string) (certPair, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return certPair{}, err
	}
	if _, err := os.Stat(keyPath); err != nil {
		return certPair{}, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return certPair{}, fmt.Errorf("%s: no PEM block", certFileName)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certPair{}, fmt.Errorf("parse certificate: %w", err)
	}
	return summarizeCert(certPEM, leaf.Raw)
}

// summarizeCert builds the reportable pair from raw PEM plus DER.
func summarizeCert(certPEM, der []byte) (certPair, error) {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return certPair{}, err
	}
	sum := sha256.Sum256(certPEM)
	return certPair{
		PEM:      string(certPEM),
		SHA256:   hex.EncodeToString(sum[:]),
		NotAfter: leaf.NotAfter.Unix(),
	}, nil
}

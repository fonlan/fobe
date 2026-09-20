// Package security holds the panel-side security primitives (design.md §4):
// argon2id password hashing, session cookies, the login-failure blacklist with
// its XFF trust chain, and AES-GCM secret storage.
package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword produces a PHC-format argon2id hash:
//
//	$argon2id$v=19$m=65536,t=1,p=2$salt$hash
//
// The parameters are the ones in the const block above (m=64 MiB, t=1, p=2 —
// the docstring used to claim t=2). They are deliberately expensive: 64 MiB per
// verification is what makes an unauthenticated flood a memory-exhaustion
// vector, which is why POST /api/login sits behind loginGate.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword compares password against a PHC argon2id hash in constant time.
func VerifyPassword(password, phc string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// RandomToken returns a URL-safe high-entropy token (design §10: subscription
// and registration tokens are stored hashed, plaintext shown once).
func RandomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var ErrWeakMasterKey = errors.New("master key must decode to exactly 32 bytes of base64")

// ParseMasterKey decodes FOBE_MASTER_KEY (base64 std or raw string fallback for dev).
func ParseMasterKey(v string) ([]byte, error) {
	if v == "" {
		return nil, ErrWeakMasterKey
	}
	if b, err := base64.StdEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, ErrWeakMasterKey
}

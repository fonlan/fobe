package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Cryptor encrypts/decrypts settings values at rest with AES-256-GCM
// (design §4.4: AI API keys, SSH credentials, bot tokens, template secrets).
type Cryptor struct {
	aead cipher.AEAD
}

func NewCryptor(masterKey []byte) (*Cryptor, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Cryptor{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext).
func (c *Cryptor) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	ct := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (c *Cryptor) Decrypt(b64 string) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(ct) < ns {
		return "", fmt.Errorf("ciphertext shorter than nonce")
	}
	pt, err := c.aead.Open(nil, ct[:ns], ct[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("gcm open: %w", err)
	}
	return string(pt), nil
}

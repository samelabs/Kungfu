package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// SecretBox seals short operator secrets (payment provider credentials)
// for storage at rest with AES-256-GCM. The key comes from the
// SETTINGS_ENC_KEY environment variable and never touches the database.
//
// Blob layout: version(1) || nonce(12) || ciphertext+tag. The caller
// supplies a purpose string that is bound as additional authenticated
// data, so a sealed API key can never be opened as a webhook secret
// (or any other field) even by someone with write access to the table.
type SecretBox struct {
	aead cipher.AEAD
}

const secretBoxVersion byte = 1

// ErrSecretBoxUnavailable is returned by a nil SecretBox (no key
// configured): callers must fail closed.
var ErrSecretBoxUnavailable = errors.New("settings encryption key is not configured")

// ParseSecretBoxKey decodes a 64-hex-char (32-byte) key.
func ParseSecretBoxKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != 32 {
		return nil, errors.New("must be 64 hex characters (32 bytes); generate one with `openssl rand -hex 32`")
	}
	return key, nil
}

// NewSecretBox builds a box from a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, errors.New("secret box key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

// Seal encrypts plaintext bound to purpose.
func (b *SecretBox) Seal(plaintext, purpose string) ([]byte, error) {
	if b == nil {
		return nil, ErrSecretBoxUnavailable
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, secretBoxVersion)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, []byte(plaintext), []byte(purpose)), nil
}

// Open decrypts a blob sealed for purpose.
func (b *SecretBox) Open(blob []byte, purpose string) (string, error) {
	if b == nil {
		return "", ErrSecretBoxUnavailable
	}
	ns := b.aead.NonceSize()
	if len(blob) < 1+ns+b.aead.Overhead() || blob[0] != secretBoxVersion {
		return "", errors.New("sealed secret is malformed")
	}
	pt, err := b.aead.Open(nil, blob[1:1+ns], blob[1+ns:], []byte(purpose))
	if err != nil {
		return "", errors.New("sealed secret cannot be opened with the configured key")
	}
	return string(pt), nil
}

// MaskSecret renders a secret for display: only the last 4 characters
// survive, and short secrets are fully hidden.
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

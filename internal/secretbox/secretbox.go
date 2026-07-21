// Package secretbox encrypts small secrets (OAuth access/refresh tokens) at
// rest with AES-256-GCM. The 32-byte key comes from TOKEN_ENCRYPTION_KEY
// (base64). Ciphertext is self-describing: a random nonce is prepended to the
// GCM output, so Open needs only the key.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Box seals and opens secrets under one key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a base64-encoded 32-byte key. It returns a clear error
// when the key is malformed so the app can refuse to boot.
func New(keyBase64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext, returning nonce||ciphertext.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal.
func (b *Box) Open(ciphertext []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	return b.aead.Open(nil, nonce, ct, nil)
}

// SealString / OpenString are convenience wrappers for string secrets.
func (b *Box) SealString(s string) ([]byte, error) { return b.Seal([]byte(s)) }

func (b *Box) OpenString(ciphertext []byte) (string, error) {
	pt, err := b.Open(ciphertext)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// GenerateKey returns a fresh base64-encoded 32-byte key, for docs/tests and
// the runbook's key-generation step.
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

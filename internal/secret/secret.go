// Package secret encrypts small values (repository tokens and SSH keys) for
// storage at rest with AES-256-GCM. The key comes from the environment and is
// never written anywhere by the server.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the required key length in bytes.
const KeySize = 32

// ErrOpen is returned when a value cannot be decrypted: wrong key, wrong
// associated data or a damaged value.
var ErrOpen = errors.New("secret: cannot decrypt (wrong key or damaged value)")

// ParseKey decodes a base64 key (standard or URL alphabet, with or without
// padding) and checks its length.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("secret key is empty")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil {
			if len(key) != KeySize {
				return nil, fmt.Errorf("secret key must be %d bytes, got %d (generate one with: openssl rand -base64 32)", KeySize, len(key))
			}
			return key, nil
		}
	}
	return nil, errors.New("secret key is not valid base64 (generate one with: openssl rand -base64 32)")
}

// Box seals and opens values with one key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret key must be %d bytes, got %d", KeySize, len(key))
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

// Seal encrypts plaintext. aad binds the result to its owner (for example a
// repository id) so a sealed value cannot be moved to another entry.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrOpen
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

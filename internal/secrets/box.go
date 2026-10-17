// Package secrets encrypts per-user credentials at rest.
//
// Every value is sealed with XChaCha20-Poly1305 under the instance master
// key. The owner and secret name are bound in as associated data, so a
// ciphertext copied into another user's row fails to open.
package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the length of a master key in bytes.
const KeySize = chacha20poly1305.KeySize

// Box seals and opens secrets with a single master key.
type Box struct {
	key []byte
}

// ParseKey decodes a base64 master key as produced by GenerateKey.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		key, err = base64.RawURLEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, fmt.Errorf("master key is not base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// GenerateKey returns a fresh base64 encoded master key.
func GenerateKey() string {
	key := make([]byte, KeySize)
	rand.Read(key)
	return base64.StdEncoding.EncodeToString(key)
}

// New returns a Box for key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes", KeySize)
	}
	return &Box{key: append([]byte(nil), key...)}, nil
}

func aad(owner, name string) []byte {
	return []byte(owner + "\x00" + name)
}

// Seal encrypts plaintext for owner/name. The nonce is prepended.
func (b *Box) Seal(owner, name string, plaintext []byte) []byte {
	aead, _ := chacha20poly1305.NewX(b.key)
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plaintext, aad(owner, name))
}

// ErrCorrupt is returned when a ciphertext fails authentication.
var ErrCorrupt = errors.New("secret failed to decrypt")

// Open decrypts a value produced by Seal for the same owner/name.
func (b *Box) Open(owner, name string, ciphertext []byte) ([]byte, error) {
	aead, _ := chacha20poly1305.NewX(b.key)
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrCorrupt
	}
	nonce, sealed := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, sealed, aad(owner, name))
	if err != nil {
		return nil, ErrCorrupt
	}
	return plain, nil
}

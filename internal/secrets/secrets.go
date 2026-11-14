// Package secrets seals values for storage with AES-256-GCM, under a key the
// server reads from its environment, so a copy of the database alone does
// not give them away.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvVar is the environment variable holding the key: 32 random bytes in
// standard base64, such as the output of "openssl rand -base64 32".
const EnvVar = "TRMNL_SECRET_KEY"

// ErrNoKey means a value had to be sealed or opened without a key.
var ErrNoKey = errors.New(EnvVar + " is not set")

// Key seals and opens values. A nil Key does neither and returns ErrNoKey.
type Key struct {
	aead cipher.AEAD
}

// FromEnv returns the key in EnvVar, or a nil Key if it is not set, which
// is fine until a value needs sealing or opening.
func FromEnv() (*Key, error) {
	v := strings.TrimSpace(os.Getenv(EnvVar))
	if v == "" {
		return nil, nil
	}
	k, err := Parse(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvVar, err)
	}
	return k, nil
}

// Parse returns the key encoded in s.
func Parse(s string) (*Key, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("key is %d bytes, want 32", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	return &Key{aead: aead}, nil
}

// Seal encrypts plaintext with a random nonce, which the result carries.
func (k *Key) Seal(plaintext []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrNoKey
	}
	// The AEAD makes a random nonce for each Seal and puts it in the
	// result; the nil here is the nonce argument it requires to be empty.
	return k.aead.Seal(nil, nil, plaintext, nil), nil //nolint:gosec // G407: NewGCMWithRandomNonce generates the nonce, and gosec cannot see that.
}

// Open decrypts what Seal returned. It fails if the value was sealed with
// another key or changed since.
func (k *Key) Open(sealed []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrNoKey
	}
	plain, err := k.aead.Open(nil, nil, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("open secret: %w", err)
	}
	return plain, nil
}

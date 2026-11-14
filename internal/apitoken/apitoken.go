// Package apitoken makes, removes and hashes the bearer tokens that
// authorize admin API requests. It has no HTTP code, so the command line
// can use it.
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// Errors from tokens.
var (
	// ErrExists means a token already has the name.
	ErrExists = errkind.New(errkind.ErrConflict, "a token with that name exists")
	// ErrNotFound means no token has the name.
	ErrNotFound = errkind.New(errkind.ErrNotFound, "no such token")
	// ErrName means a token's name is empty.
	ErrName = errkind.New(errkind.ErrInvalid, "token name is empty")
)

// Hash is how an API token is stored and looked up. A token is 256 random
// bits, so a fast hash is enough.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// New returns a new random token: 32 random bytes, base64url. Admin session
// tokens are made the same way.
func New() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Create makes an API token with the given name and returns it. It is shown
// once: only its hash is stored.
func Create(ctx context.Context, client *db.Client, name string, now time.Time) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrName
	}
	token, err := New()
	if err != nil {
		return "", err
	}
	err = client.InTx(ctx, func(q *db.Queries) error {
		exists, err := q.TokenNameExists(ctx, name)
		if err != nil {
			return fmt.Errorf("check token name: %w", err)
		}
		if exists != 0 {
			return fmt.Errorf("%w: %s", ErrExists, name)
		}
		if _, err := q.CreateToken(ctx, db.CreateTokenParams{Name: name, TokenHash: Hash(token), CreatedAt: now.Unix()}); err != nil {
			return fmt.Errorf("save token: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Delete removes the token with the given name.
func Delete(ctx context.Context, client *db.Client, name string) error {
	n, err := client.Write.DeleteToken(ctx, name)
	if err != nil {
		return fmt.Errorf("remove token: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return nil
}

// Package adminauth holds the admin password and the browser sessions it
// opens. It has no pages, so the command line can set the password and the
// web UI and the JSON API can both ask whether a request is signed in.
package adminauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// Errors from the password.
var (
	// ErrNoPassword means no admin password has been set.
	ErrNoPassword = errors.New("no admin password is set")
	// ErrWrongPassword means the password is not the admin password.
	ErrWrongPassword = errors.New("wrong password")
	// ErrPasswordLength means a new password is too short or too long. The
	// error that wraps it states the bounds.
	ErrPasswordLength = errkind.New(errkind.ErrInvalid, "password length is out of bounds")
	// ErrBadHash means the stored hash is not one this package can check.
	ErrBadHash = errors.New("stored password hash is not usable")
)

const (
	// settingPasswordHash is the setting that holds the PHC string.
	settingPasswordHash = "admin_password_hash"

	saltLen = 16
	keyLen  = 32

	// The most a stored hash may ask of Verify, so a corrupt or planted row
	// cannot make a login use all the memory or time. They are well over
	// what hashParams uses, which leaves room to raise it.
	maxMemoryKiB = 64 << 10
	maxPasses    = 10
	maxThreads   = 4
	minSaltLen   = 8
	maxSaltLen   = 64
	minKeyLen    = 16
	maxKeyLen    = 64
)

// argonParams are the cost settings of an argon2id hash.
type argonParams struct {
	memoryKiB uint32
	passes    uint32
	threads   uint8
}

// hashParams are the settings new hashes use, from OWASP's guidance for
// argon2id: 19 MiB, two passes, one thread. A test sets cheaper ones.
var hashParams = argonParams{memoryKiB: 19 * 1024, passes: 2, threads: 1}

// hashSlot lets one argon2 computation run at a time, so a burst of logins
// cannot multiply the memory they use. A computation holds it by filling it.
var hashSlot = make(chan struct{}, 1)

// derive runs argon2id once it has the slot. A caller whose ctx is done
// stops waiting for it and gets the context's error.
func derive(ctx context.Context, password string, salt []byte, p argonParams, n uint32) ([]byte, error) {
	select {
	case hashSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("wait to hash password: %w", ctx.Err())
	}
	defer func() { <-hashSlot }()
	return argon2.IDKey([]byte(password), salt, p.passes, p.memoryKiB, p.threads, n), nil
}

// hashPassword returns the PHC string for password, with a new salt.
func hashPassword(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("random salt: %w", err)
	}
	p := hashParams
	key, err := derive(ctx, password, salt, p, keyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memoryKiB, p.passes, p.threads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// parseHash splits a PHC string into its settings, salt and key. It refuses
// settings over the bounds above.
func parseHash(phc string) (p argonParams, salt, key []byte, err error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: not an argon2id PHC string", ErrBadHash)
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, nil, nil, fmt.Errorf("%w: version %q", ErrBadHash, parts[2])
	}
	// Sscanf ignores what follows its format, so the settings must also
	// print back as they were read, which refuses a sign, a leading zero
	// or trailing text.
	if _, serr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.passes, &p.threads); serr != nil ||
		parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", p.memoryKiB, p.passes, p.threads) {
		return p, nil, nil, fmt.Errorf("%w: settings %q", ErrBadHash, parts[3])
	}
	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(parts[4]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: salt: %w", ErrBadHash, err)
	}
	if key, err = enc.DecodeString(parts[5]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: key: %w", ErrBadHash, err)
	}
	switch {
	case p.threads < 1 || p.threads > maxThreads,
		p.passes < 1 || p.passes > maxPasses,
		p.memoryKiB < 8*uint32(p.threads) || p.memoryKiB > maxMemoryKiB,
		len(salt) < minSaltLen || len(salt) > maxSaltLen,
		len(key) < minKeyLen || len(key) > maxKeyLen:
		return p, nil, nil, fmt.Errorf("%w: settings are out of bounds", ErrBadHash)
	}
	return p, salt, key, nil
}

// verifyHash reports whether password is the one phc was made from. The
// settings come from phc, so they can change without breaking old hashes.
func verifyHash(ctx context.Context, phc, password string) (bool, error) {
	p, salt, want, err := parseHash(phc)
	if err != nil {
		return false, err
	}
	n := len(want)
	if n < minKeyLen || n > maxKeyLen {
		return false, fmt.Errorf("%w: key length", ErrBadHash)
	}
	got, err := derive(ctx, password, salt, p, uint32(n))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// SetPassword makes password the admin password and signs out every
// session, in one transaction. A password outside the length bounds is
// refused: the minimum is the min_password_length setting, read now, and it
// applies only here, so a shorter password set earlier still logs in.
func SetPassword(ctx context.Context, client *db.Client, password string, now time.Time) error {
	// Checked before hashing, which is slow and holds the hash slot.
	minLen, err := settings.MinPasswordLength(ctx, client.Read)
	if err != nil {
		return err
	}
	if n := len(password); n < minLen || n > settings.MaxPasswordLen {
		return fmt.Errorf("%w: must be %d to %d bytes", ErrPasswordLength, minLen, settings.MaxPasswordLen)
	}
	phc, err := hashPassword(ctx, password)
	if err != nil {
		return err
	}
	return client.InTx(ctx, func(q *db.Queries) error {
		err := q.SetSetting(ctx, db.SetSettingParams{Key: settingPasswordHash, Value: phc, UpdatedAt: now.Unix()})
		if err != nil {
			return fmt.Errorf("save password: %w", err)
		}
		if err := q.DeleteAllAdminSessions(ctx); err != nil {
			return fmt.Errorf("sign out sessions: %w", err)
		}
		return nil
	})
}

// CheckPassword returns nil if password is the admin password,
// ErrNoPassword if none is set, ErrWrongPassword if it differs, and an error
// wrapping ErrBadHash if the stored hash cannot be checked.
func CheckPassword(ctx context.Context, client *db.Client, password string) error {
	phc, err := client.Read.GetSetting(ctx, settingPasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoPassword
	}
	if err != nil {
		return fmt.Errorf("read password hash: %w", err)
	}
	// No password this long can have been set, so skip the work.
	if len(password) > settings.MaxPasswordLen {
		return ErrWrongPassword
	}
	ok, err := verifyHash(ctx, phc, password)
	if err != nil {
		return err
	}
	if !ok {
		return ErrWrongPassword
	}
	return nil
}

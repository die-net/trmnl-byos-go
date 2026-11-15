// Package settings holds the values an admin can change while the server
// runs: each has a key in the setting table, a default for when it has no
// row, and bounds. The other rows of that table, the join window and the
// admin password hash, belong to their own packages and are not reachable
// from here.
package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// The keys of the settings, as stored and as the command line and the API
// name them.
const (
	// KeyRefreshSeconds is how long a device sleeps between wakes.
	KeyRefreshSeconds = "refresh_seconds"
	// KeyMinPasswordLength is the fewest bytes a new admin password may have.
	KeyMinPasswordLength = "min_password_length"
)

// MaxPasswordLen is the most bytes an admin password may have. It is here,
// not in adminauth, because adminauth reads the settings and so imports
// this package, and it bounds min_password_length.
const MaxPasswordLen = 1024

// ErrUnknownKey means the key is not one of the settings.
var ErrUnknownKey = errkind.New(errkind.ErrNotFound, "no such setting")

// definition is one setting: a whole number between Min and Max.
type definition struct {
	key string
	// def is the value used while the setting has no row.
	def      int64
	min, max int64
}

// definitions lists every setting. Adding a setting is adding an entry.
var definitions = []definition{
	{key: KeyRefreshSeconds, def: 900, min: 1, max: 365 * 24 * 60 * 60},
	{key: KeyMinPasswordLength, def: 12, min: 1, max: MaxPasswordLen},
}

// lookup returns the definition for key.
func lookup(key string) (*definition, error) {
	for i := range definitions {
		if definitions[i].key == key {
			return &definitions[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownKey, key)
}

// parse returns the number in value, and whether it is a whole number in
// bounds.
func (d *definition) parse(value string) (int64, bool) {
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n >= d.min && n <= d.max
}

// rule says what a value must be, for an error message.
func (d *definition) rule() string {
	return fmt.Sprintf("%s must be a whole number from %d to %d", d.key, d.min, d.max)
}

// get returns the stored value of the setting, or its default if it has no
// row, and whether it has one. A stored value that is not valid is an error
// naming the key, and the default is returned with it, so a caller that must
// go on can.
func (d *definition) get(ctx context.Context, q *db.Queries) (n int64, set bool, err error) {
	v, err := q.GetSetting(ctx, d.key)
	if errors.Is(err, sql.ErrNoRows) {
		return d.def, false, nil
	}
	if err != nil {
		return d.def, false, fmt.Errorf("get setting %s: %w", d.key, err)
	}
	n, ok := d.parse(v)
	if !ok {
		// Not ErrInvalid: the caller sent nothing wrong, the row is bad.
		return d.def, true, fmt.Errorf("stored setting is not usable: %s, not %q", d.rule(), v)
	}
	return n, true, nil
}

// getKey returns the value of the setting named key, which must be defined.
func getKey(ctx context.Context, q *db.Queries, key string) (int64, error) {
	d, err := lookup(key)
	if err != nil {
		return 0, err
	}
	n, _, err := d.get(ctx, q)
	return n, err
}

// RefreshInterval returns how long a device sleeps between wakes. If the
// stored value is not usable it returns the default along with the error.
func RefreshInterval(ctx context.Context, q *db.Queries) (time.Duration, error) {
	n, err := getKey(ctx, q, KeyRefreshSeconds)
	return time.Duration(n) * time.Second, err
}

// MinPasswordLength returns the fewest bytes a new admin password may have.
// If the stored value is not usable it returns the default along with the
// error.
func MinPasswordLength(ctx context.Context, q *db.Queries) (int, error) {
	n, err := getKey(ctx, q, KeyMinPasswordLength)
	return int(n), err
}

// Setting is a setting and its value.
type Setting struct {
	// Key names the setting.
	Key string
	// Value is what is in effect: the stored value, or Default if Set is
	// false.
	Value int64
	// Default is the value used while the setting has no row.
	Default int64
	// Set is whether the setting has a row.
	Set bool
	// Min and Max bound the value.
	Min, Max int64
}

// List returns every setting with the value in effect, in a fixed order. A
// stored value that is not valid is an error naming its key.
func List(ctx context.Context, q *db.Queries) ([]Setting, error) {
	out := make([]Setting, 0, len(definitions))
	for i := range definitions {
		d := &definitions[i]
		v, set, err := d.get(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, Setting{Key: d.key, Value: v, Default: d.def, Set: set, Min: d.min, Max: d.max})
	}
	return out, nil
}

// Change is a new value for one setting.
type Change struct {
	Key, Value string
}

// Set stores a new value for the setting named key. An unknown key wraps
// errkind.ErrNotFound, and a value that is not a whole number in the
// setting's bounds wraps errkind.ErrInvalid.
func Set(ctx context.Context, client *db.Client, key, value string, now time.Time) error {
	return SetMany(ctx, client, []Change{{Key: key, Value: value}}, now)
}

// SetMany stores every change in one transaction, after checking all of
// them, so a bad one leaves the rest unwritten.
func SetMany(ctx context.Context, client *db.Client, changes []Change, now time.Time) error {
	values := make([]string, len(changes))
	for i, c := range changes {
		d, err := lookup(c.Key)
		if err != nil {
			return err
		}
		n, ok := d.parse(c.Value)
		if !ok {
			return fmt.Errorf("%w: %s, not %q", errkind.ErrInvalid, d.rule(), c.Value)
		}
		values[i] = strconv.FormatInt(n, 10)
	}
	return client.InTx(ctx, func(q *db.Queries) error {
		for i, c := range changes {
			err := q.SetSetting(ctx, db.SetSettingParams{Key: c.Key, Value: values[i], UpdatedAt: now.Unix()})
			if err != nil {
				return fmt.Errorf("set setting %s: %w", c.Key, err)
			}
		}
		return nil
	})
}

// Reset deletes the setting's row, so it goes back to its default. A key
// that is not a setting wraps errkind.ErrNotFound.
func Reset(ctx context.Context, client *db.Client, key string) error {
	if _, err := lookup(key); err != nil {
		return err
	}
	if err := client.Write.DeleteSetting(ctx, key); err != nil {
		return fmt.Errorf("reset setting %s: %w", key, err)
	}
	return nil
}

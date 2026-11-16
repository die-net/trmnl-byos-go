// Package settings holds the values an admin can change while the server
// runs: each has a key in the setting table, a default for when it has no
// row, and either bounds, for a whole number, or a fixed set of values. The
// other rows of that table, the join window and the
// admin password hash, belong to their own packages and are not reachable
// from here.
package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/locale"
)

// The keys of the settings, as stored and as the command line and the API
// name them.
const (
	// KeyRefreshSeconds is how long a device sleeps between wakes.
	KeyRefreshSeconds = "refresh_seconds"
	// KeyMinPasswordLength is the fewest bytes a new admin password may have.
	KeyMinPasswordLength = "min_password_length"
	// KeyLocale is the locale renders are made for, unless a device sets
	// its own.
	KeyLocale = "locale"
)

// MaxPasswordLen is the most bytes an admin password may have. It is here,
// not in adminauth, because adminauth reads the settings and so imports
// this package, and it bounds min_password_length.
const MaxPasswordLen = 1024

// ErrUnknownKey means the key is not one of the settings.
var ErrUnknownKey = errkind.New(errkind.ErrNotFound, "no such setting")

// definition is one setting: a whole number between min and max, or, when
// choices is not nil, one of the strings in it.
type definition struct {
	key string
	// def is the value used while the setting has no row, and min and max
	// bound it. They are not used when choices is set.
	def      int64
	min, max int64
	// choices are the values a choice setting may have, and defChoice is the
	// one used while it has no row.
	choices   []string
	defChoice string
}

// definitions lists every setting. Adding a setting is adding an entry.
var definitions = []definition{
	{key: KeyRefreshSeconds, def: 900, min: 1, max: 365 * 24 * 60 * 60},
	{key: KeyMinPasswordLength, def: 12, min: 1, max: MaxPasswordLen},
	{key: KeyLocale, choices: locale.Codes(), defChoice: locale.Default},
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

// defaultText returns the default as stored text.
func (d *definition) defaultText() string {
	if d.choices != nil {
		return d.defChoice
	}
	return strconv.FormatInt(d.def, 10)
}

// parse returns value as it is stored, and whether it is allowed: a whole
// number in bounds, written plainly, or one of the choices.
func (d *definition) parse(value string) (string, bool) {
	if d.choices != nil {
		return value, slices.Contains(d.choices, value)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return strconv.FormatInt(n, 10), err == nil && n >= d.min && n <= d.max
}

// rule says what a value must be, for an error message.
func (d *definition) rule() string {
	if d.choices != nil {
		return fmt.Sprintf("%s must be one of %s", d.key, strings.Join(d.choices, ", "))
	}
	return fmt.Sprintf("%s must be a whole number from %d to %d", d.key, d.min, d.max)
}

// read returns the value in effect for the setting as text, whether it has a
// row, and, if the row is not allowed, its text. A bad row gives the
// default. Only a database error is an error.
func (d *definition) read(ctx context.Context, q *db.Queries) (value string, set bool, bad *string, err error) {
	raw, err := q.GetSetting(ctx, d.key)
	if errors.Is(err, sql.ErrNoRows) {
		return d.defaultText(), false, nil, nil
	}
	if err != nil {
		return d.defaultText(), false, nil, fmt.Errorf("get setting %s: %w", d.key, err)
	}
	v, ok := d.parse(raw)
	if !ok {
		return d.defaultText(), true, &raw, nil
	}
	return v, true, nil, nil
}

// getKey returns the value in effect for the setting named key, which must
// be defined, as text. It is the one reader behind RefreshInterval,
// MinPasswordLength and Locale, and it reads a row the way List does: a
// stored value that is not usable is logged and gives the default, so what
// the server does is what List reports. Only a database error is an error,
// and the default comes back with it for a caller that must go on.
func getKey(ctx context.Context, q *db.Queries, key string) (string, error) {
	d, err := lookup(key)
	if err != nil {
		return "", err
	}
	v, _, bad, err := d.read(ctx, q)
	if bad != nil {
		slog.Warn("settings: stored value is not usable; using the default", "key", d.key, "value", *bad, "default", d.defaultText())
	}
	return v, err
}

// getNumber is getKey for a whole number setting.
func getNumber(ctx context.Context, q *db.Queries, key string) (int64, error) {
	v, err := getKey(ctx, q, key)
	// A value read is allowed, so it is a number.
	n, _ := strconv.ParseInt(v, 10, 64)
	return n, err
}

// RefreshInterval returns how long a device sleeps between wakes. A stored
// value that is not usable gives the default. If the setting cannot be read
// it returns the default along with the error.
func RefreshInterval(ctx context.Context, q *db.Queries) (time.Duration, error) {
	n, err := getNumber(ctx, q, KeyRefreshSeconds)
	return time.Duration(n) * time.Second, err
}

// MinPasswordLength returns the fewest bytes a new admin password may have.
// A stored value that is not usable gives the default. If the setting cannot
// be read it returns the default along with the error.
func MinPasswordLength(ctx context.Context, q *db.Queries) (int, error) {
	n, err := getNumber(ctx, q, KeyMinPasswordLength)
	return int(n), err
}

// Locale returns the locale renders are made for when a device sets none. A
// stored value that is not usable gives the default. If the setting cannot be
// read it returns the default along with the error.
func Locale(ctx context.Context, q *db.Queries) (string, error) {
	return getKey(ctx, q, KeyLocale)
}

// Setting is a setting and its value.
type Setting struct {
	// Key names the setting.
	Key string
	// Value is what is in effect: the stored value, or Default if Set is
	// false. It is 0 for a setting with Choices.
	Value int64
	// Default is the value used while the setting has no row. It is 0 for a
	// setting with Choices.
	Default int64
	// Text and DefaultText are Value and Default as text, which is all a
	// setting with Choices has.
	Text, DefaultText string
	// Set is whether the setting has a row.
	Set bool
	// Bad is the text of the row if it is not an allowed value, and nil
	// otherwise. It is a pointer because the text can be empty. Value is
	// then Default, and Set is true.
	Bad *string
	// Min and Max bound the value. They are 0 for a setting with Choices.
	Min, Max int64
	// Choices are the values the setting may have, or nil for a whole
	// number between Min and Max.
	Choices []string
}

// List returns every setting with the value in effect, in a fixed order. A
// stored value that is not valid is not an error: the setting has Bad set
// and the default in effect. Only a database error fails.
func List(ctx context.Context, q *db.Queries) ([]Setting, error) {
	out := make([]Setting, 0, len(definitions))
	for i := range definitions {
		d := &definitions[i]
		v, set, bad, err := d.read(ctx, q)
		if err != nil {
			return nil, err
		}
		st := Setting{Key: d.key, Text: v, DefaultText: d.defaultText(), Set: set, Bad: bad, Choices: d.choices}
		if d.choices == nil {
			// A value read is allowed, so it is a number.
			st.Value, _ = strconv.ParseInt(v, 10, 64)
			st.Default, st.Min, st.Max = d.def, d.min, d.max
		}
		out = append(out, st)
	}
	return out, nil
}

// Change is a new value for one setting.
type Change struct {
	Key, Value string
}

// Set stores a new value for the setting named key. An unknown key wraps
// errkind.ErrNotFound, and a value that is not allowed, a whole number in the
// setting's bounds or one of its choices, wraps errkind.ErrInvalid.
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
		v, ok := d.parse(c.Value)
		if !ok {
			return fmt.Errorf("%w: %s, not %q", errkind.ErrInvalid, d.rule(), c.Value)
		}
		values[i] = v
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

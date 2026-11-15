package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// InstanceParams are the settings of a new plugin instance.
type InstanceParams struct {
	RecipeID int64
	// Name defaults to the recipe's.
	Name string
	// Palette is the palette the instance asks for, or "" for the
	// device's own.
	Palette string
	// Values are the custom field values by keyname. A field whose value is
	// empty is left out. A password field's value is stored sealed with the
	// secrets key, and never shown back.
	Values map[string]Value
}

// InstanceUpdate is a change to a plugin instance. What it leaves nil or
// out stays as it is.
type InstanceUpdate struct {
	// Name is the new name, or "" for the recipe's.
	Name *string
	// Palette is the new palette, or "" for the device's own.
	Palette *string
	// Values are the custom field values to change by keyname. A key with
	// an empty value clears that field, secret or not, and a key left out
	// is kept.
	Values map[string]Value
}

// readFields returns the custom fields of the recipe with the given ID. It
// reads a file, so it is not called while holding the write lock.
func readFields(recipesDir string, recipeID int64) ([]Field, error) {
	s, err := ReadSettings(Dir(recipesDir, recipeID))
	if err != nil {
		return nil, fmt.Errorf("recipe %d: %w", recipeID, err)
	}
	return s.CustomFields, nil
}

// checkPalette returns an error that wraps errkind.ErrInvalid for a palette
// that is not "" and not one TRMNL has.
func checkPalette(palette string) error {
	if palette != "" && !profile.KnownPalette(palette) {
		return fmt.Errorf("%w: unknown palette %q", errkind.ErrInvalid, palette)
	}
	return nil
}

// CreateInstance records a new plugin instance, with the field values checked
// against the custom fields in the settings.yml of the recipe, which is
// installed under recipesDir. Password values need key. An empty Name is the
// recipe's. It returns ErrNoRecipe for an unknown recipe, secrets.ErrNoKey
// for a password value with no key, and an error wrapping errkind.ErrInvalid
// for an unknown palette. A value that does not fit its field, a missing
// required one, and a field the recipe does not have are a FieldErrors.
func CreateInstance(ctx context.Context, client *db.Client, key *secrets.Key, recipesDir string, p InstanceParams, now time.Time) (db.PluginInstance, error) {
	if err := checkPalette(p.Palette); err != nil {
		return db.PluginInstance{}, err
	}
	rec, err := client.Read.GetRecipe(ctx, p.RecipeID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PluginInstance{}, fmt.Errorf("%w: %d", ErrNoRecipe, p.RecipeID)
	}
	if err != nil {
		return db.PluginInstance{}, fmt.Errorf("get recipe: %w", err)
	}
	fields, err := readFields(recipesDir, p.RecipeID)
	if err != nil {
		return db.PluginInstance{}, err
	}
	values, err := checkValues(fields, p.Values, true)
	if err != nil {
		return db.PluginInstance{}, err
	}
	byName := fieldsByName(fields)
	plain, secret := map[string]Value{}, map[string]string{}
	setValues(byName, plain, secret, values)
	if len(secret) > 0 && key == nil {
		return db.PluginInstance{}, secrets.ErrNoKey
	}
	if err := checkEnvSize(plain, secret, 0); err != nil {
		return db.PluginInstance{}, err
	}
	if p.Name, err = CleanInstanceName(p.Name); err != nil {
		return db.PluginInstance{}, err
	}
	if p.Name == "" {
		p.Name = rec.Name
	}
	encoded, err := json.Marshal(plain)
	if err != nil {
		return db.PluginInstance{}, fmt.Errorf("encode fields: %w", err)
	}
	var inst db.PluginInstance
	err = client.InTx(ctx, func(q *db.Queries) error {
		var err error
		inst, err = q.CreateInstance(ctx, db.CreateInstanceParams{
			RecipeID:    p.RecipeID,
			Name:        p.Name,
			FieldValues: string(encoded),
			Palette:     p.Palette,
			CreatedAt:   now.Unix(),
		})
		if err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
		if len(secret) == 0 {
			return nil
		}
		// The sealed value is bound to the instance ID, so it can be made
		// only once the row has one.
		if inst.SecretValues, err = sealSecrets(key, inst.ID, secret); err != nil {
			return err
		}
		if err := q.SetInstanceSecrets(ctx, db.SetInstanceSecretsParams{SecretValues: inst.SecretValues, ID: inst.ID}); err != nil {
			return fmt.Errorf("save secrets: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.PluginInstance{}, err
	}
	return inst, nil
}

// UpdateInstance changes a plugin instance, and returns it as it is after.
// The values are checked as CreateInstance checks them, except that a
// required field is checked only when the update clears it: the ones it
// leaves out had values when the instance was made, and may not be openable
// without the key. The read, merge and write run in one transaction, and a
// refused update changes nothing.
//
// The stored secrets are opened, which needs key, only when the update
// touches a password field, and are then sealed again. Stored secrets that
// do not open with key, as after a key change, make such an update fail with
// an error wrapping errkind.ErrInvalid. Only a change to a field value or a
// secret raises the instance's settings_at, which makes its renders stale.
//
// It returns ErrNoInstance for an unknown ID, and the errors of
// CreateInstance for the rest.
func UpdateInstance(ctx context.Context, client *db.Client, key *secrets.Key, recipesDir string, id int64, u InstanceUpdate, now time.Time) (db.PluginInstance, error) {
	if u.Palette != nil {
		if err := checkPalette(*u.Palette); err != nil {
			return db.PluginInstance{}, err
		}
	}
	if u.Name != nil {
		name, err := CleanInstanceName(*u.Name)
		if err != nil {
			return db.PluginInstance{}, err
		}
		u.Name = &name
	}
	// An instance's recipe never changes, so the definitions read here are
	// the ones for the row read again inside the transaction.
	inst, err := client.Read.GetInstance(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PluginInstance{}, fmt.Errorf("%w: %d", ErrNoInstance, id)
	}
	if err != nil {
		return db.PluginInstance{}, fmt.Errorf("get instance: %w", err)
	}
	fields, err := readFields(recipesDir, inst.RecipeID)
	if err != nil {
		return db.PluginInstance{}, err
	}
	values, err := checkValues(fields, u.Values, false)
	if err != nil {
		return db.PluginInstance{}, err
	}
	byName := fieldsByName(fields)
	touchesSecret := false
	for k := range values {
		touchesSecret = touchesSecret || byName[k].Secret()
	}
	if touchesSecret && key == nil {
		return db.PluginInstance{}, secrets.ErrNoKey
	}
	var out db.PluginInstance
	err = client.InTx(ctx, func(q *db.Queries) error {
		cur, err := q.GetInstance(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoInstance, id)
		}
		if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		plain, secret, err := decodeFields(&cur, key, touchesSecret)
		if errors.Is(err, errUnreadable) {
			// The error is not shown: it comes from the sealed value.
			return fmt.Errorf("%w: the instance's stored secret fields cannot be opened with the secrets key, so they cannot be changed", errkind.ErrInvalid)
		}
		if err != nil {
			return err
		}
		oldPlain, oldSecret := maps.Clone(plain), maps.Clone(secret)
		setValues(byName, plain, secret, values)
		unopened := 0
		if !touchesSecret {
			unopened = len(cur.SecretValues)
		}
		if err := checkEnvSize(plain, secret, unopened); err != nil {
			return err
		}
		arg := db.UpdateInstanceParams{
			ID: id, Name: cur.Name, Palette: cur.Palette, SecretValues: cur.SecretValues, SettingsAt: cur.SettingsAt,
		}
		if u.Name != nil {
			arg.Name = *u.Name
			if arg.Name == "" {
				rec, err := q.GetRecipe(ctx, cur.RecipeID)
				if err != nil {
					return fmt.Errorf("get recipe: %w", err)
				}
				arg.Name = rec.Name
			}
		}
		if u.Palette != nil {
			arg.Palette = *u.Palette
		}
		plainChanged := !maps.EqualFunc(oldPlain, plain, Value.equal)
		secretChanged := !maps.Equal(oldSecret, secret)
		if secretChanged {
			if arg.SecretValues, err = sealSecrets(key, id, secret); err != nil {
				return err
			}
		}
		if plainChanged || secretChanged {
			// As the push path bumps updated_at: a change in the same second
			// as the last must still be newer than what a build read.
			arg.SettingsAt = max(now.Unix(), cur.SettingsAt+1)
		}
		encoded, err := json.Marshal(plain)
		if err != nil {
			return fmt.Errorf("encode fields: %w", err)
		}
		arg.FieldValues = string(encoded)
		if out, err = q.UpdateInstance(ctx, arg); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.PluginInstance{}, err
	}
	return out, nil
}

// setValues applies checked values to an instance's plain and secret field
// values: a password field's goes to secret, any other to plain, and an
// empty value removes the field.
func setValues(byName map[string]*Field, plain map[string]Value, secret map[string]string, values map[string]Value) {
	for k, v := range values {
		switch {
		case byName[k].Secret() && v.Empty():
			delete(secret, k)
		case byName[k].Secret():
			secret[k] = v.One
		case v.Empty():
			delete(plain, k)
		default:
			plain[k] = v
		}
	}
}

// MaxInstanceNameBytes is the longest an instance's name may be.
const MaxInstanceNameBytes = 64

// CleanInstanceName returns an instance name without the space around it.
// An empty name stays empty, which means the recipe's name. A name over
// MaxInstanceNameBytes, not valid UTF-8, or with a control character is an
// error that wraps errkind.ErrInvalid. The rules are an instance's own, so a
// change to device or playlist names does not change them.
func CleanInstanceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case len(name) > MaxInstanceNameBytes:
		return "", fmt.Errorf("%w: name is over %d bytes", errkind.ErrInvalid, MaxInstanceNameBytes)
	case !utf8.ValidString(name):
		return "", fmt.Errorf("%w: name is not valid UTF-8", errkind.ErrInvalid)
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("%w: name has a control character", errkind.ErrInvalid)
	}
	return name, nil
}

// checkEnvSize returns a FieldErrors, for the field with the most bytes, if
// the field values do not fit in a render job. They travel in its Env, built
// by envFor as the scheduler builds it, with the secret values in hand
// counted exactly. Sealed secrets that were not opened, since the key is not
// needed to change a plain field, count as their sealed length: it is at
// least the size of their JSON with no escaping, apart from the length of
// the names and of control characters, and the client refuses a job that
// still comes out too big, as a failed build.
func checkEnvSize(plain map[string]Value, secret map[string]string, sealed int) error {
	all := make(map[string]Value, len(plain)+len(secret))
	maps.Copy(all, plain)
	for k, v := range secret {
		all[k] = Value{One: v}
	}
	env, _ := envFor(all)
	size, err := render.EnvSize(env)
	if err != nil {
		return err
	}
	if size+sealed <= render.MaxEnvBytes {
		return nil
	}
	largest, most := "", -1
	for k, v := range all {
		if n := len(v.One) + len(strings.Join(v.Many, "")); n > most || (n == most && k < largest) {
			largest, most = k, n
		}
	}
	return FieldErrors{largest: fmt.Sprintf("the field values come to %d KB, over the %d KB limit", (size+sealed+1023)/1024, render.MaxEnvBytes/1024)}
}

// secretAD is the associated data an instance's secrets are sealed with, so
// a sealed value copied to another instance does not open.
func secretAD(id int64) []byte {
	return []byte("plugin_instance:" + strconv.FormatInt(id, 10))
}

// sealSecrets returns the secrets sealed for the instance with the ID, or
// nil for none, which the database keeps as NULL.
func sealSecrets(key *secrets.Key, id int64, secret map[string]string) ([]byte, error) {
	if len(secret) == 0 {
		return nil, nil
	}
	plain, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("encode secrets: %w", err)
	}
	sealed, err := key.Seal(plain, secretAD(id))
	if err != nil {
		return nil, fmt.Errorf("seal secrets: %w", err)
	}
	return sealed, nil
}

// errUnreadable marks an error from opening an instance's sealed secrets. Its
// text is the start of the message the render path shows.
var errUnreadable = errors.New("secret fields")

// decodeFields returns an instance's plain field values, and its secret
// ones when open is true, which need key. An error that wraps errUnreadable
// means the sealed secrets did not open. The secrets are a nil map, which
// reads as empty, when none are stored or open is false.
func decodeFields(inst *db.PluginInstance, key *secrets.Key, open bool) (plain map[string]Value, secret map[string]string, err error) {
	plain = map[string]Value{}
	if err := json.Unmarshal([]byte(inst.FieldValues), &plain); err != nil {
		return nil, nil, fmt.Errorf("decode fields: %w", err)
	}
	if !open || len(inst.SecretValues) == 0 {
		return plain, map[string]string{}, nil
	}
	opened, err := key.Open(inst.SecretValues, secretAD(inst.ID))
	if err != nil {
		return plain, nil, fmt.Errorf("%w: %w", errUnreadable, err)
	}
	secret = map[string]string{}
	if err := json.Unmarshal(opened, &secret); err != nil {
		return nil, nil, fmt.Errorf("decode secret fields: %w", err)
	}
	return plain, secret, nil
}

// inUseError is the error of an instance that playlist items show.
type inUseError struct{ items int }

func (e inUseError) Error() string {
	return fmt.Sprintf("instance is in %d playlist items", e.items)
}

// Unwrap returns ErrInstanceInUse, and through it errkind.ErrConflict.
func (inUseError) Unwrap() error { return ErrInstanceInUse }

// ErrInstanceInUse means an instance is in playlist items, which have to
// be removed before it can be.
var ErrInstanceInUse = errkind.New(errkind.ErrConflict, "instance is in use")

// DeleteInstance removes an instance and its renders. It refuses while a
// playlist item shows the instance, with an error that wraps
// ErrInstanceInUse and counts the items, since deleting the instance would
// silently delete them too.
func DeleteInstance(ctx context.Context, client *db.Client, id int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		items, err := q.ListInstanceItems(ctx, id)
		if err != nil {
			return fmt.Errorf("list items: %w", err)
		}
		if len(items) > 0 {
			return inUseError{items: len(items)}
		}
		n, err := q.DeleteInstance(ctx, id)
		if err != nil {
			return fmt.Errorf("delete instance: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: %d", ErrNoInstance, id)
		}
		return nil
	})
}

// InstanceFields returns an instance's field values, secret ones included,
// which need key. secretValues is the value of each secret field, for a
// caller that must keep them out of what it stores or shows.
func InstanceFields(inst *db.PluginInstance, key *secrets.Key) (fields map[string]Value, secretValues []string, err error) {
	fields, secret, err := decodeFields(inst, key, true)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range secret {
		fields[k] = Value{One: v}
	}
	return fields, slices.Collect(maps.Values(secret)), nil
}

// PublicFields returns an instance's field values that are not secret, and
// the sorted names of the secret fields that are set, without their values.
// The names are an empty, non-nil slice when none are set. Reading them
// needs key. When the sealed secrets cannot be opened, because there is no
// key, the key changed, or they were sealed before they were bound to the
// instance's ID, the names are nil, which means unknown, and the instance
// is logged by ID. One unreadable instance then does not fail a list.
func PublicFields(inst *db.PluginInstance, key *secrets.Key) (fields map[string]Value, secretNames []string, err error) {
	fields, secret, err := decodeFields(inst, key, true)
	if errors.Is(err, errUnreadable) {
		if !errors.Is(err, secrets.ErrNoKey) {
			// The error is not logged: it comes from the sealed value.
			slog.Warn("instance secrets cannot be opened", "instance", inst.ID)
		}
		return fields, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	names := slices.AppendSeq([]string{}, maps.Keys(secret))
	slices.Sort(names)
	return fields, names, nil
}

package recipe

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
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
	// empty is left out. A secret field's value is stored sealed with the
	// secrets key, and never shown back.
	Values map[string]Value
	// Secret marks fields secret by keyname, as InstanceUpdate.Secret does.
	// A password field is always secret.
	Secret map[string]bool
}

// InstanceUpdate is a change to a plugin instance. What it leaves nil or
// out stays as it is.
type InstanceUpdate struct {
	// Name is the new name, or "" for the recipe's.
	Name *string
	// Palette is the new palette, or "" for the device's own.
	Palette *string
	// Values are the custom field values to change by keyname. A key left
	// out is kept. An empty value clears a plain field. For a secret field
	// it is ignored, so the stored secret is kept: a form that leaves a
	// secret blank cannot wipe it. A secret is cleared with Clear.
	Values map[string]Value
	// Clear is the keynames of the fields to clear, secret or not. A
	// required field cannot be cleared, and a key in both Clear and Values
	// is refused.
	Clear []string
	// Secret marks a field secret by keyname when true, and unmarks it when
	// false. A field it leaves out keeps its mark. Only a field that
	// CanMarkSecret can be marked. A password field is always secret, so
	// marking one changes nothing and unmarking one is refused. Marking a
	// field moves its stored value into the sealed secrets. A sealed value
	// is never shown back, so unmarking a field that has one is refused
	// unless the same update gives it a new value or clears it.
	Secret map[string]bool
	// Seen, if not nil, is the InstanceState of the instance as the caller
	// saw it. The update is refused with ErrInstanceChanged if the instance's
	// state differs now. Nil makes the last write win, as for the API and
	// the command line.
	Seen *string
}

// ErrInstanceChanged means an instance is not what the caller saw, so its
// change was refused.
var ErrInstanceChanged = errkind.New(errkind.ErrConflict, "the instance has changed")

// InstanceState returns a digest of what a page shows of an instance: its
// name, palette, plain field values and marks. A form sends it back as
// InstanceUpdate.Seen, so a save from a page that is out of date is refused
// instead of undoing a change made since, without posting every value back.
// Sealed values are left out: a page never shows them, and reading them
// would need the secrets key, which a rename does not. The values are
// digested decoded, in a canonical encoding, so a write that does not change
// them, such as sealing the secrets again, leaves the digest alone.
//
// It is the hex SHA-256 of four parts, each framed by its length as an
// unsigned varint, so no two different states encode to the same bytes:
// the name, the palette, the plain values as JSON with sorted keys, and the
// sorted marks as a JSON array.
func InstanceState(inst *db.PluginInstance) (string, error) {
	plain, _, err := decodeFields(inst, nil, false)
	if err != nil {
		return "", err
	}
	marks, err := SecretKeys(inst)
	if err != nil {
		return "", err
	}
	// encoding/json writes a map's keys sorted, and Value encodes the same
	// way every time, so equal values give equal bytes.
	values, err := json.Marshal(plain)
	if err != nil {
		return "", fmt.Errorf("encode fields: %w", err)
	}
	marked, err := json.Marshal(marks)
	if err != nil {
		return "", fmt.Errorf("encode secret marks: %w", err)
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(inst.Name), []byte(inst.Palette), values, marked} {
		h.Write(binary.AppendUvarint(nil, uint64(len(part))))
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
// installed under recipesDir. Secret values need key. An empty Name is the
// recipe's. It returns ErrNoRecipe for an unknown recipe, secrets.ErrNoKey
// for a secret value with no key, and an error wrapping errkind.ErrInvalid
// for an unknown palette. A value that does not fit its field, a missing
// required one, a field the recipe does not have, and a mark that is refused
// are a FieldErrors.
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
	byName := fieldsByName(fields)
	bad := FieldErrors{}
	marked := mark(byName, map[string]bool{}, p.Secret, bad)
	isSealed := sealedBy(byName, marked)
	values := checkValues(fields, p.Values, true, isSealed, bad)
	if len(bad) > 0 {
		return db.PluginInstance{}, bad
	}
	// An empty value on create is a field left blank, which stores nothing.
	maps.DeleteFunc(values, func(_ string, v Value) bool { return v.Empty() })
	plain, secret, err := split(isSealed, values, nil)
	if err != nil {
		return db.PluginInstance{}, err
	}
	if len(secret) > 0 && key == nil {
		return db.PluginInstance{}, secrets.ErrNoKey
	}
	if err := checkEnvSize(plain, secret); err != nil {
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
	marks, err := encodeMarks(marked)
	if err != nil {
		return db.PluginInstance{}, err
	}
	var inst db.PluginInstance
	err = client.InTx(ctx, func(q *db.Queries) error {
		var err error
		inst, err = q.CreateInstance(ctx, db.CreateInstanceParams{
			RecipeID:    p.RecipeID,
			Name:        p.Name,
			FieldValues: string(encoded),
			SecretKeys:  marks,
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
		if inst.SecretValues, err = sealSecrets(key, &inst, secret); err != nil {
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
// leaves out had values when the instance was made. The read, merge and
// write run in one transaction, and a refused update changes nothing.
//
// A keyname lives in exactly one of the plain field values and the sealed
// secrets: the sealed ones when its field is a password field or marked
// secret, and the plain ones otherwise. Every value is put in its store on
// each change, so a mark moves a stored value into the sealed ones, and
// clearing removes it from both. A sealed value is never moved to the plain
// ones: an unmark of a field that has one needs a new value or a clear, and
// one whose field stopped being a password field is marked. The
// stored secrets cannot be told apart without opening them, so they are
// opened, which needs key, whenever the update changes any value or mark and
// the instance has sealed secrets, and are sealed again only when they
// changed. An update of only the name or palette never opens them. Stored
// secrets that do not open with key, as after a key change, make an update
// that has to open them fail with an error wrapping errkind.ErrInvalid. Only
// a change to a field's value raises the instance's settings_at, which makes
// its renders stale: moving a value between stores does not change what a
// build gets.
//
// When u.Seen is set, the instance's InstanceState is compared with it
// first, inside the transaction, which needs no key.
//
// It returns ErrNoInstance for an unknown ID, an error wrapping
// ErrInstanceChanged when the instance is not what u.Seen says,
// secrets.ErrNoKey when the secrets have to be opened or sealed and key is
// nil, and the errors of CreateInstance for the rest.
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
	byName := fieldsByName(fields)
	var out db.PluginInstance
	err = client.InTx(ctx, func(q *db.Queries) error {
		cur, err := q.GetInstance(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoInstance, id)
		}
		if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		// The marks are read here, in the transaction, since which values
		// are secret decides how the update's values are taken.
		curMarks, err := decodeMarks(&cur)
		if err != nil {
			return err
		}
		if u.Seen != nil {
			state, err := InstanceState(&cur)
			if err != nil {
				return err
			}
			if state != *u.Seen {
				return fmt.Errorf("%w: instance %d", ErrInstanceChanged, id)
			}
		}
		bad := FieldErrors{}
		marked := mark(byName, curMarks, u.Secret, bad)
		isSealed := sealedBy(byName, marked)
		// An empty value means keep for a field that was secret before the
		// update, since a form showed it blank, even when the update
		// unmarks it.
		values := checkValues(fields, changes(sealedBy(byName, curMarks), u, bad), false, isSealed, bad)
		if len(bad) > 0 {
			return bad
		}
		marksChanged := !maps.Equal(curMarks, marked)
		change := len(values) > 0 || marksChanged
		// Sealed secrets are opened to know which names are inside.
		open := change && len(cur.SecretValues) > 0
		if open && key == nil {
			return secrets.ErrNoKey
		}
		plain, secret, err := decodeFields(&cur, key, open)
		if errors.Is(err, errUnreadable) {
			// The error is not shown: it comes from the sealed value.
			return fmt.Errorf("%w: the instance's stored secret fields cannot be opened with the secrets key, so its fields cannot be changed", errkind.ErrInvalid)
		}
		if err != nil {
			return err
		}
		// A clear of a keyname the recipe does not declare is for a value
		// left from a field it dropped, so one that is not stored either is a
		// typo, refused like any unknown keyname.
		for k, v := range values {
			if !v.Empty() || byName[k] != nil {
				continue
			}
			_, inPlain := plain[k]
			if _, inSecret := secret[k]; !inPlain && !inSecret {
				bad[k] = "no such field"
			}
		}
		// A sealed value is never shown back. One whose field would now be
		// plain, because the update unmarks it, or because its field stopped
		// being a password field or left the recipe since it was sealed, is
		// either replaced by the update, with a new value or a clear, or kept
		// sealed. An unmark that would move it into the plain values, where
		// any caller could read it, is refused. Otherwise it is marked, so
		// its secrecy shows in the marks.
		for k := range secret {
			if isSealed(k) {
				continue
			}
			if on, given := u.Secret[k]; !given || on {
				marked[k] = true
				continue
			}
			if _, replaced := values[k]; !replaced {
				bad[k] = "is secret; give it a new value or clear it to make it plain"
			}
		}
		if len(bad) > 0 {
			return bad
		}
		arg := db.UpdateInstanceParams{
			ID: id, Name: cur.Name, Palette: cur.Palette, FieldValues: cur.FieldValues,
			SecretValues: cur.SecretValues, SecretKeys: cur.SecretKeys, SettingsAt: cur.SettingsAt,
		}
		if change {
			all := merged(plain, secret)
			before := maps.Clone(all)
			for k, v := range values {
				if v.Empty() {
					delete(all, k)
				} else {
					all[k] = v
				}
			}
			newPlain, newSecret, err := split(isSealed, all, secret)
			if err != nil {
				return err
			}
			if len(values) > 0 {
				if err := checkEnvSize(newPlain, newSecret); err != nil {
					return err
				}
			}
			if !maps.Equal(secret, newSecret) {
				if arg.SecretValues, err = sealSecrets(key, &cur, newSecret); err != nil {
					return err
				}
			}
			if !maps.EqualFunc(before, all, Value.equal) {
				// As the push path bumps updated_at: a change in the same
				// second as the last must still be newer than what a build
				// read.
				arg.SettingsAt = max(now.Unix(), cur.SettingsAt+1)
			}
			encoded, err := json.Marshal(newPlain)
			if err != nil {
				return fmt.Errorf("encode fields: %w", err)
			}
			arg.FieldValues = string(encoded)
			if arg.SecretKeys, err = encodeMarks(marked); err != nil {
				return err
			}
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

// changes returns the values of an update to check and apply, with each
// keyname in Clear as an empty value. A key in both is added to bad and
// left out. An empty value for a field that was secret is left out, since
// it means keep.
func changes(wasSealed func(string) bool, u InstanceUpdate, bad FieldErrors) map[string]Value {
	out := make(map[string]Value, len(u.Values)+len(u.Clear))
	for k, v := range u.Values {
		if wasSealed(k) && v.Empty() {
			continue
		}
		out[k] = v
	}
	for _, k := range u.Clear {
		if _, ok := u.Values[k]; ok {
			bad[k] = "is both set and cleared"
			delete(out, k)
			continue
		}
		out[k] = Value{}
	}
	return out
}

// mark returns an instance's marks, the keynames marked secret, after the
// changes in req: true marks a keyname and false unmarks it. A change that
// is refused is added to bad: marking a field the recipe does not have or
// one that cannot be marked, and unmarking a password field. Marking a
// password field changes nothing, since it is always secret, and is not
// stored as a mark. Unmarking a field the recipe no longer has is allowed
// when it is marked, so its mark can go along with a clear of its value.
func mark(byName map[string]*Field, cur, req map[string]bool, bad FieldErrors) map[string]bool {
	out := maps.Clone(cur)
	for k, on := range req {
		f := byName[k]
		switch {
		case f == nil && !on && cur[k]:
			delete(out, k)
		case f == nil:
			bad[k] = "no such field"
		case f.Secret():
			if !on {
				bad[k] = "is a password field, which is always secret"
			}
		case !on:
			delete(out, k)
		case !f.CanMarkSecret():
			bad[k] = "cannot be secret, since its value is not text"
		default:
			out[k] = true
		}
	}
	return out
}

// sealedBy returns whether a keyname's value is stored sealed: its field is
// a password field, or it is marked. A value whose field the recipe no
// longer has is sealed only if it is marked. UpdateInstance marks a value
// that is sealed and would not be by this rule, so it stays sealed.
func sealedBy(byName map[string]*Field, marked map[string]bool) func(string) bool {
	return func(k string) bool {
		f := byName[k]
		return marked[k] || (f != nil && f.Secret())
	}
}

// merged returns an instance's plain and secret field values in one map.
func merged(plain map[string]Value, secret map[string]string) map[string]Value {
	all := maps.Clone(plain)
	for k, v := range secret {
		all[k] = Value{One: v}
	}
	return all
}

// split puts each of an instance's field values in the store sealed says it
// belongs in. A value put in the secrets that is not in old, the secrets as
// they were, has to be long enough to be secret, and cannot be a list. A
// value that is not is a FieldErrors.
func split(sealed func(string) bool, all map[string]Value, old map[string]string) (plain map[string]Value, secret map[string]string, err error) {
	plain, secret = map[string]Value{}, map[string]string{}
	bad := FieldErrors{}
	for k, v := range all {
		switch {
		case !sealed(k):
			plain[k] = v
		case v.IsList():
			// Only a field whose value is text can be secret, so this is a
			// value set before its field's type changed.
			bad[k] = "has a list of values, which cannot be secret"
		default:
			if prev, ok := old[k]; !ok || prev != v.One {
				if msg := shortSecret(v.One); msg != "" {
					bad[k] = msg
				}
			}
			secret[k] = v.One
		}
	}
	if len(bad) > 0 {
		return nil, nil, bad
	}
	return plain, secret, nil
}

// decodeMarks returns the keynames an instance has marked secret.
func decodeMarks(inst *db.PluginInstance) (map[string]bool, error) {
	var names []string
	if err := json.Unmarshal([]byte(inst.SecretKeys), &names); err != nil {
		return nil, fmt.Errorf("decode secret marks: %w", err)
	}
	marked := make(map[string]bool, len(names))
	for _, k := range names {
		marked[k] = true
	}
	return marked, nil
}

// encodeMarks returns marks as the sorted JSON array the database keeps.
func encodeMarks(marked map[string]bool) (string, error) {
	// A non-nil slice, so no marks is [], not null.
	b, err := json.Marshal(sortedMarks(marked))
	if err != nil {
		return "", fmt.Errorf("encode secret marks: %w", err)
	}
	return string(b), nil
}

// SecretKeys returns the sorted keynames of an instance's fields that the
// admin marked secret, set or not, as an empty, non-nil slice when there are
// none. Password fields are secret without a mark, so they are not in it.
// Reading it needs no key, so a form can show the marks of any instance.
func SecretKeys(inst *db.PluginInstance) ([]string, error) {
	marked, err := decodeMarks(inst)
	if err != nil {
		return nil, err
	}
	return sortedMarks(marked), nil
}

// sortedMarks returns the marked keynames sorted, as a non-nil slice.
func sortedMarks(marked map[string]bool) []string {
	names := slices.AppendSeq(make([]string, 0, len(marked)), maps.Keys(marked))
	slices.Sort(names)
	return names
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
// by envFor as the scheduler builds it. A job over the limit is also refused
// by the client, as a failed build.
func checkEnvSize(plain map[string]Value, secret map[string]string) error {
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
	if size <= render.MaxEnvBytes {
		return nil
	}
	largest, most := "", -1
	for k, v := range all {
		if n := len(v.One) + len(strings.Join(v.Many, "")); n > most || (n == most && k < largest) {
			largest, most = k, n
		}
	}
	return FieldErrors{largest: fmt.Sprintf("the field values come to %d KB, over the %d KB limit", (size+1023)/1024, render.MaxEnvBytes/1024)}
}

// secretAD is the associated data an instance's secrets are sealed with, so
// a sealed value copied to another instance does not open. It has the
// recipe and the creation time as well as the ID, since an ID is reused once
// its instance is deleted, and the sealed value of the deleted one must not
// open under the next.
func secretAD(inst *db.PluginInstance) []byte {
	return []byte("plugin_instance:" + strconv.FormatInt(inst.ID, 10) + ":" + strconv.FormatInt(inst.RecipeID, 10) + ":" + strconv.FormatInt(inst.CreatedAt, 10))
}

// sealSecrets returns the secrets sealed for the instance, or
// nil for none, which the database keeps as NULL.
func sealSecrets(key *secrets.Key, inst *db.PluginInstance, secret map[string]string) ([]byte, error) {
	if len(secret) == 0 {
		return nil, nil
	}
	plain, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("encode secrets: %w", err)
	}
	sealed, err := key.Seal(plain, secretAD(inst))
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
	opened, err := key.Open(inst.SecretValues, secretAD(inst))
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

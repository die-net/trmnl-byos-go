package recipe

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// TreeHash returns the hex SHA-256 of the regular files under dir: for
// each, in lexical order of its slash-separated path, the path's length as
// an unsigned varint, the path, the size as one, and the contents. So two
// trees hash alike only when they hold the same files with the same bytes.
func TreeHash(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open recipe: %w", err)
	}
	defer root.Close()
	h := sha256.New()
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		h.Write(binary.AppendUvarint(nil, uint64(len(name))))
		h.Write([]byte(name))
		h.Write(binary.AppendUvarint(nil, uint64(max(info.Size(), 0))))
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("hash recipe: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Replaced is the outcome of a replace.
type Replaced struct {
	// Recipe is the recipe's row after the replace.
	Recipe db.Recipe
	// Unchanged is true when the new files are the ones installed, so
	// nothing was changed but the recorded source.
	Unchanged bool
	// Instances says what the replace did to each instance of the recipe,
	// and what the admin should look at, in ID order. An instance with
	// nothing to say is left out.
	Instances []ReplacedInstance
}

// ReplacedInstance is what a replace did to one instance's values.
type ReplacedInstance struct {
	ID   int64
	Name string
	// Sealed are the keynames whose plain value moved into the sealed
	// secrets, since their field became a password field.
	Sealed []string
	// Marked are the keynames marked secret, since their field stopped
	// being a password field or left the recipe, and a sealed value of it
	// stays sealed.
	Marked []string
	// Problems are the stored values that the new fields would refuse, a
	// required field with no value, and a value of a field the recipe no
	// longer has, by keyname. They are kept, and a build gets each one
	// that a field declares. An update that sets or clears the field
	// fixes it.
	Problems FieldErrors
}

// ReplaceRefused is the error of a replace that would leave a value where
// the instance's stores do not keep it. Instances has, for each instance ID,
// why for each keyname. Nothing is changed.
type ReplaceRefused struct {
	Instances map[int64]FieldErrors
	// noKey is whether a value needs sealing and there is no key.
	noKey bool
}

// Error lists, for each instance, the keynames whose values block the replace,
// and why.
func (e *ReplaceRefused) Error() string {
	var b strings.Builder
	b.WriteString("the recipe cannot be replaced, since instance values would be left unsealed:")
	for _, id := range slices.Sorted(maps.Keys(e.Instances)) {
		fmt.Fprintf(&b, " instance %d: %v;", id, e.Instances[id])
	}
	return strings.TrimSuffix(b.String(), ";")
}

// Unwrap returns secrets.ErrNoKey when the refusal is for want of the key,
// and errkind.ErrInvalid otherwise.
func (e *ReplaceRefused) Unwrap() error {
	if e.noKey {
		return secrets.ErrNoKey
	}
	return errkind.ErrInvalid
}

// Why a replace refuses an instance's value. A message never repeats the
// value.
const (
	sealNeedsKey    = "is now a password field, and sealing its value needs the secrets key"
	sealUnreadable  = "is now a password field, and the instance's stored secret fields do not open with the secrets key, so its value cannot be sealed with them, and the instance cannot be changed; create the instance again"
	sealList        = "is now a password field, and has a list of values, which cannot be secret; clear it first"
	problemDropped  = "is not in the recipe any more; clear it to remove it"
	problemRequired = "is required"
)

// Replace replaces the files of the recipe with the given ID with the
// recipe at src, a directory or a zip file, as Install reads it, and
// records src as its source. See ReplaceZip for what it does.
func Replace(ctx context.Context, client *db.Client, key *secrets.Key, recipesDir string, id int64, src string, now time.Time) (Replaced, error) {
	info, err := os.Stat(src)
	if err != nil {
		return Replaced{}, fmt.Errorf("recipe source: %w", err)
	}
	from := func(c *copier) error { return c.fromDir(src) }
	if !info.IsDir() {
		f, err := openFile(src)
		if err != nil {
			return Replaced{}, fmt.Errorf("recipe source: %w", err)
		}
		defer f.Close()
		if info, err = f.Stat(); err != nil {
			return Replaced{}, fmt.Errorf("recipe source: %w", err)
		}
		from = func(c *copier) error { return c.fromZip(f, info.Size()) }
	}
	return replaceFrom(ctx, client, key, recipesDir, id, from, src, now)
}

// ReplaceZip replaces the files of the recipe with the given ID with the
// recipe in the zip file r streams, as InstallZip reads it, and records
// UploadSource as its source. The recipe keeps its ID and its instances.
//
// The new files go in a new revision directory, and the recipe's revision
// is raised to it in one transaction with every change to its instances,
// so the commit is the switch, and a crash before it leaves the old files
// current and the new ones for Sweep. The new revision is above every
// revision directory there is, so no directory is removed before the
// commit. In that transaction, each instance's
// plain value whose field became a password field is sealed, which needs
// key; a keyname whose field stopped being one is marked secret when the
// instance has sealed values, so a sealed value stays sealed; and every
// instance's settings_at is raised, so its renders are built again. A
// value the new fields would refuse is kept and reported. The revision that
// was current is kept for builds that were reading it. Older ones are removed
// once they have not been current for longer than a build takes.
//
// Files that hash the same as the installed ones, as they are on disk,
// change nothing but the recorded source. It returns
// ErrNoRecipe for an unknown ID, an error that wraps errkind.ErrInvalid for
// a recipe that cannot be installed, and a *ReplaceRefused when a value
// cannot be sealed.
func ReplaceZip(ctx context.Context, client *db.Client, key *secrets.Key, recipesDir string, id int64, r io.Reader, now time.Time) (Replaced, error) {
	if err := os.MkdirAll(recipesDir, 0o750); err != nil {
		return Replaced{}, fmt.Errorf("create recipes dir: %w", err)
	}
	f, err := os.CreateTemp(recipesDir, ".upload-*.zip")
	if err != nil {
		return Replaced{}, fmt.Errorf("create upload file: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	size, err := io.Copy(f, r)
	if err != nil {
		return Replaced{}, fmt.Errorf("save upload: %w", err)
	}
	return replaceFrom(ctx, client, key, recipesDir, id, func(c *copier) error { return c.fromZip(f, size) }, UploadSource, now)
}

// replaceFrom replaces the recipe's files with the ones from reads, and
// records source as where they came from.
func replaceFrom(ctx context.Context, client *db.Client, key *secrets.Key, recipesDir string, id int64, from func(*copier) error, source string, now time.Time) (Replaced, error) {
	// An unknown recipe is refused before its files are copied.
	if _, err := client.Read.GetRecipe(ctx, id); errors.Is(err, sql.ErrNoRows) {
		return Replaced{}, fmt.Errorf("%w: %d", ErrNoRecipe, id)
	} else if err != nil {
		return Replaced{}, fmt.Errorf("get recipe: %w", err)
	}
	st, err := stage(recipesDir, from)
	if err != nil {
		return Replaced{}, err
	}
	defer st.remove()
	newFields := fieldsByName(st.settings.CustomFields)

	var out Replaced
	var newDir string
	// oldRevision is the revision that was current before the replace, and
	// oldReplacedAt and oldInstalledAt when it became current.
	var oldRevision, oldReplacedAt, oldInstalledAt int64
	err = client.InTx(ctx, func(q *db.Queries) error {
		out = Replaced{}
		cur, err := q.GetRecipe(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoRecipe, id)
		}
		if err != nil {
			return fmt.Errorf("get recipe: %w", err)
		}
		oldRevision, oldReplacedAt, oldInstalledAt = cur.Revision, cur.ReplacedAt, cur.InstalledAt
		// The old fields are read under the write lock, so they are the
		// current revision's, which no other replace can change meanwhile.
		// The file is at most MaxSettingsBytes. They only decide marks, so
		// a recipe whose files are gone or no longer read, after a restore
		// or a manual cleanup, can still be repaired by a replace. Its
		// marks then come from the sealed names, when they open.
		var oldFields map[string]*Field
		old, err := ReadSettings(FilesDir(recipesDir, &cur))
		if err == nil {
			oldFields = fieldsByName(old.CustomFields)
		} else {
			slog.Warn("replace: the recipe's current files cannot be read", "recipe", id, "err", err)
		}
		// The new files are no change only when the current ones are the
		// same, as hashed now, so files damaged or lost on disk since the
		// install are put back. A source given anew is still recorded, as a
		// path that moved, without a new revision.
		if oldFields != nil && cur.SourceHash == st.hash {
			onDisk, err := TreeHash(FilesDir(recipesDir, &cur))
			if err == nil && onDisk == st.hash {
				out = Replaced{Recipe: cur, Unchanged: true}
				if cur.Source == source {
					return nil
				}
				out.Recipe, err = q.ReplaceRecipe(ctx, db.ReplaceRecipeParams{
					Name: cur.Name, Source: source, RefreshInterval: cur.RefreshInterval, Revision: cur.Revision,
					SourceHash: cur.SourceHash, ReplacedAt: cur.ReplacedAt, ID: id,
				})
				if err != nil {
					return fmt.Errorf("save recipe: %w", err)
				}
				return nil
			}
		}
		insts, err := q.ListRecipeInstances(ctx, id)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}
		refused := &ReplaceRefused{Instances: map[int64]FieldErrors{}, noKey: key == nil}
		for i := range insts {
			inst := &insts[i]
			report, arg, bad, err := replaceInstance(inst, oldFields, newFields, key, now)
			if err != nil {
				return fmt.Errorf("instance %d: %w", inst.ID, err)
			}
			if len(bad) > 0 {
				refused.Instances[inst.ID] = bad
				continue
			}
			if _, err := q.UpdateInstance(ctx, arg); err != nil {
				return fmt.Errorf("save instance %d: %w", inst.ID, err)
			}
			if len(report.Sealed)+len(report.Marked)+len(report.Problems) > 0 {
				out.Instances = append(out.Instances, report)
			}
		}
		if len(refused.Instances) > 0 {
			// Only a refusal for want of the key is one the key fixes.
			for _, bad := range refused.Instances {
				for _, msg := range bad {
					if msg != sealNeedsKey {
						refused.noKey = false
					}
				}
			}
			return refused
		}
		name := st.settings.Name
		if name == "" {
			// The file names no recipe, and an upload's file name is not a
			// better name than the one the recipe has.
			name = cur.Name
		}
		// The new revision is above every revision directory there is, so
		// no directory is removed before the commit. One above the current
		// revision may be left by a replace that did not commit, or, after a
		// restore, be the only copy of the files, maybe the very source of
		// this replace.
		top := filepath.Dir(FilesDir(recipesDir, &cur))
		if err := os.MkdirAll(top, 0o750); err != nil {
			return fmt.Errorf("replace recipe: %w", err)
		}
		highest, err := highestRevision(top)
		if err != nil {
			return err
		}
		out.Recipe, err = q.ReplaceRecipe(ctx, db.ReplaceRecipeParams{
			Name:            name,
			Source:          source,
			RefreshInterval: int64(st.settings.Refresh() / time.Second),
			Revision:        max(cur.Revision, highest) + 1,
			SourceHash:      st.hash,
			ReplacedAt:      now.Unix(),
			ID:              id,
		})
		if err != nil {
			return fmt.Errorf("save recipe: %w", err)
		}
		newDir = FilesDir(recipesDir, &out.Recipe)
		if err := os.Rename(st.dir, newDir); err != nil {
			newDir = ""
			return fmt.Errorf("replace recipe: %w", err)
		}
		return nil
	})
	if err != nil {
		if newDir != "" {
			// The commit failed, so the revision is not current. Its
			// directory is the one this replace made, and nothing else.
			discard(recipesDir, newDir)
		}
		return Replaced{}, err
	}
	if !out.Unchanged {
		// Every instance's settings_at rose, so its renders are due.
		client.Changed()
		// A revision below the one that was current stopped being current
		// when that one became current. Once that is longer ago than any
		// build takes, nothing is reading it. The one that was current is
		// kept for builds that opened it before the commit, as is anything
		// younger, until a later replace or Sweep at the next start.
		if now.Sub(time.Unix(cmp.Or(oldReplacedAt, oldInstalledAt), 0)) > staleTempAge {
			if err := removeRevisions(filepath.Dir(newDir), func(r int64) bool { return r < oldRevision }); err != nil {
				slog.Warn("remove old recipe revisions", "recipe", id, "err", err)
			}
		}
	}
	return out, nil
}

// discard removes dir, a recipe's directory or a revision of one, by first
// renaming it to a temporary entry in recipesDir, so a removal that stops
// partway leaves only an entry Sweep removes once it is old, never a partial
// directory that looks like a recipe's files. If the rename fails, dir stays
// whole.
func discard(recipesDir, dir string) {
	tmp, err := os.MkdirTemp(recipesDir, ".install-")
	if err != nil {
		slog.Warn("discard recipe files", "dir", dir, "err", err)
		return
	}
	if err := os.Rename(dir, filepath.Join(tmp, "discarded")); err != nil {
		slog.Warn("discard recipe files", "dir", dir, "err", err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		slog.Warn("discard recipe files", "dir", tmp, "err", err)
	}
}

// highestRevision returns the highest revision a directory in top, a
// recipe's directory, is named by, or 0 if none is.
func highestRevision(top string) (int64, error) {
	entries, err := os.ReadDir(top)
	if err != nil {
		return 0, fmt.Errorf("list recipe revisions: %w", err)
	}
	var highest int64
	for _, e := range entries {
		if rev, ok := numericName(e); ok {
			highest = max(highest, rev)
		}
	}
	return highest, nil
}

// replaceInstance works out what a replace does to one instance, whose
// recipe's fields go from oldFields to newFields. It returns the report, the
// update to write, and, if a value cannot be kept in its store, why, by
// keyname, in which case nothing is to be written.
func replaceInstance(inst *db.PluginInstance, oldFields, newFields map[string]*Field, key *secrets.Key, now time.Time) (ReplacedInstance, db.UpdateInstanceParams, FieldErrors, error) {
	report := ReplacedInstance{ID: inst.ID, Name: inst.Name, Problems: FieldErrors{}}
	arg := db.UpdateInstanceParams{
		ID: inst.ID, Name: inst.Name, Palette: inst.Palette, FieldValues: inst.FieldValues,
		SecretValues: inst.SecretValues, SecretKeys: inst.SecretKeys,
		// The instance's builds read the recipe's files, which changed, so
		// its renders are due again, as after a change of its settings.
		SettingsAt: max(now.Unix(), inst.SettingsAt+1),
	}
	marks, err := decodeMarks(inst)
	if err != nil {
		return report, arg, nil, err
	}
	plain, _, err := decodeFields(inst, nil, false)
	if err != nil {
		return report, arg, nil, err
	}
	bad := FieldErrors{}

	// A plain value whose field is now a password field moves into the
	// sealed secrets, as it is stored: the 6-character minimum is for a
	// value being set.
	var toSeal []string
	for _, k := range slices.Sorted(maps.Keys(plain)) {
		if f := newFields[k]; f != nil && f.Secret() {
			switch {
			case plain[k].IsList():
				bad[k] = sealList
			case key == nil:
				bad[k] = sealNeedsKey
			default:
				toSeal = append(toSeal, k)
			}
		}
	}
	if len(bad) > 0 {
		return report, arg, bad, nil
	}
	var secret map[string]string
	if len(toSeal) > 0 {
		secret, err = openSecrets(inst, key)
		if errors.Is(err, errUnreadable) {
			for _, k := range toSeal {
				bad[k] = sealUnreadable
			}
			return report, arg, bad, nil
		}
		if err != nil {
			return report, arg, nil, err
		}
		for _, k := range toSeal {
			secret[k] = plain[k].One
			delete(plain, k)
		}
		if arg.SecretValues, err = sealSecrets(key, inst, secret); err != nil {
			return report, arg, nil, err
		}
		encoded, err := json.Marshal(plain)
		if err != nil {
			return report, arg, nil, fmt.Errorf("encode fields: %w", err)
		}
		arg.FieldValues = string(encoded)
		report.Sealed = toSeal
	}

	// A value of a field that was a password field can only be in the
	// sealed secrets, so a keyname whose field stops being one is marked
	// when the instance has any, without opening them. A mark with no
	// value under it does no harm: it is what a mark of an empty field is.
	// With the old fields unknown (oldFields is nil), the sealed names are
	// read when the key opens them. When it does not, nothing is marked: a
	// sealed value stays sealed, and the next update that opens the secrets
	// marks it.
	if len(inst.SecretValues) > 0 {
		wasSecret := func(k string) bool { return oldFields[k] != nil && oldFields[k].Secret() }
		names := slices.Sorted(maps.Keys(oldFields))
		if oldFields == nil && key != nil {
			if secret == nil {
				secret, err = openSecrets(inst, key)
				if errors.Is(err, errUnreadable) {
					secret, err = nil, nil
				}
				if err != nil {
					return report, arg, nil, err
				}
			}
			wasSecret = func(k string) bool { _, ok := secret[k]; return ok }
			names = slices.Sorted(maps.Keys(secret))
		}
		for _, k := range names {
			if wasSecret(k) && !marks[k] && (newFields[k] == nil || !newFields[k].Secret()) {
				marks[k] = true
				report.Marked = append(report.Marked, k)
			}
		}
		if len(report.Marked) > 0 {
			if arg.SecretKeys, err = encodeMarks(marks); err != nil {
				return report, arg, nil, err
			}
		}
	}

	// What the new fields would refuse is kept, and reported. Whether a
	// sealed field has a value is known only when the secrets were opened.
	isSealed := sealedBy(newFields, marks)
	for _, k := range slices.Sorted(maps.Keys(plain)) {
		f := newFields[k]
		if f == nil {
			report.Problems[k] = problemDropped
			continue
		}
		if _, msg := f.check(plain[k], false); msg != "" {
			report.Problems[k] = msg
		}
	}
	for _, k := range slices.Sorted(maps.Keys(newFields)) {
		f := newFields[k]
		if !f.Required() {
			continue
		}
		if _, ok := plain[k]; ok {
			continue
		}
		known := !isSealed(k) || secret != nil || len(inst.SecretValues) == 0
		if _, inSecret := secret[k]; known && !inSecret {
			report.Problems[k] = problemRequired
		}
	}
	return report, arg, nil, nil
}

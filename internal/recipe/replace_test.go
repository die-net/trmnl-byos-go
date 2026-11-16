package recipe

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// replaceWith replaces the fixture's recipe with one whose settings.yml is
// yml, and returns the outcome.
func replaceWith(t *testing.T, f fixture, key *secrets.Key, yml string, now time.Time) (Replaced, error) {
	t.Helper()
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": yml, "full.liquid": "y"})
	return Replace(t.Context(), f.client, key, f.recipesDir, f.recipe.ID, src, now)
}

// retyped returns weatherYML with keyname's field type from changed to to.
func retyped(t *testing.T, keyname, from, to string) string {
	t.Helper()
	old := "- keyname: " + keyname + "\n  field_type: " + from + "\n"
	if !strings.Contains(weatherYML, old) {
		t.Fatalf("weatherYML has no %s field of type %s", keyname, from)
	}
	return strings.Replace(weatherYML, old, "- keyname: "+keyname+"\n  field_type: "+to+"\n", 1)
}

// instanceRow reads an instance's row.
func instanceRow(t *testing.T, f fixture, id int64) db.PluginInstance {
	t.Helper()
	inst, err := f.client.Read.GetInstance(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// TestReplaceKeepsIDAndInstances checks that a replace keeps the recipe's ID
// and instances, records its new name, refresh interval, source, revision and
// hash, puts the files in the new revision's directory, keeps the one
// before, and raises every instance's settings_at.
func TestReplaceKeepsIDAndInstances(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	key := testKey(t, keyA)
	inst, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}}}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	yml := strings.Replace(strings.Replace(weatherYML, "name: Weather\n", "name: Weather 2\n", 1), "refresh_interval: 15\n", "refresh_interval: 30\n", 1)
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": yml, "full.liquid": "y"})
	now := time.Unix(1000, 0)
	got, err := Replace(t.Context(), f.client, key, f.recipesDir, f.recipe.ID, src, now)
	if err != nil {
		t.Fatal(err)
	}
	rec := got.Recipe
	if got.Unchanged || rec.ID != f.recipe.ID || rec.Name != "Weather 2" || rec.RefreshInterval != 30*60 || rec.Source != src ||
		rec.Revision != 2 || rec.ReplacedAt != now.Unix() || rec.SourceHash == f.recipe.SourceHash || rec.SourceHash == "" {
		t.Errorf("recipe after the replace: %+v, unchanged %v", rec, got.Unchanged)
	}
	if b, err := os.ReadFile(filepath.Join(FilesDir(f.recipesDir, &rec), "full.liquid")); err != nil || string(b) != "y" {
		t.Errorf("new revision's full.liquid: %q, %v", b, err)
	}
	if _, err := os.Stat(FilesDir(f.recipesDir, &f.recipe)); err != nil {
		t.Errorf("the revision before was not kept: %v", err)
	}
	if after := instanceRow(t, f, inst.ID); after.SettingsAt != now.Unix() || after.FieldValues != inst.FieldValues {
		t.Errorf("instance after the replace: %+v, want settings_at %d and the same values", after, now.Unix())
	}
	if len(got.Instances) != 0 {
		t.Errorf("instances reported %+v, want none", got.Instances)
	}
}

// TestReplaceUnchanged checks that files that hash the same as the
// installed ones change nothing: no revision, no settings_at.
func TestReplaceUnchanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	inst, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}}}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": weatherYML, "full.liquid": "x"})
	got, err := Replace(t.Context(), f.client, nil, f.recipesDir, f.recipe.ID, src, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	// The source is another path, as the installed one moved, so it is
	// recorded, with no new revision.
	if !got.Unchanged || got.Recipe.Revision != 1 || got.Recipe.ReplacedAt != 0 || got.Recipe.Source != src {
		t.Errorf("replace with the same files: %+v", got)
	}
	if rec, err := f.client.Read.GetRecipe(t.Context(), f.recipe.ID); err != nil || rec.Source != src || rec.Revision != 1 {
		t.Errorf("recipe after the replace: %+v, %v; want the new source at revision 1", rec, err)
	}
	if after := instanceRow(t, f, inst.ID); after.SettingsAt != inst.SettingsAt {
		t.Errorf("settings_at %d, want %d kept", after.SettingsAt, inst.SettingsAt)
	}

	// A file lost from the installed copy makes the same files a change,
	// which puts it back.
	if err := os.Remove(filepath.Join(FilesDir(f.recipesDir, &f.recipe), "full.liquid")); err != nil {
		t.Fatal(err)
	}
	got, err = Replace(t.Context(), f.client, nil, f.recipesDir, f.recipe.ID, src, time.Unix(2000, 0))
	if err != nil || got.Unchanged || got.Recipe.Revision != 2 {
		t.Fatalf("replace over a damaged copy: %+v, %v; want revision 2", got, err)
	}
	if _, err := os.Stat(filepath.Join(FilesDir(f.recipesDir, &got.Recipe), "full.liquid")); err != nil {
		t.Errorf("full.liquid after the replace: %v", err)
	}
}

// TestReplaceSeals checks that a plain value whose field became a password
// field is sealed in the replace, with the key, and that without the key the
// replace is refused, naming the instance and keyname, and changes nothing.
func TestReplaceSeals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	key := testKey(t, keyA)
	inst, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{
		RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}, "token": {One: "hunter22"}},
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	yml := retyped(t, "city", "string", "password")

	_, err = replaceWith(t, f, nil, yml, time.Unix(1000, 0))
	var refused *ReplaceRefused
	if !errors.As(err, &refused) || !errors.Is(err, secrets.ErrNoKey) ||
		!reflect.DeepEqual(refused.Instances, map[int64]FieldErrors{inst.ID: {"city": sealNeedsKey}}) {
		t.Fatalf("with no key: %v; want a refusal for want of the key", err)
	}
	if after := instanceRow(t, f, inst.ID); !reflect.DeepEqual(after, inst) {
		t.Errorf("a refused replace changed the instance: %+v", after)
	}
	if rec, err := f.client.Read.GetRecipe(t.Context(), f.recipe.ID); err != nil || rec.Revision != 1 {
		t.Errorf("a refused replace changed the recipe: %+v, %v", rec, err)
	}
	if _, err := os.Stat(Dir(f.recipesDir, f.recipe.ID, 2)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused replace left revision 2: %v", err)
	}

	got, err := replaceWith(t, f, key, yml, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []ReplacedInstance{{ID: inst.ID, Name: inst.Name, Sealed: []string{"city"}, Problems: FieldErrors{}}}
	if !reflect.DeepEqual(got.Instances, want) {
		t.Errorf("report %+v, want %+v", got.Instances, want)
	}
	after := instanceRow(t, f, inst.ID)
	if after.FieldValues != `{}` {
		t.Errorf("plain values %s, want city gone from them", after.FieldValues)
	}
	fields, secretValues, err := InstanceFields(&after, key)
	if err != nil || fields["city"].One != "Oslo" || fields["token"].One != "hunter22" || !slices.Contains(secretValues, "Oslo") {
		t.Errorf("fields %+v, secrets %v, %v; want city and token, both sealed", fields, secretValues, err)
	}
}

// TestReplaceSealRefusals checks a plain value that cannot be sealed: a list,
// which a sealed value cannot be, and one of an instance whose stored
// secrets do not open with the key. Each refuses the replace, as invalid
// input, and changes nothing.
func TestReplaceSealRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		values       map[string]Value
		from, to     string
		keyname, why string
	}{
		{"a list", map[string]Value{"city": {One: "Oslo"}, "tags": {Many: []string{"a"}}}, "select", "password", "tags", sealList},
		{"secrets that do not open", map[string]Value{"city": {One: "Oslo"}, "token": {One: "hunter22"}}, "string", "password", "city", sealUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			inst, err := CreateInstance(t.Context(), f.client, testKey(t, keyA), f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: tc.values}, time.Unix(1, 0))
			if err != nil {
				t.Fatal(err)
			}
			_, err = replaceWith(t, f, testKey(t, keyB), retyped(t, tc.keyname, tc.from, tc.to), time.Unix(1000, 0))
			var refused *ReplaceRefused
			if !errors.As(err, &refused) || !errors.Is(err, errkind.ErrInvalid) || errors.Is(err, secrets.ErrNoKey) ||
				refused.Instances[inst.ID][tc.keyname] != tc.why {
				t.Fatalf("%v; want a refusal of %s: %s", err, tc.keyname, tc.why)
			}
			if after := instanceRow(t, f, inst.ID); !reflect.DeepEqual(after, inst) {
				t.Errorf("a refused replace changed the instance: %+v", after)
			}
		})
	}
}

// TestReplaceSealUnreadableWayOut checks the advice of the refusal for an
// instance whose secrets do not open: a clear of the value cannot get past
// them, and deleting the instance, to create it again, lets the replace
// through.
func TestReplaceSealUnreadableWayOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	inst, err := CreateInstance(t.Context(), f.client, testKey(t, keyA), f.recipesDir, InstanceParams{
		RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}, "token": {One: "hunter22"}},
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	keyB := testKey(t, keyB)
	yml := retyped(t, "city", "string", "password")
	if _, err := replaceWith(t, f, keyB, yml, time.Unix(1000, 0)); err == nil {
		t.Fatal("replaced with secrets that do not open")
	}
	// city is required in weatherYML, so a clear of it is refused before the
	// secrets are read. A clear of the optional days has to open them, as
	// any clear does while secrets are stored, to know whether it is there.
	_, err = UpdateInstance(t.Context(), f.client, keyB, f.recipesDir, inst.ID, InstanceUpdate{Clear: []string{"days"}}, time.Unix(1000, 0))
	if !errors.Is(err, errkind.ErrInvalid) || !strings.Contains(err.Error(), "cannot be changed") {
		t.Fatalf("a change of the instance: %v; want it refused, as the message says", err)
	}
	if err := DeleteInstance(t.Context(), f.client, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := replaceWith(t, f, keyB, yml, time.Unix(1000, 0)); err != nil {
		t.Errorf("replace after the instance was deleted: %v", err)
	}
}

// TestReplaceMarks checks that a keyname whose field stops being a password
// field, or leaves the recipe, is marked when the instance has sealed
// values, without the key, so its sealed value stays sealed and an edit of
// another field does not show it, and that an instance with no sealed
// values gets no mark.
func TestReplaceMarks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	key := testKey(t, keyA)
	sealed, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{
		RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}, "token": {One: "hunter22"}},
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Bergen"}}}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	// token becomes text, and other leaves the recipe.
	yml := retyped(t, "token", "password", "string")
	yml = strings.Replace(yml, "- keyname: other\n  field_type: password\n  name: Other\n  optional: true\n", "", 1)
	got, err := replaceWith(t, f, nil, yml, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	// other left the recipe, and without the key whether it holds a value is
	// not known, so it is reported as one that may.
	want := []ReplacedInstance{{ID: sealed.ID, Name: sealed.Name, Marked: []string{"other", "token"}, Problems: FieldErrors{"other": problemMaybeDropped}}}
	if !reflect.DeepEqual(got.Instances, want) {
		t.Errorf("report %+v, want %+v", got.Instances, want)
	}
	after := instanceRow(t, f, sealed.ID)
	if marks, err := SecretKeys(&after); err != nil || !slices.Equal(marks, []string{"other", "token"}) {
		t.Errorf("marks %v, %v", marks, err)
	}
	if fields, _, err := InstanceFields(&after, key); err != nil || fields["token"].One != "hunter22" {
		t.Errorf("fields %+v, %v; want token still there, sealed", fields, err)
	}
	if after := instanceRow(t, f, plain.ID); after.SecretKeys != plain.SecretKeys {
		t.Errorf("an instance with no secrets got marks %s", after.SecretKeys)
	}
}

// TestReplaceDroppedSealed checks that a sealed value of a field the recipe
// no longer has is reported as dropped at every replace with the key, as a
// plain one is, and not only by the replace that marks it; that a dropped
// field with no value is not reported, since clearing it would be refused;
// and that a replace without the key, or with another, which cannot tell,
// reports each dropped marked keyname as one that may hold a value.
func TestReplaceDroppedSealed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	key := testKey(t, keyA)
	inst, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{
		RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}, "token": {One: "hunter22"}},
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	// token and other leave the recipe. token has a sealed value, and other
	// none.
	noSecrets := strings.NewReplacer(
		"- keyname: token\n  field_type: password\n  name: Token\n  optional: true\n", "",
		"- keyname: other\n  field_type: password\n  name: Other\n  optional: true\n", "",
	).Replace(weatherYML)
	maybe := map[string]string{"other": problemMaybeDropped, "token": problemMaybeDropped}
	for i, tc := range []struct {
		name   string
		key    *secrets.Key
		marked []string
		// problems are the instance's problems of token and other.
		problems map[string]string
	}{
		{"the replace that marks it", key, []string{"other", "token"}, map[string]string{"token": problemDropped}},
		{"a later replace", key, nil, map[string]string{"token": problemDropped}},
		{"a later replace without the key", nil, nil, maybe},
		{"a later replace with another key", testKey(t, keyB), nil, maybe},
	} {
		use := tc.key
		yml := strings.Replace(noSecrets, "refresh_interval: 15\n", "refresh_interval: "+string(rune('6'+i))+"0\n", 1)
		got, err := replaceWith(t, f, use, yml, time.Unix(int64(1000*(i+1)), 0))
		if err != nil {
			t.Fatal(err)
		}
		var r ReplacedInstance
		for _, ri := range got.Instances {
			if ri.ID == inst.ID {
				r = ri
			}
		}
		problems := map[string]string{}
		for _, k := range []string{"token", "other"} {
			if msg, ok := r.Problems[k]; ok {
				problems[k] = msg
			}
		}
		if !slices.Equal(r.Marked, tc.marked) || !maps.Equal(problems, tc.problems) {
			t.Errorf("%s: %+v; want marked %v and problems %v", tc.name, r, tc.marked, tc.problems)
		}
	}
}

// TestReplaceProblems checks that stored values the new fields would refuse
// are kept and reported: a choice no longer offered, a number past the new
// bound, a value of a dropped field, and a new required field with no value.
func TestReplaceProblems(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	inst, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, InstanceParams{
		RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}, "units": {One: "imperial"}, "days": {One: "9"}},
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	yml := strings.NewReplacer(
		"options: [Metric, Imperial]", "options: [Metric]",
		"  max: 10\n", "  max: 5\n",
		"- keyname: city\n  field_type: string\n  name: City\n  maxlength: 20\n", "- keyname: town\n  field_type: string\n  name: Town\n",
	).Replace(weatherYML)
	got, err := replaceWith(t, f, nil, yml, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Instances) != 1 {
		t.Fatalf("report %+v, want one instance", got.Instances)
	}
	want := FieldErrors{"units": "is not one of the options", "days": "must be at most 5", "city": problemDropped, "town": problemRequired}
	if !maps.Equal(got.Instances[0].Problems, want) {
		t.Errorf("problems %v, want %v", got.Instances[0].Problems, want)
	}
	if after := instanceRow(t, f, inst.ID); after.FieldValues != inst.FieldValues {
		t.Errorf("values %s, want %s kept", after.FieldValues, inst.FieldValues)
	}
}

// TestReplaceRevisions checks that a replace takes a revision above every
// revision directory, so it removes none before its commit, keeps the
// revision that was current for builds reading it, removes older ones only
// once they have not been current for over an hour, and that Sweep removes
// revisions below the current one and keeps those above it.
func TestReplaceRevisions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	revisions := func() []string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Dir(FilesDir(f.recipesDir, &f.recipe)))
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	// Left by a replace that did not commit, or the only copy of the files
	// after a restore.
	writeDir(t, Dir(f.recipesDir, f.recipe.ID, 2), map[string]string{"keep": "x"})
	// Two replaces 30 seconds apart remove nothing, since a build may still
	// be reading the revision the first one made old. One two hours later
	// removes the revisions below the one that was current.
	for i, tc := range []struct {
		at   time.Time
		want []string
	}{
		{time.Unix(1000, 0), []string{"1", "2", "3"}},
		{time.Unix(1030, 0), []string{"1", "2", "3", "4"}},
		{time.Unix(1030, 0).Add(2 * time.Hour), []string{"4", "5"}},
	} {
		want := tc.want
		yml := strings.Replace(weatherYML, "refresh_interval: 15\n", "refresh_interval: "+string(rune('6'+i))+"0\n", 1)
		got, err := replaceWith(t, f, nil, yml, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		if names := revisions(); !slices.Equal(names, want) {
			t.Errorf("replace %d, to revision %d: revisions %v, want %v", i+1, got.Recipe.Revision, names, want)
		}
		if i == 0 {
			if b, err := os.ReadFile(filepath.Join(Dir(f.recipesDir, f.recipe.ID, 2), "keep")); err != nil || string(b) != "x" {
				t.Errorf("the replace touched revision 2: %q, %v", b, err)
			}
		}
	}
	writeDir(t, Dir(f.recipesDir, f.recipe.ID, 9), map[string]string{"settings.yml": weatherYML})
	if err := Sweep(t.Context(), f.client, f.recipesDir, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := revisions(); !slices.Equal(got, []string{"5", "9"}) {
		t.Errorf("revisions %v after a sweep, want 5, the current, and 9, above it", got)
	}
}

// TestDiscard checks that discarding a revision removes it and leaves no
// temporary entry behind.
func TestDiscard(t *testing.T) {
	t.Parallel()
	recipes := t.TempDir()
	dir := Dir(recipes, 1, 2)
	writeDir(t, dir, map[string]string{"settings.yml": "a", "views/full.liquid": "b"})
	discard(recipes, dir)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("revision after discard: %v", err)
	}
	entries, err := os.ReadDir(recipes)
	if err != nil || len(entries) != 1 || entries[0].Name() != "1" {
		t.Errorf("recipes dir %v, %v; want only the recipe's directory", entries, err)
	}
}

// TestReplaceFromHigherRevision checks the way back after a restore: the
// recipe's directory is gone but for a revision above the row's, which is
// the source of the replace, and it is still there after the replace.
func TestReplaceFromHigherRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	top := filepath.Dir(FilesDir(f.recipesDir, &f.recipe))
	if err := os.RemoveAll(FilesDir(f.recipesDir, &f.recipe)); err != nil {
		t.Fatal(err)
	}
	only := Dir(f.recipesDir, f.recipe.ID, 2)
	writeDir(t, only, map[string]string{"settings.yml": strings.Replace(weatherYML, "name: Weather\n", "name: Weather 2\n", 1), "full.liquid": "y"})
	got, err := Replace(t.Context(), f.client, nil, f.recipesDir, f.recipe.ID, only, time.Unix(1000, 0))
	if err != nil || got.Recipe.Revision != 3 || got.Recipe.Name != "Weather 2" {
		t.Fatalf("replace from revision 2: %+v, %v; want revision 3", got, err)
	}
	if _, err := os.Stat(filepath.Join(only, "settings.yml")); err != nil {
		t.Errorf("the source revision went: %v", err)
	}

	// With the recipe's whole directory gone, a replace makes it again.
	if err := os.RemoveAll(top); err != nil {
		t.Fatal(err)
	}
	if got, err := replaceWith(t, f, nil, weatherYML, time.Unix(2000, 0)); err != nil || got.Recipe.Revision != 4 {
		t.Errorf("replace with the recipe's directory gone: %+v, %v; want revision 4", got, err)
	}
}

// TestReplaceMissingFiles checks that a recipe whose current files are gone,
// as after a restore or a manual cleanup, is repaired by a replace, even
// with the files it had, which are no change only while they are in place.
func TestReplaceMissingFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := os.RemoveAll(FilesDir(f.recipesDir, &f.recipe)); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": weatherYML, "full.liquid": "x"})
	got, err := Replace(t.Context(), f.client, nil, f.recipesDir, f.recipe.ID, src, time.Unix(1000, 0))
	if err != nil || got.Unchanged || got.Recipe.Revision != 2 {
		t.Fatalf("replace with the files it had: %+v, %v; want revision 2", got, err)
	}
	if _, err := os.Stat(filepath.Join(FilesDir(f.recipesDir, &got.Recipe), "settings.yml")); err != nil {
		t.Errorf("files after the replace: %v", err)
	}
}

// TestReplaceMissingFilesMarks checks the marks of a replace whose old
// fields cannot be read: a sealed name whose new field is not a password
// field is marked when the key opens the secrets, and nothing is marked
// without a key, which a later update that opens them does.
func TestReplaceMissingFilesMarks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		key    bool
		marked []string
	}{
		{"with the key", true, []string{"token"}},
		{"without a key", false, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			key := testKey(t, keyA)
			inst, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{
				RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}, "token": {One: "hunter22"}},
			}, time.Unix(1, 0))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(FilesDir(f.recipesDir, &f.recipe)); err != nil {
				t.Fatal(err)
			}
			var use *secrets.Key
			if tc.key {
				use = key
			}
			if _, err := replaceWith(t, f, use, retyped(t, "token", "password", "string"), time.Unix(1000, 0)); err != nil {
				t.Fatal(err)
			}
			after := instanceRow(t, f, inst.ID)
			marks, err := SecretKeys(&after)
			if err != nil || !slices.Equal(marks, tc.marked) {
				t.Errorf("marks %v, %v; want %v", marks, err, tc.marked)
			}
			if fields, _, err := InstanceFields(&after, key); err != nil || fields["token"].One != "hunter22" {
				t.Errorf("fields %+v, %v; want token still sealed", fields, err)
			}
		})
	}
}

// TestReplaceUnknownRecipe checks that a replace of an unknown recipe is
// refused as not found and leaves no files.
func TestReplaceUnknownRecipe(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": weatherYML, "full.liquid": "y"})
	if _, err := Replace(t.Context(), f.client, nil, f.recipesDir, f.recipe.ID+1, src, time.Now()); !errors.Is(err, ErrNoRecipe) {
		t.Errorf("%v, want ErrNoRecipe", err)
	}
	entries, err := os.ReadDir(f.recipesDir)
	if err != nil || len(entries) != 1 {
		t.Errorf("recipes dir %v, %v; want only the installed recipe", entries, err)
	}
}

// TestInstanceAfterReplace checks the instance calls against a replaced
// recipe: a create from a form drawn from the old revision is refused as a
// conflict, one with no revision or the current one is checked against the
// new fields, and an update from a page drawn before the replace is refused
// as out of date.
func TestInstanceAfterReplace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	inst, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Oslo"}}}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	seen, err := InstanceState(&inst, f.recipe.Revision)
	if err != nil {
		t.Fatal(err)
	}
	yml := strings.Replace(weatherYML, "- keyname: city\n  field_type: string\n  name: City\n  maxlength: 20\n", "- keyname: city\n  field_type: string\n  name: City\n  maxlength: 20\n- keyname: zip\n  field_type: string\n  optional: true\n", 1)
	if _, err := replaceWith(t, f, nil, yml, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}

	stale := InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Bergen"}}, Revision: 1}
	if _, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, stale, time.Unix(2000, 0)); !errors.Is(err, ErrRecipeChanged) || !errors.Is(err, errkind.ErrConflict) {
		t.Errorf("create from revision 1: %v, want ErrRecipeChanged", err)
	}
	for _, rev := range []int64{0, 2} {
		p := InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Bergen"}, "zip": {One: "5003"}}, Revision: rev}
		if _, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, p, time.Unix(2000, 0)); err != nil {
			t.Errorf("create with revision %d and the new field: %v", rev, err)
		}
	}
	name := "Work"
	_, err = UpdateInstance(t.Context(), f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Name: &name, Seen: &seen}, time.Unix(2000, 0))
	if !errors.Is(err, ErrInstanceChanged) {
		t.Errorf("update from a page drawn before the replace: %v, want ErrInstanceChanged", err)
	}
}

// TestRetryMoved checks that a call that raced a replace is made once more,
// and that two races in a row are a conflict.
func TestRetryMoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fails int
		calls int
		want  error
	}{
		{"no race", 0, 1, nil},
		{"one race", 1, 2, nil},
		{"two races", 2, 2, ErrRecipeChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			_, err := retryMoved(func() (int, error) {
				calls++
				if calls <= tc.fails {
					return 0, errRecipeMoved
				}
				return 1, nil
			})
			if calls != tc.calls || !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Errorf("%d calls, %v; want %d and %v", calls, err, tc.calls, tc.want)
			}
		})
	}
}

// TestTreeHash checks that the hash is the same for the same files and
// changes with a file's contents or name.
func TestTreeHash(t *testing.T) {
	t.Parallel()
	hash := func(files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		writeDir(t, dir, files)
		h, err := TreeHash(dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash(map[string]string{"settings.yml": "a", "views/full.liquid": "b"})
	for _, tc := range []struct {
		name  string
		files map[string]string
		same  bool
	}{
		{"the same files", map[string]string{"settings.yml": "a", "views/full.liquid": "b"}, true},
		{"other contents", map[string]string{"settings.yml": "a", "views/full.liquid": "c"}, false},
		{"another name", map[string]string{"settings.yml": "a", "views/half.liquid": "b"}, false},
		{"bytes moved between files", map[string]string{"settings.yml": "ab", "views/full.liquid": ""}, false},
	} {
		if got := hash(tc.files); (got == base) != tc.same {
			t.Errorf("%s: same hash %v, want %v", tc.name, got == base, tc.same)
		}
	}
}

// TestReplaceRefusedMessage checks that the refusal's message names each
// instance and keyname without a value.
func TestReplaceRefusedMessage(t *testing.T) {
	t.Parallel()
	err := &ReplaceRefused{Instances: map[int64]FieldErrors{3: {"city": sealNeedsKey}, 1: {"tags": sealList}}, noKey: false}
	msg := err.Error()
	if i, j := strings.Index(msg, "instance 1"), strings.Index(msg, "instance 3"); i < 0 || j < i || !strings.Contains(msg, "city") || !strings.Contains(msg, "tags") {
		t.Errorf("message %q", msg)
	}
	if b, jerr := json.Marshal(err.Instances); jerr != nil || !strings.Contains(string(b), `"3"`) {
		t.Errorf("instances as JSON %s, %v", b, jerr)
	}
}

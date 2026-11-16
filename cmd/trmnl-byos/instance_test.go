package main

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// instanceYML is a recipe with a required string, a multi-select, a number,
// and a password.
const instanceYML = `name: Weather
custom_fields:
- keyname: city
  field_type: string
- keyname: tags
  field_type: select
  multiple: true
  optional: true
  options: [a, b, c]
- keyname: days
  field_type: number
  optional: true
- keyname: token
  field_type: password
  optional: true
`

// runInstance runs `instance args...` against the data directory and
// returns its output.
func runInstance(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := instances(t.Context(), append([]string{args[0], "-data", dir}, args[1:]...), &out)
	return out.String(), err
}

// instanceDir returns a data directory with the recipe of instanceYML
// installed as recipe 1.
func instanceDir(t *testing.T) string {
	t.Helper()
	dir, src := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"settings.yml": instanceYML, "full.liquid": "x"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := recipes(t.Context(), []string{"add", "-data", dir, src}, &out); err != nil {
		t.Fatal(err)
	}
	return dir
}

// storedInstance returns instance 1 and its plain field values.
func storedInstance(t *testing.T, dir string) (inst db.PluginInstance, fields map[string]recipe.Value) {
	t.Helper()
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if inst, err = client.Read.GetInstance(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if fields, _, err = recipe.PublicFields(&inst, nil); err != nil {
		t.Fatal(err)
	}
	return inst, fields
}

// storedFields returns the name, palette, plain field values and settings_at
// of instance 1.
func storedFields(t *testing.T, dir string) (name, palette string, fields map[string]recipe.Value, settingsAt int64) {
	t.Helper()
	inst, fields := storedInstance(t, dir)
	return inst.Name, inst.Palette, fields, inst.SettingsAt
}

// storedValues returns the plain field values of instance 1.
func storedValues(t *testing.T, dir string) map[string]recipe.Value {
	t.Helper()
	_, fields := storedInstance(t, dir)
	return fields
}

func TestInstanceAddAndSet(t *testing.T) {
	t.Parallel()
	dir := instanceDir(t)
	out, err := runInstance(t, dir, "add", "-recipe", "1", "-name", "Home", "-field", "city=Zurich", "-field", "tags=a", "-field", "tags=c")
	if err != nil || !strings.Contains(out, `Created instance 1, "Home"`) {
		t.Fatalf("add: %q, %v", out, err)
	}
	name, _, fields, at := storedFields(t, dir)
	want := map[string]recipe.Value{"city": {One: "Zurich"}, "tags": {Many: []string{"a", "c"}}}
	if name != "Home" || !valuesEqual(fields, want) || at != 0 {
		t.Fatalf("after add: %q, %v, settings_at %d", name, fields, at)
	}

	for _, tc := range []struct {
		name     string
		args     []string
		wantName string
		palette  string
		want     map[string]recipe.Value
		bumped   bool
	}{
		{"change a field, the ID first", []string{"1", "-field", "city=Bern"}, "Home", "", map[string]recipe.Value{"city": {One: "Bern"}, "tags": {Many: []string{"a", "c"}}}, true},
		{"the ID last, a list replaced", []string{"-field", "tags=b", "-field", "tags=c", "1"}, "Home", "", map[string]recipe.Value{"city": {One: "Bern"}, "tags": {Many: []string{"b", "c"}}}, true},
		{"a single choice for a multi-select", []string{"1", "-field", "tags=a"}, "Home", "", map[string]recipe.Value{"city": {One: "Bern"}, "tags": {Many: []string{"a"}}}, true},
		{"clear a field", []string{"1", "-clear", "tags"}, "Home", "", map[string]recipe.Value{"city": {One: "Bern"}}, true},
		{"set a number field", []string{"1", "-field", "days=3"}, "Home", "", map[string]recipe.Value{"city": {One: "Bern"}, "days": {One: "3"}}, true},
		{"rename and palette are not settings", []string{"1", "-name", "Away", "-palette", "bw"}, "Away", "bw", map[string]recipe.Value{"city": {One: "Bern"}, "days": {One: "3"}}, false},
		{"an empty palette is the device's", []string{"1", "-palette", ""}, "Away", "", map[string]recipe.Value{"city": {One: "Bern"}, "days": {One: "3"}}, false},
		{"an empty name is the recipe's", []string{"1", "-name", ""}, "Weather", "", map[string]recipe.Value{"city": {One: "Bern"}, "days": {One: "3"}}, false},
	} {
		current, _ := storedInstance(t, dir)
		before := current.SettingsAt
		out, err := runInstance(t, dir, append([]string{"set"}, tc.args...)...)
		if err != nil || !strings.Contains(out, "Updated instance 1") {
			t.Errorf("%s: %q, %v", tc.name, out, err)
			continue
		}
		name, palette, fields, at := storedFields(t, dir)
		if name != tc.wantName || palette != tc.palette || !valuesEqual(fields, tc.want) || (at > before) != tc.bumped {
			t.Errorf("%s: %q, %q, %v, settings_at %d after %d", tc.name, name, palette, fields, at, before)
		}
	}
}

func valuesEqual(a, b map[string]recipe.Value) bool {
	return maps.EqualFunc(a, b, func(x, y recipe.Value) bool {
		return x.One == y.One && x.IsList() == y.IsList() && strings.Join(x.Many, "\x00") == strings.Join(y.Many, "\x00")
	})
}

func TestInstanceRefused(t *testing.T) {
	t.Parallel()
	dir := instanceDir(t)
	if _, err := runInstance(t, dir, "add", "-recipe", "1", "-field", "city=Zurich"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"add with a missing required field", []string{"add", "-recipe", "1"}, "city: is required"},
		{"add with an unknown field", []string{"add", "-recipe", "1", "-field", "city=x", "-field", "nope=x"}, "nope: no such field"},
		{"add with a bad field value", []string{"add", "-recipe", "1", "-field", "city=x", "-field", "days=many"}, "days: must be a number"},
		{"set with nothing to set", []string{"set", "1"}, "usage"},
		{"set with no ID", []string{"set", "-name", "x"}, "usage"},
		{"set an unknown instance", []string{"set", "9", "-name", "x"}, "no such instance"},
		{"set and clear one field", []string{"set", "1", "-field", "days=3", "-clear", "days"}, "both set and cleared"},
		{"clear a required field", []string{"set", "1", "-clear", "city"}, "city: is required"},
		{"set a bad value", []string{"set", "1", "-field", "days=many"}, "days: must be a number"},
		{"set an unknown palette", []string{"set", "1", "-palette", "neon"}, "unknown palette"},
	} {
		_, err := runInstance(t, dir, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one with %q", tc.name, err, tc.want)
		}
	}
	name, _, fields, at := storedFields(t, dir)
	if name != "Weather" || !valuesEqual(fields, map[string]recipe.Value{"city": {One: "Zurich"}}) || at != 0 {
		t.Errorf("refused commands changed the instance: %q, %v, %d", name, fields, at)
	}
}

// TestInstanceRemoveInUse checks that removing an instance a playlist shows
// is refused with a hint for the command line, and with no item IDs.
func TestInstanceRemoveInUse(t *testing.T) {
	t.Parallel()
	dir := playlistFixture(t)
	_, err := runInstance(t, dir, "remove", "1")
	if !errors.Is(err, errkind.ErrConflict) || !strings.Contains(err.Error(), "instance is in 3 playlist items; see playlist list") {
		t.Errorf("err = %v", err)
	}
}

// TestInstanceSecret checks that a password field goes through the secrets
// key of the environment, that a change to another field needs no key, and
// that a change to the secret does. It sets the environment, so it does not
// run in parallel.
func TestInstanceSecret(t *testing.T) {
	dir := instanceDir(t)
	if _, err := runInstance(t, dir, "add", "-recipe", "1", "-field", "city=Zurich", "-field", "token=hunter2"); !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("a secret with no key: %v", err)
	}
	t.Setenv(secrets.EnvVar, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if _, err := runInstance(t, dir, "add", "-recipe", "1", "-field", "city=Zurich", "-field", "token=hunter2"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secrets.EnvVar, "")
	if _, err := runInstance(t, dir, "set", "1", "-name", "Home"); err != nil {
		t.Errorf("a rename with no key: %v", err)
	}
	// The sealed secrets are opened to change any field.
	if _, err := runInstance(t, dir, "set", "1", "-field", "city=Bern"); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("a plain change with no key: %v", err)
	}
	if _, err := runInstance(t, dir, "set", "1", "-clear", "token"); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("a secret change with no key: %v", err)
	}
	t.Setenv(secrets.EnvVar, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if _, err := runInstance(t, dir, "set", "1", "-clear", "token"); err != nil {
		t.Errorf("clearing the secret: %v", err)
	}
	out, err := runInstance(t, dir, "list")
	if err != nil || strings.Contains(out, "hunter2") {
		t.Errorf("list: %q, %v", out, err)
	}
}

// TestInstanceFieldFile checks that -field-file reads a value from a file or
// from stdin, drops one trailing LF or CR LF and no more, repeats into a list,
// and refuses a missing file, a second stdin, and a file that is too big.
func TestInstanceFieldFile(t *testing.T) {
	t.Parallel()
	dir := instanceDir(t)
	files := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(files, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	city := write("city", "Zurich\n")
	tag := write("tag", "b")
	var out bytes.Buffer
	// Only one trailing newline is dropped, so a second one stays, and a
	// one-line field refuses it.
	twoLines := write("two", "Zurich\n\n")
	err := instanceAdd(t.Context(), []string{"-data", dir, "-recipe", "1", "-field-file", "city=" + twoLines}, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "city: must be one line") {
		t.Errorf("two trailing newlines: %v", err)
	}
	err = instanceAdd(t.Context(), []string{"-data", dir, "-recipe", "1", "-field-file", "city=" + city, "-field", "tags=a", "-field-file", "tags=" + tag}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatal(err)
	}
	fields := storedValues(t, dir)
	want := map[string]recipe.Value{"city": {One: "Zurich"}, "tags": {Many: []string{"a", "b"}}}
	if !valuesEqual(fields, want) {
		t.Errorf("after a file: %v, want %v", fields, want)
	}

	// From stdin, on set, with a CR LF or an LF ending.
	for _, in := range []string{"Basel\r\n", "Bern\n"} {
		err = instanceSet(t.Context(), []string{"-data", dir, "1", "-field-file", "city=-"}, strings.NewReader(in), &out)
		if fields = storedValues(t, dir); err != nil || fields["city"].One != strings.TrimRight(in, "\r\n") {
			t.Errorf("from stdin %q: %v, %v", in, fields, err)
		}
	}
	// A CR on its own is not a line ending a file can have, so it stays.
	err = instanceSet(t.Context(), []string{"-data", dir, "1", "-field-file", "city=-"}, strings.NewReader("Bern\r"), &out)
	if err == nil || !strings.Contains(err.Error(), "city: must be one line") {
		t.Errorf("a trailing lone CR: %v", err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"missing file", []string{"1", "-field-file", "city=" + filepath.Join(files, "nope")}, "", "no such file"},
		{"stdin twice", []string{"1", "-field-file", "city=-", "-field-file", "tags=-"}, "x", "only one -field-file can read stdin"},
		{"too big", []string{"1", "-field-file", "city=" + write("big", strings.Repeat("x", 70000))}, "", "over the 63 KB"},
	} {
		err := instanceSet(t.Context(), append([]string{"-data", dir}, tc.args...), strings.NewReader(tc.stdin), &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one with %q", tc.name, err, tc.want)
		}
	}
	if fields = storedValues(t, dir); fields["city"].One != "Bern" {
		t.Errorf("a refused set changed the instance: %v", fields)
	}
}

// TestInstanceClearSecret checks that -clear removes a secret and that an
// empty -field for a password keeps it.
func TestInstanceClearSecret(t *testing.T) {
	dir := instanceDir(t)
	t.Setenv(secrets.EnvVar, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if _, err := runInstance(t, dir, "add", "-recipe", "1", "-field", "city=Zurich", "-field", "token=hunter2"); err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		t.Helper()
		client, err := openDB(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		inst, err := client.Read.GetInstance(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		key, err := secrets.FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		_, names, err := recipe.PublicFields(&inst, key)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	if _, err := runInstance(t, dir, "set", "1", "-field", "token="); err != nil || len(names()) != 1 {
		t.Errorf("an empty password: %v, %v", err, names())
	}
	if _, err := runInstance(t, dir, "set", "1", "-field", "token=x", "-clear", "token"); err == nil || !strings.Contains(err.Error(), "token: is both set and cleared") {
		t.Errorf("set and cleared: %v", err)
	}
	if _, err := runInstance(t, dir, "set", "1", "-clear", "token"); err != nil || len(names()) != 0 {
		t.Errorf("clear: %v, %v", err, names())
	}
}

// storedMarks returns the names of the fields of instance 1 that are marked
// secret, its plain field values, and its settings_at.
func storedMarks(t *testing.T, dir string) (marks []string, fields map[string]recipe.Value, settingsAt int64) {
	t.Helper()
	inst, fields := storedInstance(t, dir)
	marks, err := recipe.SecretKeys(&inst)
	if err != nil {
		t.Fatal(err)
	}
	return marks, fields, inst.SettingsAt
}

// TestInstanceSecretMarks checks that -secret on add and set moves a value
// into the encrypted fields without raising settings_at, and that -plain is
// refused unless it comes with a new value, which is then the one shown. It sets the environment, so it does not
// run in parallel.
func TestInstanceSecretMarks(t *testing.T) {
	dir := instanceDir(t)
	t.Setenv(secrets.EnvVar, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if _, err := runInstance(t, dir, "add", "-recipe", "1", "-field", "city=Zurich", "-secret", "city"); err != nil {
		t.Fatal(err)
	}
	marks, fields, _ := storedMarks(t, dir)
	if len(marks) != 1 || marks[0] != "city" || len(fields) != 0 {
		t.Errorf("after add -secret: marks %v, plain fields %v", marks, fields)
	}
	if _, err := runInstance(t, dir, "set", "1", "-plain", "city"); err == nil || !strings.Contains(err.Error(), "city: is secret; give it a new value or clear it to make it plain") {
		t.Errorf("set -plain alone: %v", err)
	}
	if _, err := runInstance(t, dir, "set", "1", "-plain", "city", "-field", "city=Geneva"); err != nil {
		t.Fatal(err)
	}
	marks, fields, at := storedMarks(t, dir)
	if len(marks) != 0 || fields["city"].One != "Geneva" {
		t.Errorf("after set -plain with a value: marks %v, plain fields %v", marks, fields)
	}
	if _, err := runInstance(t, dir, "set", "1", "-secret", "city"); err != nil {
		t.Fatal(err)
	}
	marks, fields, got := storedMarks(t, dir)
	if len(marks) != 1 || len(fields) != 0 || got != at {
		t.Errorf("after set -secret: marks %v, plain fields %v, settings_at %d (was %d)", marks, fields, got, at)
	}
	// A mark alone is a change, and a refused one is reported.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-secret", "days"}, "days: cannot be secret"},
		{[]string{"-plain", "token"}, "token: is a password field"},
		{[]string{"-secret", "nope"}, "nope: no such field"},
	} {
		if _, err := runInstance(t, dir, append([]string{"set", "1"}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("set %v: err = %v, want one with %q", tc.args, err, tc.want)
		}
	}
	if _, err := runInstance(t, dir, "list"); err != nil {
		t.Errorf("list: %v", err)
	}
}

// TestInstanceSecretAndPlain checks that a key in both -secret and -plain is
// refused before the database is opened, and that a short value is refused
// as a secret.
func TestInstanceSecretAndPlain(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "never-created")
	var out bytes.Buffer
	err := instances(t.Context(), []string{"set", "-data", missing, "1", "-secret", "city", "-plain", "city"}, &out)
	if err == nil || !strings.Contains(err.Error(), `"city" is given to both -secret and -plain`) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database was opened: %v", err)
	}
	dir := instanceDir(t)
	if _, err := runInstance(t, dir, "add", "-recipe", "1", "-field", "city=abc", "-secret", "city"); err == nil || !strings.Contains(err.Error(), "city: must be at least 6 characters to be secret") {
		t.Errorf("a short secret: %v", err)
	}
}

package main

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// storedFields returns the plain field values of instance 1.
func storedFields(t *testing.T, dir string) (name, palette string, fields map[string]recipe.Value, settingsAt int64) {
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
	fields, _, err = recipe.PublicFields(&inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inst.Name, inst.Palette, fields, inst.SettingsAt
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
		_, _, _, before := storedFields(t, dir)
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
	if _, err := runInstance(t, dir, "set", "1", "-field", "city=Bern"); err != nil {
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

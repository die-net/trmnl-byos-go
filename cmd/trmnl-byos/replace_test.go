package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// replacedYML is instanceYML renamed, with the city a password field.
const replacedYML = `name: Weather 2
custom_fields:
- keyname: city
  field_type: password
  optional: true
`

// writeRecipe writes a recipe of settings into a new directory and returns
// it.
func writeRecipe(t *testing.T, settings string) string {
	t.Helper()
	src := t.TempDir()
	writeRecipeTo(t, src, settings)
	return src
}

func writeRecipeTo(t *testing.T, src, settings string) {
	t.Helper()
	for name, body := range map[string]string{"settings.yml": settings, "full.liquid": "x"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// runRecipe runs `recipe args...` against the data directory.
func runRecipe(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := recipes(t.Context(), append([]string{args[0], "-data", dir}, args[1:]...), &out)
	return out.String(), err
}

// storedRecipe returns recipe 1's name and revision.
func storedRecipe(t *testing.T, dir string) (name string, revision int64) {
	t.Helper()
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	rec, err := client.Read.GetRecipe(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Name, rec.Revision
}

func TestRecipeReplace(t *testing.T) {
	t.Parallel()
	dir := instanceDir(t)
	src := writeRecipe(t, replacedYML)
	out, err := runRecipe(t, dir, "replace", "1", src)
	if err != nil || out != "Replaced recipe 1, \"Weather 2\", now revision 2.\n" {
		t.Fatalf("replace from a directory: %q, %v", out, err)
	}
	if name, rev := storedRecipe(t, dir); name != "Weather 2" || rev != 2 {
		t.Errorf("stored %q revision %d", name, rev)
	}
	out, err = runRecipe(t, dir, "replace", "1", src)
	if err != nil || out != "Recipe 1, \"Weather 2\", is unchanged.\n" {
		t.Errorf("the same files again: %q, %v", out, err)
	}
	if _, rev := storedRecipe(t, dir); rev != 2 {
		t.Errorf("revision %d after an unchanged replace, want 2", rev)
	}
}

// TestRecipeReplaceRecordedSource checks that a replace with no path reads
// the path the recipe was installed from, and records it again.
func TestRecipeReplaceRecordedSource(t *testing.T) {
	t.Parallel()
	dir, src := dataDir(t), writeRecipe(t, instanceYML)
	if _, err := runRecipe(t, dir, "add", src); err != nil {
		t.Fatal(err)
	}
	writeRecipeTo(t, src, replacedYML)
	out, err := runRecipe(t, dir, "replace", "1")
	if err != nil || !strings.HasPrefix(out, "Replaced recipe 1, \"Weather 2\", now revision 2.") {
		t.Fatalf("replace from the recorded source: %q, %v", out, err)
	}
	if _, err := runRecipe(t, dir, "replace", "9"); err == nil || !strings.Contains(err.Error(), "no such recipe") {
		t.Errorf("an unknown recipe: %v", err)
	}
}

// TestRecipeReplaceUploaded checks that a recipe installed from an upload
// has no path to replace it from.
func TestRecipeReplaceUploaded(t *testing.T) {
	t.Parallel()
	dir := dataDir(t)
	var z bytes.Buffer
	zw := zip.NewWriter(&z)
	for name, body := range map[string]string{"settings.yml": instanceYML, "full.liquid": "x"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	client, err := openDB(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recipe.InstallZip(t.Context(), client, recipesDir(dir), &z, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = runRecipe(t, dir, "replace", "1")
	if err == nil || !strings.Contains(err.Error(), "was uploaded") {
		t.Fatalf("replace with no path: %v, want a refusal saying it was uploaded", err)
	}
	if _, rev := storedRecipe(t, dir); rev != 1 {
		t.Errorf("revision %d after a refusal", rev)
	}
	if _, err := runRecipe(t, dir, "replace", "1", writeRecipe(t, replacedYML)); err != nil {
		t.Errorf("replace with a path: %v", err)
	}
}

// TestRecipeReplaceSealing checks what a replace says about the instances it
// seals, and that one it would leave with a plain value is refused with the
// instance and keyname.
func TestRecipeReplaceSealing(t *testing.T) {
	dir := instanceDir(t)
	if _, err := runInstance(t, dir, "add", "-recipe", "1", "-field", "city=Zurich"); err != nil {
		t.Fatal(err)
	}
	src := writeRecipe(t, replacedYML)

	out, err := runRecipe(t, dir, "replace", "1", src)
	if !errors.Is(err, secrets.ErrNoKey) || !strings.Contains(out, "  Instance 1, city: ") {
		t.Fatalf("no key: %q, %v", out, err)
	}
	if _, rev := storedRecipe(t, dir); rev != 1 {
		t.Errorf("revision %d after a refusal", rev)
	}

	t.Setenv(secrets.EnvVar, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	out, err = runRecipe(t, dir, "replace", "1", src)
	want := "Replaced recipe 1, \"Weather 2\", now revision 2.\n  Instance 1, \"Weather\": sealed city.\n"
	if err != nil || out != want {
		t.Fatalf("with a key: %q, %v; want %q", out, err, want)
	}
}

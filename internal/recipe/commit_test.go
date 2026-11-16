package recipe

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failCommits makes every commit after a write of the given event to recipe
// fail. A trigger breaks a deferred foreign key, so the statements succeed,
// and what the code does after them, the rename, runs, and only COMMIT
// fails, as with both SQLite drivers.
func failCommits(t *testing.T, f fixture, event string) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE fail_parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE fail_child (p INTEGER REFERENCES fail_parent (id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER fail_commit AFTER ` + event + ` ON recipe BEGIN INSERT INTO fail_child VALUES (999); END`,
	} {
		if _, err := f.client.Writer.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// noTemps fails the test if recipesDir holds a temporary entry.
func noTemps(t *testing.T, recipesDir string) {
	t.Helper()
	entries, err := os.ReadDir(recipesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temporary entry %s left in the recipes dir", e.Name())
		}
	}
}

// TestInstallCommitFails checks that an install whose commit fails after
// the rename removes the recipe's directory, which a later recipe would get
// the ID of, and leaves no temporary entry.
func TestInstallCommitFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	failCommits(t, f, "INSERT")
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": weatherYML, "full.liquid": "x"})
	if _, err := Install(t.Context(), f.client, f.recipesDir, src, time.Unix(2, 0)); err == nil {
		t.Fatal("the install committed")
	}
	if _, err := os.Stat(filepath.Join(f.recipesDir, "2")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("recipes/2 after the failed commit: %v, want it gone", err)
	}
	noTemps(t, f.recipesDir)
}

// TestReplaceCommitFails checks that a replace whose commit fails after the
// rename removes the new revision, leaves no temporary entry, and leaves the
// recipe and its current files as they were.
func TestReplaceCommitFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	failCommits(t, f, "UPDATE")
	if _, err := replaceWith(t, f, nil, retyped(t, "city", "string", "text"), time.Unix(5000, 0)); err == nil {
		t.Fatal("the replace committed")
	}
	if _, err := os.Stat(Dir(f.recipesDir, f.recipe.ID, 2)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("revision 2 after the failed commit: %v, want it gone", err)
	}
	noTemps(t, f.recipesDir)
	if rec, err := f.client.Read.GetRecipe(t.Context(), f.recipe.ID); err != nil || rec.Revision != 1 {
		t.Errorf("recipe after the failed commit: %+v, %v; want revision 1", rec, err)
	}
	if _, err := os.Stat(filepath.Join(FilesDir(f.recipesDir, &f.recipe), "settings.yml")); err != nil {
		t.Errorf("current files after the failed commit: %v", err)
	}
}

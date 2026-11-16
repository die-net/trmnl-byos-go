package adminui

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// replaceRecipeFiles makes revision 2 of the recipe, as a replace does.
func replaceRecipeFiles(t *testing.T, tu *testUI, dir string, rec *db.Recipe) {
	t.Helper()
	revDir := recipe.Dir(dir, rec.ID, 2)
	if err := os.MkdirAll(revDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(revDir, "settings.yml"), []byte("name: Weather\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tu.db.Write.ReplaceRecipe(t.Context(), db.ReplaceRecipeParams{
		Name: rec.Name, Source: rec.Source, RefreshInterval: rec.RefreshInterval, Revision: 2, SourceHash: "new", ReplacedAt: tu.now.Unix(), ID: rec.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRecipeAddInstanceRevision checks that the add form sends the revision
// of the recipe it was drawn from, and that an add from a page drawn before a
// replace is refused and creates nothing.
func TestRecipeAddInstanceRevision(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather")
	writeSettings(t, dir, rec.ID)
	c := tu.signIn(t)
	path := "/recipes/add-instance/" + id(rec.ID)
	if body := tu.get(t, c, "/recipes/"+id(rec.ID)).Body.String(); !strings.Contains(body, `<input type="hidden" name="revision" value="1">`) {
		t.Errorf("the form lacks the hidden revision:\n%s", body)
	}

	replaceRecipeFiles(t, tu, dir, &rec)
	// The refused add is drawn again from the recipe as it is now, with the
	// name typed, so the form it shows can be sent as it is.
	for _, form := range []url.Values{{"revision": {"1"}}, {"revision": {"1"}, "name": {"Home"}}} {
		w := tu.post(t, c, path, form)
		body := w.Body.String()
		if w.Code != http.StatusConflict || !strings.Contains(body, "This recipe was replaced after the page loaded") ||
			!strings.Contains(body, `name="revision" value="2"`) || !strings.Contains(body, `value="`+form.Get("name")+`"`) {
			t.Errorf("a stale add: status %d: %s", w.Code, body)
		}
	}
	if n := len(listInstances(t, tu)); n != 0 {
		t.Fatalf("%d instances after refused adds, want none", n)
	}

	if body := tu.get(t, c, "/recipes/"+id(rec.ID)).Body.String(); !strings.Contains(body, `name="revision" value="2"`) {
		t.Errorf("the reloaded form lacks revision 2:\n%s", body)
	}
	for _, rev := range []string{"2", "", "x"} {
		form := url.Values{"name": {"Home " + rev}}
		if rev != "" {
			form.Set("revision", rev)
		}
		if w := tu.post(t, c, path, form); w.Code != http.StatusSeeOther {
			t.Errorf("an add with revision %q: status %d: %s", rev, w.Code, w.Body)
		}
	}
	if n := len(listInstances(t, tu)); n != 3 {
		t.Errorf("%d instances, want 3", n)
	}
}

package adminui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// replaceOld is the settings.yml of a recipe before a replace, and
// replaceNew after it: city becomes a password field, pass stops being one,
// units loses the choice an instance has, and zone is new and required.
const (
	replaceOld = `name: Weather
custom_fields:
- keyname: city
  name: City
  field_type: string
- keyname: pass
  name: Password
  field_type: password
  optional: true
- keyname: units
  name: Units
  field_type: select
  options: [Metric, Imperial]
`
	replaceNew = `name: Weather 2
custom_fields:
- keyname: city
  name: City
  field_type: password
- keyname: pass
  name: Password
  field_type: string
  optional: true
- keyname: units
  name: Units
  field_type: select
  options: [Kelvin]
- keyname: zone
  name: Zone
  field_type: string
`
)

// recipeZip returns a recipe zip file with the settings.yml.
func recipeZip(t *testing.T, settings string) string {
	t.Helper()
	return string(zipFiles(t, map[string]string{"settings.yml": settings, "full.liquid": "<div>hello</div>\n"}))
}

// newReplaceUI returns a test server, with a secrets key if withKey, that
// has a recipe uploaded with replaceOld as its settings.yml, and counts in
// woken how often the scheduler was woken.
func newReplaceUI(t *testing.T, withKey bool) (tu *testUI, dir string, c *http.Cookie, rec db.Recipe, woken *int) {
	t.Helper()
	dir = t.TempDir()
	cfg := Config{RecipesDir: dir}
	if withKey {
		key, err := secrets.Parse(testKeyB64)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Secrets = key
	}
	tu = newTestUI(t, cfg, true)
	woken = new(int)
	tu.db.OnChange = func() { *woken++ }
	c = tu.signIn(t)
	if status, _, body := tu.upload(t, c, []formPart{{field: "recipe", filename: "w.zip", content: recipeZip(t, replaceOld)}}); status != http.StatusSeeOther {
		t.Fatalf("upload: status %d: %s", status, body)
	}
	return tu, dir, c, listRecipes(t, tu)[0], woken
}

// replace posts the parts to the recipe's replace form.
func (tu *testUI) replace(t *testing.T, c *http.Cookie, recipeID int64, parts ...formPart) (status int, body string) {
	t.Helper()
	b, ct := multipartBody(t, parts...)
	w := tu.do(t, "POST", "/recipes/replace/"+id(recipeID), b, "Cookie", cookieHeader(c), "Content-Type", ct)
	return w.Code, w.Body.String()
}

// addPlain adds an instance of the recipe with the name, with only plain
// values, so it needs no secrets key.
func addPlain(t *testing.T, tu *testUI, c *http.Cookie, rec *db.Recipe, name string) {
	t.Helper()
	form := url.Values{"name": {name}, "f.city": {"Oslo"}, "f.units": {"metric"}}
	if w := tu.post(t, c, "/recipes/add-instance/"+id(rec.ID), form); w.Code != http.StatusSeeOther {
		t.Fatalf("add instance: status %d: %s", w.Code, w.Body)
	}
}

// recipeNow returns the recipe's row.
func recipeNow(t *testing.T, tu *testUI, recipeID int64) db.Recipe {
	t.Helper()
	rec, err := tu.db.Read.GetRecipe(t.Context(), recipeID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestRecipeReplaceForm checks that a recipe's page has the replace form,
// with no report on it.
func TestRecipeReplaceForm(t *testing.T) {
	t.Parallel()
	tu, _, c, rec, _ := newReplaceUI(t, false)
	body := tu.get(t, c, "/recipes/"+id(rec.ID)).Body.String()
	wantInOrder(t, body,
		`<h2 id="add-title">Add an instance</h2>`,
		`<section class="panel form-panel" id="replace" aria-labelledby="replace-title">`,
		`<h2 id="replace-title">Replace the files</h2>`,
		`<form method="post" action="/recipes/replace/`+id(rec.ID)+`#replace" enctype="multipart/form-data">`,
		`<input id="recipe" name="recipe" type="file" accept=".zip,application/zip" required>`,
		"up to 8 MB. The recipe keeps its instances and their values.",
		`<button class="btn btn-secondary" type="submit">Replace</button>`,
	)
	for _, not := range []string{`role="alert"`, `role="status"`, `class="replaced"`} {
		if strings.Contains(body, not) {
			t.Errorf("a page nothing was posted to has %q", not)
		}
	}
	wantOneScript(t, body)
}

// TestRecipeReplace checks that a replace installs the new files, wakes the
// scheduler, and shows the page drawn from the new files with what it did
// to each instance: the values it sealed, the names it marked, and the
// values to look at, with names escaped. An instance with nothing to say is
// not listed.
func TestRecipeReplace(t *testing.T) {
	t.Parallel()
	tu, _, c, rec, woken := newReplaceUI(t, true)
	form := url.Values{"name": {"Home <i>"}, "f.city": {"Oslo"}, "f.pass": {"hunter22"}, "f.units": {"metric"}}
	if w := tu.post(t, c, "/recipes/add-instance/"+id(rec.ID), form); w.Code != http.StatusSeeOther {
		t.Fatalf("add instance: status %d: %s", w.Code, w.Body)
	}
	home := listInstances(t, tu)[0].PluginInstance
	*woken = 0

	status, body := tu.replace(t, c, rec.ID, formPart{field: "recipe", filename: "new.zip", content: recipeZip(t, replaceNew)})
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	wantInOrder(t, body,
		"<h1>Weather 2</h1>",
		`<input type="hidden" name="revision" value="2">`,
		`<label for="field-3">Zone`,
		`<p class="done" role="status">The recipe&#39;s files were replaced. Its instances are rendered again with the new files.</p>`,
		"<p>These instances have values to look at:</p>",
		`<a href="/recipes/instances/`+id(home.ID)+`">Home &lt;i&gt;</a>`,
		"<li>Now sealed as a secret: <code>city</code></li>",
		"<li>Marked secret, so the sealed value stays sealed: <code>pass</code></li>",
		"<li><code>units</code> is not one of the options</li>",
		"<li><code>zone</code> is required</li>",
		`action="/recipes/replace/`+id(rec.ID)+`#replace"`,
	)
	if strings.Contains(body, "<i>") || strings.Contains(body, `role="alert"`) || strings.Contains(body, "hunter22") || strings.Contains(body, "Oslo") {
		t.Errorf("the report has an unescaped name, an error or a value:\n%s", body)
	}
	if *woken != 1 {
		t.Errorf("the scheduler was woken %d times, want 1", *woken)
	}
	if got := recipeNow(t, tu, rec.ID); got.Revision != 2 || got.Name != "Weather 2" {
		t.Errorf("recipe after the replace: revision %d, name %q", got.Revision, got.Name)
	}
	if got := listInstances(t, tu)[0].PluginInstance; strings.Contains(got.FieldValues, "Oslo") {
		t.Errorf("city is still a plain value: %s", got.FieldValues)
	}

	// The same files again change nothing, and wake nothing.
	status, body = tu.replace(t, c, rec.ID, formPart{field: "recipe", filename: "new.zip", content: recipeZip(t, replaceNew)})
	const unchanged = `<p class="done" role="status">These are the files already installed, so nothing was changed.</p>`
	if status != http.StatusOK || !strings.Contains(body, unchanged) || strings.Contains(body, `class="replaced"`) {
		t.Errorf("the same files: status %d: %s", status, body)
	}
	if got := recipeNow(t, tu, rec.ID); got.Revision != 2 || *woken != 1 {
		t.Errorf("the same files: revision %d, woken %d times", got.Revision, *woken)
	}
}

// TestRecipeReplaceQuiet checks that a replace with nothing to say about
// any instance shows no list.
func TestRecipeReplaceQuiet(t *testing.T) {
	t.Parallel()
	tu, _, c, rec, woken := newReplaceUI(t, false)
	addPlain(t, tu, c, &rec, "Home")
	*woken = 0
	status, body := tu.replace(t, c, rec.ID, formPart{field: "recipe", filename: "new.zip", content: recipeZip(t, replaceOld+"# a comment\n")})
	if status != http.StatusOK || !strings.Contains(body, "The recipe&#39;s files were replaced.") {
		t.Fatalf("status %d: %s", status, body)
	}
	if strings.Contains(body, `class="replaced"`) || strings.Contains(body, "values to look at") {
		t.Errorf("a replace with nothing to report lists instances:\n%s", body)
	}
	if got := recipeNow(t, tu, rec.ID); got.Revision != 2 || *woken != 1 {
		t.Errorf("revision %d, woken %d times; want 2 and 1", got.Revision, *woken)
	}
}

// TestRecipeReplaceNoKey checks that a replace that has to seal a value on
// a server with no secrets key is refused with each instance and keyname,
// changes nothing, and wakes nothing.
func TestRecipeReplaceNoKey(t *testing.T) {
	t.Parallel()
	tu, dir, c, rec, woken := newReplaceUI(t, false)
	addPlain(t, tu, c, &rec, "Home <i>")
	addPlain(t, tu, c, &rec, "Away")
	insts := listInstances(t, tu)
	*woken = 0

	status, body := tu.replace(t, c, rec.ID, formPart{field: "recipe", filename: "new.zip", content: recipeZip(t, replaceNew)})
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, body)
	}
	wantInOrder(t, body,
		"<h1>Weather</h1>",
		`<input type="hidden" name="revision" value="1">`,
		`<p class="error" role="alert">The recipe was not replaced, and nothing was changed.`,
		"the server has no secrets key to seal their values with. Set "+secrets.EnvVar+" on the server and restart it.</p>",
		`<a href="/recipes/instances/`+id(insts[0].PluginInstance.ID)+`">Home &lt;i&gt;</a>`,
		"<li><code>city</code> is now a password field, and sealing its value needs the secrets key</li>",
		`<a href="/recipes/instances/`+id(insts[1].PluginInstance.ID)+`">Away</a>`,
		"<li><code>city</code> is now a password field",
		`<button class="btn btn-secondary" type="submit">Replace</button>`,
	)
	if strings.Contains(body, `role="status"`) || strings.Contains(body, "values to look at") || strings.Contains(body, "<i>") || strings.Contains(body, "Oslo") {
		t.Errorf("a refused replace says it was done, or shows a name unescaped or a value:\n%s", body)
	}
	if got := recipeNow(t, tu, rec.ID); got.Revision != 1 || got.Name != "Weather" || *woken != 0 {
		t.Errorf("after a refused replace: revision %d, name %q, woken %d times", got.Revision, got.Name, *woken)
	}
	if got := listInstances(t, tu)[0].PluginInstance; !strings.Contains(got.FieldValues, "Oslo") {
		t.Errorf("a refused replace changed the values: %s", got.FieldValues)
	}
	// Only the recipe's own directory is left, with its one revision.
	if left, err := os.ReadDir(dir); err != nil || len(left) != 1 {
		t.Errorf("a refused replace left %v in the recipes directory (%v)", left, err)
	}
	if revs, err := os.ReadDir(recipe.Dir(dir, rec.ID, 1) + "/.."); err != nil || len(revs) != 1 {
		t.Errorf("a refused replace left revisions %v (%v)", revs, err)
	}
}

// TestRecipeReplaceUnsealable checks that a replace refused for a reason
// the key does not fix, a list that would have to be sealed, says so
// without asking for a key.
func TestRecipeReplaceUnsealable(t *testing.T) {
	t.Parallel()
	tu, _, c, rec, woken := newReplaceUI(t, true)
	const multi = "name: Weather\ncustom_fields:\n- keyname: city\n  field_type: select\n  multiple: true\n  options: [Oslo, Bergen]\n"
	if status, body := tu.replace(t, c, rec.ID, formPart{field: "recipe", filename: "m.zip", content: recipeZip(t, multi)}); status != http.StatusOK {
		t.Fatalf("replace with a multi-select: status %d: %s", status, body)
	}
	form := url.Values{"name": {"Home"}, "f.city": {"oslo", "bergen"}}
	if w := tu.post(t, c, "/recipes/add-instance/"+id(rec.ID), form); w.Code != http.StatusSeeOther {
		t.Fatalf("add instance: status %d: %s", w.Code, w.Body)
	}
	*woken = 0
	status, body := tu.replace(t, c, rec.ID, formPart{field: "recipe", filename: "new.zip", content: recipeZip(t, replaceNew)})
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, body)
	}
	wantInOrder(t, body,
		`<p class="error" role="alert">The recipe was not replaced, and nothing was changed. The new files make these fields password fields, and their values could not be sealed:</p>`,
		`">Home</a>`,
		"<li><code>city</code> is now a password field, and has a list of values, which cannot be secret; clear it first</li>",
	)
	if strings.Contains(body, secrets.EnvVar) {
		t.Errorf("a refusal the key does not fix asks for a key:\n%s", body)
	}
	if got := recipeNow(t, tu, rec.ID); got.Revision != 2 || *woken != 0 {
		t.Errorf("after a refused replace: revision %d, woken %d times; want 2 and 0", got.Revision, *woken)
	}
}

// TestRecipeReplaceRefused checks each upload a replace refuses, that the
// refusal is in the replace panel, and that the recipe keeps its files.
func TestRecipeReplaceRefused(t *testing.T) {
	t.Parallel()
	good := recipeZip(t, replaceNew)
	for _, tc := range []struct {
		name   string
		parts  []formPart
		status int
		want   string
	}{
		{"no file part", []formPart{{field: "note", content: "x"}}, http.StatusBadRequest, chooseFile},
		{"no file chosen", []formPart{{field: "recipe", empty: true}}, http.StatusBadRequest, chooseFile},
		{"not a zip", []formPart{{field: "recipe", filename: "a.zip", content: "hello"}}, http.StatusBadRequest, "not a zip"},
		{"no settings.yml", []formPart{{field: "recipe", filename: "a.zip", content: string(zipFiles(t, map[string]string{"readme.txt": "x"}))}}, http.StatusBadRequest, "settings.yml"},
		{"file too large", []formPart{{field: "recipe", filename: "a.zip", content: good + strings.Repeat("x", recipe.MaxRecipeBytes)}}, http.StatusRequestEntityTooLarge, "The file is over 8 MB."},
		{"body too large", []formPart{{field: "note", content: strings.Repeat("x", recipe.MaxRecipeBytes+maxFormBody)}, {field: "recipe", filename: "a.zip", content: good}}, http.StatusRequestEntityTooLarge, "The upload is too large."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir, c, rec, woken := newReplaceUI(t, true)
			*woken = 0
			status, body := tu.replace(t, c, rec.ID, tc.parts...)
			if status != tc.status {
				t.Fatalf("status %d, want %d: %s", status, tc.status, body)
			}
			wantInOrder(t, body, `<h2 id="replace-title">Replace the files</h2>`, `<p class="error" role="alert">`, tc.want, `<form method="post" action="/recipes/replace/`+id(rec.ID)+`#replace"`)
			if strings.Count(body, `role="alert"`) != 1 || strings.Contains(body, `role="status"`) {
				t.Errorf("want one error, in the replace panel, and no report:\n%s", body)
			}
			if got := recipeNow(t, tu, rec.ID); got.Revision != 1 || *woken != 0 {
				t.Errorf("after a refused replace: revision %d, woken %d times", got.Revision, *woken)
			}
			// Only the recipe's own directory is left.
			if left, err := os.ReadDir(dir); err != nil || len(left) != 1 {
				t.Errorf("a refused replace left %v in the recipes directory (%v)", left, err)
			}
		})
	}
}

// TestRecipeReplaceCutOff checks that a replace whose body ends early is a
// client error that changes nothing.
func TestRecipeReplaceCutOff(t *testing.T) {
	t.Parallel()
	tu, _, c, rec, _ := newReplaceUI(t, true)
	body, ct := multipartBody(t, formPart{field: "recipe", filename: "a.zip", content: recipeZip(t, replaceNew)})
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/recipes/replace/"+id(rec.ID),
		io.MultiReader(strings.NewReader(body[:strings.Index(body, "PK")+40]), iotest.ErrReader(io.ErrUnexpectedEOF)))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookieHeader(c))
	w := tu.serve(req)
	const want = `<p class="error" role="alert">The upload was cut off or could not be read.</p>`
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
		t.Errorf("status %d, body %s; want 400 with %q", w.Code, w.Body, want)
	}
	if got := recipeNow(t, tu, rec.ID); got.Revision != 1 {
		t.Errorf("revision %d after a cut-off replace", got.Revision)
	}
}

// TestRecipeReplaceGuards checks that a replace of an unknown recipe is not
// found, that one that is not a form asks for a file, and that one with no
// session changes nothing.
func TestRecipeReplaceGuards(t *testing.T) {
	t.Parallel()
	tu, _, c, rec, woken := newReplaceUI(t, true)
	*woken = 0
	file := formPart{field: "recipe", filename: "new.zip", content: recipeZip(t, replaceNew)}
	for _, path := range []string{"999", "x"} {
		b, ct := multipartBody(t, file)
		w := tu.do(t, "POST", "/recipes/replace/"+path, b, "Cookie", cookieHeader(c), "Content-Type", ct)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "No such recipe") {
			t.Errorf("recipe %s: status %d: %s", path, w.Code, w.Body)
		}
	}
	w := tu.post(t, c, "/recipes/replace/"+id(rec.ID), url.Values{"recipe": {"x"}})
	if body := w.Body.String(); w.Code != http.StatusBadRequest || !strings.Contains(body, chooseFile) {
		t.Errorf("not a multipart form: status %d: %s", w.Code, body)
	}
	b, ct := multipartBody(t, file)
	if w := tu.do(t, "POST", "/recipes/replace/"+id(rec.ID), b, "Content-Type", ct); w.Code != http.StatusSeeOther || w.Header().Get("Location") == "" {
		t.Errorf("no session: status %d, want a redirect to sign in", w.Code)
	}
	if got := recipeNow(t, tu, rec.ID); got.Revision != 1 || *woken != 0 {
		t.Errorf("revision %d, woken %d times; want 1 and 0", got.Revision, *woken)
	}
}

package adminui

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
)

// newRecipesUI returns a test server that installs recipes into its own
// temp directory, which it also returns.
func newRecipesUI(t *testing.T) (*testUI, string) {
	t.Helper()
	dir := t.TempDir()
	return newTestUI(t, Config{RecipesDir: dir}, true), dir
}

// addRecipe records a recipe installed from the directory /src/name (a slash
// in the name becomes an underscore), which has refreshed every 15 minutes
// since 3 Jan 2026 04:30 in testZone, and returns it.
func addRecipe(t *testing.T, tu *testUI, name string) db.Recipe {
	t.Helper()
	r, err := tu.db.Write.CreateRecipe(t.Context(), db.CreateRecipeParams{
		Name: name, Source: "/src/" + strings.ReplaceAll(name, "/", "_"), RefreshInterval: 900, InstalledAt: time.Date(2026, 1, 2, 23, 0, 0, 0, time.UTC).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// writeSettings gives the recipe the settings.yml that creating an instance
// of it reads, which has no custom fields.
func writeSettings(t *testing.T, recipesDir string, recipeID int64) {
	t.Helper()
	dir := recipe.Dir(recipesDir, recipeID, 1)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yml"), []byte("name: Weather\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newInstance creates an instance of the recipe with the palette.
func newInstance(t *testing.T, tu *testUI, recipeID int64, name, palette string) db.PluginInstance {
	t.Helper()
	inst, err := tu.db.Write.CreateInstance(t.Context(), db.CreateInstanceParams{RecipeID: recipeID, Name: name, FieldValues: "{}", SecretKeys: "[]", Palette: palette, CreatedAt: tu.now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// listInstances returns every instance.
func listInstances(t *testing.T, tu *testUI) []db.ListInstancesRow {
	t.Helper()
	rows, err := tu.db.Read.ListInstances(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// listRecipes returns every recipe.
func listRecipes(t *testing.T, tu *testUI) []db.Recipe {
	t.Helper()
	rows, err := tu.db.Read.ListRecipes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// testZip returns a recipe zip file whose settings.yml has the name, or
// none if name is "".
func testZip(t *testing.T, name string) []byte {
	t.Helper()
	settings := "refresh_interval: 20\n"
	if name != "" {
		settings = "name: " + name + "\n" + settings
	}
	return zipFiles(t, map[string]string{"settings.yml": settings, "full.liquid": "<div>hello</div>\n"})
}

// zipFiles returns a zip file of the files.
func zipFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for file, content := range files {
		w, err := zw.Create(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// formPart is one part of a multipart body. A part with a filename is a
// file, and one with Empty set is how a browser sends a file field that
// has no file chosen.
type formPart struct {
	field, filename, content string
	empty                    bool
}

// multipartBody returns a multipart/form-data body of the parts and its
// Content-Type.
func multipartBody(t *testing.T, parts ...formPart) (body, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var err error
		var w interface{ Write([]byte) (int, error) }
		switch {
		case p.empty:
			w, err = mw.CreatePart(map[string][]string{"Content-Disposition": {fmt.Sprintf(`form-data; name=%q; filename=""`, p.field)}, "Content-Type": {"application/octet-stream"}})
		case p.filename != "":
			w, err = mw.CreateFormFile(p.field, p.filename)
		default:
			w, err = mw.CreateFormField(p.field)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(p.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), mw.FormDataContentType()
}

// upload posts the parts to /recipes signed in, with the headers.
func (tu *testUI) upload(t *testing.T, c *http.Cookie, parts []formPart, headers ...string) (status int, location, body string) {
	t.Helper()
	b, ct := multipartBody(t, parts...)
	w := tu.do(t, "POST", "/recipes", b, append([]string{"Cookie", cookieHeader(c), "Content-Type", ct}, headers...)...)
	return w.Code, w.Header().Get("Location"), w.Body.String()
}

func TestRecipesEmpty(t *testing.T) {
	t.Parallel()
	tu, _ := newRecipesUI(t)
	body := tu.get(t, tu.signIn(t), "/recipes").Body.String()
	for _, want := range []string{
		"<h1>Recipes</h1>", "<h2>No recipes yet</h2>", "Upload a recipe zip file below",
		`<form method="post" action="/recipes#upload" enctype="multipart/form-data">`,
		`<input id="recipe" name="recipe" type="file" accept=".zip,application/zip" required>`,
		"up to 8 MB.", `<button class="btn btn-primary" type="submit">Upload</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="stack"`) {
		t.Error("empty page has a recipe list")
	}
	if strings.Contains(body, `role="alert"`) {
		t.Error("empty page has an error")
	}
}

func TestRecipesList(t *testing.T) {
	t.Parallel()
	tu, _ := newRecipesUI(t)
	ctx := t.Context()
	weather := addRecipe(t, tu, "<b>x</b>")
	empty := addRecipe(t, tu, "Quotes")
	if _, err := tu.db.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "Sent", Source: recipe.UploadSource, RefreshInterval: 900, InstalledAt: 1}); err != nil {
		t.Fatal(err)
	}
	bw := newInstance(t, tu, weather.ID, "Home <i>", "bw")
	plain := newInstance(t, tu, weather.ID, "Plain", "")
	// A palette the registry does not have shows as its ID.
	if _, err := tu.db.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: weather.ID, Name: "Odd", FieldValues: "{}", Palette: "gone", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	// An instance in two playlists, one of them twice.
	for _, name := range []string{"Morning", "Evening", "Morning"} {
		if _, err := addItemTo(ctx, tu.db, name, "", bw.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	morning, evening := playlistID(t, tu, "Morning"), playlistID(t, tu, "Evening")

	body := tu.get(t, tu.signIn(t), "/recipes").Body.String()
	for _, want := range []string{
		`<li class="panel" id="recipe-` + id(weather.ID) + `">`, `<h2>&lt;b&gt;x&lt;/b&gt;</h2>`,
		`href="/recipes/` + id(weather.ID) + `" aria-label="Add instance of &lt;b&gt;x&lt;/b&gt;">Add instance</a>`,
		`<dd title="/src/&lt;b&gt;x&lt;_b&gt;">&lt;b&gt;x&lt;_b&gt;</dd>`, "<dd>Uploaded</dd>", "<dd>Every 15 min</dd>", `<dd><time datetime="2026-01-02T23:00:00Z" data-local="day">3 Jan 2026</time></dd>`,
		`<span class="item-name">Home &lt;i&gt;</span>`, `<span class="rel">Black &amp; White (1-bit)</span>`,
		`<span class="rel">Device&#39;s palette</span>`, `<span class="rel">gone</span>`,
		`In <a href="/playlists#playlist-` + id(evening) + `">Evening</a>, <a href="/playlists#playlist-` + id(morning) + `">Morning</a>`,
		`href="/recipes/instances/` + id(plain.ID) + `/delete" aria-label="Remove Plain">Remove</a>`,
		`id="recipe-` + id(empty.ID) + `"`, `<p class="rel">This recipe has no instances.</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %q:\n%s", want, body)
		}
	}
	// An instance that a playlist shows has no way to remove it.
	if strings.Contains(body, `/recipes/instances/`+id(bw.ID)+`/delete`) || strings.Contains(body, `aria-label="Remove Home`) {
		t.Error("an instance in a playlist has a Remove link")
	}
	if strings.Contains(body, "title=\"upload\"") || strings.Contains(body, "<dd>upload</dd>") {
		t.Error("an upload shows its stored source, not Uploaded")
	}
	if strings.Contains(body, "<b>x") || strings.Contains(body, "<i>") {
		t.Error("list has unescaped names")
	}
	if strings.Count(body, "In <a") != 1 {
		t.Error("an instance that is in no playlist says it is in one")
	}
	if strings.Contains(body, "No recipes yet") {
		t.Error("list has the empty state")
	}
}

func TestRecipeUpload(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	status, loc, body := tu.upload(t, tu.signIn(t), []formPart{
		{field: "note", content: "ignored"},
		{field: "recipe", filename: "weather.zip", content: string(testZip(t, "Test Recipe"))},
	})
	recipes := listRecipes(t, tu)
	if status != http.StatusSeeOther || len(recipes) != 1 {
		t.Fatalf("status %d with %d recipes: %s", status, len(recipes), body)
	}
	rec := recipes[0]
	if want := "/recipes#recipe-" + id(rec.ID); loc != want {
		t.Errorf("redirect to %q, want %q", loc, want)
	}
	if rec.Name != "Test Recipe" || rec.Source != "upload" || rec.RefreshInterval != 20*60 {
		t.Errorf("recipe = %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(recipe.Dir(dir, rec.ID, 1), "full.liquid")); err != nil {
		t.Errorf("recipe files are not installed: %v", err)
	}
	if page := tu.get(t, tu.signIn(t), "/recipes").Body.String(); !strings.Contains(page, `id="recipe-`+id(rec.ID)+`"`) || !strings.Contains(page, "Test Recipe") {
		t.Error("the list does not show the uploaded recipe")
	}
}

// TestRecipeUploadName checks that a recipe with no name in its settings.yml
// is called by the name of the file that was uploaded, and shows as
// uploaded in the list.
func TestRecipeUploadName(t *testing.T) {
	t.Parallel()
	tu, _ := newRecipesUI(t)
	status, _, body := tu.upload(t, tu.signIn(t), []formPart{{field: "recipe", filename: `C:\dl\Weather Report.zip`, content: string(testZip(t, ""))}})
	recipes := listRecipes(t, tu)
	if status != http.StatusSeeOther || len(recipes) != 1 || recipes[0].Name != "Weather Report" || recipes[0].Source != recipe.UploadSource {
		t.Fatalf("status %d with recipes %+v: %s", status, recipes, body)
	}
	if page := tu.get(t, tu.signIn(t), "/recipes").Body.String(); !strings.Contains(page, "<h2>Weather Report</h2>") || !strings.Contains(page, "<dd>Uploaded</dd>") {
		t.Errorf("the list does not show the recipe by its file name as uploaded:\n%s", page)
	}
}

func TestRecipeUploadRefused(t *testing.T) {
	t.Parallel()
	const choose = "Choose a recipe zip file to upload."
	const bigFile = "The file is over 8 MB."
	const bigBody = "The upload is too large."
	zipFile := string(testZip(t, "x"))
	noSettings := func() string {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		if _, err := zw.Create("readme.txt"); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}()
	for _, tc := range []struct {
		name   string
		parts  []formPart
		status int
		want   string
	}{
		{"no recipe part", []formPart{{field: "note", content: "hi"}}, http.StatusBadRequest, choose},
		{"no parts", nil, http.StatusBadRequest, choose},
		{"empty file name", []formPart{{field: "recipe", empty: true}}, http.StatusBadRequest, choose},
		{"recipe field that is not a file", []formPart{{field: "recipe", content: zipFile}}, http.StatusBadRequest, choose},
		{"file in another field", []formPart{{field: "other", filename: "a.zip", content: zipFile}}, http.StatusBadRequest, choose},
		{"not a zip", []formPart{{field: "recipe", filename: "a.zip", content: "this is not a zip file"}}, http.StatusBadRequest, "The file is not a zip archive."},
		{"empty file", []formPart{{field: "recipe", filename: "a.zip"}}, http.StatusBadRequest, "The file is empty."},
		{"zip without settings.yml", []formPart{{field: "recipe", filename: "a.zip", content: noSettings}}, http.StatusBadRequest, "The zip has no settings.yml at its top, in src/, or in one top-level folder."},
		{"zip without full.liquid", []formPart{{field: "recipe", filename: "a.zip", content: string(zipFiles(t, map[string]string{"settings.yml": "name: x\n"}))}}, http.StatusBadRequest, "The recipe has no full.liquid."},
		{"name that leaves the directory", []formPart{{field: "recipe", filename: "a.zip", content: string(zipFiles(t, map[string]string{"settings.yml": "name: x\n", "full.liquid": "x", "../escape": "x"}))}}, http.StatusBadRequest, `The zip has a file name that leaves its directory: "../escape".`},
		{"unreadable settings.yml", []formPart{{field: "recipe", filename: "a.zip", content: string(zipFiles(t, map[string]string{"settings.yml": "name: [", "full.liquid": "x"}))}}, http.StatusBadRequest, "The recipe's settings.yml is not valid YAML."},
		{"file one byte over the limit", []formPart{{field: "recipe", filename: "a.zip", content: strings.Repeat("a", recipe.MaxRecipeBytes+1)}}, http.StatusRequestEntityTooLarge, bigFile},
		{"file at the limit is not too big", []formPart{{field: "recipe", filename: "a.zip", content: strings.Repeat("a", recipe.MaxRecipeBytes)}}, http.StatusBadRequest, "The file is not a zip archive."},
		{"large field before the file", []formPart{{field: "note", content: strings.Repeat("a", recipe.MaxRecipeBytes+maxFormBody+1)}, {field: "recipe", filename: "a.zip", content: zipFile}}, http.StatusRequestEntityTooLarge, bigBody},
		{"large field after the file", []formPart{{field: "recipe", filename: "a.zip", content: noSettings}, {field: "note", content: strings.Repeat("a", recipe.MaxRecipeBytes+maxFormBody+1)}}, http.StatusBadRequest, "The zip has no settings.yml at its top, in src/, or in one top-level folder."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newRecipesUI(t)
			status, _, body := tu.upload(t, tu.signIn(t), tc.parts)
			// The message is in the upload panel, just above the file field.
			wantHTML := `<form method="post" action="/recipes#upload" enctype="multipart/form-data">` + "\n" +
				`<p class="error" role="alert">` + html.EscapeString(tc.want) + "</p>\n" + `<div class="field">`
			if status != tc.status || !strings.Contains(body, wantHTML) || !strings.Contains(body, "<h1>Recipes</h1>") || strings.Count(body, `role="alert"`) != 1 {
				t.Errorf("status %d, body %s; want %d with %q above the file field", status, body, tc.status, tc.want)
			}
			if got := len(listRecipes(t, tu)); got != 0 {
				t.Errorf("%d recipes after a refused upload", got)
			}
			left, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Errorf("a refused upload left %d files in the recipes directory", len(left))
			}
		})
	}
}

// TestRecipeUploadCutOff checks that a client that drops the connection part
// way through an upload gets a client error and leaves no row or file,
// whether it is cut off in a field before the file or in the file, and
// whether the body ends cleanly (as a test's reader does) or with a read
// error (as a server's does when the client hangs up).
func TestRecipeUploadCutOff(t *testing.T) {
	t.Parallel()
	body, ct := multipartBody(t,
		formPart{field: "note", content: "some words in a field"},
		formPart{field: "recipe", filename: "a.zip", content: string(testZip(t, "x"))})
	for _, tc := range []struct {
		name string
		cut  int
	}{
		{"in a field", strings.Index(body, "some words") + 5},
		{"in the file", strings.Index(body, "PK") + 40},
	} {
		for _, ending := range []struct {
			name string
			err  error
		}{{"EOF", nil}, {"unexpected EOF", io.ErrUnexpectedEOF}} {
			t.Run(tc.name+" then "+ending.name, func(t *testing.T) {
				t.Parallel()
				tu, dir := newRecipesUI(t)
				c := tu.signIn(t)
				var tail io.Reader = strings.NewReader("")
				if ending.err != nil {
					tail = iotest.ErrReader(ending.err)
				}
				req := httptest.NewRequestWithContext(t.Context(), "POST", "/recipes", io.MultiReader(strings.NewReader(body[:tc.cut]), tail))
				req.Header.Set("Content-Type", ct)
				req.Header.Set("Cookie", cookieHeader(c))
				w := tu.serve(req)
				const want = `<p class="error" role="alert">The upload was cut off or could not be read.</p>`
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
					t.Errorf("status %d, body %s; want 400 with %q", w.Code, w.Body, want)
				}
				if got := len(listRecipes(t, tu)); got != 0 {
					t.Errorf("%d recipes after a cut-off upload", got)
				}
				if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
					t.Errorf("a cut-off upload left %v in the recipes directory (%v)", left, err)
				}
			})
		}
	}
}

// hangUp is a request body whose client hangs up partway: it yields data,
// then ends the request's context and fails, as the server does when the
// connection drops.
type hangUp struct {
	data   io.Reader
	cancel context.CancelFunc
}

func (h *hangUp) Read(p []byte) (int, error) {
	n, err := h.data.Read(p)
	if errors.Is(err, io.EOF) {
		h.cancel()
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// TestRecipeUploadClientGone checks that an upload whose client hung up is
// not answered as a server failure. The request's context has ended, so the
// list the refusal would show cannot be read, and there is no one to show
// it to.
func TestRecipeUploadClientGone(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	c := tu.signIn(t)
	body, ct := multipartBody(t, formPart{field: "recipe", filename: "a.zip", content: string(testZip(t, "x"))})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cut := strings.Index(body, "PK") + 40
	req := httptest.NewRequestWithContext(ctx, "POST", "/recipes", &hangUp{data: strings.NewReader(body[:cut]), cancel: cancel})
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Cookie", cookieHeader(c))
	w := tu.serve(req)
	if w.Code == http.StatusInternalServerError || w.Body.Len() != 0 {
		t.Errorf("status %d, body %q; want nothing written for a client that hung up", w.Code, w.Body)
	}
	if got := len(listRecipes(t, tu)); got != 0 {
		t.Errorf("%d recipes after the client hung up", got)
	}
	if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
		t.Errorf("an upload whose client hung up left %v in the recipes directory (%v)", left, err)
	}
}

func TestRecipeUploadNotMultipart(t *testing.T) {
	t.Parallel()
	tu, _ := newRecipesUI(t)
	w := tu.post(t, tu.signIn(t), "/recipes", url.Values{"recipe": {"x"}})
	if body := w.Body.String(); w.Code != http.StatusBadRequest || !strings.Contains(body, "Choose a recipe zip file to upload.") {
		t.Errorf("status %d, body %s; want a 400 asking for a file", w.Code, body)
	}
}

func TestRecipePageAndAddInstance(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather <i>")
	writeSettings(t, dir, rec.ID)
	c := tu.signIn(t)
	path := "/recipes/" + id(rec.ID)
	body := tu.get(t, c, path).Body.String()
	for _, want := range []string{
		`<a class="back" href="/recipes">`, "<h1>Weather &lt;i&gt;</h1>", "<h2 id=\"add-title\">Add an instance</h2>",
		`<form method="post" action="/recipes/add-instance/` + id(rec.ID) + `">`,
		`placeholder="Weather &lt;i&gt;"`, "Leave empty to use the recipe's name.",
		`<option value="" selected>Device&#39;s palette</option>`, `<option value="bw">Black &amp; White (1-bit)</option>`,
		`<button class="btn btn-primary" type="submit">Add</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recipe page lacks %q", want)
		}
	}
	if strings.Contains(body, "maxlength") {
		t.Error("the name field has a length limit the domain does not")
	}
	// The palettes are the registry's, in its order.
	if first, second := strings.Index(body, `value="bw"`), strings.Index(body, `value="gray-4"`); first < 0 || second < first {
		t.Error("palettes are not in the registry's order")
	}
}

func TestRecipeAddInstance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		form         url.Values
		wantName     string
		wantPalette  string
		wantRedirect bool
	}{
		{"default name", url.Values{"name": {""}, "palette": {""}}, "Weather", "", true},
		{"trimmed name and palette", url.Values{"name": {"  Home  "}, "palette": {"bw"}}, "Home", "bw", true},
		{"fields left out", url.Values{}, "Weather", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, dir := newRecipesUI(t)
			rec := addRecipe(t, tu, "Weather")
			writeSettings(t, dir, rec.ID)
			w := tu.post(t, tu.signIn(t), "/recipes/add-instance/"+id(rec.ID), tc.form)
			if want := "/recipes#recipe-" + id(rec.ID); w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
				t.Fatalf("status %d to %q, want 303 to %q: %s", w.Code, w.Header().Get("Location"), want, w.Body)
			}
			insts := listInstances(t, tu)
			if len(insts) != 1 || insts[0].PluginInstance.Name != tc.wantName || insts[0].PluginInstance.Palette != tc.wantPalette || insts[0].PluginInstance.RecipeID != rec.ID {
				t.Errorf("instances = %+v, want one named %q with palette %q", insts, tc.wantName, tc.wantPalette)
			}
		})
	}
}

func TestRecipeAddInstanceRefused(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather")
	writeSettings(t, dir, rec.ID)
	w := tu.post(t, tu.signIn(t), "/recipes/add-instance/"+id(rec.ID), url.Values{"name": {"Mine <b>"}, "palette": {"nope"}})
	body := w.Body.String()
	for _, want := range []string{
		`<p class="error" role="alert">Unknown palette &#34;nope&#34;.</p>`, `value="Mine &lt;b&gt;"`, "<h1>Weather</h1>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refused form lacks %q:\n%s", want, body)
		}
	}
	if w.Code != http.StatusBadRequest || len(listInstances(t, tu)) != 0 {
		t.Errorf("status %d with %d instances, want 400 and none", w.Code, len(listInstances(t, tu)))
	}
}

func TestRecipeFormTooBig(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather")
	writeSettings(t, dir, rec.ID)
	w := tu.post(t, tu.signIn(t), "/recipes/add-instance/"+id(rec.ID), url.Values{"name": {strings.Repeat("a", maxInstanceFormBody)}})
	if w.Code != http.StatusRequestEntityTooLarge || len(listInstances(t, tu)) != 0 {
		t.Errorf("status %d with %d instances, want 413 and none", w.Code, len(listInstances(t, tu)))
	}
}

func TestRecipeNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, path, want string
	}{
		{"GET", "/recipes/99", "No such recipe"},
		{"GET", "/recipes/abc", "No such recipe"},
		{"POST", "/recipes/add-instance/99", "No such recipe"},
		{"POST", "/recipes/add-instance/abc", "No such recipe"},
		{"GET", "/recipes/instances/99/delete", "No such instance"},
		{"GET", "/recipes/instances/abc/delete", "No such instance"},
		{"POST", "/recipes/instances/99/delete", "No such instance"},
		{"POST", "/recipes/instances/abc/delete", "No such instance"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			tu, _ := newRecipesUI(t)
			newInstance(t, tu, addRecipe(t, tu, "Weather").ID, "Home", "")
			c := tu.signIn(t)
			w := tu.get(t, c, tc.path)
			if tc.method == "POST" {
				w = tu.post(t, c, tc.path, url.Values{"name": {"x"}})
			}
			body := w.Body.String()
			if w.Code != http.StatusNotFound || !strings.Contains(body, tc.want) || !strings.Contains(body, `<a href="/recipes">Back to Recipes</a>`) ||
				!strings.Contains(body, `<a href="/recipes" aria-current="page">`) {
				t.Errorf("status %d, body %s; want a 404 page for Recipes with %q", w.Code, body, tc.want)
			}
			if got := len(listInstances(t, tu)); got != 1 {
				t.Errorf("%d instances left, want 1", got)
			}
		})
	}
}

func TestInstanceDeleteConfirm(t *testing.T) {
	t.Parallel()
	tu, _ := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather")
	free := newInstance(t, tu, rec.ID, "Free <b>", "")
	used := newInstance(t, tu, rec.ID, "Busy & <i>", "")
	for _, name := range []string{"Zed", "Alpha"} {
		if _, err := addItemTo(t.Context(), tu.db, name, "", used.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	c := tu.signIn(t)
	cancel := `<a class="btn btn-secondary" href="/recipes#recipe-` + id(rec.ID) + `">`

	path := "/recipes/instances/" + id(free.ID) + "/delete"
	w := tu.get(t, c, path)
	body := w.Body.String()
	for _, want := range []string{
		"<h1>Remove instance</h1>", "Remove Free &lt;b&gt;?", "Its renders go too.",
		`<form method="post" action="` + path + `">`, `btn btn-primary btn-danger-solid`, cancel + "Cancel</a>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page lacks %q", want)
		}
	}
	if w.Code != http.StatusOK || strings.Contains(body, "<b>") {
		t.Errorf("status %d, or unescaped name", w.Code)
	}

	w = tu.get(t, c, "/recipes/instances/"+id(used.ID)+"/delete")
	body = w.Body.String()
	alpha, zed := playlistID(t, tu, "Alpha"), playlistID(t, tu, "Zed")
	for _, want := range []string{
		`Busy &amp; &lt;i&gt; is in <a href="/playlists#playlist-` + id(alpha) + `">Alpha</a>, <a href="/playlists#playlist-` + id(zed) + `">Zed</a>. Remove it from them first.`,
		cancel + "Back</a>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("in-use page lacks %q:\n%s", want, body)
		}
	}
	if w.Code != http.StatusOK || strings.Contains(body, "<form") && strings.Contains(body, `action="/recipes/instances/`) {
		t.Errorf("status %d, or the in-use page has a Remove form", w.Code)
	}
	if got := len(listInstances(t, tu)); got != 2 {
		t.Errorf("GET removed an instance: %d left", got)
	}
}

func TestInstanceDelete(t *testing.T) {
	t.Parallel()
	tu, _ := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather")
	free := newInstance(t, tu, rec.ID, "Free", "")
	used := newInstance(t, tu, rec.ID, "Busy", "")
	if _, err := addItemTo(t.Context(), tu.db, "Morning", "", used.ID, 0); err != nil {
		t.Fatal(err)
	}
	c := tu.signIn(t)

	w := tu.post(t, c, "/recipes/instances/"+id(used.ID)+"/delete", url.Values{})
	body := w.Body.String()
	if w.Code != http.StatusConflict || !strings.Contains(body, "Busy is in") || !strings.Contains(body, "Morning") {
		t.Errorf("in use: status %d, body %s; want 409 with the confirm page", w.Code, body)
	}
	if strings.Contains(body, "playlist remove") || strings.Contains(body, "item IDs") || strings.Contains(body, `class="btn btn-primary btn-danger-solid"`) {
		t.Errorf("the 409 page shows the domain error or a Remove button: %s", body)
	}
	if got := len(listInstances(t, tu)); got != 2 {
		t.Fatalf("an instance in a playlist was removed: %d left", got)
	}

	w = tu.post(t, c, "/recipes/instances/"+id(free.ID)+"/delete", url.Values{})
	if want := "/recipes#recipe-" + id(rec.ID); w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Fatalf("status %d to %q, want 303 to %q", w.Code, w.Header().Get("Location"), want)
	}
	if insts := listInstances(t, tu); len(insts) != 1 || insts[0].PluginInstance.ID != used.ID {
		t.Errorf("instances = %+v, want only the one in a playlist", insts)
	}
	// Removing it again finds nothing.
	if w := tu.post(t, c, "/recipes/instances/"+id(free.ID)+"/delete", url.Values{}); w.Code != http.StatusNotFound {
		t.Errorf("second removal: status %d, want 404", w.Code)
	}
}

func TestRecipesNeedSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/recipes"},
		{"POST", "/recipes"},
		{"GET", "/recipes/1"},
		{"POST", "/recipes/add-instance/1"},
		{"GET", "/recipes/instances/1/delete"},
		{"POST", "/recipes/instances/1/delete"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			tu, dir := newRecipesUI(t)
			rec := addRecipe(t, tu, "Weather")
			inst := newInstance(t, tu, rec.ID, "Home", "")
			body, ct := multipartBody(t, formPart{field: "recipe", filename: "a.zip", content: string(testZip(t, "x"))})
			for _, cookie := range []string{"", adminauth.CookieName + "=nope"} {
				h := []string{"Content-Type", ct}
				if cookie != "" {
					h = append(h, "Cookie", cookie)
				}
				b := ""
				if tc.method == "POST" {
					b = body
				}
				w := tu.do(t, tc.method, tc.path, b, h...)
				if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
					t.Errorf("cookie %q: status %d to %q, want 303 to /login", cookie, w.Code, w.Header().Get("Location"))
				}
			}
			if len(listRecipes(t, tu)) != 1 || len(listInstances(t, tu)) != 1 || listInstances(t, tu)[0].PluginInstance.ID != inst.ID {
				t.Error("a request without a session changed something")
			}
			if left, _ := os.ReadDir(dir); len(left) != 0 {
				t.Errorf("a request without a session wrote %d files", len(left))
			}
		})
	}
}

func TestRecipesCrossOrigin(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather")
	inst := newInstance(t, tu, rec.ID, "Home", "")
	c := tu.signIn(t)
	status, _, _ := tu.upload(t, c, []formPart{{field: "recipe", filename: "a.zip", content: string(testZip(t, "x"))}}, "Sec-Fetch-Site", "cross-site")
	if status != http.StatusForbidden {
		t.Errorf("cross-origin upload: status %d, want 403", status)
	}
	for _, path := range []string{"/recipes/add-instance/" + id(rec.ID), "/recipes/instances/" + id(inst.ID) + "/delete"} {
		if w := tu.post(t, c, path, url.Values{"name": {"x"}}, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
			t.Errorf("cross-origin post to %s: status %d, want 403", path, w.Code)
		}
	}
	if len(listRecipes(t, tu)) != 1 || len(listInstances(t, tu)) != 1 {
		t.Error("a cross-origin post changed something")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("a cross-origin upload wrote %d files", len(left))
	}
}

func TestRecipePagesNoInlineCode(t *testing.T) {
	t.Parallel()
	tu, dir := newRecipesUI(t)
	rec := addRecipe(t, tu, "Weather")
	writeSettings(t, dir, rec.ID)
	inst := newInstance(t, tu, rec.ID, "Home", "")
	c := tu.signIn(t)
	for _, path := range []string{"/recipes", "/recipes/" + id(rec.ID), "/recipes/instances/" + id(inst.ID), "/recipes/instances/" + id(inst.ID) + "/delete", "/recipes/99"} {
		body := tu.get(t, c, path).Body.String()
		for _, bad := range []string{"<script>", "style=", "<style", " onclick=", " onsubmit="} {
			if strings.Contains(body, bad) {
				t.Errorf("%s has inline %q, which the CSP refuses", path, bad)
			}
		}
		wantOneScript(t, body)
		if n := strings.Count(body, "<h1>"); n != 1 {
			t.Errorf("%s has %d h1 elements, want 1", path, n)
		}
		if strings.Count(body, `aria-current="page"`) != 1 || !strings.Contains(body, `<a href="/recipes" aria-current="page">`) {
			t.Errorf("%s does not mark Recipes as the current page", path)
		}
	}
}

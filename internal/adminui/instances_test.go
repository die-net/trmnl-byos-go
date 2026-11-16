package adminui

import (
	"bytes"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// formRecipe is a recipe with a field of every kind a form shows. Its
// fields, in order: city (field-0) and api_key (field-1) are required, and
// api_key looks like a credential. notes, count, units, mode, days, alerts,
// start, at, webhook_token and pass follow, then a display-only field.
const formRecipe = `name: Kitchen
custom_fields:
- keyname: city
  name: City
  field_type: string
  maxlength: 20
  placeholder: Oslo
  description: Where you are.
- keyname: api_key
  name: API key
  field_type: string
- keyname: notes
  name: Notes
  field_type: text
  optional: true
  rows: 6
  default: hello
- keyname: count
  name: Count
  field_type: number
  min: 1
  max: 10
  default: 3
- keyname: units
  name: Units
  field_type: select
  options: [Metric, Imperial]
  default: Metric
- keyname: mode
  name: Mode
  field_type: select
  optional: true
  options: [Fast, Slow]
- keyname: days
  name: Days
  field_type: select
  multiple: true
  optional: true
  options: [Mon, Tue, Wed]
- keyname: alerts
  name: Alerts
  field_type: boolean
  default: true
- keyname: start
  name: Start date
  field_type: date
  optional: true
- keyname: at
  name: At
  field_type: time
  optional: true
- keyname: webhook_token
  name: Webhook token
  field_type: string
  optional: true
- keyname: pass
  name: Password
  field_type: password
  optional: true
- keyname: about
  name: About
  field_type: author_bio
  description: Made by someone.
`

// testKeyB64 is a secrets key for the tests.
const testKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// newFormUI returns a test server with a secrets key, and a recipe whose
// settings.yml is settings.
func newFormUI(t *testing.T, settings string, withKey bool) (*testUI, db.Recipe) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{RecipesDir: dir}
	if withKey {
		key, err := secrets.Parse(testKeyB64)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Secrets = key
	}
	tu := newTestUI(t, cfg, true)
	rec := addRecipe(t, tu, "Kitchen")
	rdir := recipe.Dir(dir, rec.ID, 1)
	if err := os.MkdirAll(rdir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rdir, "settings.yml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return tu, rec
}

// good is a form that creates an instance of formRecipe.
func good(extra url.Values) url.Values {
	v := url.Values{"name": {"Home"}, "palette": {""}, "f.city": {"Oslo"}, "f.api_key": {"abcdef12345"}, "s.api_key": {"1"}}
	for k, vals := range extra {
		v[k] = vals
	}
	return v
}

// createdInstance posts the form and returns the one instance it made.
func createdInstance(t *testing.T, tu *testUI, rec db.Recipe, form url.Values) db.PluginInstance {
	t.Helper()
	w := tu.post(t, tu.signIn(t), "/recipes/add-instance/"+id(rec.ID), form)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: status %d: %s", w.Code, w.Body)
	}
	rows := listInstances(t, tu)
	if len(rows) != 1 {
		t.Fatalf("%d instances after a create, want 1", len(rows))
	}
	return rows[0].PluginInstance
}

// current reads an instance again, with its field values and marks.
func current(t *testing.T, tu *testUI, instID int64) (inst db.PluginInstance, fields map[string]recipe.Value, marks []string) {
	t.Helper()
	inst, err := tu.db.Read.GetInstance(t.Context(), instID)
	if err != nil {
		t.Fatal(err)
	}
	if fields, _, err = recipe.InstanceFields(&inst, tu.srv.secrets); err != nil {
		t.Fatal(err)
	}
	if marks, err = recipe.SecretKeys(&inst); err != nil {
		t.Fatal(err)
	}
	return inst, fields, marks
}

// publicOf returns what PublicFields shows of an instance.
func publicOf(t *testing.T, tu *testUI, inst *db.PluginInstance) (map[string]recipe.Value, []string) {
	t.Helper()
	fields, names, err := recipe.PublicFields(inst, tu.srv.secrets)
	if err != nil {
		t.Fatal(err)
	}
	return fields, names
}

func TestInstanceCreateForm(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	body := tu.get(t, tu.signIn(t), "/recipes/"+id(rec.ID)).Body.String()
	wantInOrder(t, body,
		`<label for="name">Name</label>`, `<label for="palette">Palette</label>`,
		// A required text field with a limit, a placeholder and a hint.
		`<label for="field-0">City <span class="req">Required</span></label>`,
		`<input id="field-0" name="f.city" type="text" maxlength="20" placeholder="Oslo" value="" required aria-describedby="field-0-hint0">`,
		`<p class="hint" id="field-0-hint0">Where you are.</p>`,
		// A field that looks like a credential starts marked secret.
		`<input id="field-1" name="f.api_key" type="text" autocomplete="off" required>`,
		`<input id="field-1-secret" name="s.api_key" type="checkbox" value="1" checked aria-describedby="field-1-secret-hint">`,
		`Keep this value secret`,
		// A text field with a default.
		"<textarea id=\"field-2\" name=\"f.notes\" rows=\"6\">\nhello</textarea>",
		`<input id="field-2-secret" name="s.notes" type="checkbox" value="1" aria-describedby="field-2-secret-hint">`,
		`<input id="field-3" name="f.count" type="number" step="any" min="1" max="10" value="3">`,
		`<select id="field-4" name="f.units">`, `<option value=""></option>`,
		`<option value="metric" selected>Metric</option>`, `<option value="imperial">Imperial</option>`,
		`<select id="field-5" name="f.mode">`, `<option value=""></option>`, `<option value="fast">Fast</option>`,
		`<legend>Days</legend>`,
		`<input id="field-6-o0" name="f.days" type="checkbox" value="mon">`, `<input id="field-6-o2" name="f.days" type="checkbox" value="wed">`,
		`<input id="field-7" name="f.alerts" type="checkbox" value="true" checked>`,
		`<input id="field-8" name="f.start" type="date" value="">`,
		`<input id="field-9" name="f.at" type="time" value="">`,
		// A field that is marked secret, once it is not a credential's name.
		`<input id="field-10" name="f.webhook_token" type="text" autocomplete="off">`,
		`<input id="field-11" name="f.pass" type="password" autocomplete="new-password" aria-describedby="field-11-hint0">`,
		`<p class="field-title">About</p>`, `Made by someone.`,
		`>Add</button>`,
	)
	for _, bad := range []string{`name="s.count"`, `name="s.units"`, `name="s.alerts"`, `name="s.pass"`, `name="f.about"`, `name="c.`} {
		if strings.Contains(body, bad) {
			t.Errorf("create form has %s", bad)
		}
	}
	if strings.Contains(body, `name="f.pass" type="password" autocomplete="new-password" value`) {
		t.Error("the password input has a value")
	}
	if strings.Contains(body, "Check the fields below.") {
		t.Error("a fresh form has an error line")
	}
	if strings.Contains(body, "A value is stored") {
		t.Error("the create form talks about stored values")
	}
}

func TestInstanceCreate(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	form := good(url.Values{
		"f.notes": {"line one\nline two"}, "f.count": {"7"}, "f.units": {"imperial"}, "f.days": {"mon", "wed"},
		"f.alerts": {"true"}, "f.start": {"2026-02-03"}, "f.at": {"07:30"}, "f.webhook_token": {"plain-token-1"}, "f.pass": {"hunter22"},
	})
	inst := createdInstance(t, tu, rec, form)
	if inst.Name != "Home" {
		t.Errorf("name = %q", inst.Name)
	}
	_, all, marks := current(t, tu, inst.ID)
	one := func(k string) recipe.Value { return all[k] }
	for k, want := range map[string]string{
		"city": "Oslo", "api_key": "abcdef12345", "notes": "line one\nline two", "count": "7", "units": "imperial",
		"alerts": "true", "start": "2026-02-03", "at": "07:30", "webhook_token": "plain-token-1", "pass": "hunter22",
	} {
		if got := one(k); got.IsList() || got.One != want {
			t.Errorf("%s = %+v, want %q", k, got, want)
		}
	}
	if got := one("days"); strings.Join(got.Many, ",") != "mon,wed" {
		t.Errorf("days = %+v", got)
	}
	// Only api_key is marked, and the plain values hold neither it nor the password.
	if strings.Join(marks, ",") != "api_key" {
		t.Errorf("marks = %v, want [api_key]", marks)
	}
	plain, names := publicOf(t, tu, &inst)
	if _, ok := plain["api_key"]; ok {
		t.Error("a marked value is plain")
	}
	if _, ok := plain["pass"]; ok {
		t.Error("a password is plain")
	}
	if plain["webhook_token"].One != "plain-token-1" || plain["city"].One != "Oslo" {
		t.Errorf("plain = %+v", plain)
	}
	if strings.Join(names, ",") != "api_key,pass" {
		t.Errorf("secret names = %v, want [api_key pass]", names)
	}
}

func TestInstanceCreateBoolean(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := createdInstance(t, tu, rec, good(nil))
	_, all, _ := current(t, tu, inst.ID)
	// A box that is not ticked posts nothing, and is saved as false.
	if all["alerts"].One != "false" {
		t.Errorf("alerts = %+v, want false", all["alerts"])
	}
}

func TestInstanceCreateRefused(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	c := tu.signIn(t)
	form := url.Values{
		"name": {"Mine"}, "palette": {"bw"}, "f.city": {""}, "f.api_key": {"abc"}, "s.api_key": {"1"},
		"f.notes": {"keep <me>"}, "f.count": {"many"}, "f.units": {"imperial"}, "f.days": {"tue"}, "f.alerts": {"true"},
		"f.webhook_token": {"short"}, "s.webhook_token": {"1"}, "f.pass": {"abc"},
	}
	w := tu.post(t, c, "/recipes/add-instance/"+id(rec.ID), form)
	body := w.Body.String()
	if w.Code != http.StatusBadRequest || len(listInstances(t, tu)) != 0 {
		t.Fatalf("status %d with %d instances, want 400 and none", w.Code, len(listInstances(t, tu)))
	}
	wantInOrder(t, body,
		`<p class="error" role="alert">Check the fields below.</p>`,
		`value="Mine"`, `<option value="bw" selected>`,
		`<label for="field-0">City`, `aria-invalid="true"`, `<p class="field-error" id="field-0-err">City is required.</p>`,
		`<p class="field-error" id="field-1-err">API key must be at least 6 characters to be secret.</p>`,
		"<textarea id=\"field-2\" name=\"f.notes\" rows=\"6\">\nkeep &lt;me&gt;</textarea>",
		`value="many"`, `<p class="field-error" id="field-3-err">Count must be a number.</p>`,
		`<option value="imperial" selected>`,
		`<input id="field-6-o1" name="f.days" type="checkbox" value="tue" checked>`,
		`<input id="field-7" name="f.alerts" type="checkbox" value="true" checked`,
		`<p class="field-error" id="field-10-err">Webhook token must be at least 6 characters to be secret.</p>`,
		`<p class="field-error" id="field-11-err">Password must be at least 6 characters to be secret.</p>`,
	)
	// A secret value is never echoed, and its block says to enter it again.
	for _, secret := range []string{`value="abc"`, `value="short"`, ">abc<", ">short<"} {
		if strings.Contains(body, secret) {
			t.Errorf("the page echoes a secret as %s", secret)
		}
	}
	if n := strings.Count(body, "Enter the secret value again."); n != 3 {
		t.Errorf("%d blocks ask for the secret again, want 3", n)
	}
	// The ticked boxes stay ticked.
	if !strings.Contains(body, `<input id="field-1-secret" name="s.api_key" type="checkbox" value="1" checked`) {
		t.Error("the secret box was not kept")
	}
}

func TestInstanceCreateFieldErrorElsewhere(t *testing.T) {
	t.Parallel()
	// An error for a keyname the form does not show comes at the top.
	tu, rec := newFormUI(t, formRecipe, true)
	body := tu.post(t, tu.signIn(t), "/recipes/add-instance/"+id(rec.ID), good(url.Values{"s.units": {"1"}, "f.about": {"x"}})).Body.String()
	// s.units is ignored, since units cannot be marked, so the form is saved.
	if len(listInstances(t, tu)) != 1 {
		t.Errorf("a mark of a field that is not text was not ignored: %s", body)
	}
	src := &fieldSource{errs: recipe.FieldErrors{"size": "is too big", "city": "is required"}}
	views, orphans, shown := src.build([]recipe.Field{{Keyname: "city", Label: "City", Type: "string"}})
	if shown != 1 || len(orphans) != 1 || orphans[0] != "size: is too big." || views[0].Error != "City is required." {
		t.Errorf("shown %d, orphans %q, error %q", shown, orphans, views[0].Error)
	}
}

func TestInstanceCreateNoKey(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, false)
	c := tu.signIn(t)
	w := tu.post(t, c, "/recipes/add-instance/"+id(rec.ID), good(nil))
	body := w.Body.String()
	if w.Code != http.StatusBadRequest || !strings.Contains(body, "The server has no secrets key, so a secret value cannot be saved or kept. Set "+secrets.EnvVar) || len(listInstances(t, tu)) != 0 {
		t.Fatalf("status %d with %d instances: %s", w.Code, len(listInstances(t, tu)), body)
	}
	if strings.Contains(body, "abcdef12345") {
		t.Error("the page echoes the secret")
	}
	// With nothing secret the form still saves.
	w = tu.post(t, c, "/recipes/add-instance/"+id(rec.ID), url.Values{"f.city": {"Oslo"}, "f.api_key": {"abcdef12345"}})
	if w.Code != http.StatusSeeOther || len(listInstances(t, tu)) != 1 {
		t.Errorf("a plain save: status %d with %d instances", w.Code, len(listInstances(t, tu)))
	}
}

func TestInstanceEditForm(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	c := tu.signIn(t)
	inst := createdInstance(t, tu, rec, good(url.Values{
		"f.notes": {"stored notes"}, "f.count": {"9"}, "f.units": {"imperial"}, "f.days": {"tue"}, "f.alerts": {"true"},
		"f.webhook_token": {"plain-token-1"}, "s.webhook_token": {"1"}, "f.pass": {"hunter22"},
	}))
	body := tu.get(t, c, "/recipes/instances/"+id(inst.ID)).Body.String()
	wantInOrder(t, body,
		`href="/recipes#recipe-`+id(rec.ID)+`"`, `<h1>Home</h1>`, `<form method="post" action="/recipes/instances/`+id(inst.ID)+`">`,
		`placeholder="Kitchen" value="Home"`,
		`name="f.city" type="text" maxlength="20" placeholder="Oslo" value="Oslo" aria-describedby="field-0-hint0">`,
		// A secret field is empty, says a value is stored, and can be cleared unless it is required.
		`<input id="field-1" name="f.api_key" type="text" autocomplete="off" aria-describedby="field-1-hint0">`,
		`A value is stored. Leave this empty to keep it.`,
		`name="s.api_key" type="checkbox" value="1" checked`,
		"<textarea id=\"field-2\" name=\"f.notes\" rows=\"6\">\nstored notes</textarea>",
		`value="9"`, `<option value="imperial" selected>Imperial</option>`,
		`<input id="field-6-o1" name="f.days" type="checkbox" value="tue" checked>`,
		`<input id="field-7" name="f.alerts" type="checkbox" value="true" checked`,
		`<input id="field-10" name="f.webhook_token" type="text" autocomplete="off" aria-describedby="field-10-hint0">`,
		`name="c.webhook_token"`, `Remove the stored value`,
		`<input id="field-11" name="f.pass" type="password" autocomplete="new-password" aria-describedby="field-11-hint0">`,
		`name="c.pass"`, `>Save</button>`,
	)
	for _, secret := range []string{"abcdef12345", "plain-token-1", "hunter22"} {
		if strings.Contains(body, secret) {
			t.Errorf("the edit page shows the secret %q", secret)
		}
	}
	if strings.Contains(body, `name="c.api_key"`) || strings.Contains(body, `name="c.city"`) {
		t.Error("a required field can be cleared")
	}
	if strings.Contains(body, "cannot be read") {
		t.Error("readable secrets are said to be unreadable")
	}
	// The edit form never has the required attribute: an update checks only
	// what it changes.
	if strings.Contains(body, " required") {
		t.Error("the edit form has the required attribute")
	}
	create := tu.get(t, c, "/recipes/"+id(rec.ID)).Body.String()
	if !strings.Contains(create, `value="" required`) {
		t.Error("the create form lacks the required attribute on a required text field")
	}
}

// savedInstance creates an instance of formRecipe with a secret api_key, a
// plain webhook_token and a secret pass, which is what the save tests start
// from.
func savedInstance(t *testing.T, tu *testUI, rec db.Recipe) db.PluginInstance {
	t.Helper()
	return createdInstance(t, tu, rec, good(url.Values{"f.webhook_token": {"plain-token-1"}, "f.pass": {"hunter22"}, "f.alerts": {"true"}}))
}

// instanceStateOf returns the recipe.InstanceState of an instance as it is
// stored now, which is what its edit page sends back as seen.
func instanceStateOf(t *testing.T, tu *testUI, instID int64) string {
	t.Helper()
	inst, err := tu.db.Read.GetInstance(t.Context(), instID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := recipe.InstanceState(&inst, 1)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// postEdit posts the edit form of an instance. It adds the state the page
// would send as seen unless the form has a seen field of its own, where an
// empty one is a form that sends none.
func postEdit(t *testing.T, tu *testUI, c *http.Cookie, instID int64, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := form["seen"]; !ok {
		form.Set("seen", instanceStateOf(t, tu, instID))
	}
	return tu.post(t, c, "/recipes/instances/"+id(instID), form)
}

// editForm is what the edit page of savedInstance posts back when nothing
// was changed, with the changes applied.
func editForm(changes url.Values) url.Values {
	v := url.Values{"name": {"Home"}, "palette": {""}, "f.city": {"Oslo"}, "f.api_key": {""}, "s.api_key": {"1"}, "f.alerts": {"true"}, "f.webhook_token": {"plain-token-1"}}
	for k, vals := range changes {
		if len(vals) == 0 {
			delete(v, k)
			continue
		}
		v[k] = vals
	}
	return v
}

func TestInstanceSave(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		change  url.Values
		check   func(t *testing.T, all map[string]recipe.Value, marks []string)
		wantErr string
	}{
		{
			name:   "plain value and name",
			change: url.Values{"f.city": {"Bergen"}, "name": {"  Work "}},
			check: func(t *testing.T, all map[string]recipe.Value, _ []string) {
				if all["city"].One != "Bergen" || all["api_key"].One != "abcdef12345" || all["pass"].One != "hunter22" {
					t.Errorf("fields = %+v", all)
				}
			},
		},
		{
			name:   "blank secret is kept",
			change: url.Values{"f.api_key": {""}, "f.pass": {""}},
			check: func(t *testing.T, all map[string]recipe.Value, _ []string) {
				if all["api_key"].One != "abcdef12345" || all["pass"].One != "hunter22" {
					t.Errorf("fields = %+v", all)
				}
			},
		},
		{
			name:   "new secret value",
			change: url.Values{"f.api_key": {"another-key-9"}},
			check: func(t *testing.T, all map[string]recipe.Value, _ []string) {
				if all["api_key"].One != "another-key-9" {
					t.Errorf("api_key = %+v", all["api_key"])
				}
			},
		},
		{
			name:   "clear a secret",
			change: url.Values{"c.pass": {"1"}},
			check: func(t *testing.T, all map[string]recipe.Value, _ []string) {
				if _, ok := all["pass"]; ok || all["api_key"].One != "abcdef12345" {
					t.Errorf("fields = %+v", all)
				}
			},
		},
		{
			name:   "clear a plain value",
			change: url.Values{"f.webhook_token": {""}},
			check: func(t *testing.T, all map[string]recipe.Value, _ []string) {
				if _, ok := all["webhook_token"]; ok {
					t.Errorf("webhook_token is still set: %+v", all)
				}
			},
		},
		{
			name:   "mark moves a value to the secrets",
			change: url.Values{"s.webhook_token": {"1"}},
			check: func(t *testing.T, all map[string]recipe.Value, marks []string) {
				if all["webhook_token"].One != "plain-token-1" || strings.Join(marks, ",") != "api_key,webhook_token" {
					t.Errorf("fields = %+v, marks = %v", all, marks)
				}
			},
		},
		{
			name:   "unmark with a new value",
			change: url.Values{"s.api_key": nil, "f.api_key": {"plain-now-1"}},
			check: func(t *testing.T, all map[string]recipe.Value, marks []string) {
				if all["api_key"].One != "plain-now-1" || len(marks) != 0 {
					t.Errorf("fields = %+v, marks = %v", all, marks)
				}
			},
		},
		{
			name:   "unmark with a clear",
			change: url.Values{"s.api_key": nil, "c.api_key": {"1"}},
			// A required field cannot be cleared, so the page has no box for it, but a post can.
			wantErr: "API key is required.",
		},
		{
			name:    "unmark with no value",
			change:  url.Values{"s.api_key": nil},
			wantErr: "API key is secret. Give it a new value to make it plain.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, formRecipe, true)
			inst := savedInstance(t, tu, rec)
			c := tu.signIn(t)
			before, beforeAll, beforeMarks := current(t, tu, inst.ID)
			w := postEdit(t, tu, c, inst.ID, editForm(tc.change))
			after, all, marks := current(t, tu, inst.ID)
			if tc.wantErr == "" {
				if want := "/recipes#recipe-" + id(rec.ID); w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
					t.Fatalf("status %d to %q, want 303 to %q: %s", w.Code, w.Header().Get("Location"), want, w.Body)
				}
				tc.check(t, all, marks)
				return
			}
			body := w.Body.String()
			if w.Code != http.StatusBadRequest || !strings.Contains(body, tc.wantErr) || !strings.Contains(body, "Check the fields below.") {
				t.Fatalf("status %d, want 400 with %q: %s", w.Code, tc.wantErr, body)
			}
			if after.Name != before.Name || after.FieldValues != before.FieldValues || !bytes.Equal(after.SecretValues, before.SecretValues) || len(all) != len(beforeAll) || len(marks) != len(beforeMarks) {
				t.Error("a refused save changed the instance")
			}
			for _, secret := range []string{"abcdef12345", "hunter22"} {
				if strings.Contains(body, secret) {
					t.Errorf("the refusal shows the secret %q", secret)
				}
			}
		})
	}
}

func TestInstanceSaveName(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := savedInstance(t, tu, rec)
	w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"name": {"  Work "}, "palette": {"bw"}}))
	got, _, _ := current(t, tu, inst.ID)
	if w.Code != http.StatusSeeOther || got.Name != "Work" || got.Palette != "bw" {
		t.Errorf("status %d, name %q, palette %q", w.Code, got.Name, got.Palette)
	}
	if got.SettingsAt != inst.SettingsAt {
		t.Error("a rename made the instance render again")
	}
}

// TestInstanceUnmarkRefusedPage checks the page a refused unmark comes back
// as: the untick is kept, the two ways out are offered, and nothing of the
// secret is shown.
func TestInstanceUnmarkRefusedPage(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := createdInstance(t, tu, rec, good(url.Values{"f.webhook_token": {"plain-token-1"}, "s.webhook_token": {"1"}}))
	w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"s.webhook_token": nil, "f.webhook_token": {""}}))
	body := w.Body.String()
	wantInOrder(t, body,
		`<label for="field-10">Webhook token</label>`, `<input id="field-10" name="f.webhook_token" type="text" value="" aria-describedby="field-10-hint0 field-10-err" aria-invalid="true">`,
		`A secret value is stored and cannot be shown.`,
		`<p class="field-error" id="field-10-err">Webhook token is secret. Give it a new value, or tick Remove the stored value, to make it plain.</p>`,
		`<input id="field-10-secret" name="s.webhook_token" type="checkbox" value="1" aria-describedby="field-10-secret-hint">`,
		`<input id="field-10-clear" name="c.webhook_token" type="checkbox" value="1" aria-describedby="field-10-clear-hint">`,
	)
	if w.Code != http.StatusBadRequest || strings.Contains(body, "plain-token-1") {
		t.Errorf("status %d, or the secret is shown", w.Code)
	}
	// The page's own way out works: a clear with the box unticked.
	w = postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"s.webhook_token": nil, "f.webhook_token": {""}, "c.webhook_token": {"1"}}))
	_, all, marks := current(t, tu, inst.ID)
	if _, ok := all["webhook_token"]; w.Code != http.StatusSeeOther || ok || strings.Join(marks, ",") != "api_key" {
		t.Errorf("status %d, fields %+v, marks %v", w.Code, all, marks)
	}
}

func TestInstanceMultiAndBoolean(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := createdInstance(t, tu, rec, good(url.Values{"f.days": {"mon", "tue"}, "f.alerts": {"true"}}))
	c := tu.signIn(t)
	post := func(changes url.Values) map[string]recipe.Value {
		t.Helper()
		form := editForm(changes)
		if w := postEdit(t, tu, c, inst.ID, form); w.Code != http.StatusSeeOther {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		_, all, _ := current(t, tu, inst.ID)
		return all
	}
	all := post(url.Values{"f.days": {"mon", "tue"}})
	if strings.Join(all["days"].Many, ",") != "mon,tue" || all["alerts"].One != "true" {
		t.Errorf("unchanged save: %+v", all)
	}
	// An unticked boolean is saved as false, and a multi-select with no box ticked is cleared.
	all = post(url.Values{"f.alerts": nil})
	if all["alerts"].One != "false" || strings.Join(all["days"].Many, ",") != "" {
		t.Errorf("after unticking: alerts %+v, days %+v", all["alerts"], all["days"])
	}
	if _, ok := all["days"]; ok {
		t.Error("an empty multi-select is stored")
	}
	all = post(url.Values{"f.days": {"wed"}, "f.alerts": {"true"}})
	if strings.Join(all["days"].Many, ",") != "wed" || all["alerts"].One != "true" {
		t.Errorf("after ticking: %+v", all)
	}
}

func TestInstanceNoKey(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, false)
	c := tu.signIn(t)
	// An instance with no secrets saves, and a rename saves.
	inst := createdInstance(t, tu, rec, url.Values{"f.city": {"Oslo"}, "f.api_key": {"abcdef12345"}})
	w := postEdit(t, tu, c, inst.ID, url.Values{"name": {"Work"}, "f.city": {"Bergen"}, "f.api_key": {"abcdef12345"}, "f.alerts": {"false"}})
	got, _, _ := current(t, tu, inst.ID)
	if w.Code != http.StatusSeeOther || got.Name != "Work" {
		t.Fatalf("a plain save: status %d, name %q: %s", w.Code, got.Name, w.Body)
	}
	// Setting a secret needs the key.
	w = postEdit(t, tu, c, inst.ID, url.Values{"name": {"Work"}, "f.city": {"Bergen"}, "f.api_key": {"abcdef12345"}, "s.api_key": {"1"}})
	body := w.Body.String()
	if w.Code != http.StatusBadRequest || !strings.Contains(body, "The server has no secrets key") || strings.Contains(body, "abcdef12345") {
		t.Errorf("status %d: %s", w.Code, body)
	}
}

// TestInstanceSealedWithoutKey checks that an instance whose secrets cannot
// be opened shows its plain values, says so once, claims nothing about its
// secrets, and still saves a new name.
func TestInstanceSealedWithoutKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		withKey bool
	}{
		{"no key", false},
		{"another key", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, formRecipe, true)
			inst := savedInstance(t, tu, rec)
			c := tu.signIn(t)
			tu.srv.secrets = nil
			if tc.withKey {
				other, err := secrets.Parse("HR4fICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw=")
				if err != nil {
					t.Fatal(err)
				}
				tu.srv.secrets = other
			}
			w := tu.get(t, c, "/recipes/instances/"+id(inst.ID))
			body := w.Body.String()
			if w.Code != http.StatusOK || strings.Count(body, "The stored secret values cannot be read") != 1 || !strings.Contains(body, `value="Oslo"`) {
				t.Fatalf("status %d: %s", w.Code, body)
			}
			for _, claim := range []string{"A value is stored", "No value is stored", "Remove the stored value", "abcdef12345"} {
				if strings.Contains(body, claim) {
					t.Errorf("the page says %q with unreadable secrets", claim)
				}
			}
			// A form that changes only the name saves, since nothing is opened.
			w = postEdit(t, tu, c, inst.ID, editForm(url.Values{"name": {"Work"}}))
			got, err := tu.db.Read.GetInstance(t.Context(), inst.ID)
			if err != nil || w.Code != http.StatusSeeOther || got.Name != "Work" || !bytes.Equal(got.SecretValues, inst.SecretValues) {
				t.Errorf("rename: status %d, name %q, err %v", w.Code, got.Name, err)
			}
			// A change of a value has to open them, which is refused with a reason.
			w = postEdit(t, tu, c, inst.ID, editForm(url.Values{"f.city": {"Bergen"}}))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `role="alert"`) {
				t.Errorf("value change: status %d: %s", w.Code, w.Body)
			}
		})
	}
}

func TestInstanceLargeValue(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := createdInstance(t, tu, rec, good(nil))
	c := tu.signIn(t)
	big := strings.Repeat("x", 20<<10)
	w := postEdit(t, tu, c, inst.ID, editForm(url.Values{"f.notes": {big}}))
	_, all, _ := current(t, tu, inst.ID)
	if w.Code != http.StatusSeeOther || all["notes"].One != big {
		t.Fatalf("status %d, stored %d bytes", w.Code, len(all["notes"].One))
	}
	// Line breaks go over the wire as CR LF, each byte three times as long
	// again once encoded, and are stored as LF.
	breaks := "text" + strings.Repeat("\r\n", 10<<10)
	w = postEdit(t, tu, c, inst.ID, editForm(url.Values{"f.notes": {breaks}}))
	_, all, _ = current(t, tu, inst.ID)
	if want := "text" + strings.Repeat("\n", 10<<10); w.Code != http.StatusSeeOther || all["notes"].One != want {
		t.Fatalf("line breaks: status %d, stored %d bytes", w.Code, len(all["notes"].One))
	}
	// Just over the bound is refused, on both forms, and nothing changes.
	tooBig := url.Values{"name": {strings.Repeat("a", maxInstanceFormBody)}}
	for _, path := range []string{"/recipes/instances/" + id(inst.ID), "/recipes/add-instance/" + id(rec.ID)} {
		if w := tu.post(t, c, path, tooBig); w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("POST %s: status %d, want 413", path, w.Code)
		}
	}
	if len(listInstances(t, tu)) != 1 {
		t.Error("a refused body made an instance")
	}
}

func TestInstanceEscaping(t *testing.T) {
	t.Parallel()
	const evil = `<b>"x"</b>`
	settings := "name: Kitchen\ncustom_fields:\n- keyname: '" + `a<b>"q` + "'\n  name: 'L<b>\"abel'\n  field_type: select\n  description: 'D<i>'\n  options: ['O<u>\"1', Two]\n" +
		"- keyname: 'k<b>2'\n  name: 'K<b>\"2'\n  field_type: string\n  optional: true\n"
	tu, rec := newFormUI(t, settings, true)
	c := tu.signIn(t)
	form := url.Values{"f.k<b>2": {evil}, "f.a<b>\"q": {"o<u>\"1"}}
	inst := createdInstance(t, tu, rec, form)
	for _, path := range []string{"/recipes/" + id(rec.ID), "/recipes/instances/" + id(inst.ID)} {
		body := tu.get(t, c, path).Body.String()
		if strings.Contains(body, `<option value=""></option>`) {
			t.Errorf("%s: a required select has a blank choice", path)
		}
		for _, bad := range []string{"<b>", "<i>", "<u>", `"q"`} {
			if strings.Contains(body, bad) {
				t.Errorf("%s has unescaped %q", path, bad)
			}
		}
		wantInOrder(t, body, `L&lt;b&gt;&#34;abel`, `O&lt;u&gt;&#34;1`, `D&lt;i&gt;`)
		if path != "/recipes/"+id(rec.ID) {
			wantInOrder(t, body, `name="f.a&lt;b&gt;&#34;q"`, `value="&lt;b&gt;&#34;x&#34;&lt;/b&gt;"`)
		}
	}
	// An error names the field by its label, escaped.
	w := postEdit(t, tu, c, inst.ID, url.Values{"f.a<b>\"q": {"nope"}, "f.k<b>2": {evil}})
	if body := w.Body.String(); w.Code != http.StatusBadRequest || !strings.Contains(body, `L&lt;b&gt;&#34;abel is not one of the options.`) || strings.Contains(body, "<b>") {
		t.Errorf("status %d: %s", w.Code, body)
	}
}

func TestInstanceEditNotFoundAndSession(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := savedInstance(t, tu, rec)
	c := tu.signIn(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/recipes/instances/99"},
		{"GET", "/recipes/instances/abc"},
		{"POST", "/recipes/instances/99"},
		{"POST", "/recipes/instances/abc"},
	} {
		t.Run("not found "+tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			rr := tu.get(t, c, tc.path)
			if tc.method == "POST" {
				rr = tu.post(t, c, tc.path, url.Values{"name": {"x"}})
			}
			if body := rr.Body.String(); rr.Code != http.StatusNotFound || !strings.Contains(body, "No such instance") {
				t.Errorf("status %d: %s", rr.Code, body)
			}
		})
	}
	for _, method := range []string{"GET", "POST"} {
		t.Run("signed out "+method, func(t *testing.T) {
			t.Parallel()
			rr := tu.do(t, method, "/recipes/instances/"+id(inst.ID), "name=x")
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
				t.Errorf("status %d to %q, want 303 to /login", rr.Code, rr.Header().Get("Location"))
			}
		})
	}
	if got, _, _ := current(t, tu, inst.ID); got.Name != inst.Name {
		t.Error("a request without a session changed the instance")
	}
}

func TestRecipesListEditLink(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := savedInstance(t, tu, rec)
	body := tu.get(t, tu.signIn(t), "/recipes").Body.String()
	wantInOrder(t, body, `<a class="btn btn-secondary" href="/recipes/instances/`+id(inst.ID)+`" aria-label="Edit Home">Edit</a>`, `Remove</a>`)
}

// hiddenSeen returns the value of the seen field of a page.
func hiddenSeen(t *testing.T, body string) string {
	t.Helper()
	const prefix = `<input type="hidden" name="seen" value="`
	_, rest, ok := strings.Cut(body, prefix)
	if !ok {
		t.Fatalf("page has no seen field:\n%s", body)
	}
	seen, _, _ := strings.Cut(rest, `"`)
	return seen
}

func TestInstanceSeenOnPage(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := savedInstance(t, tu, rec)
	body := tu.get(t, tu.signIn(t), "/recipes/instances/"+id(inst.ID)).Body.String()
	if got, want := hiddenSeen(t, body), instanceStateOf(t, tu, inst.ID); got != want || got == "" {
		t.Errorf("seen = %q, want the instance's state %q", got, want)
	}
}

// TestInstanceSaveRebuilds checks that a saved form asks the scheduler to
// look for due renders at once, and that a refused one does not.
func TestInstanceSaveRebuilds(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	woken := 0
	tu.db.OnChange = func() { woken++ }
	inst := savedInstance(t, tu, rec)
	w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"seen": {""}, "name": {"Work"}}))
	if w.Code != http.StatusBadRequest || woken != 0 {
		t.Fatalf("refused save: status %d, %d wake-ups; want 400 and none", w.Code, woken)
	}
	w = postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"name": {"Work"}}))
	if w.Code != http.StatusSeeOther || woken != 1 {
		t.Errorf("saved form: status %d, %d wake-ups; want 303 and 1", w.Code, woken)
	}
}

func TestInstanceSaveNoSeen(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := savedInstance(t, tu, rec)
	w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"seen": {""}, "name": {"Work"}}))
	got, _, _ := current(t, tu, inst.ID)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Reload the page and try again.") || got.Name != "Home" {
		t.Errorf("status %d, name %q: %s", w.Code, got.Name, w.Body)
	}
}

func TestInstanceSaveStale(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name   string
		change recipe.InstanceUpdate
		// noted are the blocks that say their value differs from the stored
		// one: the typed name, and city, are always among them.
		noted []string
	}{
		{"rename", recipe.InstanceUpdate{Name: str("Renamed")}, []string{"name", "field-0"}},
		{"palette", recipe.InstanceUpdate{Palette: str("bw")}, []string{"name", "palette", "field-0"}},
		// The token is a field the admin does not touch, which the other change altered.
		{"plain value", recipe.InstanceUpdate{Values: map[string]recipe.Value{"city": {One: "Bergen"}, "webhook_token": {One: "changed-elsewhere"}}}, []string{"name", "field-0", "field-10"}},
		// The token is sealed now, so it is neither compared nor shown.
		{"mark", recipe.InstanceUpdate{Secret: map[string]bool{"webhook_token": true}}, []string{"name", "field-0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, formRecipe, true)
			inst := savedInstance(t, tu, rec)
			c := tu.signIn(t)
			old := instanceStateOf(t, tu, inst.ID)
			if _, err := recipe.UpdateInstance(t.Context(), tu.db, tu.srv.secrets, tu.srv.recipesDir, inst.ID, tc.change, tu.now); err != nil {
				t.Fatal(err)
			}
			cur := instanceStateOf(t, tu, inst.ID)
			if cur == old {
				t.Fatal("the change did not change the state")
			}
			before, beforeAll, beforeMarks := current(t, tu, inst.ID)

			typed := url.Values{"seen": {old}, "name": {"Typed name"}, "f.city": {"Typed city"}}
			w := postEdit(t, tu, c, inst.ID, editForm(typed))
			body := w.Body.String()
			after, all, marks := current(t, tu, inst.ID)
			if w.Code != http.StatusConflict || !strings.Contains(body, instanceChanged) {
				t.Fatalf("status %d, want 409 with the changed sentence: %s", w.Code, body)
			}
			if after.Name != before.Name || after.FieldValues != before.FieldValues || after.Palette != before.Palette ||
				!bytes.Equal(after.SecretValues, before.SecretValues) || len(all) != len(beforeAll) || len(marks) != len(beforeMarks) {
				t.Error("a stale save changed the instance")
			}
			if !strings.Contains(body, `placeholder="Kitchen" value="Typed name"`) || !strings.Contains(body, `value="Typed city"`) {
				t.Errorf("the page lost the typed values: %s", body)
			}
			if got := hiddenSeen(t, body); got != cur {
				t.Errorf("seen = %q, want the current state %q", got, cur)
			}
			// Each field that differs from what is stored now says so, and the others do not.
			if n := strings.Count(body, differsText); n != len(tc.noted) {
				t.Errorf("%d notes, want %d (%v)", n, len(tc.noted), tc.noted)
			}
			for _, name := range []string{"name", "palette", "field-0", "field-2", "field-3", "field-10"} {
				want := slices.Contains(tc.noted, name)
				if got := strings.Contains(blockOf(t, body, name), differsText); got != want {
					t.Errorf("block %s has the note = %v, want %v", name, got, want)
				}
			}

			// Saving again with the page's own seen applies the typed values.
			typed.Set("seen", hiddenSeen(t, body))
			w = postEdit(t, tu, c, inst.ID, editForm(typed))
			saved, all, _ := current(t, tu, inst.ID)
			if w.Code != http.StatusSeeOther || saved.Name != "Typed name" || all["city"].One != "Typed city" {
				t.Errorf("second save: status %d, name %q, city %q: %s", w.Code, saved.Name, all["city"].One, w.Body)
			}
		})
	}
}

// blockOf returns the part of a page from the label of a field, "name",
// "palette" or "field-N", to the label of the next one.
func blockOf(t *testing.T, body, field string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<label for="`+field+`">`)
	if !ok {
		_, rest, ok = strings.Cut(body, `<label for="`+field+`"><input`)
	}
	if !ok {
		// A group of boxes has no label of its own.
		_, rest, ok = strings.Cut(body, `id="`+field+`-o0"`)
	}
	if !ok {
		t.Fatalf("page has no block for %s", field)
	}
	end := len(rest)
	for _, next := range []string{`<div class="field field-setting`, `<div class="field">`, `<fieldset class="field`} {
		if i := strings.Index(rest, next); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}

// TestInstanceSaveSecretOnlyChange checks that a change that sets or
// replaces a secret value and no mark leaves the state alone, so a page
// loaded before it still saves.
func TestInstanceSaveSecretOnlyChange(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := savedInstance(t, tu, rec)
	old := instanceStateOf(t, tu, inst.ID)
	change := recipe.InstanceUpdate{Values: map[string]recipe.Value{"api_key": {One: "replaced-key-1"}}}
	if _, err := recipe.UpdateInstance(t.Context(), tu.db, tu.srv.secrets, tu.srv.recipesDir, inst.ID, change, tu.now); err != nil {
		t.Fatal(err)
	}
	if instanceStateOf(t, tu, inst.ID) != old {
		t.Fatal("replacing a secret value changed the state")
	}
	w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"seen": {old}, "f.city": {"Bergen"}}))
	_, all, _ := current(t, tu, inst.ID)
	if w.Code != http.StatusSeeOther || all["city"].One != "Bergen" || all["api_key"].One != "replaced-key-1" {
		t.Errorf("status %d, fields %+v: %s", w.Code, all, w.Body)
	}
}

func TestInstanceLineBreaks(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	c := tu.signIn(t)
	// The create form stores LF.
	inst := createdInstance(t, tu, rec, good(url.Values{"f.notes": {"a\r\nb"}}))
	if _, all, _ := current(t, tu, inst.ID); all["notes"].One != "a\nb" {
		t.Fatalf("created notes = %q, want a LF only", all["notes"].One)
	}
	before, _, _ := current(t, tu, inst.ID)
	// A form that sends the same text with CR LF and a new name changes nothing in the values.
	w := postEdit(t, tu, c, inst.ID, editForm(url.Values{"f.notes": {"a\r\nb"}, "name": {"Work"}, "f.alerts": nil, "f.webhook_token": nil}))
	after, all, _ := current(t, tu, inst.ID)
	if w.Code != http.StatusSeeOther || after.Name != "Work" || all["notes"].One != "a\nb" || strings.Contains(after.FieldValues, `\r`) {
		t.Fatalf("status %d, name %q, notes %q, values %s", w.Code, after.Name, all["notes"].One, after.FieldValues)
	}
	if after.SettingsAt != before.SettingsAt {
		t.Errorf("settings_at went from %d to %d for the same text", before.SettingsAt, after.SettingsAt)
	}
	// A new value with CR LF is stored with LF.
	w = postEdit(t, tu, c, inst.ID, editForm(url.Values{"f.notes": {"x\r\ny\r\nz"}}))
	if _, all, _ = current(t, tu, inst.ID); w.Code != http.StatusSeeOther || all["notes"].One != "x\ny\nz" {
		t.Errorf("status %d, notes %q", w.Code, all["notes"].One)
	}
}

func TestInstanceLeadingNewline(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := createdInstance(t, tu, rec, good(url.Values{"f.notes": {"\nfirst"}}))
	body := tu.get(t, tu.signIn(t), "/recipes/instances/"+id(inst.ID)).Body.String()
	if !strings.Contains(body, ">\n\nfirst</textarea>") {
		t.Errorf("a stored leading newline is lost in the page:\n%s", body)
	}
}

func TestInstanceMultiSelectErrorMarkup(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	body := tu.post(t, tu.signIn(t), "/recipes/add-instance/"+id(rec.ID), good(url.Values{"f.days": {"bogus"}})).Body.String()
	if !strings.Contains(body, "Days is not one of the options.") {
		t.Fatalf("no error for the multi-select: %s", body)
	}
	if i := strings.Index(body, "<fieldset"); i < 0 || strings.Contains(body[i:i+strings.Index(body[i:], ">")], "aria-invalid") {
		t.Error("the fieldset has aria-invalid")
	}
}

// retypedFieldRecipe has a text field that a later settings.yml makes a
// password field.
const retypedFieldRecipe = `name: Kitchen
custom_fields:
- keyname: city
  name: City
  field_type: string
  optional: true
- keyname: pw
  name: Pass
  field_type: %s
  optional: %s
`

func TestInstancePasswordWithPlainValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		optional string
		canClear bool
	}{
		{"optional", "true", true},
		{"required", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, strings.Replace(strings.Replace(retypedFieldRecipe, "%s", "string", 1), "%s", tc.optional, 1), true)
			inst := createdInstance(t, tu, rec, url.Values{"f.pw": {"plain-pass-1"}})
			if _, names := publicOf(t, tu, &inst); len(names) != 0 {
				t.Fatalf("setup: the value is sealed: %v", names)
			}
			// The field becomes a password field.
			settings := filepath.Join(recipe.Dir(tu.srv.recipesDir, rec.ID, 1), "settings.yml")
			text := strings.Replace(strings.Replace(retypedFieldRecipe, "%s", "password", 1), "%s", tc.optional, 1)
			if err := os.WriteFile(settings, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			c := tu.signIn(t)
			body := tu.get(t, c, "/recipes/instances/"+id(inst.ID)).Body.String()
			if !strings.Contains(body, "A value is stored. Leave this empty to keep it.") || strings.Contains(body, "plain-pass-1") {
				t.Errorf("the page does not treat the value as stored, or shows it: %s", body)
			}
			if has := strings.Contains(body, `name="c.pw"`); has != tc.canClear {
				t.Errorf("remove box = %v, want %v", has, tc.canClear)
			}
			// An empty input keeps it.
			w := postEdit(t, tu, c, inst.ID, url.Values{"name": {"Home"}, "f.city": {"Oslo"}, "f.pw": {""}})
			if _, all, _ := current(t, tu, inst.ID); w.Code != http.StatusSeeOther || all["pw"].One != "plain-pass-1" {
				t.Fatalf("keep: status %d, pw %q", w.Code, all["pw"].One)
			}
			if !tc.canClear {
				return
			}
			w = postEdit(t, tu, c, inst.ID, url.Values{"name": {"Home"}, "f.city": {"Oslo"}, "f.pw": {""}, "c.pw": {"1"}})
			if _, all, _ := current(t, tu, inst.ID); w.Code != http.StatusSeeOther || all["pw"].One != "" {
				t.Errorf("remove: status %d, pw %q", w.Code, all["pw"].One)
			}
		})
	}
}

// TestInstanceMarkWhileEmptying checks that a field that is secret in the
// form keeps its stored value when its input is blank. The form cannot tell
// an input the admin emptied from one a re-render hid, since a hidden value
// posts as blank either way, so a blank never clears. Ticking the box with a
// plain value stored seals that value.
func TestInstanceMarkWhileEmptying(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key string
	}{
		{"optional", "webhook_token"},
		{"required", "api_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, formRecipe, true)
			inst := createdInstance(t, tu, rec, good(url.Values{"s.api_key": nil, "f.webhook_token": {"plain-token-1"}}))
			w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"s." + tc.key: {"1"}, "f." + tc.key: {""}}))
			cur, all, marks := current(t, tu, inst.ID)
			plain, names := publicOf(t, tu, &cur)
			if _, ok := plain[tc.key]; w.Code != http.StatusSeeOther || ok || !slices.Contains(names, tc.key) || !slices.Contains(marks, tc.key) {
				t.Errorf("status %d, plain %+v, sealed %v, marks %v: %s", w.Code, plain, names, marks, w.Body)
			}
			if all[tc.key].One == "" {
				t.Errorf("%s lost its value: %+v", tc.key, all)
			}
		})
	}

	// Emptying the input of a plain field with its box unticked clears it.
	t.Run("clear optional", func(t *testing.T) {
		t.Parallel()
		tu, rec := newFormUI(t, formRecipe, true)
		inst := savedInstance(t, tu, rec)
		w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"f.webhook_token": {""}}))
		if _, all, _ := current(t, tu, inst.ID); w.Code != http.StatusSeeOther || all["webhook_token"].One != "" {
			t.Errorf("status %d, fields %+v", w.Code, all)
		}
	})
	t.Run("clear required", func(t *testing.T) {
		t.Parallel()
		tu, rec := newFormUI(t, formRecipe, true)
		inst := createdInstance(t, tu, rec, good(url.Values{"s.api_key": nil}))
		before, _, _ := current(t, tu, inst.ID)
		w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"s.api_key": nil, "f.api_key": {""}}))
		after, all, _ := current(t, tu, inst.ID)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "API key is required.") || after.FieldValues != before.FieldValues || all["api_key"].One == "" {
			t.Errorf("status %d, fields %+v: %s", w.Code, all, w.Body)
		}
	})
	// A mark with the value left in place still seals it.
	t.Run("value kept", func(t *testing.T) {
		t.Parallel()
		tu, rec := newFormUI(t, formRecipe, true)
		inst := savedInstance(t, tu, rec)
		w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"s.webhook_token": {"1"}}))
		cur, all, marks := current(t, tu, inst.ID)
		plain, _ := publicOf(t, tu, &cur)
		if _, ok := plain["webhook_token"]; w.Code != http.StatusSeeOther || ok || all["webhook_token"].One != "plain-token-1" || strings.Join(marks, ",") != "api_key,webhook_token" {
			t.Errorf("status %d, fields %+v, marks %v", w.Code, all, marks)
		}
	})
}

var (
	controlRE = regexp.MustCompile(`(?s)<input[^>]*>|<textarea[^>]*>.*?</textarea>|<select[^>]*>.*?</select>`)
	attrRE    = regexp.MustCompile(`([a-zA-Z][a-zA-Z-]*)(?:="([^"]*)")?`)
	optionRE  = regexp.MustCompile(`(?s)<option([^>]*)>`)
)

// attrs returns the attributes of an opening tag, unescaped. A boolean
// attribute has the value "".
func attrs(tag string) map[string]string {
	tag = strings.TrimPrefix(strings.TrimSuffix(tag, ">"), "<")
	_, tag, ok := strings.Cut(tag, " ")
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, m := range attrRE.FindAllStringSubmatch(tag, -1) {
		out[m[1]] = html.UnescapeString(m[2])
	}
	return out
}

// formControls returns what a browser would post for the edit or add form of
// a page: each input's value, each checked box, each select's chosen option
// and each textarea's text, with the first newline of a textarea dropped and
// its line breaks sent as CR LF.
func formControls(t *testing.T, body string) url.Values {
	t.Helper()
	_, rest, ok := strings.Cut(body, `<form method="post" action="/recipes/`)
	if !ok {
		t.Fatalf("page has no instance form:\n%s", body)
	}
	form, _, _ := strings.Cut(rest, "</form>")
	out := url.Values{}
	for _, c := range controlRE.FindAllString(form, -1) {
		open, _, _ := strings.Cut(c, ">")
		a := attrs(open + ">")
		name, named := a["name"]
		if !named {
			continue
		}
		switch {
		case strings.HasPrefix(c, "<input"):
			if typ := a["type"]; typ == "checkbox" || typ == "radio" {
				if _, checked := a["checked"]; checked {
					v, ok := a["value"]
					if !ok {
						v = "on"
					}
					out.Add(name, v)
				}
				continue
			}
			out.Add(name, a["value"])
		case strings.HasPrefix(c, "<textarea"):
			text := strings.TrimSuffix(strings.TrimPrefix(c, open+">"), "</textarea>")
			text = strings.TrimPrefix(html.UnescapeString(text), "\n")
			out.Add(name, strings.ReplaceAll(text, "\n", "\r\n"))
		case strings.HasPrefix(c, "<select"):
			chosen, first, found := "", "", false
			for i, m := range optionRE.FindAllStringSubmatch(c, -1) {
				oa := attrs("<option" + m[1] + ">")
				if i == 0 {
					first = oa["value"]
				}
				if _, sel := oa["selected"]; sel && !found {
					chosen, found = oa["value"], true
				}
			}
			if !found {
				chosen = first
			}
			out.Add(name, chosen)
		}
	}
	return out
}

// TestInstanceRefusedPostKeepsTickedValue checks that a plain value whose
// secret box was ticked in a post that was refused is still stored, and is
// sealed, when the page that came back is saved after the other field is
// fixed. The second post is built from the page, as a browser builds it.
func TestInstanceRefusedPostKeepsTickedValue(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	const notes = "line one\nline two"
	inst := createdInstance(t, tu, rec, good(url.Values{"f.notes": {notes}, "f.webhook_token": {"plain-token-1"}}))
	c := tu.signIn(t)

	// The page as a browser posts it, with the boxes of two plain values
	// ticked, and a 4 character new api_key, which is refused as a secret.
	page := tu.get(t, c, "/recipes/instances/"+id(inst.ID)).Body.String()
	form := formControls(t, page)
	if form.Get("f.webhook_token") != "plain-token-1" || form.Get("f.notes") != "line one\r\nline two" || form.Get("seen") == "" {
		t.Fatalf("the page's form is %v", form)
	}
	for _, k := range []string{"webhook_token", "notes"} {
		form.Set("s."+k, "1")
	}
	form.Set("f.api_key", "abcd")
	w := tu.post(t, c, "/recipes/instances/"+id(inst.ID), form)
	body := w.Body.String()
	if w.Code != http.StatusBadRequest || !strings.Contains(body, "API key must be at least 6 characters to be secret.") {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	for _, field := range []string{"field-2", "field-10"} {
		block := blockOf(t, body, field)
		for _, want := range []string{"A value is stored. Leave this empty to keep it.", `name="c.`} {
			if !strings.Contains(block, want) {
				t.Errorf("%s lacks %q:\n%s", field, want, block)
			}
		}
		for _, bad := range []string{"No value is stored.", "Enter the secret value again", "plain-token-1", "line one"} {
			if strings.Contains(block, bad) {
				t.Errorf("%s has %q:\n%s", field, bad, block)
			}
		}
	}
	if !strings.Contains(body, `<input id="field-10" name="f.webhook_token" type="text" autocomplete="off"`) || strings.Contains(body, `name="f.webhook_token" type="text" autocomplete="off" value=`) {
		t.Error("the hidden text input is not empty")
	}

	// The next post comes from the re-rendered page, with the api_key fixed.
	next := formControls(t, body)
	next.Set("f.api_key", "abcdef12345")
	if w = tu.post(t, c, "/recipes/instances/"+id(inst.ID), next); w.Code != http.StatusSeeOther {
		t.Fatalf("second post: status %d: %s", w.Code, w.Body)
	}
	cur, all, marks := current(t, tu, inst.ID)
	plain, names := publicOf(t, tu, &cur)
	if all["webhook_token"].One != "plain-token-1" || all["notes"].One != notes {
		t.Errorf("the values were lost: %+v", all)
	}
	for _, k := range []string{"webhook_token", "notes"} {
		if _, ok := plain[k]; ok || !slices.Contains(names, k) || !slices.Contains(marks, k) {
			t.Errorf("%s is not sealed and marked: plain %+v, sealed %v, marks %v", k, plain, names, marks)
		}
	}
	if all["api_key"].One != "abcdef12345" || plain["city"].One != "Oslo" {
		t.Errorf("api_key %q, plain %+v", all["api_key"].One, plain)
	}
}

// TestInstanceUntickHiddenValueKeepsIt checks that unticking the secret box
// on a page that hid a plain value behind the tick keeps the value. The
// input is blank only because the page hid it, which the page says with its
// h. field. The value stays plain, through a second refused post too, which
// shows it again.
func TestInstanceUntickHiddenValueKeepsIt(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	const notes = "line one\nline two"
	inst := createdInstance(t, tu, rec, good(url.Values{"f.notes": {notes}, "f.webhook_token": {"plain-token-1"}}))
	c := tu.signIn(t)
	path := "/recipes/instances/" + id(inst.ID)
	form := formControls(t, tu.get(t, c, path).Body.String())
	for _, k := range []string{"webhook_token", "notes"} {
		form.Set("s."+k, "1")
	}
	form.Set("f.api_key", "abcd")
	w := tu.post(t, c, path, form)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	// The admin changes their mind and unticks both boxes on the page that
	// hid the values, and the api_key is still wrong.
	next := formControls(t, w.Body.String())
	if !next.Has("h.webhook_token") || !next.Has("h.notes") || next.Get("f.webhook_token") != "" || next.Get("f.notes") != "" {
		t.Fatalf("the re-rendered form is %v", next)
	}
	next.Del("s.webhook_token")
	next.Del("s.notes")
	next.Set("f.api_key", "abcd")
	w = tu.post(t, c, path, next)
	body := w.Body.String()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("second post: status %d: %s", w.Code, body)
	}
	// The values are plain and the boxes unticked, so the page shows them.
	last := formControls(t, body)
	if last.Get("f.webhook_token") != "plain-token-1" || last.Get("f.notes") != "line one\r\nline two" || last.Has("h.webhook_token") || last.Has("h.notes") {
		t.Fatalf("the form after unticking is %v", last)
	}
	last.Set("f.api_key", "abcdef12345")
	if w = tu.post(t, c, path, last); w.Code != http.StatusSeeOther {
		t.Fatalf("third post: status %d: %s", w.Code, w.Body)
	}
	cur, _, marks := current(t, tu, inst.ID)
	plain, _ := publicOf(t, tu, &cur)
	if plain["webhook_token"].One != "plain-token-1" || plain["notes"].One != notes || slices.Contains(marks, "notes") || slices.Contains(marks, "webhook_token") {
		t.Errorf("plain %+v, marks %v", plain, marks)
	}

	// Unticking on the page that hid them and saving at once keeps them too.
	form = formControls(t, tu.get(t, c, path).Body.String())
	form.Set("s.notes", "1")
	form.Set("f.city", "")
	w = tu.post(t, c, path, form)
	next = formControls(t, w.Body.String())
	next.Del("s.notes")
	next.Set("f.city", "Oslo")
	if w = tu.post(t, c, path, next); w.Code != http.StatusSeeOther {
		t.Fatalf("save after untick: status %d: %s", w.Code, w.Body)
	}
	cur, _, _ = current(t, tu, inst.ID)
	if plain, _ = publicOf(t, tu, &cur); plain["notes"].One != notes {
		t.Errorf("notes were lost: %+v", plain)
	}
}

// TestInstanceRemoveHiddenPlainValue checks the Remove box of a plain value
// that a ticked box hides, and with the value still in the input.
func TestInstanceRemoveHiddenPlainValue(t *testing.T) {
	t.Parallel()
	t.Run("from the re-rendered page", func(t *testing.T) {
		t.Parallel()
		tu, rec := newFormUI(t, formRecipe, true)
		inst := createdInstance(t, tu, rec, good(url.Values{"f.webhook_token": {"plain-token-1"}}))
		c := tu.signIn(t)
		form := formControls(t, tu.get(t, c, "/recipes/instances/"+id(inst.ID)).Body.String())
		form.Set("s.webhook_token", "1")
		form.Set("f.api_key", "abcd")
		w := tu.post(t, c, "/recipes/instances/"+id(inst.ID), form)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d", w.Code)
		}
		next := formControls(t, w.Body.String())
		next.Set("f.api_key", "abcdef12345")
		next.Set("c.webhook_token", "1")
		if w = tu.post(t, c, "/recipes/instances/"+id(inst.ID), next); w.Code != http.StatusSeeOther {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		if _, all, _ := current(t, tu, inst.ID); all["webhook_token"].One != "" {
			t.Errorf("the value is still stored: %+v", all)
		}
	})
	// A Remove tick with the stored value still in the input is a plain clear.
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"value shown", editForm(url.Values{"c.webhook_token": {"1"}})},
		{"value shown and box ticked", editForm(url.Values{"c.webhook_token": {"1"}, "s.webhook_token": {"1"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, formRecipe, true)
			inst := savedInstance(t, tu, rec)
			w := postEdit(t, tu, tu.signIn(t), inst.ID, tc.form)
			if _, all, _ := current(t, tu, inst.ID); w.Code != http.StatusSeeOther || all["webhook_token"].One != "" {
				t.Errorf("status %d, fields %+v: %s", w.Code, all, w.Body)
			}
		})
	}
}

// TestInstanceStaleStoredNow checks the notes a refused stale save puts on
// each field whose value is not what is stored now.
func TestInstanceStaleStoredNow(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := createdInstance(t, tu, rec, good(url.Values{
		"f.units": {"imperial"}, "f.days": {"mon", "tue"}, "f.alerts": {"true"}, "f.notes": {"short"},
		"f.webhook_token": {"plain-token-1"},
	}))
	c := tu.signIn(t)
	old := instanceStateOf(t, tu, inst.ID)
	long := strings.Repeat("x", maxStoredShown+50)
	str := func(s string) *string { return &s }
	change := recipe.InstanceUpdate{
		Name: str("Renamed"), Palette: str("bw"), Clear: []string{"webhook_token"},
		Values: map[string]recipe.Value{"city": {One: "Bergen"}, "notes": {One: long}},
	}
	if _, err := recipe.UpdateInstance(t.Context(), tu.db, tu.srv.secrets, tu.srv.recipesDir, inst.ID, change, tu.now); err != nil {
		t.Fatal(err)
	}
	typed := url.Values{
		"seen": {old}, "name": {"Typed name"}, "palette": {""}, "f.city": {"Typed city"}, "f.units": {"metric"}, "f.days": {"wed"},
		"f.alerts": nil, "f.notes": {""}, "f.webhook_token": {"typed-token"},
	}
	w := postEdit(t, tu, c, inst.ID, editForm(typed))
	body := w.Body.String()
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	for _, tc := range []struct{ block, want string }{
		{"name", "Stored now: Renamed"},
		{"palette", "Stored now: Black &amp; White (1-bit)"},
		{"field-0", "Stored now: Bergen"},
		{"field-2", "Stored now, cut short: " + strings.Repeat("x", maxStoredShown) + "<"},
		{"field-4", "Stored now: Imperial"},
		{"field-6", "Stored now: Mon, Tue"},
		{"field-7", "Stored now: on"},
		{"field-10", "Nothing is stored now."},
	} {
		block := blockOf(t, body, tc.block)
		if !strings.Contains(block, differsText) || !strings.Contains(block, tc.want) {
			t.Errorf("block %s lacks the note %q:\n%s", tc.block, tc.want, block)
		}
		if strings.Index(block, differsText) > strings.Index(block, tc.want) {
			t.Errorf("block %s has the notes in the wrong order", tc.block)
		}
	}
	if strings.Contains(body, strings.Repeat("x", maxStoredShown+1)) {
		t.Error("a long stored value is shown whole")
	}
}

// TestInstanceStaleSealedGetsNoNote checks that a field whose stored value is
// sealed has no note and that the sealed text is nowhere on the page.
func TestInstanceStaleSealedGetsNoNote(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := createdInstance(t, tu, rec, good(url.Values{"f.webhook_token": {"sealed-secret-xyz"}, "s.webhook_token": {"1"}}))
	old := instanceStateOf(t, tu, inst.ID)
	name := "Renamed"
	if _, err := recipe.UpdateInstance(t.Context(), tu.db, tu.srv.secrets, tu.srv.recipesDir, inst.ID, recipe.InstanceUpdate{Name: &name}, tu.now); err != nil {
		t.Fatal(err)
	}
	// The page's box is unticked, so the field shows as a plain one.
	w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"seen": {old}, "s.webhook_token": nil, "f.webhook_token": {"typed-token"}}))
	body := w.Body.String()
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d", w.Code)
	}
	block := blockOf(t, body, "field-10")
	if strings.Contains(block, differsText) || strings.Contains(block, "Stored now") || strings.Contains(body, "sealed-secret-xyz") {
		t.Errorf("a sealed value is noted or shown:\n%s", block)
	}
}

// TestInstanceStaleAtUpdate checks the refusal that UpdateInstance makes
// when the instance changes after the handler read it. The server's clock is
// the hook: the handler calls it, after its own read and its check of seen,
// as the argument of UpdateInstance, and the session check calls it once
// before that. The hook renames the instance on its second call, so the
// handler sees a change only the domain's own check can find.
func TestInstanceStaleAtUpdate(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, formRecipe, true)
	inst := savedInstance(t, tu, rec)
	c := tu.signIn(t)
	old := instanceStateOf(t, tu, inst.ID)
	calls, fired := 0, false
	tu.srv.now = func() time.Time {
		calls++
		if calls == 2 {
			fired = true
			name := "Renamed"
			if _, err := recipe.UpdateInstance(t.Context(), tu.db, tu.srv.secrets, tu.srv.recipesDir, inst.ID, recipe.InstanceUpdate{Name: &name}, tu.now); err != nil {
				t.Error(err)
			}
		}
		return tu.now.UTC()
	}
	typed := url.Values{"seen": {old}, "name": {"Typed name"}, "f.city": {"Typed city"}}
	w := postEdit(t, tu, c, inst.ID, editForm(typed))
	body := w.Body.String()
	if !fired {
		t.Fatalf("the clock was called %d times, so the hook never ran", calls)
	}
	cur := instanceStateOf(t, tu, inst.ID)
	if w.Code != http.StatusConflict || !strings.Contains(body, instanceChanged) {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	if !strings.Contains(body, `value="Typed name"`) || !strings.Contains(body, `value="Typed city"`) || hiddenSeen(t, body) != cur {
		t.Errorf("typed values or seen wrong (current state %s):\n%s", cur, body)
	}
	if !strings.Contains(blockOf(t, body, "name"), "Stored now: Renamed") {
		t.Error("the name has no note for the other rename")
	}
	if got, _, _ := current(t, tu, inst.ID); got.Name != "Renamed" {
		t.Errorf("name = %q: the refused save changed the instance", got.Name)
	}
	typed.Set("seen", hiddenSeen(t, body))
	w = postEdit(t, tu, c, inst.ID, editForm(typed))
	if got, all, _ := current(t, tu, inst.ID); w.Code != http.StatusSeeOther || got.Name != "Typed name" || all["city"].One != "Typed city" {
		t.Errorf("second save: status %d, name %q", w.Code, got.Name)
	}
}

// TestInstanceSharedOptionValueUnchangedSave checks that an unchanged save
// of a multi-select whose options share a value is stored as it was. The
// page ticks both options that share the chosen value, so the browser posts
// it twice.
func TestInstanceSharedOptionValueUnchangedSave(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, `name: Colors
custom_fields:
- keyname: colors
  name: Colors
  field_type: select
  multiple: true
  options: [Red, red, Green]
`, true)
	inst := createdInstance(t, tu, rec, url.Values{"name": {"Home"}, "palette": {""}, "f.colors": {"red"}})
	before, all, _ := current(t, tu, inst.ID)
	if strings.Join(all["colors"].Many, ",") != "red" {
		t.Fatalf("created colors = %v, want [red]", all["colors"].Many)
	}
	c := tu.signIn(t)
	form := formControls(t, tu.get(t, c, "/recipes/instances/"+id(inst.ID)).Body.String())
	if got := form["f.colors"]; strings.Join(got, ",") != "red,red" {
		t.Fatalf("the page posts colors %v, want red twice", got)
	}
	w := tu.post(t, c, "/recipes/instances/"+id(inst.ID), form)
	after, _, _ := current(t, tu, inst.ID)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if after.FieldValues != before.FieldValues || !bytes.Equal(after.SecretValues, before.SecretValues) || after.SettingsAt != before.SettingsAt || after.Name != before.Name {
		t.Errorf("an unchanged save changed the row: before %+v, after %+v", before, after)
	}
}

// TestInstanceSharedOptionRenameWithoutKey checks that a rename from the page
// of an instance with a sealed secret, on a server with no secrets key, saves
// when the page posts a multi-select whose options share a value. The form
// compares the stored form of what it posted with what is stored, so it
// sends no value, and the update needs no key.
func TestInstanceSharedOptionRenameWithoutKey(t *testing.T) {
	t.Parallel()
	const colorsRecipe = `name: Colors
custom_fields:
- keyname: colors
  name: Colors
  field_type: select
  multiple: true
  options: [Red, red, Green]
- keyname: pass
  name: Password
  field_type: password
  optional: true
`
	tu, rec := newFormUI(t, colorsRecipe, false)
	key, err := secrets.Parse(testKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := recipe.CreateInstance(t.Context(), tu.db, key, tu.srv.recipesDir, recipe.InstanceParams{
		RecipeID: rec.ID, Name: "Home", Values: map[string]recipe.Value{"colors": {Many: []string{"red"}}, "pass": {One: "hunter22"}},
	}, tu.now)
	if err != nil {
		t.Fatal(err)
	}
	c := tu.signIn(t)
	form := formControls(t, tu.get(t, c, "/recipes/instances/"+id(inst.ID)).Body.String())
	if got := form["f.colors"]; strings.Join(got, ",") != "red,red" {
		t.Fatalf("the page posts colors %v, want red twice", got)
	}
	form.Set("name", "Work")
	w := tu.post(t, c, "/recipes/instances/"+id(inst.ID), form)
	after, err := tu.db.Read.GetInstance(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusSeeOther || after.Name != "Work" {
		t.Fatalf("status %d, name %q: %s", w.Code, after.Name, w.Body)
	}
	if after.FieldValues != inst.FieldValues || !bytes.Equal(after.SecretValues, inst.SecretValues) || after.SettingsAt != inst.SettingsAt {
		t.Errorf("a rename changed the values: before %+v, after %+v", inst, after)
	}
}

// TestPostedValueStoredForm checks that the form reads a posted value in the
// form the field stores it, so sameValue, which decides whether a save
// sends the value and what a stale page shows, sees no change in a shared
// value posted twice or a textarea's CR LF.
func TestPostedValueStoredForm(t *testing.T) {
	t.Parallel()
	multi := recipe.Field{Keyname: "colors", Type: "select", Multiple: true, Options: []recipe.Option{{Label: "Red", Value: "red"}, {Label: "red", Value: "red"}, {Label: "Green", Value: "green"}}}
	notes := recipe.Field{Keyname: "notes", Type: "text"}
	for _, tc := range []struct {
		name   string
		f      recipe.Field
		post   url.Values
		stored recipe.Value
	}{
		{"a shared value posted twice", multi, url.Values{"f.colors": {"red", "red"}}, recipe.Value{Many: []string{"red"}}},
		{"choices in another order", multi, url.Values{"f.colors": {"green", "red"}}, recipe.Value{Many: []string{"red", "green"}}},
		{"CR LF in a textarea", notes, url.Values{"f.notes": {"a\r\nb"}}, recipe.Value{One: "a\nb"}},
	} {
		if got := postedValue(&tc.f, tc.post); !sameValue(&tc.f, got, tc.stored) {
			t.Errorf("%s: posted %+v is not the stored %+v", tc.name, got, tc.stored)
		}
	}
}

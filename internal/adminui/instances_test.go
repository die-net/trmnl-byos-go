package adminui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	rdir := recipe.Dir(dir, rec.ID)
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
	state, err := recipe.InstanceState(&inst)
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
			wantErr: "API key is secret; give it a new value or clear it to make it plain.",
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
		`<p class="field-error" id="field-10-err">Webhook token is secret; give it a new value or clear it to make it plain.</p>`,
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
		{"mark", recipe.InstanceUpdate{Secret: map[string]bool{"webhook_token": true}}, []string{"name", "field-0", "field-10"}},
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
		t.Fatalf("page has no block for %s", field)
	}
	if end := strings.Index(rest, `<div class="field`); end >= 0 {
		rest = rest[:end]
	}
	return rest
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
			settings := filepath.Join(recipe.Dir(tu.srv.recipesDir, rec.ID), "settings.yml")
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

func TestInstanceMarkWhileEmptying(t *testing.T) {
	t.Parallel()
	// An optional plain field that is marked with its input emptied is cleared, and stays marked.
	t.Run("optional", func(t *testing.T) {
		t.Parallel()
		tu, rec := newFormUI(t, formRecipe, true)
		inst := savedInstance(t, tu, rec)
		w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"s.webhook_token": {"1"}, "f.webhook_token": {""}}))
		_, all, marks := current(t, tu, inst.ID)
		if _, ok := all["webhook_token"]; w.Code != http.StatusSeeOther || ok || strings.Join(marks, ",") != "api_key,webhook_token" {
			t.Errorf("status %d, fields %+v, marks %v: %s", w.Code, all, marks, w.Body)
		}
	})
	// A required one is refused and nothing changes.
	t.Run("required", func(t *testing.T) {
		t.Parallel()
		tu, rec := newFormUI(t, formRecipe, true)
		inst := createdInstance(t, tu, rec, good(url.Values{"s.api_key": nil}))
		before, beforeAll, _ := current(t, tu, inst.ID)
		w := postEdit(t, tu, tu.signIn(t), inst.ID, editForm(url.Values{"f.api_key": {""}, "s.api_key": {"1"}}))
		after, all, marks := current(t, tu, inst.ID)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "API key is required.") {
			t.Errorf("status %d: %s", w.Code, w.Body)
		}
		if after.FieldValues != before.FieldValues || len(marks) != 0 || all["api_key"].One != beforeAll["api_key"].One || all["api_key"].One == "" {
			t.Errorf("a refused save changed the instance: %+v, marks %v", all, marks)
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

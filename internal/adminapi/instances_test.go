package adminapi

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestInstanceFieldErrors checks that bad field values answer 400 with the
// message of every bad field, by keyname, in the error body's fields, and
// create nothing.
func TestInstanceFieldErrors(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	ta.addRecipe(t)
	var body errorBody
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": 1, "fields": {"days": "0", "tags": ["z"], "nope": "x", "k": "`+testSecret+`"}}`, 400, &body)
	want := map[string]string{"days": "must be at least 1", "tags": "is not one of the options", "nope": "no such field"}
	if len(body.Fields) != len(want) {
		t.Errorf("fields = %v, want %v", body.Fields, want)
	}
	for k, msg := range want {
		if body.Fields[k] != msg {
			t.Errorf("%s: %q, want %q", k, body.Fields[k], msg)
		}
	}
	if !strings.HasPrefix(body.Error, "days: must be at least 1; nope: no such field; tags: ") {
		t.Errorf("error = %q", body.Error)
	}
	var list []instanceJSON
	ta.call(t, "GET", "/api/v1/instances", "", 200, &list)
	if len(list) != 0 {
		t.Errorf("a refused create left %+v", list)
	}
	// An error that is not about fields has no fields.
	w := ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": 1, "palette": "neon"}`, 400, nil)
	if strings.Contains(w.Body.String(), `"fields"`) {
		t.Errorf("body %s", w.Body)
	}
}

// TestPatchInstance checks that an update changes only what it names, that
// an empty string or array clears a field, that secrets stay out of every
// response, and that a refused update changes nothing.
func TestPatchInstance(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	rec := ta.addRecipe(t)
	var inst instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "name": "Home", "fields": {"city": "Oslo", "tags": ["a"], "apikey": "`+testSecret+`"}}`, 201, &inst)
	path := "/api/v1/instances/" + strconv.FormatInt(inst.ID, 10)

	var got instanceJSON
	w := ta.call(t, "PATCH", path, `{"name": "Away", "fields": {"city": "Bergen", "tags": ["b", "c"], "zkey": "`+testSecret+`-2"}}`, 200, &got)
	if got.Name != "Away" || got.Palette != "" || got.Fields["city"].One != "Bergen" || !slices.Equal(got.Fields["tags"].Many, []string{"b", "c"}) ||
		strings.Join(got.SecretFields, ",") != "apikey,zkey" {
		t.Errorf("after an update: %+v", got)
	}
	bodies := append(make([]string, 0, 12), w.Body.String())

	// An empty string and an empty array clear, a secret too; a key left out
	// is kept.
	got = instanceJSON{}
	w = ta.call(t, "PATCH", path, `{"fields": {"city": "", "tags": [], "apikey": ""}}`, 200, &got)
	bodies = append(bodies, w.Body.String())
	// An empty password is kept, and Clear is how a secret goes.
	if got.Name != "Away" || len(got.Fields) != 0 || strings.Join(got.SecretFields, ",") != "apikey,zkey" {
		t.Errorf("after clearing: %+v", got)
	}
	got = instanceJSON{}
	w = ta.call(t, "PATCH", path, `{"clear": ["apikey", "tags"]}`, 200, &got)
	bodies = append(bodies, w.Body.String())
	if strings.Join(got.SecretFields, ",") != "zkey" {
		t.Errorf("after Clear: %+v", got)
	}
	again := instanceJSON{}
	bodies = append(bodies, ta.call(t, "GET", path, "", 200, &again).Body.String())
	if again.Name != got.Name || len(again.Fields) != 0 || strings.Join(again.SecretFields, ",") != "zkey" {
		t.Errorf("get after clearing: %+v", again)
	}

	// The name alone, and the recipe's name for an empty one.
	got = instanceJSON{}
	ta.call(t, "PATCH", path, `{"name": ""}`, 200, &got)
	if got.Name != "Weather" || strings.Join(got.SecretFields, ",") != "zkey" {
		t.Errorf("after an empty name: %+v", got)
	}

	// Refused updates, each with the instance unchanged.
	for _, tc := range []struct {
		name, body string
		status     int
		contains   string
	}{
		{"bad value", `{"name": "x", "fields": {"days": "zero"}}`, 400, "days: must be a number"},
		{"set and cleared", `{"fields": {"city": "x"}, "clear": ["city"]}`, 400, "city: is both set and cleared"},
		{"a value for an unknown field", `{"fields": {"nope": "x"}}`, 400, "nope: no such field"},
		{"unknown palette", `{"name": "x", "palette": "neon"}`, 400, "unknown palette"},
		{"secrets is not a field", `{"secrets": {"k": "` + testSecret + `"}}`, 400, "unknown field"},
		{"a number for a value", `{"fields": {"city": 7}}`, 400, "string or an array of strings"},
		{"short secret", `{"fields": {"zkey": "abc"}}`, 400, "zkey: must be at least 6 characters"},
	} {
		w := ta.do(t, "PATCH", path, strings.NewReader(tc.body))
		bodies = append(bodies, w.Body.String())
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) {
			t.Errorf("%s: status %d: %s", tc.name, w.Code, w.Body)
		}
	}
	again = instanceJSON{}
	ta.call(t, "GET", path, "", 200, &again)
	if again.Name != "Weather" || len(again.Fields) != 0 || strings.Join(again.SecretFields, ",") != "zkey" {
		t.Errorf("after refused updates: %+v", again)
	}
	ta.wantError(t, "PATCH", "/api/v1/instances/99", `{"name": "x"}`, 404, "no such instance")
	ta.wantError(t, "PATCH", "/api/v1/instances/x", `{"name": "x"}`, 404, "no such instance")
	for _, b := range bodies {
		if strings.Contains(b, testSecret) {
			t.Errorf("response shows a secret: %s", b)
		}
	}
}

// TestPatchInstanceNeedsKey checks that a server with no secrets key
// changes the plain fields of an instance whose secrets are sealed, and
// refuses a change to a secret.
func TestPatchInstanceNeedsKey(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	rec := ta.addRecipe(t)
	var inst instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`}`, 201, &inst)
	path := "/api/v1/instances/" + strconv.FormatInt(inst.ID, 10)
	ta.wantError(t, "PATCH", path, `{"fields": {"k": "`+testSecret+`"}}`, 400, "TRMNL_SECRET_KEY")
	var got instanceJSON
	ta.call(t, "PATCH", path, `{"fields": {"city": "Oslo"}}`, 200, &got)
	if got.Fields["city"].One != "Oslo" {
		t.Errorf("after an update: %+v", got)
	}
}

// TestMarkSecret checks that mark_secret hides a field's value and lists it
// in secret_keys and secret_fields, that an unmark that would show the
// sealed value is refused, that an unmark with a new value shows only that,
// and that a refused mark answers 400 with the field error.
func TestMarkSecret(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	rec := ta.addRecipe(t)
	var inst instanceJSON
	w := ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "fields": {"city": "`+testSecret+`"}, "mark_secret": {"city": true}}`, 201, &inst)
	bodies := append(make([]string, 0, 8), w.Body.String())
	if _, ok := inst.Fields["city"]; ok || strings.Join(inst.SecretKeys, ",") != "city" || strings.Join(inst.SecretFields, ",") != "city" {
		t.Errorf("after a create with a mark: %+v", inst)
	}
	path := "/api/v1/instances/" + strconv.FormatInt(inst.ID, 10)

	// An instance with no marks has an empty array, not null.
	w = ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`}`, 201, nil)
	if !strings.Contains(w.Body.String(), `"secret_keys":[]`) {
		t.Errorf("an instance with no marks: %s", w.Body)
	}

	// Refused marks answer 400 with the field error, and change nothing.
	for _, tc := range []struct{ body, field, msg string }{
		{`{"mark_secret": {"days": true}}`, "days", "cannot be secret, since its value is not text"},
		{`{"mark_secret": {"apikey": false}}`, "apikey", "is a password field, which is always secret"},
		{`{"mark_secret": {"nope": true}}`, "nope", "no such field"},
	} {
		var e errorBody
		bodies = append(bodies, ta.call(t, "PATCH", path, tc.body, 400, &e).Body.String())
		if e.Fields[tc.field] != tc.msg {
			t.Errorf("%s: fields = %v, want %q", tc.body, e.Fields, tc.msg)
		}
	}
	var e errorBody
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": 1, "mark_secret": {"days": true}}`, 400, &e)
	if e.Fields["days"] != "cannot be secret, since its value is not text" {
		t.Errorf("a refused mark on create: %v", e.Fields)
	}

	// An unmark alone, or with a blank value, would show the sealed value,
	// so it is refused.
	for _, body := range []string{`{"mark_secret": {"city": false}}`, `{"mark_secret": {"city": false}, "fields": {"city": ""}}`} {
		var e errorBody
		bodies = append(bodies, ta.call(t, "PATCH", path, body, 400, &e).Body.String())
		if e.Fields["city"] != "is secret; give it a new value or clear it to make it plain" {
			t.Errorf("%s: fields = %v", body, e.Fields)
		}
	}
	// An unmark with a new value shows only the new value.
	var got instanceJSON
	w = ta.call(t, "PATCH", path, `{"mark_secret": {"city": false}, "fields": {"city": "Geneva"}}`, 200, &got)
	bodies = append(bodies, w.Body.String())
	if got.Fields["city"].One != "Geneva" || len(got.SecretKeys) != 0 || len(got.SecretFields) != 0 {
		t.Errorf("after unmarking with a new value: %+v", got)
	}
	if !strings.Contains(w.Body.String(), `"secret_keys":[]`) {
		t.Errorf("unmarked body: %s", w.Body)
	}
	// And marking hides it again.
	got = instanceJSON{}
	bodies = append(bodies, ta.call(t, "PATCH", path, `{"mark_secret": {"city": true}}`, 200, &got).Body.String())
	if _, ok := got.Fields["city"]; ok || strings.Join(got.SecretKeys, ",") != "city" {
		t.Errorf("after marking: %+v", got)
	}
	for _, b := range bodies {
		if strings.Contains(b, testSecret) {
			t.Errorf("response shows a secret: %s", b)
		}
	}
}

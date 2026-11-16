package adminapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
)

// replaceYML is a recipe named "Weather 2" whose city is a password field.
const replaceYML = `name: Weather 2
refresh_interval: 60
custom_fields:
- keyname: city
  field_type: password
  optional: true
`

// replaceZip is a zip of replaceYML.
func replaceZip(t *testing.T) []byte {
	t.Helper()
	return zipBytes(t, map[string]string{"settings.yml": replaceYML, "full.liquid": "hi"})
}

// replace sends body to PUT /api/v1/recipes/{id} as a zip.
func (ta *testAPI) replace(t *testing.T, id string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	return ta.do(t, "PUT", "/api/v1/recipes/"+id, body, "Content-Type", "application/zip")
}

// unmarshal decodes a response body into v.
func unmarshal(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
}

// TestReplaceRecipe checks that a replace changes the recipe, that the same
// files again change nothing, and that a wake-up follows only a change.
func TestReplaceRecipe(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	var woken atomic.Int32
	ta.db.OnChange = func() { woken.Add(1) }
	rec := ta.addRecipe(t)
	id := strconv.FormatInt(rec.ID, 10)
	if rec.Revision != 1 || rec.SourceHash == "" || rec.ReplacedAt != 0 {
		t.Fatalf("installed %+v", rec)
	}

	w := ta.replace(t, id, bytes.NewReader(replaceZip(t)))
	var got replacedJSON
	unmarshal(t, w, &got)
	if w.Code != 200 || got.Name != "Weather 2" || got.Revision != 2 || got.Unchanged || got.ReplacedAt != ta.now.Unix() || len(got.CustomFields) != 1 {
		t.Errorf("replaced: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"instances":[]`) {
		t.Errorf("body %s, want an empty instances array", w.Body)
	}
	if n := woken.Load(); n != 1 {
		t.Errorf("%d wake-ups after a replace, want 1", n)
	}

	w = ta.replace(t, id, bytes.NewReader(replaceZip(t)))
	var again replacedJSON
	unmarshal(t, w, &again)
	if w.Code != 200 || !again.Unchanged || again.Revision != 2 {
		t.Errorf("the same zip again: %d %s", w.Code, w.Body)
	}
	if n := woken.Load(); n != 1 {
		t.Errorf("%d wake-ups after an unchanged replace, want still 1", n)
	}
	var one recipeDetailJSON
	ta.call(t, "GET", "/api/v1/recipes/"+id, "", 200, &one)
	if one.Name != "Weather 2" || one.Revision != 2 {
		t.Errorf("get %+v", one)
	}
}

// TestReplaceRecipeRefused checks the errors of a replace, and that none
// changes the recipe or wakes the scheduler.
func TestReplaceRecipeRefused(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	var woken atomic.Int32
	ta.db.OnChange = func() { woken.Add(1) }
	rec := ta.addRecipe(t)
	id := strconv.FormatInt(rec.ID, 10)
	z := replaceZip(t)
	for _, tc := range []struct {
		name, id string
		body     io.Reader
		status   int
		contains string
	}{
		{"unknown recipe", "99", bytes.NewReader(z), 404, "no such recipe"},
		{"not a number", "x", bytes.NewReader(z), 404, "no such recipe"},
		{"bad zip", id, strings.NewReader("not a zip"), 400, ""},
		{"cut off", id, io.MultiReader(bytes.NewReader(z[:len(z)/2]), iotest.ErrReader(io.ErrUnexpectedEOF)), 400, "the upload was cut off"},
	} {
		w := ta.replace(t, tc.id, tc.body)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) {
			t.Errorf("%s: status %d: %s; want %d with %q", tc.name, w.Code, w.Body, tc.status, tc.contains)
		}
	}
	if w := ta.do(t, "PUT", "/api/v1/recipes/"+id, bytes.NewReader(z), "Content-Type", "text/plain"); w.Code != 415 {
		t.Errorf("not a zip content type: status %d, want 415", w.Code)
	}
	var one recipeDetailJSON
	ta.call(t, "GET", "/api/v1/recipes/"+id, "", 200, &one)
	if one.Revision != 1 || one.Name != "Weather" {
		t.Errorf("a refused replace changed the recipe: %+v", one)
	}
	if n := woken.Load(); n != 0 {
		t.Errorf("%d wake-ups after refused replaces, want none", n)
	}
}

// TestReplaceRecipeSealing checks a replace that makes a field with a plain
// value a password field: with no secrets key it is refused, naming the
// instance and keyname, and with a key the value is sealed.
func TestReplaceRecipeSealing(t *testing.T) {
	t.Parallel()
	for _, withKey := range []bool{false, true} {
		ta := newTestAPI(t, withKey)
		var woken atomic.Int32
		ta.db.OnChange = func() { woken.Add(1) }
		rec := ta.addRecipe(t)
		id := strconv.FormatInt(rec.ID, 10)
		var inst instanceJSON
		ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+id+`, "name": "Home", "fields": {"city": "`+testSecret+`"}}`, 201, &inst)
		if inst.Fields["city"].One != testSecret {
			t.Fatalf("the instance %+v", inst)
		}
		instID := strconv.FormatInt(inst.ID, 10)

		w := ta.replace(t, id, bytes.NewReader(replaceZip(t)))
		if strings.Contains(w.Body.String(), testSecret) {
			t.Errorf("key %v: the response shows the value: %s", withKey, w.Body)
		}
		var after instanceJSON
		if !withKey {
			var e errorBody
			unmarshal(t, w, &e)
			if w.Code != 400 || len(e.Instances) != 1 || e.Instances[instID]["city"] == "" {
				t.Errorf("no key: status %d: %s; want 400 naming the instance and city", w.Code, w.Body)
			}
			ta.call(t, "GET", "/api/v1/instances/"+instID, "", 200, &after)
			if after.Fields["city"].One != testSecret {
				t.Errorf("a refused replace changed the instance: %+v", after)
			}
			if n := woken.Load(); n != 0 {
				t.Errorf("%d wake-ups after a refused replace", n)
			}
			continue
		}
		var got replacedJSON
		unmarshal(t, w, &got)
		if w.Code != 200 || len(got.Instances) != 1 || got.Instances[0].ID != inst.ID || got.Instances[0].Name != "Home" || !slices.Equal(got.Instances[0].Sealed, []string{"city"}) {
			t.Fatalf("key: status %d: %s", w.Code, w.Body)
		}
		ta.call(t, "GET", "/api/v1/instances/"+instID, "", 200, &after)
		if _, ok := after.Fields["city"]; ok || !slices.Equal(after.SecretFields, []string{"city"}) {
			t.Errorf("after the replace: fields %v, secret fields %v", after.Fields, after.SecretFields)
		}
		if n := woken.Load(); n != 1 {
			t.Errorf("%d wake-ups after a replace, want 1", n)
		}
	}
}

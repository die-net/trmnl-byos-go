package adminapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newInstancePath makes an instance of the test recipe and returns its ID
// and the path of its data.
func (ta *testAPI) newInstancePath(t *testing.T) (id int64, path string) {
	t.Helper()
	rec := ta.addRecipe(t)
	var inst instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`}`, 201, &inst)
	return inst.ID, "/api/custom_plugins/" + strconv.FormatInt(inst.ID, 10)
}

// push sends a push and returns the merge_variables of the response as
// text.
func (ta *testAPI) push(t *testing.T, path, body string, want int) string {
	t.Helper()
	var resp struct {
		MergeVariables map[string]any `json:"merge_variables"`
	}
	w := ta.call(t, "POST", path, body, want, &resp)
	if want != 200 {
		return w.Body.String()
	}
	return strings.TrimSpace(w.Body.String())
}

func TestPushNeedsToken(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	_, path := ta.newInstancePath(t)
	for _, method := range []string{"GET", "POST"} {
		for _, auth := range []string{"", "Bearer nope"} {
			req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(`{"merge_variables": {}}`))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			w := httptest.NewRecorder()
			ta.mux.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("%s with %q: status %d", method, auth, w.Code)
			}
		}
	}
	if _, err := ta.db.Read.GetInstanceData(t.Context(), 1); err == nil {
		t.Error("an unauthorized push stored data")
	}
}

func TestPushStrategies(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	_, path := ta.newInstancePath(t)
	ta.wantEmpty(t, path)

	for _, tc := range []struct{ name, body, want string }{
		{
			"default replaces", `{"merge_variables": {"a": 1, "b": {"x": 1, "y": 2}, "l": [1]}}`,
			`{"merge_variables":{"a":1,"b":{"x":1,"y":2},"l":[1]}}`,
		},
		{
			"deep merge", `{"merge_strategy": "deep_merge", "merge_variables": {"b": {"y": 3, "z": 4}, "l": [2], "c": "new"}}`,
			`{"merge_variables":{"a":1,"b":{"x":1,"y":3,"z":4},"c":"new","l":[2]}}`,
		},
		{
			"stream appends", `{"merge_strategy": "stream", "merge_variables": {"l": [3, 4], "a": 9}}`,
			`{"merge_variables":{"a":9,"b":{"x":1,"y":3,"z":4},"c":"new","l":[2,3,4]}}`,
		},
		{
			"stream limit keeps the newest", `{"merge_strategy": "stream", "stream_limit": 3, "merge_variables": {"l": [5, 6]}}`,
			`{"merge_variables":{"a":9,"b":{"x":1,"y":3,"z":4},"c":"new","l":[4,5,6]}}`,
		},
		{
			"stream limit cuts a new array", `{"merge_strategy": "stream", "stream_limit": 2, "merge_variables": {"m": [1, 2, 3]}}`,
			`{"merge_variables":{"a":9,"b":{"x":1,"y":3,"z":4},"c":"new","l":[4,5,6],"m":[2,3]}}`,
		},
		{
			"replace drops other keys", `{"merge_strategy": "replace", "merge_variables": {"n": 12345678901234567890}}`,
			`{"merge_variables":{"n":12345678901234567890}}`,
		},
	} {
		if got := ta.push(t, path, tc.body, 200); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	// GET returns what was stored.
	if w := ta.call(t, "GET", path, "", 200, nil); strings.TrimSpace(w.Body.String()) != `{"merge_variables":{"n":12345678901234567890}}` {
		t.Errorf("get: %s", w.Body)
	}
}

// wantEmpty checks that an instance with no pushes has an empty object.
func (ta *testAPI) wantEmpty(t *testing.T, path string) {
	t.Helper()
	if w := ta.call(t, "GET", path, "", 200, nil); strings.TrimSpace(w.Body.String()) != `{"merge_variables":{}}` {
		t.Errorf("get before a push: %s", w.Body)
	}
}

func TestPushRefusals(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	_, path := ta.newInstancePath(t)
	ta.call(t, "POST", path, `{"merge_variables": {"keep": 1}}`, 200, nil)
	big := `"` + strings.Repeat("x", 40<<10) + `"`
	ta.call(t, "POST", path, `{"merge_strategy": "deep_merge", "merge_variables": {"a": `+big+`}}`, 200, nil)
	for _, tc := range []struct {
		name, path, body string
		status           int
		contains         string
	}{
		{"unknown instance", "/api/custom_plugins/99", `{"merge_variables": {}}`, 404, "no such instance"},
		{"bad id", "/api/custom_plugins/x", `{"merge_variables": {}}`, 404, "no such instance"},
		{"unknown strategy", path, `{"merge_strategy": "append", "merge_variables": {}}`, 400, "unknown merge_strategy"},
		{"array", path, `{"merge_variables": [1]}`, 400, "not a JSON object"},
		{"string", path, `{"merge_variables": "x"}`, 400, "not a JSON object"},
		{"null", path, `{"merge_variables": null}`, 400, "not a JSON object"},
		{"missing", path, `{}`, 400, "not a JSON object"},
		{"unknown field", path, `{"merge_variables": {}, "merge": 1}`, 400, "unknown field"},
		{"not json", path, `merge`, 400, "bad request body"},
		{"trmnl key", path, `{"merge_variables": {"trmnl": {"device": {}}}}`, 400, "trmnl"},
		{"trmnl key when deep merging", path, `{"merge_strategy": "deep_merge", "merge_variables": {"trmnl": 1}}`, 400, "trmnl"},
		{"negative limit", path, `{"merge_strategy": "stream", "stream_limit": -1, "merge_variables": {}}`, 400, "stream_limit"},
		{"body over 64 KB", path, `{"merge_variables": {"a": "` + strings.Repeat("x", 64<<10) + `"}}`, 413, "body is over"},
		{"result over 64 KB", path, `{"merge_strategy": "deep_merge", "merge_variables": {"b": ` + big + `}}`, 413, "over the limit"},
	} {
		ta.wantError(t, "POST", tc.path, tc.body, tc.status, tc.contains)
	}
	// A refused push leaves the data as it was.
	for _, k := range []string{`"keep":1`, `"a":"xxx`} {
		if w := ta.call(t, "GET", path, "", 200, nil); !strings.Contains(w.Body.String(), k) {
			t.Errorf("data lost %s", k)
		}
	}
	ta.wantError(t, "GET", "/api/custom_plugins/99", "", 404, "no such instance")
}

func TestPushRateLimit(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	_, path := ta.newInstancePath(t)
	_, other := ta.newInstancePath(t)
	for range pushLimit {
		ta.call(t, "POST", path, `{"merge_variables": {}}`, 200, nil)
	}
	w := ta.call(t, "POST", path, `{"merge_variables": {}}`, 429, nil)
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After %q, want 60", got)
	}
	// A refused push takes no token, so the wait does not grow.
	w = ta.call(t, "POST", path, `{"merge_variables": {}}`, 429, nil)
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("second Retry-After %q, want 60", got)
	}
	// Reading is not limited, and another instance has its own limit.
	ta.call(t, "GET", path, "", 200, nil)
	ta.call(t, "POST", other, `{"merge_variables": {}}`, 200, nil)
	// A minute later there is room for one.
	ta.now = ta.now.Add(time.Minute)
	ta.call(t, "POST", path, `{"merge_variables": {}}`, 200, nil)
	ta.call(t, "POST", path, `{"merge_variables": {}}`, 429, nil)
}

func TestPushDeletedWithInstance(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	id, path := ta.newInstancePath(t)
	ta.call(t, "POST", path, `{"merge_variables": {"a": 1}}`, 200, nil)
	if _, err := ta.db.Read.GetInstanceData(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	ta.call(t, "DELETE", "/api/v1/instances/"+strconv.FormatInt(id, 10), "", 204, nil)
	if _, err := ta.db.Read.GetInstanceData(t.Context(), id); err == nil {
		t.Error("the instance's data is still there")
	}
	ta.wantError(t, "POST", path, `{"merge_variables": {}}`, 404, "no such instance")
}

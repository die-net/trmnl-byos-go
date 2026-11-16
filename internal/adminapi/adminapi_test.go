package adminapi

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/apitoken"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

const (
	testMAC    = "D8:3B:DA:12:34:56"
	testFriend = "ABC123"
	// testSecret is a secret field value no response may contain.
	testSecret = "hunter2-secret-value"
)

// testAPI is a server on a temp database with one token.
type testAPI struct {
	srv     *Server
	db      *db.Client
	mux     *http.ServeMux
	token   string
	now     time.Time
	recipes string
	// firmware is where uploaded firmware is stored.
	firmware string
}

func newTestAPI(t *testing.T, withKey bool) *testAPI {
	t.Helper()
	dir := t.TempDir()
	client, err := db.Open(t.Context(), filepath.Join(dir, "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var key *secrets.Key
	if withKey {
		if key, err = secrets.Parse(base64.StdEncoding.EncodeToString(make([]byte, 32))); err != nil {
			t.Fatal(err)
		}
	}
	ta := &testAPI{db: client, mux: http.NewServeMux(), now: time.Unix(1_000_000, 0), recipes: filepath.Join(dir, "recipes"), firmware: filepath.Join(dir, "firmware")}
	ta.srv = New(client, Config{Key: key, RecipesDir: ta.recipes, FirmwareDir: ta.firmware})
	ta.srv.now = func() time.Time { return ta.now }
	ta.srv.Register(ta.mux)
	if ta.token, err = apitoken.Create(t.Context(), client, "test", ta.now); err != nil {
		t.Fatal(err)
	}
	return ta
}

// do sends a request with the test token, or with auth when it is not
// empty, and returns the response.
func (ta *testAPI) do(t *testing.T, method, path string, body io.Reader, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, body)
	req.Header.Set("Authorization", "Bearer "+ta.token)
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	w := httptest.NewRecorder()
	ta.mux.ServeHTTP(w, req)
	return w
}

// call sends a request with a JSON body, checks the status, and decodes the
// response into out unless it is nil.
func (ta *testAPI) call(t *testing.T, method, path, body string, want int, out any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	w := ta.do(t, method, path, r)
	if w.Code != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, w.Code, want, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, w.Body, err)
		}
	}
	return w
}

// wantError checks that a request fails with the status and an error body
// holding contains.
func (ta *testAPI) wantError(t *testing.T, method, path, body string, status int, contains string) {
	t.Helper()
	var e errorBody
	ta.call(t, method, path, body, status, &e)
	if !strings.Contains(e.Error, contains) {
		t.Errorf("%s %s: error %q, want it to contain %q", method, path, e.Error, contains)
	}
}

func (ta *testAPI) addDevice(t *testing.T) db.Device {
	t.Helper()
	dev, err := ta.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: testMAC, ApiKeyHash: []byte("not a real hash, but 32 bytes!!!"), FriendlyID: testFriend,
		FwModel: "xiao_epaper_display", FwVersion: "1.8.10", CreatedAt: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

// settingsYML is a recipe whose custom fields are all optional, so that an
// instance needs no values: a string, three passwords, a multi-select, and
// a bounded number.
const settingsYML = `name: Weather
refresh_interval: 60
custom_fields:
- keyname: city
  field_type: string
  name: City
  optional: true
- keyname: apikey
  field_type: password
  optional: true
- keyname: zkey
  field_type: password
  optional: true
- keyname: k
  field_type: password
  optional: true
- keyname: tags
  field_type: select
  multiple: true
  optional: true
  options: [a, b, c]
- keyname: days
  field_type: number
  optional: true
  min: 1
`

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (ta *testAPI) addRecipe(t *testing.T) recipeJSON {
	t.Helper()
	w := ta.do(t, "POST", "/api/v1/recipes", bytes.NewReader(zipBytes(t, map[string]string{"settings.yml": settingsYML, "full.liquid": "hi"})),
		"Content-Type", "application/zip")
	if w.Code != http.StatusCreated {
		t.Fatalf("install recipe: %d %s", w.Code, w.Body)
	}
	var rec recipeJSON
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestAuth(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	removed, err := apitoken.Create(t.Context(), ta.db, "old", ta.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := apitoken.Delete(t.Context(), ta.db, "old"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, header string
		want         int
	}{
		{"none", "", 401},
		{"basic", "Basic " + ta.token, 401},
		{"no token", "Bearer", 401},
		{"empty token", "Bearer  ", 401},
		{"unknown", "Bearer " + base64.RawURLEncoding.EncodeToString(make([]byte, 32)), 401},
		{"removed", "Bearer " + removed, 401},
		{"lower case scheme", "bearer " + ta.token, 200},
		{"good", "Bearer " + ta.token, 200},
	} {
		for _, path := range []string{"/api/v1/devices", "/api/v1/nothing"} {
			w := ta.do(t, "GET", path, nil, "Authorization", tc.header)
			want := tc.want
			if want == 200 && path == "/api/v1/nothing" {
				want = 404
			}
			if w.Code != want {
				t.Errorf("%s %s: status %d, want %d", tc.name, path, w.Code, want)
			}
			if want == 401 {
				if got := w.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("%s %s: WWW-Authenticate %q", tc.name, path, got)
				}
				var e errorBody
				if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error == "" {
					t.Errorf("%s %s: body %q is not a JSON error", tc.name, path, w.Body)
				}
			}
		}
	}
	// A wrong method on a real path is refused without a token too.
	if w := ta.do(t, "DELETE", "/api/v1/devices", nil, "Authorization", ""); w.Code != 401 {
		t.Errorf("DELETE /devices without token: %d", w.Code)
	}
}

func TestTokenLastUsed(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	lastUsed := func() int64 {
		rows, err := ta.db.Read.ListTokens(t.Context())
		if err != nil || len(rows) != 1 {
			t.Fatalf("tokens %v, %v", rows, err)
		}
		return rows[0].LastUsedAt
	}
	if got := lastUsed(); got != 0 {
		t.Fatalf("last used %d before any use", got)
	}
	start := ta.now
	for _, tc := range []struct {
		after time.Duration
		want  time.Duration
	}{
		{0, 0},
		{30 * time.Second, 0},
		{59 * time.Second, 0},
		{61 * time.Second, 61 * time.Second},
		{100 * time.Second, 61 * time.Second},
		{200 * time.Second, 200 * time.Second},
	} {
		ta.now = start.Add(tc.after)
		ta.call(t, "GET", "/api/v1/join", "", 200, nil)
		if got, want := lastUsed(), start.Add(tc.want).Unix(); got != want {
			t.Errorf("after %v: last used %d, want %d", tc.after, got, want)
		}
	}
	// A refused request does not count as a use.
	ta.now = start.Add(time.Hour)
	ta.do(t, "GET", "/api/v1/join", nil, "Authorization", "Bearer nope")
	if got, want := lastUsed(), start.Add(200*time.Second).Unix(); got != want {
		t.Errorf("after a refused request: last used %d, want %d", got, want)
	}
}

func TestTokens(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ctx := t.Context()
	if _, err := apitoken.Create(ctx, ta.db, "test", ta.now); !errors.Is(err, apitoken.ErrExists) {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := apitoken.Create(ctx, ta.db, "  ", ta.now); !errors.Is(err, apitoken.ErrName) {
		t.Errorf("blank name: %v", err)
	}
	if err := apitoken.Delete(ctx, ta.db, "nobody"); !errors.Is(err, apitoken.ErrNotFound) {
		t.Errorf("remove unknown: %v", err)
	}
	other, err := apitoken.Create(ctx, ta.db, "other", ta.now)
	if err != nil {
		t.Fatal(err)
	}
	if other == ta.token || len(other) != 43 {
		t.Errorf("token %q: want 43 base64url characters, different from %q", other, ta.token)
	}
	rows, err := ta.db.Read.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if bytes.Contains(r.TokenHash, []byte(other)) || string(r.TokenHash) == other {
			t.Errorf("token %q is stored in the clear", r.Name)
		}
	}
}

func TestDevices(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	dev := ta.addDevice(t)
	err := ta.db.Write.UpsertDeviceState(t.Context(), db.UpsertDeviceStateParams{
		DeviceID: dev.ID, LastSeen: 77, LastRefreshRateSent: 900, BatteryV: new(3.9), Rssi: new(int64(-60)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var list []deviceJSON
	w := ta.call(t, "GET", "/api/v1/devices", "", 200, &list)
	if len(list) != 1 || list[0].ID != testFriend || list[0].MAC != testMAC || list[0].Model != "xiao_epaper_display" ||
		list[0].LastSeen == nil || *list[0].LastSeen != 77 || list[0].WakeTimeMs != nil {
		t.Errorf("devices %+v", list)
	}
	if s := w.Body.String(); strings.Contains(strings.ToLower(s), "api_key") || strings.Contains(s, "hash") {
		t.Errorf("response mentions the key: %s", s)
	}
	for _, id := range []string{testFriend, "abc123", testMAC, strings.ToLower(testMAC)} {
		var one deviceJSON
		ta.call(t, "GET", "/api/v1/devices/"+id, "", 200, &one)
		if one.ID != testFriend || one.BatteryV == nil || *one.BatteryV != 3.9 {
			t.Errorf("device %s: %+v", id, one)
		}
	}
	ta.wantError(t, "GET", "/api/v1/devices/ZZZ999", "", 404, "no such device")
}

func TestJoin(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	var j joinJSON
	ta.call(t, "GET", "/api/v1/join", "", 200, &j)
	if j.Open || j.Until != 0 {
		t.Errorf("fresh join %+v", j)
	}
	ta.call(t, "PUT", "/api/v1/join", `{"for_seconds": 600}`, 200, &j)
	if !j.Open || j.Until != ta.now.Unix()+600 {
		t.Errorf("opened join %+v", j)
	}
	ta.call(t, "GET", "/api/v1/join", "", 200, &j)
	if !j.Open {
		t.Errorf("join read back %+v", j)
	}
	ta.now = ta.now.Add(time.Hour)
	ta.call(t, "GET", "/api/v1/join", "", 200, &j)
	if j.Open {
		t.Errorf("join after expiry %+v", j)
	}
	ta.call(t, "PUT", "/api/v1/join", `{"for_seconds": 600}`, 200, nil)
	ta.call(t, "PUT", "/api/v1/join", `{"for_seconds": 0}`, 200, &j)
	if j.Open {
		t.Errorf("closed join %+v", j)
	}
	for _, tc := range []struct{ name, body, contains string }{
		{"missing", `{}`, "for_seconds is required"},
		{"negative", `{"for_seconds": -1}`, "seconds"},
		{"huge", `{"for_seconds": 9223372036854775807}`, "seconds"},
		{"unknown field", `{"for_seconds": 1, "extra": true}`, "unknown field"},
		{"bad json", `{"for_seconds": `, "bad request body"},
		{"wrong type", `{"for_seconds": "ten"}`, "bad request body"},
		{"trailing", `{"for_seconds": 1} {}`, "bad request body"},
		{"trailing closer", `{"for_seconds": 1}}`, "bad request body"},
		{"empty", ``, "bad request body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := ta.do(t, "PUT", "/api/v1/join", strings.NewReader(tc.body))
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.contains) {
				t.Errorf("status %d: %s", w.Code, w.Body)
			}
		})
	}
	if w := ta.do(t, "PUT", "/api/v1/join", strings.NewReader("{\"for_seconds\": 1}\n \n")); w.Code != 200 {
		t.Errorf("trailing white space: %d: %s", w.Code, w.Body)
	}
	// A body over the limit is refused before it is parsed.
	big := `{"for_seconds": 1, "pad": "` + strings.Repeat("x", maxJSONBody) + `"}`
	if w := ta.do(t, "PUT", "/api/v1/join", strings.NewReader(big)); w.Code != 413 {
		t.Errorf("oversize body: %d", w.Code)
	}
}

func TestCalibrate(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	dev := ta.addDevice(t)
	var c calibrateResponse
	ta.call(t, "POST", "/api/v1/devices/"+testFriend+"/calibrate", `{"dot": 2, "for_seconds": 120}`, 200, &c)
	if c.Device != testFriend || c.Dot != 2 || c.Until != ta.now.Unix()+120 {
		t.Errorf("response %+v", c)
	}
	got, err := ta.db.Read.GetDeviceByMAC(t.Context(), dev.Mac)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScreenOverride != device.ScreenCalibrate2x2 || got.ScreenOverrideUntil != c.Until {
		t.Errorf("override %q until %d", got.ScreenOverride, got.ScreenOverrideUntil)
	}
	// An empty body takes the defaults.
	ta.call(t, "POST", "/api/v1/devices/"+testMAC+"/calibrate", "", 200, &c)
	if c.Dot != 1 || c.Until != ta.now.Add(device.DefaultCalibration).Unix() {
		t.Errorf("defaults %+v", c)
	}
	ta.wantError(t, "POST", "/api/v1/devices/"+testFriend+"/calibrate", `{"dot": 3}`, 400, "dot size")
	ta.wantError(t, "POST", "/api/v1/devices/"+testFriend+"/calibrate", `{"for_seconds": -5}`, 400, "seconds")
	ta.wantError(t, "POST", "/api/v1/devices/"+testFriend+"/calibrate", `{"dots": 1}`, 400, "unknown field")
	ta.wantError(t, "POST", "/api/v1/devices/ZZZ999/calibrate", `{}`, 404, "no such device")
}

func TestRecipes(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	rec := ta.addRecipe(t)
	if rec.Name != "Weather" || rec.RefreshSeconds != 3600 || rec.Source != "upload" || rec.InstalledAt != ta.now.Unix() {
		t.Errorf("installed %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(recipe.Dir(ta.recipes, rec.ID), "full.liquid")); err != nil {
		t.Errorf("recipe files: %v", err)
	}
	var list []recipeJSON
	ta.call(t, "GET", "/api/v1/recipes", "", 200, &list)
	if len(list) != 1 || list[0] != rec {
		t.Errorf("list %+v", list)
	}
	var one recipeDetailJSON
	w := ta.call(t, "GET", "/api/v1/recipes/"+strconv.FormatInt(rec.ID, 10), "", 200, &one)
	if one.recipeJSON != rec || len(one.CustomFields) != 6 {
		t.Errorf("get %+v", one)
	}
	if f := one.CustomFields[0]; f.Keyname != "city" || f.Label != "City" || f.Type != "string" || f.Input != "text" || f.Required || f.Secret {
		t.Errorf("the first field %+v", f)
	}
	if f := one.CustomFields[1]; f.Keyname != "apikey" || f.Input != "password" || !f.Secret {
		t.Errorf("the password field %+v", f)
	}
	if f := one.CustomFields[0]; !f.CanMarkSecret || f.LooksSecret {
		t.Errorf("the string field's marks %+v", f)
	}
	if f := one.CustomFields[1]; f.CanMarkSecret || !f.LooksSecret {
		t.Errorf("the password field's marks %+v", f)
	}
	if f := one.CustomFields[5]; f.CanMarkSecret || f.LooksSecret {
		t.Errorf("the number field's marks %+v", f)
	}
	tags := one.CustomFields[4]
	if tags.Input != "select" || !tags.Multiple || len(tags.Options) != 3 || tags.Options[1] != (optionJSON{Label: "b", Value: "b"}) {
		t.Errorf("the select field %+v", tags)
	}
	if lo := one.CustomFields[5].Min; lo == nil || *lo != 1 || !strings.Contains(w.Body.String(), `"custom_fields":[{"keyname":"city"`) {
		t.Errorf("the number field's min %v in %s", lo, w.Body)
	}
	if !strings.Contains(ta.call(t, "GET", "/api/v1/recipes", "", 200, nil).Body.String(), `"source"`) ||
		strings.Contains(ta.call(t, "GET", "/api/v1/recipes", "", 200, nil).Body.String(), "custom_fields") {
		t.Error("the list shows the field definitions, which would read every recipe's settings.yml")
	}
	ta.wantError(t, "GET", "/api/v1/recipes/99", "", 404, "no such recipe")
	ta.wantError(t, "GET", "/api/v1/recipes/abc", "", 404, "no such recipe")

	good := zipBytes(t, map[string]string{"settings.yml": settingsYML, "full.liquid": "hi"})
	for _, tc := range []struct {
		name, contentType string
		body              []byte
		status            int
		// contains is what the error says, when it says more than the status.
		contains string
	}{
		{"not a zip", "application/zip", []byte("plain text"), 400, "the file is not a zip archive"},
		{"escaping path", "application/zip", zipBytes(t, map[string]string{"settings.yml": settingsYML, "full.liquid": "x", "../escape": "x"}), 400, "the zip has a file name that leaves its directory"},
		{"no full.liquid", "application/zip", zipBytes(t, map[string]string{"settings.yml": settingsYML}), 400, "the recipe has no full.liquid"},
		{"over the limit", "application/zip", bytes.Repeat([]byte{'x'}, recipe.MaxRecipeBytes+1), 413, ""},
		{"wrong content type", "application/json", good, 415, ""},
		{"no content type", "", good, 415, ""},
		{"zip with parameters", "application/zip; charset=binary", good, 201, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ta := newTestAPI(t, false)
			ta.addRecipe(t)
			before, err := ta.db.Read.ListRecipes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			w := ta.do(t, "POST", "/api/v1/recipes", bytes.NewReader(tc.body), "Content-Type", tc.contentType)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.contains) {
				t.Errorf("body %s, want it to contain %q", w.Body, tc.contains)
			}
			after, err := ta.db.Read.ListRecipes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := len(before)
			if tc.status == 201 {
				want++
			}
			if len(after) != want {
				t.Errorf("%d recipes, want %d", len(after), want)
			}
			// No upload file or half-installed directory is left behind.
			entries, err := os.ReadDir(ta.recipes)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(after) {
				t.Errorf("recipes dir has %d entries for %d recipes", len(entries), len(after))
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(ta.recipes), "escape")); err == nil {
				t.Error("the zip escaped the recipes directory")
			}
		})
	}
}

// TestRecipeUploadCutOff checks that a client that drops the connection
// part way through a recipe upload gets a client error, not a server one,
// and that no row or file is left.
func TestRecipeUploadCutOff(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	z := zipBytes(t, map[string]string{"settings.yml": settingsYML, "full.liquid": "hi"})
	body := io.MultiReader(bytes.NewReader(z[:len(z)/2]), iotest.ErrReader(io.ErrUnexpectedEOF))
	w := ta.do(t, "POST", "/api/v1/recipes", body, "Content-Type", "application/zip")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "the upload was cut off") {
		t.Fatalf("status %d: %s; want 400 saying the upload was cut off", w.Code, w.Body)
	}
	if rows, err := ta.db.Read.ListRecipes(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("recipes = %v, %v; want none", rows, err)
	}
	if entries, err := os.ReadDir(ta.recipes); err != nil || len(entries) != 0 {
		t.Errorf("recipes dir = %v, %v; want it empty", entries, err)
	}
}

func TestInstances(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	rec := ta.addRecipe(t)
	body := `{"recipe_id": ` + strconv.FormatInt(rec.ID, 10) + `, "name": "Home", "palette": "bw", "fields": {"city": "Oslo", "tags": ["a", "c"], "zkey": "` + testSecret + `", "apikey": "` + testSecret + `"}}`
	var created instanceJSON
	w := ta.call(t, "POST", "/api/v1/instances", body, 201, &created)
	if created.Name != "Home" || created.RecipeName != "Weather" || created.Palette != "bw" ||
		created.Fields["city"].One != "Oslo" || !slices.Equal(created.Fields["tags"].Many, []string{"a", "c"}) || len(created.Fields) != 2 || strings.Join(created.SecretFields, ",") != "apikey,zkey" {
		t.Errorf("created %+v", created)
	}
	id := strconv.FormatInt(created.ID, 10)
	bodies := append(make([]string, 0, 8), w.Body.String())
	var list []instanceJSON
	bodies = append(bodies, ta.call(t, "GET", "/api/v1/instances", "", 200, &list).Body.String())
	var one instanceJSON
	bodies = append(bodies, ta.call(t, "GET", "/api/v1/instances/"+id, "", 200, &one).Body.String())
	if len(list) != 1 || one.ID != created.ID || strings.Join(one.SecretFields, ",") != "apikey,zkey" {
		t.Errorf("list %+v, get %+v", list, one)
	}
	// Every response is checked for the secret, including the ones for a
	// failure.
	for _, tc := range []struct{ name, body, contains string }{
		{"unknown palette", `{"recipe_id": 1, "palette": "neon", "fields": {"k": "` + testSecret + `"}}`, "unknown palette"},
		{"short secret", `{"recipe_id": 1, "fields": {"k": "abc"}}`, "k: must be at least 6 characters"},
		{"no such field", `{"recipe_id": 1, "fields": {"nope": "` + testSecret + `"}}`, "nope: no such field"},
		{"unknown recipe", `{"recipe_id": 99, "fields": {"k": "` + testSecret + `"}}`, "no such recipe"},
		{"secrets is gone", `{"recipe_id": 1, "secrets": {"k": "` + testSecret + `"}}`, "unknown field"},
		{"bad json", `{"recipe_id": 1, "fields": {"k": "` + testSecret, "bad request body"},
		{"a number for a value", `{"recipe_id": 1, "fields": {"k": 7}}`, "string or an array of strings"},
	} {
		w := ta.do(t, "POST", "/api/v1/instances", strings.NewReader(tc.body))
		bodies = append(bodies, w.Body.String())
		wantStatus := 400
		if tc.name == "unknown recipe" {
			wantStatus = 404
		}
		if w.Code != wantStatus || !strings.Contains(w.Body.String(), tc.contains) {
			t.Errorf("%s: status %d: %s", tc.name, w.Code, w.Body)
		}
	}
	for _, b := range bodies {
		if strings.Contains(b, testSecret) {
			t.Errorf("response shows a secret: %s", b)
		}
	}
	// The recipe's name is the default.
	var plain instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": 1}`, 201, &plain)
	if plain.Name != "Weather" || plain.Fields == nil || plain.SecretFields == nil || len(plain.SecretFields) != 0 {
		t.Errorf("defaults %+v", plain)
	}
	ta.wantError(t, "GET", "/api/v1/instances/99", "", 404, "no such instance")
	ta.wantError(t, "DELETE", "/api/v1/instances/99", "", 404, "no such instance")
	ta.wantError(t, "DELETE", "/api/v1/instances/x", "", 404, "no such instance")

	// An instance in a playlist cannot be removed until its item is.
	ta.addDevice(t)
	ta.call(t, "POST", "/api/v1/playlists", `{"name": "p"}`, 201, nil)
	var item addItemResponse
	ta.call(t, "POST", "/api/v1/playlists/items", `{"playlist": "p", "device": "`+testFriend+`", "instance_id": `+id+`}`, 201, &item)
	ta.wantError(t, "DELETE", "/api/v1/instances/"+id, "", 409, "instance is in 1 playlist items")
	ta.call(t, "DELETE", "/api/v1/playlists/items/"+strconv.FormatInt(item.ID, 10), "", 204, nil)
	ta.call(t, "DELETE", "/api/v1/instances/"+id, "", 204, nil)
	ta.wantError(t, "GET", "/api/v1/instances/"+id, "", 404, "no such instance")
}

// TestInstanceSecretsNeedKey checks that a server with no secrets key
// refuses a new secret value, and that it still lists an instance whose
// secrets were sealed earlier, with secret_fields null.
func TestInstanceSecretsNeedKey(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	rec := ta.addRecipe(t)
	recID := strconv.FormatInt(rec.ID, 10)
	ta.wantError(t, "POST", "/api/v1/instances", `{"recipe_id": `+recID+`, "fields": {"k": "`+testSecret+`"}}`, 400, "TRMNL_SECRET_KEY")
	var list []instanceJSON
	ta.call(t, "GET", "/api/v1/instances", "", 200, &list)
	if len(list) != 0 {
		t.Errorf("instances %+v", list)
	}

	// An instance sealed under a key, then read by a server without one.
	key, err := secrets.Parse(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := recipe.CreateInstance(t.Context(), ta.db, key, ta.recipes, recipe.InstanceParams{
		RecipeID: rec.ID, Name: "sealed", Values: map[string]recipe.Value{"city": {One: "Oslo"}, "k": {One: testSecret}},
	}, ta.now)
	if err != nil {
		t.Fatal(err)
	}
	plain := ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+recID+`, "name": "plain"}`, 201, nil)
	if !strings.Contains(plain.Body.String(), `"secret_fields":[]`) {
		t.Errorf("an instance with no secrets: %s", plain.Body)
	}
	w := ta.call(t, "GET", "/api/v1/instances", "", 200, &list)
	if len(list) != 2 || list[0].Name != "sealed" || list[0].Fields["city"].One != "Oslo" || list[0].SecretFields != nil ||
		list[1].SecretFields == nil || len(list[1].SecretFields) != 0 {
		t.Errorf("list %+v", list)
	}
	if !strings.Contains(w.Body.String(), `"secret_fields":null`) || strings.Contains(w.Body.String(), testSecret) {
		t.Errorf("body %s", w.Body)
	}
	var one instanceJSON
	ta.call(t, "GET", "/api/v1/instances/"+strconv.FormatInt(sealed.ID, 10), "", 200, &one)
	if one.Name != "sealed" || one.SecretFields != nil {
		t.Errorf("get %+v", one)
	}
}

func TestPlaylists(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	rec := ta.addRecipe(t)
	var inst instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "name": "Home"}`, 201, &inst)
	instID := strconv.FormatInt(inst.ID, 10)

	var pa, pk playlistJSON
	ta.call(t, "POST", "/api/v1/playlists", `{"name": "`+testFriend+`"}`, 201, &pa)
	ta.call(t, "POST", "/api/v1/playlists", `{"name": "kitchen"}`, 201, &pk)
	var a, b addItemResponse
	ta.call(t, "POST", "/api/v1/playlists/items", `{"playlist_id": `+strconv.FormatInt(pa.ID, 10)+`, "device": "`+testMAC+`", "instance_id": `+instID+`, "duration_seconds": 600}`, 201, &a)
	ta.call(t, "POST", "/api/v1/playlists/items", `{"playlist": "kitchen", "instance_id": `+instID+`, "duration_seconds": 0}`, 201, &b)
	if a.PlaylistID != pa.ID || a.Position != 0 || b.PlaylistID != pk.ID {
		t.Errorf("added %+v and %+v", a, b)
	}
	var lists []playlistJSON
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	if len(lists) != 2 || lists[0].Name != testFriend || lists[1].Name != "kitchen" {
		t.Fatalf("playlists %+v", lists)
	}
	if d := lists[0].Devices; len(d) != 1 || d[0].Device != testFriend {
		t.Errorf("devices %+v", d)
	}
	if it := lists[0].Items; len(it) != 1 || it[0].ID != a.ID || it[0].DurationSeconds != 600 || it[0].InstanceName != "Home" {
		t.Errorf("items %+v", it)
	}
	if lists[1].Devices == nil || len(lists[1].Devices) != 0 || lists[1].Items[0].DurationSeconds != 0 {
		t.Errorf("kitchen %+v", lists[1])
	}
	// Left out, the duration is the default.
	var c addItemResponse
	ta.call(t, "POST", "/api/v1/playlists/items", `{"playlist": "kitchen", "instance_id": `+instID+`}`, 201, &c)
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	if got := lists[1].Items[1]; got.ID != c.ID || got.DurationSeconds != 900 {
		t.Errorf("default duration %+v", got)
	}

	for _, tc := range []struct {
		name, body string
		status     int
		contains   string
	}{
		{"unknown instance", `{"playlist": "kitchen", "instance_id": 99}`, 404, "no such instance"},
		{"missing playlist", `{"playlist": "nope", "instance_id": ` + instID + `}`, 404, "no such playlist"},
		{"missing playlist ID", `{"playlist_id": 999, "instance_id": ` + instID + `}`, 404, "no such playlist"},
		{"both playlist and ID", `{"playlist": "kitchen", "playlist_id": ` + strconv.FormatInt(pk.ID, 10) + `, "instance_id": ` + instID + `}`, 400, "exactly one of"},
		{"device alone", `{"device": "` + testFriend + `", "instance_id": ` + instID + `}`, 400, "exactly one of"},
		{"unknown device", `{"playlist": "kitchen", "device": "ZZZ999", "instance_id": ` + instID + `}`, 404, "no such device"},
		{"no playlist", `{"instance_id": ` + instID + `}`, 400, "exactly one of"},
		{"too short", `{"playlist": "kitchen", "instance_id": ` + instID + `, "duration_seconds": 10}`, 400, "under"},
		{"negative", `{"playlist": "kitchen", "instance_id": ` + instID + `, "duration_seconds": -1}`, 400, "seconds"},
		{"unknown field", `{"playlist": "kitchen", "instance": ` + instID + `}`, 400, "unknown field"},
	} {
		ta.wantError(t, "POST", "/api/v1/playlists/items", tc.body, tc.status, tc.contains)
	}
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	if len(lists) != 2 {
		t.Errorf("a refused item made a playlist: %+v", lists)
	}

	ta.call(t, "DELETE", "/api/v1/playlists/items/"+strconv.FormatInt(a.ID, 10), "", 204, nil)
	ta.wantError(t, "DELETE", "/api/v1/playlists/items/"+strconv.FormatInt(a.ID, 10), "", 404, "no such playlist item")
	ta.wantError(t, "DELETE", "/api/v1/playlists/items/x", "", 404, "no such playlist item")
}

// TestPlaylistCRUD checks creating, renaming, deleting and linking playlists:
// the statuses and bodies, and that a refused request changes nothing.
func TestPlaylistCRUD(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	var a, b playlistJSON
	ta.call(t, "POST", "/api/v1/playlists", `{"name": " Mornings "}`, 201, &a)
	ta.call(t, "POST", "/api/v1/playlists", `{"name": "Evenings"}`, 201, &b)
	if a.ID == 0 || a.Name != "Mornings" || a.Items == nil || a.Devices == nil || a.Schedule.Days == nil {
		t.Errorf("created %+v", a)
	}
	pa := "/api/v1/playlists/" + strconv.FormatInt(a.ID, 10)
	pb := "/api/v1/playlists/" + strconv.FormatInt(b.ID, 10)

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		contains                 string
	}{
		{"create empty name", "POST", "/api/v1/playlists", `{"name": " "}`, 400, "empty"},
		{"create no name", "POST", "/api/v1/playlists", `{}`, 400, "empty"},
		{"create unknown field", "POST", "/api/v1/playlists", `{"name": "x", "id": 4}`, 400, "unknown field"},
		{"create duplicate", "POST", "/api/v1/playlists", `{"name": "Mornings"}`, 409, "a playlist has that name"},
		{"rename to another's", "PATCH", pa, `{"name": "Evenings"}`, 409, "a playlist has that name"},
		{"rename empty", "PATCH", pa, `{"name": ""}`, 400, "empty"},
		{"rename with bad schedule", "PATCH", pa, `{"name": "Fresh", "schedule": {"days": []}}`, 400, "at least one weekday"},
		{"rename unknown", "PATCH", "/api/v1/playlists/999", `{"name": "Fresh"}`, 404, "no such playlist"},
		{"delete unknown", "DELETE", "/api/v1/playlists/999", "", 404, "no such playlist"},
		{"delete malformed", "DELETE", "/api/v1/playlists/x", "", 404, "no such playlist"},
		{"link unknown playlist", "PUT", "/api/v1/playlists/999/devices/" + testFriend, "", 404, "no such playlist"},
		{"link unknown device", "PUT", pa + "/devices/ZZZ999", "", 404, "no such device"},
		{"link takes no priority", "PUT", pa + "/devices/" + testFriend, `{"priority": 0}`, 400, "unknown field"},
		{"move unlinked", "PATCH", pa + "/devices/" + testFriend, `{"order": [], "priority": 0}`, 404, "not linked"},
		{"move unknown playlist", "PATCH", "/api/v1/playlists/999/devices/" + testFriend, `{"order": [], "priority": 0}`, 404, "no such playlist"},
		{"move unknown device", "PATCH", pa + "/devices/ZZZ999", `{"order": [], "priority": 0}`, 404, "no such device"},
		{"move no priority", "PATCH", pa + "/devices/" + testFriend, `{"order": []}`, 400, "give both order and priority"},
		{"move no from", "PATCH", pa + "/devices/" + testFriend, `{"priority": 0}`, 400, "give both order and priority"},
		{"move no body", "PATCH", pa + "/devices/" + testFriend, ``, 400, "bad request body"},
		{"move priority not a number", "PATCH", pa + "/devices/" + testFriend, `{"order": [], "priority": "first"}`, 400, "bad request body"},
		{"unlink not linked", "DELETE", pa + "/devices/" + testFriend, "", 404, "not linked"},
		{"unlink unknown device", "DELETE", pa + "/devices/ZZZ999", "", 404, "no such device"},
		{"unlink unknown playlist", "DELETE", "/api/v1/playlists/999/devices/" + testFriend, "", 404, "no such playlist"},
	} {
		ta.wantError(t, tc.method, tc.path, tc.body, tc.status, tc.contains)
	}
	var lists []playlistJSON
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	if len(lists) != 2 || lists[0].Name != "Evenings" || lists[1].Name != "Mornings" || len(lists[0].Devices)+len(lists[1].Devices) != 0 {
		t.Fatalf("a refused request changed the playlists: %+v", lists)
	}

	// Rename keeps the schedule, and a schedule patch keeps the name.
	var got playlistJSON
	ta.call(t, "PATCH", pa, `{"schedule": {"days": ["sat"]}}`, 200, &got)
	ta.call(t, "PATCH", pa, `{"name": "Early"}`, 200, &got)
	if got.Name != "Early" || !reflect.DeepEqual(got.Schedule.Days, []string{"sat"}) {
		t.Errorf("renamed %+v", got)
	}
	ta.call(t, "PATCH", pa, `{"name": "Early"}`, 200, &got)

	// A move that is refused makes no link.
	ta.wantError(t, "PATCH", pa+"/devices/"+testFriend, `{"order": [], "priority": 0}`, 404, "not linked")
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	if len(lists[0].Devices)+len(lists[1].Devices) != 0 {
		t.Errorf("a refused move linked a device: %+v", lists)
	}
	// Link by friendly ID and then by MAC, with no body, then move one.
	ta.call(t, "PUT", pa+"/devices/"+testFriend, "", 200, &got)
	if len(got.Devices) != 1 || got.Devices[0] != (playlistDeviceJSON{Device: testFriend, Priority: 0}) {
		t.Errorf("linked %+v", got.Devices)
	}
	ta.call(t, "PUT", pb+"/devices/"+testMAC, "", 200, &got)
	if len(got.Devices) != 1 || got.Devices[0].Priority != 1 {
		t.Errorf("linked last %+v", got.Devices)
	}
	ab := fmt.Sprintf(`[%d, %d]`, a.ID, b.ID)
	ba := fmt.Sprintf(`[%d, %d]`, b.ID, a.ID)
	ta.wantError(t, "PATCH", pb+"/devices/"+testMAC, `{"order": `+ab+`, "priority": 2}`, 400, "2 is not a place in the device's playlist order, which runs from 0 to 1")
	ta.wantError(t, "PATCH", pb+"/devices/"+testMAC, `{"order": `+ab+`, "priority": -1}`, 400, "-1 is not a place")
	ta.wantError(t, "PATCH", pb+"/devices/"+testMAC, `{"order": `+ba+`, "priority": 0}`, 409, "the device's playlist order has changed")
	ta.wantError(t, "PATCH", pb+"/devices/"+testMAC, `{"order": [`+fmt.Sprint(a.ID)+`], "priority": 0}`, 409, "the device's playlist order has changed")
	ta.call(t, "PATCH", pb+"/devices/"+testMAC, `{"order": `+ab+`, "priority": 0}`, 200, &got)
	if len(got.Devices) != 1 || got.Devices[0].Priority != 0 {
		t.Errorf("moved to the front %+v", got.Devices)
	}
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	// Playlists list by name: Early, then Evenings.
	if lists[0].Devices[0].Priority != 1 || lists[1].Devices[0].Priority != 0 {
		t.Errorf("priorities after the move: %+v", lists)
	}
	// Linking again leaves it, and a move changes it.
	ta.call(t, "PUT", pa+"/devices/"+testFriend, `{}`, 200, &got)
	if got.Devices[0].Priority != 1 {
		t.Errorf("relinked %+v", got.Devices)
	}
	ta.call(t, "PATCH", pa+"/devices/"+testFriend, `{"order": `+ba+`, "priority": 0}`, 200, &got)
	if got.Devices[0].Priority != 0 {
		t.Errorf("moved %+v", got.Devices)
	}

	// Unlinking and deleting close the gap.
	ta.call(t, "DELETE", pa+"/devices/"+testFriend, "", 204, nil)
	ta.wantError(t, "DELETE", pa+"/devices/"+testFriend, "", 404, "not linked")
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	if len(lists[0].Devices) != 0 || lists[1].Devices[0].Priority != 0 {
		t.Errorf("after unlink: %+v", lists)
	}
	ta.call(t, "PUT", pa+"/devices/"+testFriend, "", 200, nil)
	ta.call(t, "DELETE", pb, "", 204, nil)
	ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
	if len(lists) != 1 || lists[0].ID != a.ID || len(lists[0].Devices) != 1 || lists[0].Devices[0].Priority != 0 {
		t.Errorf("after delete: %+v", lists)
	}
	ta.wantError(t, "DELETE", pb, "", 404, "no such playlist")
}

// TestPatchPlaylists checks the schedule, hidden flag, duration and position
// of items and the schedule of playlists: what a successful PATCH answers and
// stores, and that a refused one, whatever else it asked for, changes nothing.
func TestPatchPlaylists(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	rec := ta.addRecipe(t)
	var inst instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "name": "Home"}`, 201, &inst)
	instID := strconv.FormatInt(inst.ID, 10)
	ta.call(t, "POST", "/api/v1/playlists", `{"name": "kitchen"}`, 201, nil)
	var items [3]addItemResponse
	for i := range items {
		ta.call(t, "POST", "/api/v1/playlists/items", `{"playlist": "kitchen", "instance_id": `+instID+`}`, 201, &items[i])
	}
	path := func(i int) string { return "/api/v1/playlists/items/" + strconv.FormatInt(items[i].ID, 10) }
	list := func() playlistJSON {
		t.Helper()
		var lists []playlistJSON
		ta.call(t, "GET", "/api/v1/playlists", "", 200, &lists)
		if len(lists) != 1 {
			t.Fatalf("playlists %+v", lists)
		}
		return lists[0]
	}

	// A new playlist and item are unrestricted.
	pl := list()
	always := scheduleJSON{Days: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, WindowStart: "00:00", WindowEnd: "00:00"}
	if pl.ID == 0 || !reflect.DeepEqual(pl.Schedule, always) || pl.Items[0].Hidden || !reflect.DeepEqual(pl.Items[0].Schedule, always) {
		t.Fatalf("new playlist %+v", pl)
	}

	// Everything at once.
	var got playlistItemJSON
	ta.call(t, "PATCH", path(0), `{"position": 2, "duration_seconds": 1200, "hidden": true,
		"schedule": {"days": ["mon", "fri"], "window_start": "22:00", "window_end": "06:30", "starts_at": 100, "ends_at": 200}}`, 200, &got)
	want := scheduleJSON{Days: []string{"mon", "fri"}, WindowStart: "22:00", WindowEnd: "06:30", StartsAt: 100, EndsAt: 200}
	if got.ID != items[0].ID || got.Position != 2 || got.DurationSeconds != 1200 || !got.Hidden || !reflect.DeepEqual(got.Schedule, want) {
		t.Fatalf("patched item %+v", got)
	}
	pl = list()
	if len(pl.Items) != 3 || pl.Items[0].ID != items[1].ID || pl.Items[1].ID != items[2].ID || pl.Items[2].ID != items[0].ID {
		t.Fatalf("order after the move: %+v", pl.Items)
	}
	for i, it := range pl.Items {
		if it.Position != int64(i) {
			t.Errorf("item %d has position %d", it.ID, it.Position)
		}
	}
	// Left out fields are unchanged, and a schedule replaces the old one.
	ta.call(t, "PATCH", path(0), `{"hidden": false}`, 200, &got)
	if got.Hidden || got.Position != 2 || got.DurationSeconds != 1200 || !reflect.DeepEqual(got.Schedule, want) {
		t.Errorf("after a partial patch %+v", got)
	}
	ta.call(t, "PATCH", path(0), `{"schedule": {"days": ["sat"]}}`, 200, &got)
	if wantSat := (scheduleJSON{Days: []string{"sat"}, WindowStart: "00:00", WindowEnd: "00:00"}); !reflect.DeepEqual(got.Schedule, wantSat) || got.DurationSeconds != 1200 {
		t.Errorf("after replacing the schedule %+v", got)
	}
	ta.call(t, "PATCH", path(0), `{}`, 200, &got)

	// The playlist's schedule.
	plPath := "/api/v1/playlists/" + strconv.FormatInt(pl.ID, 10)
	var gotPl playlistJSON
	ta.call(t, "PATCH", plPath, `{"schedule": {"days": ["sat", "sun"], "window_start": "08:00", "window_end": "20:00", "ends_at": 5000}}`, 200, &gotPl)
	wantPl := scheduleJSON{Days: []string{"sun", "sat"}, WindowStart: "08:00", WindowEnd: "20:00", EndsAt: 5000}
	if gotPl.ID != pl.ID || !reflect.DeepEqual(gotPl.Schedule, wantPl) || len(gotPl.Items) != 3 {
		t.Fatalf("patched playlist %+v", gotPl)
	}

	// A request that is refused changes nothing, including its valid parts.
	before := list()
	for _, tc := range []struct {
		name, path, body string
		status           int
		contains         string
	}{
		{"unknown day", path(1), `{"hidden": true, "position": 0, "schedule": {"days": ["monday"]}}`, 400, "not a weekday"},
		{"no days", path(1), `{"hidden": true, "schedule": {"days": []}}`, 400, "at least one weekday"},
		{"bad window", path(1), `{"hidden": true, "schedule": {"window_start": "25:00"}}`, 400, "HH:MM"},
		{"bad window end", path(1), `{"hidden": true, "schedule": {"window_end": "9am"}}`, 400, "HH:MM"},
		{"backwards range", path(1), `{"hidden": true, "schedule": {"starts_at": 20, "ends_at": 10}}`, 400, "starts_at must be before"},
		{"negative start", path(1), `{"hidden": true, "schedule": {"starts_at": -1}}`, 400, "cannot be negative"},
		{"position past the end", path(1), `{"hidden": true, "duration_seconds": 1800, "position": 3}`, 400, "3 is not a place in the playlist, which runs from 0 to 2"},
		{"negative position", path(1), `{"hidden": true, "position": -1}`, 400, "-1 is not a place in the playlist"},
		{"too short", path(1), `{"hidden": true, "position": 0, "duration_seconds": 10}`, 400, "under"},
		{"negative duration", path(1), `{"hidden": true, "duration_seconds": -5}`, 400, "seconds"},
		{"unknown field", path(1), `{"hidden": true, "visible": true}`, 400, "unknown field"},
		{"hidden is not a bool", path(1), `{"hidden": "yes"}`, 400, "bad request body"},
		{"empty body", path(1), ``, 400, "bad request body"},
		{"unknown item", "/api/v1/playlists/items/999", `{"hidden": true}`, 404, "no such playlist item"},
		{"malformed item", "/api/v1/playlists/items/x", `{"hidden": true}`, 404, "no such playlist item"},
		{"playlist unknown day", plPath, `{"schedule": {"days": ["someday"]}}`, 400, "not a weekday"},
		{"playlist bad range", plPath, `{"schedule": {"starts_at": 9, "ends_at": 9}}`, 400, "starts_at must be before"},
		{"playlist unknown field", plPath, `{"color": "red"}`, 400, "unknown field"},
		{"unknown playlist", "/api/v1/playlists/999", `{"schedule": {}}`, 404, "no such playlist"},
		{"malformed playlist", "/api/v1/playlists/x", `{"schedule": {}}`, 404, "no such playlist"},
	} {
		ta.wantError(t, "PATCH", tc.path, tc.body, tc.status, tc.contains)
		if after := list(); !reflect.DeepEqual(after, before) {
			t.Errorf("%s changed the playlist:\nbefore %+v\nafter  %+v", tc.name, before, after)
		}
	}
}

func TestRenders(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	dev := ta.addDevice(t)
	rec := ta.addRecipe(t)
	var inst instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`}`, 201, &inst)
	ctx := t.Context()
	if _, err := ta.db.Write.SaveRenderSuccess(ctx, db.SaveRenderSuccessParams{
		InstanceID: inst.ID, Width: 800, Height: 480, Palette: "bw", RenderedAt: 50, AttemptedAt: 60, DurationMs: 2500, Log: "shared build log",
	}); err != nil {
		t.Fatal(err)
	}
	err := ta.db.Write.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
		InstanceID: inst.ID, DeviceID: dev.ID, Width: 800, Height: 480, Palette: "bw", AttemptedAt: 70, DurationMs: 300, Error: "boom", Log: "device build log",
	})
	if err != nil {
		t.Fatal(err)
	}
	var list []renderJSON
	w := ta.call(t, "GET", "/api/v1/renders", "", 200, &list)
	if len(list) != 2 || list[0].Device != nil || list[0].RenderedAt != 50 || list[0].DurationSeconds != 2.5 || list[0].Error != "" ||
		list[1].Device == nil || *list[1].Device != testFriend || list[1].Error != "boom" || list[1].RenderedAt != 0 ||
		list[0].Width != 800 || list[0].Palette != "bw" || list[0].InstanceID != inst.ID {
		t.Errorf("renders %+v", list)
	}
	if strings.Contains(w.Body.String(), "build log") {
		t.Errorf("list has a log: %s", w.Body)
	}
	var one renderDetailJSON
	ta.call(t, "GET", "/api/v1/renders/"+strconv.FormatInt(list[1].ID, 10), "", 200, &one)
	if one.Log != "device build log" || one.Error != "boom" || one.Device == nil {
		t.Errorf("render %+v", one)
	}
	ta.wantError(t, "GET", "/api/v1/renders/99", "", 404, "no such render")
}

// TestServerError checks that a failure the caller did not cause is
// answered with a generic message, not the detail.
func TestServerError(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	rec := ta.addRecipe(t)
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "fields": {"k": "hunter2"}}`, 201, nil)
	// A closed database fails the list in a way the caller did not cause.
	_ = ta.db.Close()
	var e errorBody
	ta.call(t, "GET", "/api/v1/instances", "", 500, &e)
	if e.Error != "internal error" {
		t.Errorf("error %q", e.Error)
	}
}

// TestSlowUpload checks that an upload that takes longer than the server's
// own read timeout still completes, since the upload routes set their own
// deadlines.
func TestSlowUpload(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	srv := httptest.NewUnstartedServer(ta.mux)
	srv.Config.ReadTimeout = 200 * time.Millisecond
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	body := zipBytes(t, map[string]string{"settings.yml": settingsYML, "full.liquid": "hi"})
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(body[:len(body)/2])
		time.Sleep(500 * time.Millisecond)
		_, err := pw.Write(body[len(body)/2:])
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/recipes", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ta.token)
	req.Header.Set("Content-Type", "application/zip")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow upload: %d %s", resp.StatusCode, out)
	}
}

// TestFailStatus checks the status fail gives an error of each kind, with an
// error from each package that wraps one, and that an error of no kind is a
// 500 whose message is not shown.
func TestFailStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"errkind invalid", fmt.Errorf("%w: x", errkind.ErrInvalid), http.StatusBadRequest},
		{"errkind not found", fmt.Errorf("%w: x", errkind.ErrNotFound), http.StatusNotFound},
		{"errkind conflict", fmt.Errorf("%w: x", errkind.ErrConflict), http.StatusConflict},
		{"errkind too big", fmt.Errorf("%w: x", errkind.ErrTooBig), http.StatusRequestEntityTooLarge},
		{"a new error of a kind", fmt.Errorf("op: %w", errkind.New(errkind.ErrConflict, "new")), http.StatusConflict},
		{"request body", &http.MaxBytesError{Limit: 5}, http.StatusRequestEntityTooLarge},
		{"bad body", fmt.Errorf("%w: x", errBadBody), http.StatusBadRequest},
		{"no render", fmt.Errorf("%w: 1", errNoRender), http.StatusNotFound},
		{"apitoken name", apitoken.ErrName, http.StatusBadRequest},
		{"apitoken exists", fmt.Errorf("%w: x", apitoken.ErrExists), http.StatusConflict},
		{"apitoken missing", fmt.Errorf("%w: x", apitoken.ErrNotFound), http.StatusNotFound},
		{"device", fmt.Errorf("%w: x", device.ErrNoDevice), http.StatusNotFound},
		{"firmware image", fmt.Errorf("%w: x", firmware.ErrImage), http.StatusBadRequest},
		{"firmware too big", fmt.Errorf("%w: x", firmware.ErrTooBig), http.StatusRequestEntityTooLarge},
		{"firmware missing", fmt.Errorf("%w: x", firmware.ErrNoFirmware), http.StatusNotFound},
		{"firmware exists", fmt.Errorf("%w: x", firmware.ErrExists), http.StatusConflict},
		{"firmware version", func() error { _, err := firmware.ParseVersion("1.x"); return err }(), http.StatusBadRequest},
		{"playlist item", fmt.Errorf("%w: 1", playlist.ErrNoItem), http.StatusNotFound},
		{"recipe missing", fmt.Errorf("%w: 1", recipe.ErrNoRecipe), http.StatusNotFound},
		{"instance missing", fmt.Errorf("%w: 1", recipe.ErrNoInstance), http.StatusNotFound},
		{"instance in use", fmt.Errorf("%w: 1", recipe.ErrInstanceInUse), http.StatusConflict},
		{"instance data", fmt.Errorf("%w of 5 bytes", recipe.ErrDataTooBig), http.StatusRequestEntityTooLarge},
		{"no secrets key", secrets.ErrNoKey, http.StatusBadRequest},
		{"no kind", errors.New("disk on fire"), http.StatusInternalServerError},
	} {
		rec := httptest.NewRecorder()
		fail(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/x", http.NoBody), tc.err)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
		if tc.want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "disk") {
			t.Errorf("%s: body %q shows the error", tc.name, rec.Body.String())
		}
	}
}

// TestInstanceSecretsUnreadable checks that an instance whose sealed secrets
// do not open, sealed under another key or without the instance ID binding,
// shows secret_fields null in the list and in a single GET, and does not
// fail the list for the other instances.
func TestInstanceSecretsUnreadable(t *testing.T) {
	t.Parallel()
	other, err := secrets.Parse(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  *secrets.Key
		ad   []byte
	}{
		{"another key", other, nil},
		{"no binding", nil, []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ta := newTestAPI(t, true)
			rec := ta.addRecipe(t)
			good := ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "name": "good", "fields": {"k": "`+testSecret+`"}}`, 201, nil)
			if !strings.Contains(good.Body.String(), `"secret_fields":["k"]`) {
				t.Fatalf("good instance: %s", good.Body)
			}
			inst, err := ta.db.Write.CreateInstance(t.Context(), db.CreateInstanceParams{
				RecipeID: rec.ID, Name: tc.name, FieldValues: `{"city":"Oslo"}`, SecretKeys: "[]", Palette: "bw", CreatedAt: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			key := tc.key
			ad := []byte("plugin_instance:" + strconv.FormatInt(inst.ID, 10))
			if key == nil {
				// The server's own key, but sealed with no associated data.
				if key, err = secrets.Parse(base64.StdEncoding.EncodeToString(make([]byte, 32))); err != nil {
					t.Fatal(err)
				}
				ad = nil
			}
			sealed, err := key.Seal([]byte(`{"k":"`+testSecret+`"}`), ad)
			if err != nil {
				t.Fatal(err)
			}
			if err := ta.db.Write.SetInstanceSecrets(t.Context(), db.SetInstanceSecretsParams{SecretValues: sealed, ID: inst.ID}); err != nil {
				t.Fatal(err)
			}
			var list []instanceJSON
			w := ta.call(t, "GET", "/api/v1/instances", "", 200, &list)
			var one instanceJSON
			w1 := ta.call(t, "GET", "/api/v1/instances/"+strconv.FormatInt(inst.ID, 10), "", 200, &one)
			if len(list) < 2 || list[0].SecretFields == nil || one.SecretFields != nil || one.Fields["city"].One != "Oslo" {
				t.Errorf("list %+v, get %+v", list, one)
			}
			for _, in := range list {
				if in.ID == inst.ID && in.SecretFields != nil {
					t.Errorf("list shows %v for the unreadable instance", in.SecretFields)
				}
			}
			if !strings.Contains(w1.Body.String(), `"secret_fields":null`) || strings.Contains(w.Body.String()+w1.Body.String(), testSecret) {
				t.Errorf("body %s", w1.Body)
			}
		})
	}
}

// TestDecode checks the cases of decode that no route test reaches on its
// own: an empty body with and without emptyOK, trailing garbage, a second
// value, trailing white space, and a trailing byte over the size limit,
// which must still come back as a MaxBytesError and not a bad body.
func TestDecode(t *testing.T) {
	t.Parallel()
	type body struct {
		N int `json:"n"`
	}
	for _, tc := range []struct {
		name    string
		in      string
		emptyOK bool
		wantN   int
		wantErr string // "" for none, "bad" for errBadBody, "big" for MaxBytesError.
	}{
		{"value", `{"n": 3}`, false, 3, ""},
		{"trailing white space", "{\"n\": 3}\n \t\r\n", false, 3, ""},
		{"empty", ``, false, 0, "bad"},
		{"empty ok", ``, true, 0, ""},
		{"white space only ok", " \n", true, 0, ""},
		{"empty ok but garbage", `x`, true, 0, "bad"},
		{"second value", `{"n": 3} {"n": 4}`, false, 3, "bad"},
		{"trailing garbage", `{"n": 3} x`, false, 3, "bad"},
		{"trailing closer", `{"n": 3}}`, false, 3, "bad"},
		{"trailing array closer", `{"n": 3}]`, false, 3, "bad"},
		{"unknown field", `{"m": 3}`, false, 0, "bad"},
		{"too big", `{"n": 3}` + strings.Repeat(" ", maxJSONBody), false, 3, "big"},
		{"too big inside", `{"n": "` + strings.Repeat("x", maxJSONBody) + `"}`, false, 0, "big"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/", strings.NewReader(tc.in))
			var v body
			err := decode(httptest.NewRecorder(), r, &v, tc.emptyOK)
			var tooBig *http.MaxBytesError
			switch {
			case tc.wantErr == "" && err != nil,
				tc.wantErr == "bad" && !errors.Is(err, errBadBody),
				tc.wantErr == "big" && (!errors.As(err, &tooBig) || errors.Is(err, errBadBody)):
				t.Fatalf("decode(%.40q) = %v, want %q", tc.in, err, tc.wantErr)
			}
			if tc.wantErr == "" && v.N != tc.wantN {
				t.Errorf("n = %d, want %d", v.N, tc.wantN)
			}
		})
	}
}

// TestBuildPlaylists checks that device and item rows are joined to their
// playlist by ID, and that a row for a playlist that is not in the list is
// skipped, as a read that is not one snapshot could give.
func TestBuildPlaylists(t *testing.T) {
	t.Parallel()
	lists := []db.Playlist{{ID: 1, Name: "same"}, {ID: 2, Name: "other"}}
	devs := []db.ListPlaylistDevicesRow{
		{Playlist: "other", PlaylistID: 1, FriendlyID: "ABC123"}, // The name is stale. The ID decides.
		{Playlist: "ghost", PlaylistID: 9, FriendlyID: "ABC124"},
	}
	items := []db.ListPlaylistItemsRow{
		{Playlist: "same", PlaylistID: 2, ID: 7},
		{Playlist: "ghost", PlaylistID: 9, ID: 8},
	}
	got := buildPlaylists(lists, devs, items)
	if len(got) != 2 || len(got[0].Devices) != 1 || got[0].Devices[0].Device != "ABC123" || len(got[0].Items) != 0 {
		t.Errorf("first playlist %+v", got[0])
	}
	if len(got[1].Devices) != 0 || len(got[1].Items) != 1 || got[1].Items[0].ID != 7 {
		t.Errorf("second playlist %+v", got[1])
	}
}

// TestVanishedRowIsNotFound checks that a device or an instance that is gone
// by the time a response reads it, as after a delete that lands just after
// the caller's own write, is not found (404) rather than a server error.
func TestVanishedRowIsNotFound(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, true)
	ctx := t.Context()
	if _, err := ta.srv.deviceStatus(ctx, ta.db.Read, 999); !errors.Is(err, device.ErrNoDevice) || !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("deviceStatus of a missing device: %v", err)
	}
	gone := db.PluginInstance{ID: 7, RecipeID: 999}
	if _, err := ta.srv.instanceWithRecipe(ctx, ta.db.Read, &gone); !errors.Is(err, recipe.ErrNoInstance) || !errors.Is(err, errkind.ErrNotFound) {
		t.Errorf("instanceWithRecipe of an instance whose recipe is gone: %v", err)
	}
}

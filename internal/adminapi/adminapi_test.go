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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/apitoken"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
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
	ta := &testAPI{db: client, mux: http.NewServeMux(), now: time.Unix(1_000_000, 0), recipes: filepath.Join(dir, "recipes")}
	ta.srv = New(client, Config{Key: key, RecipesDir: ta.recipes})
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

const settingsYML = "name: Weather\nrefresh_interval: 60\n"

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
	var one recipeJSON
	ta.call(t, "GET", "/api/v1/recipes/"+strconv.FormatInt(rec.ID, 10), "", 200, &one)
	if one != rec {
		t.Errorf("get %+v", one)
	}
	ta.wantError(t, "GET", "/api/v1/recipes/99", "", 404, "no such recipe")
	ta.wantError(t, "GET", "/api/v1/recipes/abc", "", 404, "no such recipe")

	good := zipBytes(t, map[string]string{"settings.yml": settingsYML, "full.liquid": "hi"})
	for _, tc := range []struct {
		name, contentType string
		body              []byte
		status            int
	}{
		{"not a zip", "application/zip", []byte("plain text"), 400},
		{"escaping path", "application/zip", zipBytes(t, map[string]string{"settings.yml": settingsYML, "full.liquid": "x", "../escape": "x"}), 400},
		{"no full.liquid", "application/zip", zipBytes(t, map[string]string{"settings.yml": settingsYML}), 400},
		{"over the limit", "application/zip", bytes.Repeat([]byte{'x'}, recipe.MaxRecipeBytes+1), 413},
		{"wrong content type", "application/json", good, 415},
		{"no content type", "", good, 415},
		{"zip with parameters", "application/zip; charset=binary", good, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := ta.db.Read.ListRecipes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			w := ta.do(t, "POST", "/api/v1/recipes", bytes.NewReader(tc.body), "Content-Type", tc.contentType)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
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
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ta.recipes), "escape")); err == nil {
		t.Error("the zip escaped the recipes directory")
	}
}

func TestInstances(t *testing.T) {
	ta := newTestAPI(t, true)
	rec := ta.addRecipe(t)
	body := `{"recipe_id": ` + strconv.FormatInt(rec.ID, 10) + `, "name": "Home", "palette": "bw", "fields": {"city": "Oslo"}, "secrets": {"zkey": "` + testSecret + `", "apikey": "` + testSecret + `"}}`
	var created instanceJSON
	w := ta.call(t, "POST", "/api/v1/instances", body, 201, &created)
	if created.Name != "Home" || created.RecipeName != "Weather" || created.Palette != "bw" ||
		created.Fields["city"] != "Oslo" || len(created.Fields) != 1 || strings.Join(created.SecretFields, ",") != "apikey,zkey" {
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
		{"unknown palette", `{"recipe_id": 1, "palette": "neon", "secrets": {"k": "` + testSecret + `"}}`, "unknown palette"},
		{"field and secret", `{"recipe_id": 1, "fields": {"k": "a"}, "secrets": {"k": "` + testSecret + `"}}`, "both a field and a secret"},
		{"unknown recipe", `{"recipe_id": 99, "secrets": {"k": "` + testSecret + `"}}`, "no such recipe"},
		{"unknown field", `{"recipe_id": 1, "secret": {"k": "` + testSecret + `"}}`, "unknown field"},
		{"bad json", `{"recipe_id": 1, "secrets": {"k": "` + testSecret, "bad request body"},
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
	var item addItemResponse
	ta.call(t, "POST", "/api/v1/playlists/items", `{"device": "`+testFriend+`", "instance_id": `+id+`}`, 201, &item)
	ta.wantError(t, "DELETE", "/api/v1/instances/"+id, "", 409, "item IDs "+strconv.FormatInt(item.ID, 10))
	ta.call(t, "DELETE", "/api/v1/playlists/items/"+strconv.FormatInt(item.ID, 10), "", 204, nil)
	ta.call(t, "DELETE", "/api/v1/instances/"+id, "", 204, nil)
	ta.wantError(t, "GET", "/api/v1/instances/"+id, "", 404, "no such instance")
}

// TestInstanceSecretsNeedKey checks that a server with no secrets key
// refuses a new secret value, and that it still lists an instance whose
// secrets were sealed earlier, with secret_fields null.
func TestInstanceSecretsNeedKey(t *testing.T) {
	ta := newTestAPI(t, false)
	rec := ta.addRecipe(t)
	recID := strconv.FormatInt(rec.ID, 10)
	ta.wantError(t, "POST", "/api/v1/instances", `{"recipe_id": `+recID+`, "secrets": {"k": "`+testSecret+`"}}`, 400, "TRMNL_SECRET_KEY")
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
	sealed, err := recipe.CreateInstance(t.Context(), ta.db, key, recipe.InstanceParams{
		RecipeID: rec.ID, Name: "sealed", Fields: map[string]string{"city": "Oslo"}, Secrets: map[string]string{"k": testSecret},
	}, ta.now)
	if err != nil {
		t.Fatal(err)
	}
	plain := ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+recID+`, "name": "plain"}`, 201, nil)
	if !strings.Contains(plain.Body.String(), `"secret_fields":[]`) {
		t.Errorf("an instance with no secrets: %s", plain.Body)
	}
	w := ta.call(t, "GET", "/api/v1/instances", "", 200, &list)
	if len(list) != 2 || list[0].Name != "sealed" || list[0].Fields["city"] != "Oslo" || list[0].SecretFields != nil ||
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
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	rec := ta.addRecipe(t)
	var inst instanceJSON
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "name": "Home"}`, 201, &inst)
	instID := strconv.FormatInt(inst.ID, 10)

	var a, b addItemResponse
	ta.call(t, "POST", "/api/v1/playlists/items", `{"device": "`+testMAC+`", "instance_id": `+instID+`, "duration_seconds": 600}`, 201, &a)
	ta.call(t, "POST", "/api/v1/playlists/items", `{"playlist": "kitchen", "instance_id": `+instID+`, "duration_seconds": 0}`, 201, &b)
	if a.Playlist != testFriend || a.Position != 0 || b.Playlist != "kitchen" {
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
		{"unknown instance", `{"playlist": "p", "instance_id": 99}`, 404, "no such instance"},
		{"unknown device", `{"device": "ZZZ999", "instance_id": ` + instID + `}`, 404, "no such device"},
		{"no playlist or device", `{"instance_id": ` + instID + `}`, 400, "playlist name or a device"},
		{"too short", `{"playlist": "p", "instance_id": ` + instID + `, "duration_seconds": 10}`, 400, "under"},
		{"negative", `{"playlist": "p", "instance_id": ` + instID + `, "duration_seconds": -1}`, 400, "seconds"},
		{"unknown field", `{"playlist": "p", "instance": ` + instID + `}`, 400, "unknown field"},
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

func TestRenders(t *testing.T) {
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
	ta := newTestAPI(t, true)
	rec := ta.addRecipe(t)
	ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "secrets": {"k": "hunter2"}}`, 201, nil)
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
		{"playlist item", fmt.Errorf("%w: 1", playlist.ErrNoItem), http.StatusNotFound},
		{"recipe missing", fmt.Errorf("%w: 1", recipe.ErrNoRecipe), http.StatusNotFound},
		{"instance missing", fmt.Errorf("%w: 1", recipe.ErrNoInstance), http.StatusNotFound},
		{"instance in use", fmt.Errorf("%w: 1", recipe.ErrInstanceInUse), http.StatusConflict},
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
	ta := newTestAPI(t, true)
	rec := ta.addRecipe(t)
	other, err := secrets.Parse(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	good := ta.call(t, "POST", "/api/v1/instances", `{"recipe_id": `+strconv.FormatInt(rec.ID, 10)+`, "name": "good", "secrets": {"k": "`+testSecret+`"}}`, 201, nil)
	if !strings.Contains(good.Body.String(), `"secret_fields":["k"]`) {
		t.Fatalf("good instance: %s", good.Body)
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
			inst, err := ta.db.Write.CreateInstance(t.Context(), db.CreateInstanceParams{
				RecipeID: rec.ID, Name: tc.name, FieldValues: `{"city":"Oslo"}`, Palette: "bw", CreatedAt: 1,
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
			if len(list) < 2 || list[0].SecretFields == nil || one.SecretFields != nil || one.Fields["city"] != "Oslo" {
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

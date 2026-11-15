package recipe

import (
	"bytes"
	"cmp"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// weatherYML is a recipe with one field of each kind the instance tests
// need: a required string, a secret, a select with a default, a
// multi-select, and a bounded number.
const weatherYML = `name: Weather
refresh_interval: 15
custom_fields:
- keyname: city
  field_type: string
  name: City
  maxlength: 20
- keyname: token
  field_type: password
  name: Token
  optional: true
- keyname: other
  field_type: password
  name: Other
  optional: true
- keyname: notes
  field_type: code
  optional: true
- keyname: units
  field_type: select
  name: Units
  options: [Metric, Imperial]
  default: metric
- keyname: tags
  field_type: select
  name: Tags
  multiple: true
  optional: true
  options: [a, b, c]
- keyname: days
  field_type: number
  name: Days
  optional: true
  min: 1
  max: 10
- keyname: about
  field_type: author_bio
  name: About
`

// fixture is a database with the recipe of weatherYML installed.
type fixture struct {
	client     *db.Client
	recipesDir string
	recipe     db.Recipe
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	client := openDB(t)
	src := t.TempDir()
	writeDir(t, src, map[string]string{"settings.yml": weatherYML, "full.liquid": "x"})
	recipes := filepath.Join(t.TempDir(), "recipes")
	rec, err := Install(t.Context(), client, recipes, src, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	return fixture{client: client, recipesDir: recipes, recipe: rec}
}

func testKey(t *testing.T, b64 string) *secrets.Key {
	t.Helper()
	key, err := secrets.Parse(b64)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

const (
	keyA = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	keyB = "HR4fICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw="
)

func TestInstanceFields(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	key := testKey(t, keyA)
	p := InstanceParams{RecipeID: f.recipe.ID, Name: "i", Values: map[string]Value{
		"city": {One: "Zurich"}, "token": {One: "hunter2"}, "tags": {Many: []string{"a", "c"}}, "days": {},
	}}
	if _, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, p, time.Now()); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("secret stored without a key: %v", err)
	}
	inst, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(inst.SecretValues, []byte("hunter2")) || strings.Contains(inst.FieldValues, "hunter2") {
		t.Error("secret stored in the clear")
	}
	if inst.FieldValues != `{"city":"Zurich","tags":["a","c"]}` {
		t.Errorf("stored fields = %s, want the non-secret ones, with the empty one left out", inst.FieldValues)
	}
	want := map[string]Value{"city": {One: "Zurich"}, "token": {One: "hunter2"}, "tags": {Many: []string{"a", "c"}}}
	got, secret, err := InstanceFields(&inst, key)
	if err != nil || !maps.EqualFunc(got, want, Value.equal) || !slices.Equal(secret, []string{"hunter2"}) {
		t.Errorf("InstanceFields = %v, %v, %v", got, secret, err)
	}
	public, names, err := PublicFields(&inst, key)
	if err != nil || len(public) != 2 || !slices.Equal(names, []string{"token"}) {
		t.Errorf("PublicFields = %v, %v, %v", public, names, err)
	}
	if _, _, err := InstanceFields(&inst, nil); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("secrets opened without a key: %v", err)
	}
	other, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Bern"}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other.SecretValues = inst.SecretValues
	if _, _, err := InstanceFields(&other, key); err == nil {
		t.Error("secrets copied to another instance opened")
	}
}

// TestCreateInstanceInvalid checks that bad settings are refused, with every
// bad field in one FieldErrors, that an unknown recipe is ErrNoRecipe, and
// that nothing is stored for any of them.
func TestCreateInstanceInvalid(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	key := testKey(t, keyA)
	for _, tc := range []struct {
		name       string
		p          InstanceParams
		key        *secrets.Key
		want       error
		wantFields []string
	}{
		{"unknown palette", InstanceParams{RecipeID: f.recipe.ID, Palette: "neon", Values: map[string]Value{"city": {One: "x"}}}, key, errkind.ErrInvalid, nil},
		{"unknown recipe", InstanceParams{RecipeID: f.recipe.ID + 1}, key, ErrNoRecipe, nil},
		{"required field missing", InstanceParams{RecipeID: f.recipe.ID}, key, errkind.ErrInvalid, []string{"city"}},
		{"required field empty", InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {}}}, key, errkind.ErrInvalid, []string{"city"}},
		{"several bad fields", InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{
			"city": {One: strings.Repeat("x", 21)}, "days": {One: "11"}, "tags": {Many: []string{"z"}}, "nope": {One: "x"}, "about": {One: "x"},
			"token": {One: "abcde"},
		}}, key, errkind.ErrInvalid, []string{"about", "city", "days", "nope", "tags", "token"}},
		{"secret without a key", InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "x"}, "token": {One: "hunter2"}}}, nil, secrets.ErrNoKey, nil},
	} {
		_, err := CreateInstance(ctx, f.client, tc.key, f.recipesDir, tc.p, time.Now())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		var fe FieldErrors
		if got := errors.As(err, &fe); got != (tc.wantFields != nil) || (got && !slices.Equal(slices.Sorted(maps.Keys(fe)), tc.wantFields)) {
			t.Errorf("%s: field errors = %v, want fields %v", tc.name, fe, tc.wantFields)
		}
		for _, v := range tc.p.Values {
			if err != nil && v.One != "" && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("%s: the error shows a secret: %v", tc.name, err)
			}
		}
	}
	if left, err := f.client.Read.ListInstances(ctx); err != nil || len(left) != 0 {
		t.Errorf("refused instances were stored: %+v, %v", left, err)
	}
	inst, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Palette: "bw", Values: map[string]Value{
		"city": {One: "Zurich"}, "tags": {One: "b"},
	}}, time.Now())
	if err != nil || inst.Name != "Weather" || inst.FieldValues != `{"city":"Zurich","tags":["b"]}` {
		t.Fatalf("default name and a single choice for a multi-select: %+v, %v", inst, err)
	}
}

// TestUpdateInstance checks each way an update merges, validates and bumps
// settings_at, from one instance made with a city, a secret token, and two
// tags, whose settings_at is 0.
func TestUpdateInstance(t *testing.T) {
	t.Parallel()
	const settingsAt = 5000
	now := time.Unix(settingsAt, 0)
	initial := map[string]Value{"city": {One: "Zurich"}, "tags": {Many: []string{"a", "b"}}}
	for _, tc := range []struct {
		desc       string
		update     InstanceUpdate
		key        string // "a" for the key the instance was made with, "b" for another, "" for none
		wantErr    error
		wantFields []string // keynames of the field errors
		// What the instance holds after: the plain fields, the secrets, the
		// name, and the palette. A refused update leaves all as they were.
		fields     map[string]Value
		secret     map[string]string
		name       string
		palette    string
		settingsAt int64 // 0 when it is not raised
	}{
		{
			desc: "change a field", update: InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}}}, key: "a",
			fields: map[string]Value{"city": {One: "Bern"}, "tags": {Many: []string{"a", "b"}}}, secret: map[string]string{"token": "hunter2"}, settingsAt: settingsAt,
		},
		{desc: "same values", update: InstanceUpdate{Values: map[string]Value{"city": {One: "Zurich"}, "tags": {Many: []string{"a", "b"}}, "token": {One: "hunter2"}}}, key: "a"},
		{
			desc: "same list in another order", update: InstanceUpdate{Values: map[string]Value{"tags": {Many: []string{"b", "a"}}}}, key: "a",
			fields: map[string]Value{"city": {One: "Zurich"}, "tags": {Many: []string{"b", "a"}}}, secret: map[string]string{"token": "hunter2"}, settingsAt: settingsAt,
		},
		{desc: "rename and palette are not settings", update: InstanceUpdate{Name: ptr("Home"), Palette: ptr("bw")}, key: "a", name: "Home", palette: "bw"},
		{desc: "empty name is the recipe's", update: InstanceUpdate{Name: ptr("")}, key: "a", name: "Weather"},
		{
			desc: "a field left out is kept", update: InstanceUpdate{Values: map[string]Value{"days": {One: "3"}}}, key: "a",
			fields: map[string]Value{"city": {One: "Zurich"}, "tags": {Many: []string{"a", "b"}}, "days": {One: "3"}}, secret: map[string]string{"token": "hunter2"}, settingsAt: settingsAt,
		},
		{
			desc: "an empty list clears", update: InstanceUpdate{Values: map[string]Value{"tags": {Many: []string{}}}}, key: "a",
			fields: map[string]Value{"city": {One: "Zurich"}}, secret: map[string]string{"token": "hunter2"}, settingsAt: settingsAt,
		},
		{desc: "clearing what is not set changes nothing", update: InstanceUpdate{Values: map[string]Value{"days": {}}}, key: "a"},
		{desc: "a required field cannot be cleared", update: InstanceUpdate{Values: map[string]Value{"city": {}}}, key: "a", wantErr: errkind.ErrInvalid, wantFields: []string{"city"}},
		{desc: "a field with a default can be cleared", update: InstanceUpdate{Values: map[string]Value{"units": {}}}, key: "a"},
		{
			desc: "bad fields at once, and no change", update: InstanceUpdate{Name: ptr("x"), Values: map[string]Value{"city": {One: "ok"}, "days": {One: "0"}, "units": {One: "kelvin"}}}, key: "a",
			wantErr: errkind.ErrInvalid, wantFields: []string{"days", "units"},
		},
		{desc: "unknown palette, and no change", update: InstanceUpdate{Name: ptr("x"), Palette: ptr("neon")}, key: "a", wantErr: errkind.ErrInvalid},
		{
			desc: "a secret changes", update: InstanceUpdate{Values: map[string]Value{"token": {One: "swordfish"}}}, key: "a",
			fields: initial, secret: map[string]string{"token": "swordfish"}, settingsAt: settingsAt,
		},
		{desc: "a secret clears with Clear", update: InstanceUpdate{Clear: []string{"token"}}, key: "a", fields: initial, secret: map[string]string{}, settingsAt: settingsAt},
		{desc: "an empty password keeps the secret", update: InstanceUpdate{Values: map[string]Value{"token": {}, "other": {One: "   "}}}, key: "a"},
		{desc: "an empty password with no key is ignored", update: InstanceUpdate{Values: map[string]Value{"token": {}}}, key: ""},
		{
			desc: "a plain field clears with Clear", update: InstanceUpdate{Clear: []string{"tags"}}, key: "a",
			fields: map[string]Value{"city": {One: "Zurich"}}, secret: map[string]string{"token": "hunter2"}, settingsAt: settingsAt,
		},
		{
			desc: "a key in both Values and Clear", update: InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}, "days": {One: "0"}}, Clear: []string{"city", "tags"}}, key: "a",
			wantErr: errkind.ErrInvalid, wantFields: []string{"city", "days"},
		},
		{desc: "Clear of a required field", update: InstanceUpdate{Clear: []string{"city"}}, key: "a", wantErr: errkind.ErrInvalid, wantFields: []string{"city"}},
		{desc: "Clear of a field neither declared nor stored", update: InstanceUpdate{Clear: []string{"nope"}}, key: "a", wantErr: errkind.ErrInvalid, wantFields: []string{"nope"}},
		{desc: "a rename with no key opens nothing", update: InstanceUpdate{Name: ptr("Home"), Palette: ptr("bw")}, key: "", name: "Home", palette: "bw"},
		{desc: "a rename under another key opens nothing", update: InstanceUpdate{Name: ptr("Home")}, key: "b", name: "Home"},
		{desc: "a plain change must open the sealed secrets, and has no key", update: InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}}}, key: "", wantErr: secrets.ErrNoKey},
		{desc: "a plain clear has no key", update: InstanceUpdate{Clear: []string{"tags"}}, key: "", wantErr: secrets.ErrNoKey},
		{desc: "a plain change under another key", update: InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}}}, key: "b", wantErr: errkind.ErrInvalid},
		{desc: "a secret changed with no key", update: InstanceUpdate{Values: map[string]Value{"token": {One: "swordfish"}}}, key: "", wantErr: secrets.ErrNoKey},
		{desc: "a secret changed under another key", update: InstanceUpdate{Values: map[string]Value{"token": {One: "swordfish"}}}, key: "b", wantErr: errkind.ErrInvalid},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(t)
			keys := map[string]*secrets.Key{"a": testKey(t, keyA), "b": testKey(t, keyB), "": nil}
			values := maps.Clone(initial)
			values["token"] = Value{One: "hunter2"}
			before, err := CreateInstance(ctx, f.client, keys["a"], f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: values}, now)
			if err != nil {
				t.Fatal(err)
			}
			got, err := UpdateInstance(ctx, f.client, keys[tc.key], f.recipesDir, before.ID, tc.update, now)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			var fe FieldErrors
			if errors.As(err, &fe) != (tc.wantFields != nil) || !slices.Equal(slices.Sorted(maps.Keys(fe)), tc.wantFields) {
				t.Errorf("field errors = %v, want fields %v", fe, tc.wantFields)
			}
			stored, err := f.client.Read.GetInstance(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != nil {
				if !reflect.DeepEqual(stored, before) {
					t.Errorf("a refused update changed the instance:\nbefore %+v\nafter  %+v", before, stored)
				}
				return
			}
			if !reflect.DeepEqual(stored, got) {
				t.Errorf("the update returned %+v, but %+v is stored", got, stored)
			}
			wantFields, wantSecret := tc.fields, tc.secret
			if wantFields == nil {
				wantFields, wantSecret = initial, map[string]string{"token": "hunter2"}
			}
			plain, secret, err := decodeFields(&stored, keys["a"], true)
			if err != nil || !maps.EqualFunc(plain, wantFields, Value.equal) || !maps.Equal(secret, wantSecret) {
				t.Errorf("fields %v, secrets %v, %v; want %v, %v", plain, secret, err, wantFields, wantSecret)
			}
			if len(wantSecret) == 0 && stored.SecretValues != nil {
				t.Error("no secrets left, but a sealed value is stored")
			}
			if tc.settingsAt == 0 && !bytes.Equal(stored.SecretValues, before.SecretValues) {
				t.Error("secrets were sealed again though no secret changed")
			}
			wantName, wantPalette := cmp.Or(tc.name, before.Name), cmp.Or(tc.palette, before.Palette)
			if stored.Name != wantName || stored.Palette != wantPalette || stored.SettingsAt != tc.settingsAt {
				t.Errorf("name %q, palette %q, settings_at %d; want %q, %q, %d", stored.Name, stored.Palette, stored.SettingsAt, wantName, wantPalette, tc.settingsAt)
			}
		})
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// TestDeleteInstance checks that an instance that items show is kept, and
// that the error counts the items without naming them, and that deleting an
// instance deletes its renders.
func TestDeleteInstance(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	client := f.client
	rec := f.recipe
	ids := make([]int64, 0, 2)
	for _, name := range []string{"a", "b"} {
		inst, err := CreateInstance(ctx, client, nil, f.recipesDir, InstanceParams{RecipeID: rec.ID, Name: name, Values: map[string]Value{"city": {One: "x"}}}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, inst.ID)
	}
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: "D8:3B:DA:12:34:56", ApiKeyHash: []byte{1}, FriendlyID: "ABC123", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	pl, err := client.Write.CreatePlaylist(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := playlist.LinkDevice(ctx, client, playlist.ByID(pl.ID), dev.FriendlyID); err != nil {
		t.Fatal(err)
	}
	items := make([]int64, 0, len(ids))
	for _, inst := range ids {
		item, err := client.Write.CreatePlaylistItem(ctx, db.CreatePlaylistItemParams{PlaylistID: pl.ID, Position: int64(len(items)), Layout: "full"})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Write.CreatePlaylistSlot(ctx, db.CreatePlaylistSlotParams{ItemID: item.ID, Slot: 0, InstanceID: inst}); err != nil {
			t.Fatal(err)
		}
		items = append(items, item.ID)
	}
	renderID, err := client.Write.SaveRenderSuccess(ctx, db.SaveRenderSuccessParams{InstanceID: ids[0], Width: 800, Height: 480, Palette: "bw", RenderedAt: 1, AttemptedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Write.SaveRenderImage(ctx, db.SaveRenderImageParams{RenderID: renderID, Layout: "full", Image: "img", Bytes: 1}); err != nil {
		t.Fatal(err)
	}

	err = DeleteInstance(ctx, client, ids[0])
	if !errors.Is(err, ErrInstanceInUse) || !errors.Is(err, errkind.ErrConflict) || err.Error() != "instance is in 1 playlist items" {
		t.Fatalf("instance in a playlist item: %v", err)
	}
	if refs, err := client.Read.ListReferencedImages(ctx); err != nil || len(refs) != 1 {
		t.Fatalf("a refused delete changed renders: %v, %v", refs, err)
	}
	if err := DeleteInstance(ctx, client, 999); !errors.Is(err, ErrNoInstance) {
		t.Errorf("unknown instance: %v", err)
	}

	// With its item removed it goes, and its renders with it.
	if _, err := client.Write.DeletePlaylistItem(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	if err := DeleteInstance(ctx, client, ids[0]); err != nil {
		t.Fatal(err)
	}
	if refs, err := client.Read.ListReferencedImages(ctx); err != nil || len(refs) != 0 {
		t.Errorf("renders left behind: %v, %v", refs, err)
	}
	left, err := client.Read.ListInstances(ctx)
	if err != nil || len(left) != 1 || left[0].PluginInstance.ID != ids[1] {
		t.Errorf("instances left: %+v, %v", left, err)
	}
	if err := DeleteInstance(ctx, client, ids[1]); !errors.Is(err, ErrInstanceInUse) {
		t.Errorf("the other instance's item was not protected: %v", err)
	}
}

// TestUpdateInstanceSettingsAt checks that settings_at rises with every
// change, by at least one when changes come in the same second or the clock
// goes back, so a build that read the earlier settings is never taken for
// current.
func TestUpdateInstanceSettingsAt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	inst, err := CreateInstance(ctx, f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "a"}}}, time.Unix(100, 0))
	if err != nil || inst.SettingsAt != 0 {
		t.Fatalf("a new instance: %+v, %v", inst, err)
	}
	for _, tc := range []struct {
		city string
		at   int64
		want int64
	}{
		{"b", 100, 100},
		{"c", 100, 101},
		{"d", 100, 102},
		{"d", 500, 102},
		{"e", 500, 500},
		{"f", 400, 501},
	} {
		got, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Values: map[string]Value{"city": {One: tc.city}}}, time.Unix(tc.at, 0))
		if err != nil || got.SettingsAt != tc.want {
			t.Errorf("city %q at %d: settings_at %d, %v; want %d", tc.city, tc.at, got.SettingsAt, err, tc.want)
		}
	}
	if _, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, inst.ID+1, InstanceUpdate{}, time.Now()); !errors.Is(err, ErrNoInstance) {
		t.Errorf("unknown instance: %v", err)
	}
}

// TestUpdateTwoSecrets checks that clearing or changing one secret keeps the
// other, by value, and that an update that touches no secret leaves the sealed
// bytes as they were though there is no key to open them.
func TestUpdateTwoSecrets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	key := testKey(t, keyA)
	now := time.Unix(5000, 0)
	inst, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{
		"city": {One: "Zurich"}, "token": {One: "hunter2"}, "other": {One: "swordfish"},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	// A rename needs no key, and leaves the sealed bytes as they were.
	got, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Name: ptr("Home")}, now)
	if err != nil || !bytes.Equal(got.SecretValues, inst.SecretValues) || got.Name != "Home" {
		t.Fatalf("a rename with no key: %v; sealed bytes changed: %v", err, !bytes.Equal(got.SecretValues, inst.SecretValues))
	}
	// A plain field cannot be changed with no key: the secrets must be opened.
	if _, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}}}, now); !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("a plain update with no key: %v", err)
	}
	// With the key, an update that changes no secret seals nothing again.
	same, err := UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}, "token": {One: "hunter2"}}}, now)
	if err != nil || !bytes.Equal(same.SecretValues, inst.SecretValues) {
		t.Fatalf("an update that changes no secret: %v; sealed bytes changed: %v", err, !bytes.Equal(same.SecretValues, inst.SecretValues))
	}
	// An empty password is kept, not cleared.
	if got, err = UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Values: map[string]Value{"token": {}}}, now); err != nil || !bytes.Equal(got.SecretValues, inst.SecretValues) {
		t.Fatalf("an empty password: %v", err)
	}
	for _, tc := range []struct {
		name   string
		update InstanceUpdate
		want   map[string]Value
	}{
		{"an empty value keeps both", InstanceUpdate{Values: map[string]Value{"token": {}, "other": {}}}, map[string]Value{"city": {One: "Bern"}, "token": {One: "hunter2"}, "other": {One: "swordfish"}}},
		{"clear one", InstanceUpdate{Clear: []string{"token"}}, map[string]Value{"city": {One: "Bern"}, "other": {One: "swordfish"}}},
		{"change the other", InstanceUpdate{Values: map[string]Value{"other": {One: "trout-fish"}}}, map[string]Value{"city": {One: "Bern"}, "other": {One: "trout-fish"}}},
		{"set one back", InstanceUpdate{Values: map[string]Value{"token": {One: "hunter3"}}}, map[string]Value{"city": {One: "Bern"}, "other": {One: "trout-fish"}, "token": {One: "hunter3"}}},
	} {
		got, err = UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, tc.update, now)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		fields, _, err := InstanceFields(&got, key)
		if err != nil || !maps.EqualFunc(fields, tc.want, Value.equal) {
			t.Errorf("%s: fields %v, %v; want %v", tc.name, fields, err, tc.want)
		}
	}
}

// TestInstanceValuesTooBig checks that values that cannot fit in a render
// job are refused at create and update, as a field error on the largest
// field, whether they are one field, several, or a secret, and that sealed
// secrets that are not opened still count.
func TestInstanceValuesTooBig(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	key := testKey(t, keyA)
	now := time.Unix(5000, 0)
	// 20,000 of these are 20 KB, as sent, but 120 KB if < were escaped.
	html := strings.Repeat("<", 20000)
	if _, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Zurich"}, "notes": {One: html}}}, now); err != nil {
		t.Fatalf("20 KB of HTML: %v", err)
	}
	big := strings.Repeat("x", 40000)
	var fe FieldErrors
	for _, tc := range []struct {
		name   string
		values map[string]Value
		want   string
	}{
		{"one field", map[string]Value{"city": {One: "Zurich"}, "notes": {One: strings.Repeat("x", 70000)}}, "notes"},
		{"a secret", map[string]Value{"city": {One: "Zurich"}, "notes": {One: big}, "token": {One: big}}, "notes"},
	} {
		_, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: tc.values}, now)
		if !errors.As(err, &fe) || len(fe) != 1 || !strings.Contains(fe[tc.want], "KB, over the 63 KB limit") || !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("create, %s: %v", tc.name, err)
		}
	}
	if left, err := f.client.Read.ListInstances(ctx); err != nil || len(left) != 1 {
		t.Errorf("refused creates left %d instances, %v; want the one that fit", len(left), err)
	}

	inst, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{
		"city": {One: "Zurich"}, "token": {One: strings.Repeat("s", 30000)},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Updates: a plain field that does not fit with the sealed secret that
	// is not opened, one that does, and a secret that does not fit.
	for _, tc := range []struct {
		name    string
		key     *secrets.Key
		values  map[string]Value
		wantErr string
	}{
		{"fits with the secret", key, map[string]Value{"notes": {One: strings.Repeat("n", 20000)}}, ""},
		{"too big with the secret opened", key, map[string]Value{"notes": {One: strings.Repeat("n", 40000)}}, "notes"},
		{"a secret too big", key, map[string]Value{"token": {One: strings.Repeat("s", 70000)}}, "token"},
	} {
		_, err := UpdateInstance(ctx, f.client, tc.key, f.recipesDir, inst.ID, InstanceUpdate{Values: tc.values}, now)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("update, %s: %v", tc.name, err)
		case tc.wantErr != "" && (!errors.As(err, &fe) || len(fe) != 1 || fe[tc.wantErr] == ""):
			t.Errorf("update, %s: %v", tc.name, err)
		}
	}
	stored, err := f.client.Read.GetInstance(ctx, inst.ID)
	if err != nil || !strings.Contains(stored.FieldValues, strings.Repeat("n", 20000)) || strings.Contains(stored.FieldValues, strings.Repeat("n", 40000)) {
		t.Errorf("a refused update changed the instance, or the fitting one was lost: %v", err)
	}
}

// TestInstanceNames checks that names are cleaned as a playlist's are, on
// create and update, and that an empty one is the recipe's.
func TestInstanceNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	values := map[string]Value{"city": {One: "Zurich"}}
	for _, tc := range []struct {
		name, in, want string
		ok             bool
	}{
		{"trimmed", "  Home  ", "Home", true},
		{"empty is the recipe's", "", "Weather", true},
		{"blank is the recipe's", " \t ", "Weather", true},
		{"at the limit", strings.Repeat("é", 32), strings.Repeat("é", 32), true},
		{"over the limit", strings.Repeat("a", 65), "", false},
		{"a newline", "a\nb", "", false},
		{"a control character", "a\x07b", "", false},
		{"not UTF-8", "a\xffb", "", false},
	} {
		inst, err := CreateInstance(ctx, f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Name: tc.in, Values: values}, time.Unix(1, 0))
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, errkind.ErrInvalid)) || (tc.ok && inst.Name != tc.want) {
			t.Errorf("create %s: %q, %v", tc.name, inst.Name, err)
			continue
		}
		if !tc.ok {
			continue
		}
		upd, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Name: ptr(tc.in)}, time.Unix(1, 0))
		if err != nil || upd.Name != tc.want {
			t.Errorf("update %s: %q, %v", tc.name, upd.Name, err)
		}
	}
	inst, err := CreateInstance(ctx, f.client, nil, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Name: "Home", Values: values}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Repeat("a", 65), "a\nb"} {
		if _, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, inst.ID, InstanceUpdate{Name: ptr(bad)}, time.Unix(1, 0)); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("update to %q: %v", bad, err)
		}
	}
	if got, err := f.client.Read.GetInstance(ctx, inst.ID); err != nil || got.Name != "Home" {
		t.Errorf("a refused name changed the instance: %q, %v", got.Name, err)
	}
}

// retype rewrites the installed settings.yml so that the field keyname has
// the type, as a changed recipe would.
func retype(t *testing.T, f fixture, keyname, from, to string) {
	t.Helper()
	root, err := os.OpenRoot(Dir(f.recipesDir, f.recipe.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	b, err := root.ReadFile("settings.yml")
	if err != nil {
		t.Fatal(err)
	}
	old := "- keyname: " + keyname + "\n  field_type: " + from
	if !strings.Contains(string(b), old) {
		t.Fatalf("settings.yml has no %q", old)
	}
	if err := root.WriteFile("settings.yml", []byte(strings.Replace(string(b), old, "- keyname: "+keyname+"\n  field_type: "+to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateOneStorePerKey checks that a key lives in one of the plain
// values and the sealed secrets: when a field's type changes between password
// and plain, setting it moves the value to the store of the new type and
// removes the old one, and clearing removes it from both, so no stale copy
// is left to be shown or to override the new one.
func TestUpdateOneStorePerKey(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	key := testKey(t, keyA)
	now := time.Unix(5000, 0)
	for _, tc := range []struct {
		name         string
		field        string
		from, to     string
		initial      Value
		update       InstanceUpdate
		wantFields   map[string]Value // by InstanceFields, with the secrets in it
		wantPlain    map[string]Value // the plain store
		wantSecretOf []string         // PublicFields' secret names
	}{
		{
			// It was sealed and nothing unmarked it, so it stays sealed.
			"password to string, set", "token", "password", "string",
			Value{One: "hunter2"},
			InstanceUpdate{Values: map[string]Value{"token": {One: "notvisible"}}},
			map[string]Value{"city": {One: "Zurich"}, "token": {One: "notvisible"}},
			map[string]Value{"city": {One: "Zurich"}},
			[]string{"token"},
		},
		{
			"password to string, clear", "token", "password", "string",
			Value{One: "hunter2"},
			InstanceUpdate{Clear: []string{"token"}},
			map[string]Value{"city": {One: "Zurich"}},
			map[string]Value{"city": {One: "Zurich"}},
			[]string{},
		},
		{
			"string to password, set", "notes", "code", "password",
			Value{One: "plaintext"},
			InstanceUpdate{Values: map[string]Value{"notes": {One: "sealedvalue"}}},
			map[string]Value{"city": {One: "Zurich"}, "notes": {One: "sealedvalue"}},
			map[string]Value{"city": {One: "Zurich"}},
			[]string{"notes"},
		},
		{
			"string to password, clear", "notes", "code", "password",
			Value{One: "plaintext"},
			InstanceUpdate{Clear: []string{"notes"}},
			map[string]Value{"city": {One: "Zurich"}},
			map[string]Value{"city": {One: "Zurich"}},
			[]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			inst, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Zurich"}, tc.field: tc.initial}}, now)
			if err != nil {
				t.Fatal(err)
			}
			retype(t, f, tc.field, tc.from, tc.to)
			got, err := UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, tc.update, now)
			if err != nil {
				t.Fatal(err)
			}
			all, _, err := InstanceFields(&got, key)
			if err != nil || !maps.EqualFunc(all, tc.wantFields, Value.equal) {
				t.Errorf("InstanceFields = %v, %v; want %v", all, err, tc.wantFields)
			}
			public, names, err := PublicFields(&got, key)
			if err != nil || !maps.EqualFunc(public, tc.wantPlain, Value.equal) || !slices.Equal(names, tc.wantSecretOf) {
				t.Errorf("PublicFields = %v, %v, %v; want %v, %v", public, names, err, tc.wantPlain, tc.wantSecretOf)
			}
			if got.SettingsAt != now.Unix() {
				t.Errorf("settings_at = %d, want %d", got.SettingsAt, now.Unix())
			}
		})
	}
}

// TestSecretsDoNotOpenUnderAReusedID checks that the sealed secrets of a
// deleted instance do not open under a new one that is given its ID. The
// database does not reuse an ID (AUTOINCREMENT), but a database edit or a
// restored backup can, so the test makes it by lowering the sequence.
func TestSecretsDoNotOpenUnderAReusedID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	key := testKey(t, keyA)
	values := map[string]Value{"city": {One: "Zurich"}, "token": {One: "hunter2"}}
	first, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: values}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteInstance(ctx, f.client, first.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.client.Writer.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = 'plugin_instance'`, first.ID-1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Bern"}}}, time.Unix(2000, 0))
	if err != nil || second.ID != first.ID {
		t.Fatalf("the ID was not reused: %d, %d, %v", first.ID, second.ID, err)
	}
	second.SecretValues = first.SecretValues
	if _, _, err := InstanceFields(&second, key); err == nil {
		t.Error("the deleted instance's secrets opened under the new one with its ID")
	}
	if _, _, err := InstanceFields(&first, key); err != nil {
		t.Errorf("the instance's own secrets do not open: %v", err)
	}
}

// markFixture is a fixture with an instance of the recipe of weatherYML made
// with the key and the given values and marks. Its "notes" field is a code
// field, which an admin can mark secret.
func markFixture(t *testing.T, key *secrets.Key, values map[string]Value, marks map[string]bool) (fixture, db.PluginInstance) {
	t.Helper()
	f := newFixture(t)
	values = maps.Clone(values)
	if values == nil {
		values = map[string]Value{}
	}
	values["city"] = Value{One: "Zurich"}
	inst, err := CreateInstance(t.Context(), f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: values, Secret: marks}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return f, inst
}

// wantFieldErrors checks that err is a FieldErrors with exactly the given
// messages by keyname.
func wantFieldErrors(t *testing.T, err error, want map[string]string) {
	t.Helper()
	var fe FieldErrors
	if !errors.As(err, &fe) || !maps.Equal(fe, FieldErrors(want)) {
		t.Errorf("err = %v, want field errors %v", err, want)
	}
}

// TestCreateMarkedSecret checks that a field marked secret at creation has
// its value sealed, hidden, and listed as a mark.
func TestCreateMarkedSecret(t *testing.T) {
	t.Parallel()
	const value = "sw0rdfish-note"
	key := testKey(t, keyA)
	f, inst := markFixture(t, key, map[string]Value{"notes": {One: value}}, map[string]bool{"notes": true})
	if strings.Contains(inst.FieldValues, "notes") || strings.Contains(inst.FieldValues, value) || inst.SecretValues == nil {
		t.Errorf("field_values = %s, secret_values set = %v; want the value only in the secrets", inst.FieldValues, inst.SecretValues != nil)
	}
	if bytes.Contains(inst.SecretValues, []byte(value)) {
		t.Error("secret stored in the clear")
	}
	if marks, err := SecretKeys(&inst); err != nil || !slices.Equal(marks, []string{"notes"}) {
		t.Errorf("SecretKeys = %v, %v", marks, err)
	}
	public, names, err := PublicFields(&inst, key)
	if err != nil || !slices.Equal(names, []string{"notes"}) || !maps.EqualFunc(public, map[string]Value{"city": {One: "Zurich"}}, Value.equal) {
		t.Errorf("PublicFields = %v, %v, %v", public, names, err)
	}
	all, secret, err := InstanceFields(&inst, key)
	if err != nil || all["notes"].One != value || !slices.Equal(secret, []string{value}) {
		t.Errorf("InstanceFields = %v, %v, %v", all, secret, err)
	}
	if stored, err := f.client.Read.GetInstance(t.Context(), inst.ID); err != nil || stored.SecretKeys != `["notes"]` {
		t.Errorf("stored marks = %q, %v", stored.SecretKeys, err)
	}
}

// TestCreateMarkRefused checks that a mark that is refused is a field error
// on its keyname, that several are reported together with other bad values,
// and that nothing is created.
func TestCreateMarkRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	key := testKey(t, keyA)
	for _, tc := range []struct {
		name  string
		marks map[string]bool
		extra map[string]Value
		want  map[string]string
	}{
		{"a non-text field", map[string]bool{"days": true}, nil, map[string]string{"days": "cannot be secret, since its value is not text"}},
		{"a select", map[string]bool{"units": true}, nil, map[string]string{"units": "cannot be secret, since its value is not text"}},
		{"a display-only field", map[string]bool{"about": true}, nil, map[string]string{"about": "cannot be secret, since its value is not text"}},
		{"unmarking a password field", map[string]bool{"token": false}, nil, map[string]string{"token": "is a password field, which is always secret"}},
		{"an unknown keyname", map[string]bool{"nope": true}, nil, map[string]string{"nope": "no such field"}},
		{"several together with a bad value", map[string]bool{"days": true, "token": false, "nope": true}, map[string]Value{"units": {One: "kelvin"}}, map[string]string{
			"days": "cannot be secret, since its value is not text", "token": "is a password field, which is always secret",
			"nope": "no such field", "units": "is not one of the options",
		}},
	} {
		values := map[string]Value{"city": {One: "Zurich"}}
		maps.Copy(values, tc.extra)
		_, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: values, Secret: tc.marks}, time.Now())
		if !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%s: err = %v, want errkind.ErrInvalid", tc.name, err)
		}
		wantFieldErrors(t, err, tc.want)
	}
	if left, err := f.client.Read.ListInstances(ctx); err != nil || len(left) != 0 {
		t.Errorf("refused instances were stored: %+v, %v", left, err)
	}
	// Marking a password field is allowed and is not stored as a mark.
	inst, err := CreateInstance(ctx, f.client, key, f.recipesDir, InstanceParams{RecipeID: f.recipe.ID, Values: map[string]Value{"city": {One: "Zurich"}}, Secret: map[string]bool{"token": true, "notes": false}}, time.Now())
	if err != nil || inst.SecretKeys != `[]` {
		t.Errorf("marks of a password field and an unmarked field: %q, %v", inst.SecretKeys, err)
	}
}

// TestCreateEmptySecretValues checks that an empty value for an optional
// password field and for an optional marked field stores nothing and is not
// refused, even with no key.
func TestCreateEmptySecretValues(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	inst, err := CreateInstance(t.Context(), f.client, nil, f.recipesDir, InstanceParams{
		RecipeID: f.recipe.ID,
		Values:   map[string]Value{"city": {One: "Zurich"}, "token": {}, "notes": {}},
		Secret:   map[string]bool{"notes": true},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if inst.FieldValues != `{"city":"Zurich"}` || inst.SecretValues != nil || inst.SecretKeys != `["notes"]` {
		t.Errorf("field_values = %s, secret_values set = %v, secret_keys = %s", inst.FieldValues, inst.SecretValues != nil, inst.SecretKeys)
	}
}

// TestUpdateMarks checks that marking and unmarking on update moves a value
// between the stores without raising settings_at, and the refusals of it.
func TestUpdateMarks(t *testing.T) {
	t.Parallel()
	const note = "a plain note"
	key := testKey(t, keyA)
	ctx := t.Context()
	now := time.Unix(5000, 0)
	notes := map[string]Value{"notes": {One: note}}

	t.Run("mark moves a plain value into the secrets", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, notes, nil)
		got, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Secret: map[string]bool{"notes": true}}, now)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := f.client.Read.GetInstance(ctx, before.ID)
		if err != nil || !reflect.DeepEqual(stored, got) {
			t.Fatalf("stored %+v, %v; returned %+v", stored, err, got)
		}
		if strings.Contains(stored.FieldValues, "notes") || stored.SecretValues == nil || stored.SecretKeys != `["notes"]` || stored.SettingsAt != before.SettingsAt {
			t.Errorf("field_values = %s, secrets set = %v, secret_keys = %s, settings_at = %d (was %d)",
				stored.FieldValues, stored.SecretValues != nil, stored.SecretKeys, stored.SettingsAt, before.SettingsAt)
		}
		if _, secret, err := InstanceFields(&stored, key); err != nil || !slices.Equal(secret, []string{note}) {
			t.Errorf("secrets = %v, %v", secret, err)
		}
	})

	for _, tc := range []struct {
		name   string
		values map[string]Value
	}{
		{"unmark alone is refused", nil},
		{"unmark with a blank value is refused", map[string]Value{"notes": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, before := markFixture(t, key, notes, map[string]bool{"notes": true})
			_, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Secret: map[string]bool{"notes": false}, Values: tc.values}, now)
			wantFieldErrors(t, err, map[string]string{"notes": "is secret; give it a new value or clear it to make it plain"})
			if err != nil && strings.Contains(err.Error(), note) {
				t.Errorf("the error shows the value: %v", err)
			}
			stored, err := f.client.Read.GetInstance(ctx, before.ID)
			if err != nil || !reflect.DeepEqual(stored, before) || strings.Contains(stored.FieldValues, note) {
				t.Errorf("a refused unmark changed the instance: %+v, %v", stored, err)
			}
		})
	}

	t.Run("unmark with a new value shows only the new value", func(t *testing.T) {
		t.Parallel()
		const fresh = "a fresh plain note"
		f, before := markFixture(t, key, notes, map[string]bool{"notes": true})
		got, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Secret: map[string]bool{"notes": false}, Values: map[string]Value{"notes": {One: fresh}}}, now)
		if err != nil {
			t.Fatal(err)
		}
		public, names, err := PublicFields(&got, key)
		if err != nil || len(names) != 0 || public["notes"].One != fresh || strings.Contains(got.FieldValues, note) || got.SecretValues != nil || got.SecretKeys != `[]` {
			t.Errorf("PublicFields = %v, %v, %v; field_values %s, secrets set %v, marks %s", public, names, err, got.FieldValues, got.SecretValues != nil, got.SecretKeys)
		}
	})

	t.Run("unmark with a clear removes the value from both stores", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, notes, map[string]bool{"notes": true})
		got, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Secret: map[string]bool{"notes": false}, Clear: []string{"notes"}}, now)
		if err != nil {
			t.Fatal(err)
		}
		all, _, err := InstanceFields(&got, key)
		if _, ok := all["notes"]; ok || err != nil || got.SecretValues != nil || got.SecretKeys != `[]` {
			t.Errorf("InstanceFields = %v, %v; secrets set %v, marks %s", all, err, got.SecretValues != nil, got.SecretKeys)
		}
	})

	t.Run("mark with no key and a value to move", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, nil, notes, nil)
		_, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, before.ID, InstanceUpdate{Secret: map[string]bool{"notes": true}}, now)
		if !errors.Is(err, secrets.ErrNoKey) {
			t.Fatalf("err = %v, want secrets.ErrNoKey", err)
		}
		if stored, err := f.client.Read.GetInstance(ctx, before.ID); err != nil || !reflect.DeepEqual(stored, before) {
			t.Errorf("a refused update changed the instance: %+v, %v", stored, err)
		}
	})

	t.Run("mark with no key and no value", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, nil, nil, nil)
		got, err := UpdateInstance(ctx, f.client, nil, f.recipesDir, before.ID, InstanceUpdate{Secret: map[string]bool{"notes": true}}, now)
		if err != nil {
			t.Fatal(err)
		}
		if marks, err := SecretKeys(&got); err != nil || !slices.Equal(marks, []string{"notes"}) || got.SettingsAt != before.SettingsAt {
			t.Errorf("marks = %v, %v, settings_at = %d", marks, err, got.SettingsAt)
		}
	})

	t.Run("mark a value that is too short", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, map[string]Value{"notes": {One: "abc"}}, nil)
		_, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Secret: map[string]bool{"notes": true}}, now)
		wantFieldErrors(t, err, map[string]string{"notes": "must be at least 6 characters to be secret"})
		if stored, err := f.client.Read.GetInstance(ctx, before.ID); err != nil || !reflect.DeepEqual(stored, before) {
			t.Errorf("a refused update changed the instance: %+v, %v", stored, err)
		}
	})

	t.Run("set a short value on a marked field", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, notes, map[string]bool{"notes": true})
		_, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Values: map[string]Value{"notes": {One: "abc"}}}, now)
		wantFieldErrors(t, err, map[string]string{"notes": "must be at least 6 characters to be secret"})
		if stored, err := f.client.Read.GetInstance(ctx, before.ID); err != nil || !reflect.DeepEqual(stored, before) {
			t.Errorf("a refused update changed the instance: %+v, %v", stored, err)
		}
	})

	t.Run("empty keeps a marked value and Clear clears it", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, notes, map[string]bool{"notes": true})
		got, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Values: map[string]Value{"notes": {}}}, now)
		if err != nil {
			t.Fatal(err)
		}
		if all, _, err := InstanceFields(&got, key); err != nil || all["notes"].One != note || got.SettingsAt != before.SettingsAt {
			t.Errorf("after an empty value: %v, %v, settings_at = %d", all, err, got.SettingsAt)
		}
		got, err = UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, InstanceUpdate{Clear: []string{"notes"}}, now)
		if err != nil {
			t.Fatal(err)
		}
		all, _, err := InstanceFields(&got, key)
		if _, ok := all["notes"]; ok || err != nil || got.SecretKeys != `["notes"]` || got.SettingsAt != now.Unix() {
			t.Errorf("after Clear: %v, %v, secret_keys = %s, settings_at = %d", all, err, got.SecretKeys, got.SettingsAt)
		}
	})
}

// TestUpdateMarkRefused checks the refusals of a mark on update, that none
// changes the instance, and that no error text, the plain fields or the
// stored row shows the secret value.
func TestUpdateMarkRefused(t *testing.T) {
	t.Parallel()
	const secret = "topsecret-note"
	key := testKey(t, keyA)
	ctx := t.Context()
	f, before := markFixture(t, key, map[string]Value{"notes": {One: secret}, "token": {One: "hunter2"}}, map[string]bool{"notes": true})
	if strings.Contains(before.FieldValues, secret) || strings.Contains(before.FieldValues, "hunter2") {
		t.Errorf("field_values = %s shows a secret", before.FieldValues)
	}
	if public, _, err := PublicFields(&before, key); err != nil || public["notes"].One != "" || public["token"].One != "" {
		t.Errorf("PublicFields shows a secret: %v, %v", public, err)
	}
	for _, tc := range []struct {
		name string
		u    InstanceUpdate
		want map[string]string
	}{
		{"mark a number", InstanceUpdate{Secret: map[string]bool{"days": true}}, map[string]string{"days": "cannot be secret, since its value is not text"}},
		{"unmark a password field", InstanceUpdate{Secret: map[string]bool{"token": false}}, map[string]string{"token": "is a password field, which is always secret"}},
		{"unknown keyname", InstanceUpdate{Secret: map[string]bool{"nope": true}}, map[string]string{"nope": "no such field"}},
		{"unmark of an unknown keyname that is not marked", InstanceUpdate{Secret: map[string]bool{"nope": false}}, map[string]string{"nope": "no such field"}},
		{"several at once", InstanceUpdate{Secret: map[string]bool{"days": true, "nope": true}}, map[string]string{"days": "cannot be secret, since its value is not text", "nope": "no such field"}},
		{"short value", InstanceUpdate{Values: map[string]Value{"notes": {One: "abc"}}}, map[string]string{"notes": "must be at least 6 characters to be secret"}},
		{"set and cleared, with a value that would also fail", InstanceUpdate{Values: map[string]Value{"notes": {One: "abc"}}, Clear: []string{"notes"}}, map[string]string{"notes": "is both set and cleared"}},
		{"set and cleared, a number out of range", InstanceUpdate{Values: map[string]Value{"days": {One: "0"}}, Clear: []string{"days"}}, map[string]string{"days": "is both set and cleared"}},
	} {
		_, err := UpdateInstance(ctx, f.client, key, f.recipesDir, before.ID, tc.u, time.Unix(5000, 0))
		wantFieldErrors(t, err, tc.want)
		if err != nil && (strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "abc")) {
			t.Errorf("%s: the error shows a value: %v", tc.name, err)
		}
		if stored, err := f.client.Read.GetInstance(ctx, before.ID); err != nil || !reflect.DeepEqual(stored, before) {
			t.Errorf("%s: a refused update changed the instance: %+v, %v", tc.name, stored, err)
		}
	}
}

// TestSealedStaysSealed checks that a sealed password value stays sealed,
// and is marked, when its field stops being a password field or leaves the
// recipe and an update changes another field, that an unmark alone is
// refused, and that an unmark with a clear removes it from both stores.
func TestSealedStaysSealed(t *testing.T) {
	t.Parallel()
	const secret = "hunter2"
	key := testKey(t, keyA)
	now := time.Unix(5000, 0)
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f fixture)
	}{
		{"retyped to string", func(t *testing.T, f fixture) { retype(t, f, "token", "password", "string") }},
		{"retyped to select", func(t *testing.T, f fixture) { retype(t, f, "token", "password", "select") }},
		{"dropped", func(t *testing.T, f fixture) { dropField(t, f, "token") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f, inst := markFixture(t, key, map[string]Value{"token": {One: secret}}, nil)
			tc.change(t, f)
			got, err := UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}}}, now)
			if err != nil {
				t.Fatal(err)
			}
			public, names, err := PublicFields(&got, key)
			if err != nil || strings.Contains(got.FieldValues, secret) || public["token"].One != "" || !slices.Equal(names, []string{"token"}) || got.SecretKeys != `["token"]` {
				t.Fatalf("after another field's edit: field_values %s, public %v, names %v, marks %s, %v", got.FieldValues, public, names, got.SecretKeys, err)
			}
			// An unmark alone would show it, so it is refused.
			_, err = UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Secret: map[string]bool{"token": false}}, now)
			wantFieldErrors(t, err, map[string]string{"token": "is secret; give it a new value or clear it to make it plain"})
			// Unmarked with a clear, it goes from both stores, though the
			// recipe may no longer declare it.
			got, err = UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Secret: map[string]bool{"token": false}, Clear: []string{"token"}}, now)
			if err != nil {
				t.Fatal(err)
			}
			all, _, err := InstanceFields(&got, key)
			if _, ok := all["token"]; ok || err != nil || strings.Contains(got.FieldValues, secret) || got.SecretValues != nil || got.SecretKeys != `[]` {
				t.Errorf("after the unmark and clear: fields %v, field_values %s, secrets set %v, marks %s, %v", all, got.FieldValues, got.SecretValues != nil, got.SecretKeys, err)
			}
		})
	}
}

// dropField removes the field with the keyname from the fixture recipe's
// settings.yml, as a recipe update that drops it would.
func dropField(t *testing.T, f fixture, keyname string) {
	t.Helper()
	root, err := os.OpenRoot(Dir(f.recipesDir, f.recipe.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	b, err := root.ReadFile("settings.yml")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return l == "- keyname: "+keyname+"\n" })
	if start < 0 {
		t.Fatalf("settings.yml has no field %q", keyname)
	}
	end := start + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "  ") {
		end++
	}
	if err := root.WriteFile("settings.yml", []byte(strings.Join(slices.Delete(lines, start, end), "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClearDroppedField checks that clear takes the keyname of a stored
// value whose field the recipe dropped, and removes it, while a keyname that
// is neither declared nor stored is refused as a typo.
func TestClearDroppedField(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	key := testKey(t, keyA)
	now := time.Unix(5000, 0)
	f, inst := markFixture(t, key, map[string]Value{"notes": {One: "a plain note"}}, nil)
	dropField(t, f, "notes")
	_, err := UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Clear: []string{"notez"}}, now)
	wantFieldErrors(t, err, map[string]string{"notez": "no such field"})
	got, err := UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Clear: []string{"notes"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.FieldValues != `{"city":"Zurich"}` {
		t.Errorf("field_values = %s, want the dropped field's value gone", got.FieldValues)
	}
	// Once it is gone, the keyname names nothing.
	_, err = UpdateInstance(ctx, f.client, key, f.recipesDir, inst.ID, InstanceUpdate{Clear: []string{"notes"}}, now)
	wantFieldErrors(t, err, map[string]string{"notes": "no such field"})
}

// TestUpdateInstanceSeen checks that an update with the state the page saw
// saves, with no key for a rename even when the instance has secrets, that a
// change since to the name, palette, a plain value or a mark refuses it with
// ErrInstanceChanged and changes nothing, and that no seen state lets the
// last write win.
func TestUpdateInstanceSeen(t *testing.T) {
	t.Parallel()
	key := testKey(t, keyA)
	now := time.Unix(5000, 0)
	values := map[string]Value{"notes": {One: "a sealed note"}, "tags": {Many: []string{"a", "c"}}, "token": {One: "hunter22"}}
	marks := map[string]bool{"notes": true}
	stateOf := func(t *testing.T, inst *db.PluginInstance) string {
		t.Helper()
		s, err := InstanceState(inst)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	for _, tc := range []struct {
		name   string
		change InstanceUpdate // made after the page loaded
	}{
		{"name", InstanceUpdate{Name: ptr("Elsewhere")}},
		{"palette", InstanceUpdate{Palette: ptr("bw")}},
		{"a plain value", InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}}}},
		{"a plain value added", InstanceUpdate{Values: map[string]Value{"days": {One: "3"}}}},
		{"a plain value cleared", InstanceUpdate{Clear: []string{"tags"}}},
		{"a list's choices", InstanceUpdate{Values: map[string]Value{"tags": {Many: []string{"a"}}}}},
		{"a mark added", InstanceUpdate{Secret: map[string]bool{"city": true}}},
		{"a mark removed", InstanceUpdate{Secret: map[string]bool{"notes": false}, Values: map[string]Value{"notes": {One: "a plain note"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, before := markFixture(t, key, values, marks)
			seen := stateOf(t, &before)
			changed, err := UpdateInstance(t.Context(), f.client, key, f.recipesDir, before.ID, tc.change, now)
			if err != nil {
				t.Fatal(err)
			}
			if stateOf(t, &changed) == seen {
				t.Fatal("the change left the state as it was")
			}
			_, err = UpdateInstance(t.Context(), f.client, nil, f.recipesDir, before.ID, InstanceUpdate{Name: ptr("New"), Seen: &seen}, now)
			if !errors.Is(err, ErrInstanceChanged) || !errors.Is(err, errkind.ErrConflict) {
				t.Fatalf("err = %v, want ErrInstanceChanged", err)
			}
			if stored, err := f.client.Read.GetInstance(t.Context(), before.ID); err != nil || !reflect.DeepEqual(stored, changed) {
				t.Errorf("a refused update changed the instance: %+v, %v", stored, err)
			}
		})
	}

	t.Run("a match saves, with no key for a rename", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, values, marks)
		seen := stateOf(t, &before)
		got, err := UpdateInstance(t.Context(), f.client, nil, f.recipesDir, before.ID, InstanceUpdate{Name: ptr("New"), Palette: ptr("bw"), Seen: &seen}, now)
		if err != nil || got.Name != "New" || got.Palette != "bw" {
			t.Fatalf("rename: %+v, %v", got, err)
		}
		// The page reloaded, and a value change with the key saves too.
		seen = stateOf(t, &got)
		if _, err = UpdateInstance(t.Context(), f.client, key, f.recipesDir, before.ID, InstanceUpdate{Values: map[string]Value{"city": {One: "Bern"}}, Seen: &seen}, now); err != nil {
			t.Fatalf("value change: %v", err)
		}
		// The stale page's next save is refused.
		if _, err := UpdateInstance(t.Context(), f.client, key, f.recipesDir, before.ID, InstanceUpdate{Name: ptr("Again"), Seen: &seen}, now); !errors.Is(err, ErrInstanceChanged) {
			t.Errorf("save from the stale page: %v", err)
		}
	})

	t.Run("a secret change leaves the state alone", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, values, marks)
		seen := stateOf(t, &before)
		got, err := UpdateInstance(t.Context(), f.client, key, f.recipesDir, before.ID, InstanceUpdate{Values: map[string]Value{"token": {One: "swordfish"}, "notes": {One: "another sealed note"}}}, now)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got.SecretValues, before.SecretValues) || stateOf(t, &got) != seen {
			t.Errorf("secrets resealed %v, state %s, want %s", !bytes.Equal(got.SecretValues, before.SecretValues), stateOf(t, &got), seen)
		}
		if _, err := UpdateInstance(t.Context(), f.client, nil, f.recipesDir, before.ID, InstanceUpdate{Name: ptr("New"), Seen: &seen}, now); err != nil {
			t.Errorf("save after a secret change: %v", err)
		}
	})

	t.Run("no seen state lets the last write win", func(t *testing.T) {
		t.Parallel()
		f, before := markFixture(t, key, values, marks)
		if _, err := UpdateInstance(t.Context(), f.client, nil, f.recipesDir, before.ID, InstanceUpdate{Name: ptr("First")}, now); err != nil {
			t.Fatal(err)
		}
		got, err := UpdateInstance(t.Context(), f.client, nil, f.recipesDir, before.ID, InstanceUpdate{Name: ptr("Second")}, now)
		if err != nil || got.Name != "Second" {
			t.Errorf("second write: %+v, %v", got, err)
		}
	})
}

// TestInstanceState checks that the digest does not depend on the stored
// JSON's key order or the order of the marks, and that its framing keeps
// different states apart where plain concatenation would not.
func TestInstanceState(t *testing.T) {
	t.Parallel()
	state := func(inst db.PluginInstance) string {
		t.Helper()
		s, err := InstanceState(&inst)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	marks, reordered := `["a","b"]`, `["b","a"]`
	base := db.PluginInstance{Name: "ab", Palette: "", FieldValues: `{"city":"Zurich","tags":["a","c"]}`, SecretKeys: marks}
	same := base
	same.FieldValues, same.SecretKeys = `{"tags":["a","c"], "city":"Zurich"}`, reordered
	if state(base) != state(same) {
		t.Error("key or mark order changed the state")
	}
	moved := base
	moved.Name, moved.Palette = "a", "b"
	if state(base) == state(moved) {
		t.Error(`name "ab" with no palette and name "a" with palette "b" have one state`)
	}
	asList := base
	asList.FieldValues = `{"city":["Zurich"],"tags":["a","c"]}`
	if state(base) == state(asList) {
		t.Error("a value and a one-element list have one state")
	}
	bad := base
	bad.FieldValues = `{`
	if _, err := InstanceState(&bad); err == nil {
		t.Error("undecodable field_values gave a state")
	}
}

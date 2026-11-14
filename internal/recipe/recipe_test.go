package recipe

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

const settingsYML = "---\nstrategy: polling\nname: Test Recipe\nrefresh_interval: 60\ncustom_fields:\n- keyname: city\n  field_type: string\n"

func openDB(t *testing.T) *db.Client {
	t.Helper()
	client, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// recipeFiles is a recipe as TRMNL's archive holds it, plus a hidden file
// that should not be installed.
var recipeFiles = map[string]string{
	"settings.yml":         settingsYML,
	"full.liquid":          "<div>{{ city }}</div>",
	"half_vertical.liquid": "<div>half</div>",
	".DS_Store":            "junk",
}

func writeDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
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
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func prefixed(prefix string, files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[prefix+k] = v
	}
	return out
}

// TestInstall checks each layout a recipe comes in: a directory, a trmnlp
// project with the recipe in src/, and zips with the recipe at the top or
// inside one directory.
func TestInstall(t *testing.T) {
	src := t.TempDir()
	writeDir(t, filepath.Join(src, "plain"), recipeFiles)
	writeDir(t, filepath.Join(src, "project"), prefixed("src/", recipeFiles))
	writeDir(t, filepath.Join(src, "project"), map[string]string{".trmnlp.yml": "x", "_build/full.png": "x"})
	writeZip(t, filepath.Join(src, "top.zip"), recipeFiles)
	writeZip(t, filepath.Join(src, "nested.zip"), prefixed("trmnl-recipe-main/src/", recipeFiles))

	client := openDB(t)
	recipes := filepath.Join(t.TempDir(), "recipes")
	for _, name := range []string{"plain", "project", "top.zip", "nested.zip"} {
		rec, err := Install(t.Context(), client, recipes, filepath.Join(src, name), time.Unix(1000, 0))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rec.Name != "Test Recipe" || rec.RefreshInterval != 3600 {
			t.Errorf("%s: recipe %+v", name, rec)
		}
		var got []string
		err = fs.WalkDir(os.DirFS(Dir(recipes, rec.ID)), ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				got = append(got, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != "full.liquid,half_vertical.liquid,settings.yml" {
			t.Errorf("%s: installed %v", name, got)
		}
	}
	left, err := os.ReadDir(recipes)
	if err != nil || len(left) != 4 {
		t.Errorf("recipes dir holds %d entries, %v; want 4, with no install dirs left", len(left), err)
	}
}

func TestInstallRefuses(t *testing.T) {
	src := t.TempDir()
	writeZip(t, filepath.Join(src, "escape.zip"), map[string]string{"settings.yml": settingsYML, "full.liquid": "x", "../escape": "x"})
	writeZip(t, filepath.Join(src, "nofull.zip"), map[string]string{"settings.yml": settingsYML})
	writeZip(t, filepath.Join(src, "nosettings.zip"), map[string]string{"full.liquid": "x"})
	writeDir(t, filepath.Join(src, "empty"), map[string]string{"README": "x"})
	writeDir(t, filepath.Join(src, "badyaml"), map[string]string{"settings.yml": "refresh_interval: [", "full.liquid": "x"})

	client := openDB(t)
	recipes := filepath.Join(t.TempDir(), "recipes")
	for _, name := range []string{"escape.zip", "nofull.zip", "nosettings.zip", "empty", "badyaml"} {
		if _, err := Install(t.Context(), client, recipes, filepath.Join(src, name), time.Now()); err == nil || !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%s: err = %v, want errkind.ErrInvalid", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(recipes), "escape")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a zip entry escaped the install dir: %v", err)
	}
	if rows, err := client.Read.ListRecipes(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("recipes recorded: %v, %v", rows, err)
	}
	left, _ := os.ReadDir(recipes)
	if len(left) != 0 {
		t.Errorf("install dirs left behind: %v", left)
	}
}

func TestRefresh(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		want    time.Duration
	}{
		{0, DefaultRefresh},
		{1, MinRefresh},
		{1440, 24 * time.Hour},
	} {
		if got := (Settings{RefreshInterval: tc.minutes}).Refresh(); got != tc.want {
			t.Errorf("%d minutes: %v, want %v", tc.minutes, got, tc.want)
		}
	}
}

// TestProject checks that a project holds the recipe under src/ and a
// .trmnlp.yml that names every field value by environment variable, so no
// value, secret or not, is in a file or read as Liquid.
func TestProject(t *testing.T) {
	dir := t.TempDir()
	writeDir(t, dir, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	p := NewProject(dir, map[string]string{"city": "{{ evil }}", "api_key": "hunter2"}, "America/Los_Angeles",
		[]string{"trmnl.com", "fonts.gstatic.com"})
	if p.Env["FIELD_0"] != "hunter2" || p.Env["FIELD_1"] != "{{ evil }}" || len(p.Env) != 2 {
		t.Errorf("Env = %v", p.Env)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := p.Write(tw); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	files := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
	if files["src/full.liquid"] != "x" || files["src/settings.yml"] != settingsYML {
		t.Errorf("project files: %v", files)
	}
	const want = `time_zone: America/Los_Angeles
custom_fields:
    api_key: '{{ env.FIELD_0 }}'
    city: '{{ env.FIELD_1 }}'
framework_asset_host: http://trmnl.com
asset_overrides:
    https://fonts.gstatic.com/: http://fonts.gstatic.com/
    https://trmnl.com/: http://trmnl.com/
`
	if got := files[".trmnlp.yml"]; got != want {
		t.Errorf(".trmnlp.yml:\n%s\nwant:\n%s", got, want)
	}
}

func TestInstanceFields(t *testing.T) {
	client := openDB(t)
	rec, err := client.Write.CreateRecipe(t.Context(), db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	key, err := secrets.Parse("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if err != nil {
		t.Fatal(err)
	}
	p := InstanceParams{RecipeID: rec.ID, Name: "i", Fields: map[string]string{"city": "Zurich"}, Secrets: map[string]string{"token": "hunter2"}}
	if _, err := CreateInstance(t.Context(), client, nil, p, time.Now()); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("secret stored without a key: %v", err)
	}
	inst, err := CreateInstance(t.Context(), client, key, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(inst.SecretValues, []byte("hunter2")) || strings.Contains(inst.FieldValues, "hunter2") {
		t.Error("secret stored in the clear")
	}
	got, secret, err := InstanceFields(&inst, key)
	if err != nil || got["city"] != "Zurich" || got["token"] != "hunter2" || !slices.Equal(secret, []string{"hunter2"}) {
		t.Errorf("InstanceFields = %v, %v, %v", got, secret, err)
	}
	if _, _, err := InstanceFields(&inst, nil); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("secrets opened without a key: %v", err)
	}
	other, err := CreateInstance(t.Context(), client, key, InstanceParams{RecipeID: rec.ID, Name: "j"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other.SecretValues = inst.SecretValues
	if _, _, err := InstanceFields(&other, key); err == nil {
		t.Error("secrets copied to another instance opened")
	}
}

func TestUsesDevice(t *testing.T) {
	// The marker split across the reader's 32 KiB chunk edge.
	edge := strings.Repeat("x", 32<<10-5) + "trmnl.device"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"none", map[string]string{"settings.yml": settingsYML, "full.liquid": "{{ trmnl.user.name }} device"}, false},
		{"markup", map[string]string{"settings.yml": settingsYML, "full.liquid": "{{ trmnl.device.friendly_id }}"}, true},
		{"polling url", map[string]string{"settings.yml": "polling_url: http://x/{{ trmnl.device.width }}\n", "full.liquid": "x"}, true},
		{"transform", map[string]string{"settings.yml": settingsYML, "full.liquid": "x", "lib/transform.js": "trmnl.device.height"}, true},
		{"chunk edge", map[string]string{"settings.yml": settingsYML, "full.liquid": edge}, true},
	} {
		dir := t.TempDir()
		writeDir(t, dir, tc.files)
		if got, err := UsesDevice(dir); err != nil || got != tc.want {
			t.Errorf("%s: UsesDevice = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := UsesDevice(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing recipe was scanned")
	}
}

func TestNewDevice(t *testing.T) {
	volts, rssi, rate := 3.6, int64(-75), int64(900)
	got := NewDevice(&db.GetRenderDeviceRow{FriendlyID: "ABC123", FwVersion: "1.8.10", BatteryV: &volts, Rssi: &rssi, LastRefreshRateSent: &rate})
	if got.FriendlyID != "ABC123" || got.FirmwareVersion != "1.8.10" || got.RefreshIntervalSeconds != 900 ||
		got.PercentCharged == nil || *got.PercentCharged < 49.9 || *got.PercentCharged > 50.1 ||
		got.WifiStrength == nil || *got.WifiStrength != 50 {
		t.Errorf("NewDevice = %+v", got)
	}
	// A device that has not woken has no readings, and one past the ends is clamped.
	got = NewDevice(&db.GetRenderDeviceRow{FriendlyID: "ABC123"})
	if got.PercentCharged != nil || got.WifiStrength != nil {
		t.Errorf("readings invented: %+v", got)
	}
	volts, rssi = 5, -30
	got = NewDevice(&db.GetRenderDeviceRow{BatteryV: &volts, Rssi: &rssi})
	if *got.PercentCharged != 100 || *got.WifiStrength != 100 {
		t.Errorf("not clamped: %v %v", *got.PercentCharged, *got.WifiStrength)
	}
}

// projectConfig returns the .trmnlp.yml a project writes.
func projectConfig(t *testing.T, p Project) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := p.Write(tw); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatal(".trmnlp.yml not found")
		}
		if h.Name == ".trmnlp.yml" {
			b, _ := io.ReadAll(tr)
			return string(b)
		}
	}
}

// TestProjectDevice checks that a per-device project carries the device's
// values under variables.trmnl.device, as typed YAML that a hostile value
// cannot break out of, and that a shared project has no variables.
func TestProjectDevice(t *testing.T) {
	dir := t.TempDir()
	writeDir(t, dir, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	config := func(p Project) string { return projectConfig(t, p) }
	shared := NewProject(dir, nil, "UTC", nil)
	if got := config(shared); strings.Contains(got, "variables") {
		t.Errorf("shared project has variables:\n%s", got)
	}
	pct, wifi := 85.5, 70
	got := config(shared.WithDevice(Device{FriendlyID: "{{ x }}\nmac: 1", PercentCharged: &pct, WifiStrength: &wifi}))
	const want = `variables:
    trmnl:
        device:
            friendly_id: |-
                {{ x }}
                mac: 1
            percent_charged: 85.5
            wifi_strength: 70
`
	if !strings.Contains(got, want) {
		t.Errorf(".trmnlp.yml:\n%s\nwant it to hold:\n%s", got, want)
	}
	if strings.Contains(config(shared), "variables") {
		t.Error("WithDevice changed the project it was called on")
	}
	// A project that already has variables is not changed either.
	a := shared.WithDevice(Device{FriendlyID: "AAA111"})
	before := config(a)
	b := a.WithDevice(Device{FriendlyID: "BBB222"})
	if after := config(a); after != before || strings.Contains(after, "BBB222") {
		t.Errorf("WithDevice changed the project it was called on:\n%s", after)
	}
	if !strings.Contains(config(b), "BBB222") {
		t.Errorf("WithDevice result lacks the new device:\n%s", config(b))
	}
}

func TestDeleteInstance(t *testing.T) {
	ctx := t.Context()
	client := openDB(t)
	rec, err := client.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, 2)
	for _, name := range []string{"a", "b"} {
		inst, err := CreateInstance(ctx, client, nil, InstanceParams{RecipeID: rec.ID, Name: name}, time.Now())
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
	if err := client.Write.LinkPlaylistDevice(ctx, db.LinkPlaylistDeviceParams{DeviceID: dev.ID, PlaylistID: pl.ID}); err != nil {
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
	if !errors.Is(err, ErrInstanceInUse) || !strings.Contains(err.Error(), "item IDs "+strconv.FormatInt(items[0], 10)+";") {
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

// TestCreateInstanceInvalid checks that bad settings are refused with
// errkind.ErrInvalid, an unknown recipe with ErrNoRecipe, and that nothing is
// stored for either.
func TestCreateInstanceInvalid(t *testing.T) {
	ctx := t.Context()
	client := openDB(t)
	rec, err := client.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "Weather", Source: "s", RefreshInterval: 900, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	key, err := secrets.Parse("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		p    InstanceParams
		want error
	}{
		{"unknown palette", InstanceParams{RecipeID: rec.ID, Palette: "neon"}, errkind.ErrInvalid},
		{"field and secret", InstanceParams{RecipeID: rec.ID, Fields: map[string]string{"k": "a"}, Secrets: map[string]string{"k": "hunter2"}}, errkind.ErrInvalid},
		{"short secret", InstanceParams{RecipeID: rec.ID, Secrets: map[string]string{"k": "abcde"}}, errkind.ErrInvalid},
		{"short secret in bytes", InstanceParams{RecipeID: rec.ID, Secrets: map[string]string{"k": "ééééé"}}, errkind.ErrInvalid},
		{"empty secret", InstanceParams{RecipeID: rec.ID, Secrets: map[string]string{"k": ""}}, errkind.ErrInvalid},
		{"unknown recipe", InstanceParams{RecipeID: rec.ID + 1}, ErrNoRecipe},
	} {
		_, err := CreateInstance(ctx, client, key, tc.p, time.Now())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		for _, v := range tc.p.Secrets {
			if v != "" && err != nil && strings.Contains(err.Error(), v) {
				t.Errorf("%s: the error shows the secret: %v", tc.name, err)
			}
		}
	}
	if left, err := client.Read.ListInstances(ctx); err != nil || len(left) != 0 {
		t.Errorf("refused instances were stored: %+v, %v", left, err)
	}
	inst, err := CreateInstance(ctx, client, key, InstanceParams{RecipeID: rec.ID, Palette: "bw", Secrets: map[string]string{"b": "abcdef", "a": "ghijkl"}}, time.Now())
	if err != nil || inst.Name != "Weather" {
		t.Fatalf("default name: %+v, %v", inst, err)
	}
	fields, names, err := PublicFields(&inst, key)
	if err != nil || len(fields) != 0 || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("PublicFields = %v, %v, %v", fields, names, err)
	}
}

// TestInstallZip checks that a zip read from a stream installs like a zip
// file, records where it came from, and leaves no temporary file, also when
// the reader fails or the zip is refused.
func TestInstallZip(t *testing.T) {
	src := t.TempDir()
	writeZip(t, filepath.Join(src, "r.zip"), recipeFiles)
	writeZip(t, filepath.Join(src, "escape.zip"), map[string]string{"settings.yml": settingsYML, "full.liquid": "x", "../escape": "x"})
	client := openDB(t)
	recipes := filepath.Join(t.TempDir(), "recipes")

	open := func(name string) *os.File {
		f, err := os.OpenInRoot(src, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	rec, err := InstallZip(t.Context(), client, recipes, open("r.zip"), time.Unix(1000, 0))
	if err != nil || rec.Source != "upload" || rec.InstalledAt != 1000 {
		t.Fatalf("InstallZip = %+v, %v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(Dir(recipes, rec.ID), "full.liquid")); err != nil {
		t.Error(err)
	}
	boom := errors.New("connection reset")
	if _, err := InstallZip(t.Context(), client, recipes, io.MultiReader(strings.NewReader("PK"), iotest.ErrReader(boom)), time.Now()); !errors.Is(err, boom) {
		t.Errorf("failing reader: %v", err)
	}
	if _, err := InstallZip(t.Context(), client, recipes, open("escape.zip"), time.Now()); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("escaping zip: %v", err)
	}
	entries, err := os.ReadDir(recipes)
	if err != nil || len(entries) != 1 {
		t.Errorf("recipes dir = %v, %v; want only the installed recipe", entries, err)
	}
	if rows, err := client.Read.ListRecipes(t.Context()); err != nil || len(rows) != 1 {
		t.Errorf("recipes = %v, %v", rows, err)
	}
}

// TestInstallRenameFails checks that a recipe whose directory cannot be put
// in place leaves no row behind, since the row and the rename share one
// transaction.
func TestInstallRenameFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "r")
	writeDir(t, src, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	client := openDB(t)
	recipes := filepath.Join(t.TempDir(), "recipes")
	// A non-empty directory where recipe 1 goes makes the rename fail.
	blocker := Dir(recipes, 1)
	writeDir(t, blocker, map[string]string{"x": "x"})
	if _, err := Install(t.Context(), client, recipes, src, time.Now()); err == nil {
		t.Fatal("installed over an existing directory")
	}
	if rows, err := client.Read.ListRecipes(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("recipes recorded: %v, %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(blocker, "x")); err != nil {
		t.Errorf("existing directory was disturbed: %v", err)
	}
}

// TestInstallZipConflictingNames checks that a zip that names a file twice,
// or names a path both as a file and as a directory, is refused as invalid
// input and not as an internal failure, and leaves nothing behind.
func TestInstallZipConflictingNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
	}{
		{"same file twice", []string{"settings.yml", "full.liquid", "full.liquid"}},
		{"file then directory", []string{"settings.yml", "full.liquid", "assets", "assets/a.png"}},
		{"directory then file", []string{"settings.yml", "full.liquid", "assets/a.png", "assets"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			for _, name := range tc.entries {
				w, err := zw.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				body := "x"
				if name == "settings.yml" {
					body = settingsYML
				}
				_, _ = io.WriteString(w, body)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			client := openDB(t)
			recipes := filepath.Join(t.TempDir(), "recipes")
			_, err := InstallZip(t.Context(), client, recipes, &buf, time.Now())
			if !errors.Is(err, errkind.ErrInvalid) {
				t.Fatalf("InstallZip = %v, want errkind.ErrInvalid", err)
			}
			if rows, err := client.Read.ListRecipes(t.Context()); err != nil || len(rows) != 0 {
				t.Errorf("recipes = %v, %v; want none", rows, err)
			}
			if entries, err := os.ReadDir(recipes); err == nil && len(entries) != 0 {
				t.Errorf("recipes dir = %v, want empty", entries)
			}
		})
	}
}

// TestInstallZipCorruptMember checks that a zip member whose bytes do not
// match its CRC is refused as invalid input, since the upload is damaged.
func TestInstallZipCorruptMember(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"settings.yml": settingsYML, "full.liquid": "<div>NEEDLE-PAYLOAD</div>"} {
		// Stored, so the bytes are in the archive as they are and one can be flipped.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	i := bytes.Index(b, []byte("NEEDLE-PAYLOAD"))
	if i < 0 {
		t.Fatal("payload not found in the zip")
	}
	b[i] ^= 0xff
	client := openDB(t)
	recipes := filepath.Join(t.TempDir(), "recipes")
	_, err := InstallZip(t.Context(), client, recipes, bytes.NewReader(b), time.Now())
	if !errors.Is(err, errkind.ErrInvalid) || !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("corrupt member: %v, want invalid input from a checksum error", err)
	}
	if rows, err := client.Read.ListRecipes(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("recipes recorded: %v, %v", rows, err)
	}
}

// TestCopierReadVersusWrite checks that only a failure reading a zip member
// is the upload's fault. A directory source's read error is not marked.
func TestCopierReadVersusWrite(t *testing.T) {
	boom := errors.New("boom")
	out, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	c := &copier{out: out}
	if err := c.add("plain", iotest.ErrReader(boom)); !errors.Is(err, boom) || errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("read error from a directory source: %v, want it unmarked", err)
	}
}

// TestSweep checks that a recipe directory with no row, left by a crash
// between the rename and the commit, is removed, so the next install does
// not collide with it, and that a recipe with a row is kept.
func TestSweep(t *testing.T) {
	ctx := t.Context()
	src := filepath.Join(t.TempDir(), "r")
	writeDir(t, src, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	client := openDB(t)
	recipes := filepath.Join(t.TempDir(), "recipes")
	kept, err := Install(ctx, client, recipes, src, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	orphan := Dir(recipes, kept.ID+1)
	writeDir(t, orphan, map[string]string{"x": "x"})
	if _, err := Install(ctx, client, recipes, src, time.Now()); err == nil {
		t.Fatal("installed over an orphan directory")
	}
	if err := Sweep(ctx, client, recipes, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("orphan directory: %v, want it removed", err)
	}
	if _, err := os.Stat(Dir(recipes, kept.ID)); err != nil {
		t.Errorf("installed recipe: %v", err)
	}
	if rec, err := Install(ctx, client, recipes, src, time.Now()); err != nil || rec.ID != kept.ID+1 {
		t.Errorf("install after the sweep = %+v, %v", rec, err)
	}
}

// TestSweepTemps checks that an old temporary entry is removed, and one a
// running install may still be using is kept.
func TestSweepTemps(t *testing.T) {
	now := time.Now()
	recipes, firmwareDir := t.TempDir(), t.TempDir()
	old, fresh := now.Add(-2*time.Hour), now.Add(-time.Minute)
	for _, e := range []struct {
		dir, name string
		dirEntry  bool
		mtime     time.Time
	}{
		{recipes, ".install-old", true, old},
		{recipes, ".install-fresh", true, fresh},
		{recipes, ".upload-old.zip", false, old},
		{recipes, ".upload-fresh.zip", false, fresh},
		{recipes, "notes.txt", false, old},
		{firmwareDir, ".upload-old.bin", false, old},
		{firmwareDir, ".upload-fresh.bin", false, fresh},
		{firmwareDir, "keep.bin", false, old},
	} {
		p := filepath.Join(e.dir, e.name)
		if e.dirEntry {
			writeDir(t, p, map[string]string{"x": "x"})
		} else {
			writeDir(t, e.dir, map[string]string{e.name: "x"})
		}
		if err := os.Chtimes(p, e.mtime, e.mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := Sweep(t.Context(), openDB(t), recipes, firmwareDir, now); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		dir, name string
		kept      bool
	}{
		{recipes, ".install-old", false},
		{recipes, ".install-fresh", true},
		{recipes, ".upload-old.zip", false},
		{recipes, ".upload-fresh.zip", true},
		{recipes, "notes.txt", true},
		{firmwareDir, ".upload-old.bin", false},
		{firmwareDir, ".upload-fresh.bin", true},
		{firmwareDir, "keep.bin", true},
	} {
		_, err := os.Stat(filepath.Join(e.dir, e.name))
		if kept := err == nil; kept != e.kept {
			t.Errorf("%s: kept = %v (%v), want %v", e.name, kept, err, e.kept)
		}
	}
}

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
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
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
		if _, err := Install(t.Context(), client, recipes, filepath.Join(src, name), time.Now()); err == nil {
			t.Errorf("%s: installed", name)
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
	got, err := InstanceFields(&inst, key)
	if err != nil || got["city"] != "Zurich" || got["token"] != "hunter2" {
		t.Errorf("InstanceFields = %v, %v", got, err)
	}
	if _, err := InstanceFields(&inst, nil); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("secrets opened without a key: %v", err)
	}
}

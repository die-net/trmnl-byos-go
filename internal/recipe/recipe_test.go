package recipe

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
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
	t.Parallel()
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
	t.Parallel()
	src := t.TempDir()
	writeZip(t, filepath.Join(src, "escape.zip"), map[string]string{"settings.yml": settingsYML, "full.liquid": "x", "../escape": "x"})
	writeZip(t, filepath.Join(src, "nofull.zip"), map[string]string{"settings.yml": settingsYML})
	writeZip(t, filepath.Join(src, "nosettings.zip"), map[string]string{"full.liquid": "x"})
	writeDir(t, filepath.Join(src, "empty"), map[string]string{"README": "x"})
	writeDir(t, filepath.Join(src, "badyaml"), map[string]string{"settings.yml": "refresh_interval: [", "full.liquid": "x"})
	for name, fields := range map[string]string{
		"fields-not-a-list": "custom_fields: 7\n",
		"field-not-a-map":   "custom_fields:\n- city\n",
		"no-keyname":        "custom_fields:\n- field_type: string\n",
		"duplicate":         "custom_fields:\n- keyname: a\n- keyname: a\n",
		"bad-number":        "custom_fields:\n- keyname: a\n  maxlength: lots\n",
		"bad-option":        "custom_fields:\n- keyname: a\n  field_type: select\n  options:\n  - [x]\n",
	} {
		writeDir(t, filepath.Join(src, name), map[string]string{"settings.yml": "name: x\n" + fields, "full.liquid": "x"})
	}

	client := openDB(t)
	recipes := filepath.Join(t.TempDir(), "recipes")
	for _, name := range []string{
		"escape.zip", "nofull.zip", "nosettings.zip", "empty", "badyaml",
		"fields-not-a-list", "field-not-a-map", "no-keyname", "duplicate", "bad-number", "bad-option",
	} {
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
	t.Parallel()
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
	t.Parallel()
	dir := t.TempDir()
	writeDir(t, dir, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	p := NewProject(dir, map[string]Value{"city": {One: "{{ evil }}"}, "api_key": {One: "hunter2"}, "tags": {Many: []string{"a b", "{% x %}"}}}, "America/Los_Angeles", "",
		[]string{"trmnl.com", "fonts.gstatic.com"})
	wantEnv := map[string]string{"FIELD_0": "hunter2", "FIELD_1": "{{ evil }}", "FIELD_2_0": "a b", "FIELD_2_1": "{% x %}"}
	if !maps.Equal(p.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", p.Env, wantEnv)
	}
	for name := range p.Env {
		if !strings.HasPrefix(name, "FIELD_") {
			t.Errorf("env name %q does not start with FIELD_, which the renderer requires", name)
		}
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
    tags:
        - '{{ env.FIELD_2_0 }}'
        - '{{ env.FIELD_2_1 }}'
framework_asset_host: http://trmnl.com
asset_overrides:
    https://fonts.gstatic.com/: http://fonts.gstatic.com/
    https://trmnl.com/: http://trmnl.com/
`
	if got := files[".trmnlp.yml"]; got != want {
		t.Errorf(".trmnlp.yml:\n%s\nwant:\n%s", got, want)
	}
}

func TestUsesDevice(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	volts, rssi, rate := 3.6, int64(-75), int64(900)
	got := NewDevice(&db.GetRenderDeviceRow{FriendlyID: "ABC123", FwVersion: "1.8.10", SleepStart: -1, SleepEnd: -1, BatteryV: &volts, Rssi: &rssi, LastRefreshRateSent: &rate})
	if got.FriendlyID != "ABC123" || got.FirmwareVersion != "1.8.10" || got.RefreshIntervalSeconds != 900 ||
		got.PercentCharged == nil || *got.PercentCharged < 49.9 || *got.PercentCharged > 50.1 ||
		got.WifiStrength == nil || *got.WifiStrength != 50 {
		t.Errorf("NewDevice = %+v", got)
	}
	// A device that has not woken has no readings, and one past the ends is clamped.
	got = NewDevice(&db.GetRenderDeviceRow{FriendlyID: "ABC123", SleepStart: -1, SleepEnd: -1})
	if got.PercentCharged != nil || got.WifiStrength != nil {
		t.Errorf("readings invented: %+v", got)
	}
	volts, rssi = 5, -30
	got = NewDevice(&db.GetRenderDeviceRow{BatteryV: &volts, Rssi: &rssi, SleepStart: -1, SleepEnd: -1})
	if *got.PercentCharged != 100 || *got.WifiStrength != 100 {
		t.Errorf("not clamped: %v %v", *got.PercentCharged, *got.WifiStrength)
	}
}

// TestNewDeviceSleep checks that a sleep window reaches trmnl.device as
// trmnlp's sleep_mode_enabled, sleep_start_time and sleep_end_time, in
// minutes, and that no window is false with no times. A window that starts
// at midnight still has its start.
func TestNewDeviceSleep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDir(t, dir, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	shared := NewProject(dir, nil, "UTC", "", nil)
	for _, tc := range []struct {
		name       string
		start, end int64
		want       string
		absent     []string
	}{
		{"window", 23 * 60, 6*60 + 30, "sleep_mode_enabled: true\n            sleep_start_time: 1380\n            sleep_end_time: 390\n", nil},
		{"window from midnight", 0, 300, "sleep_mode_enabled: true\n            sleep_start_time: 0\n            sleep_end_time: 300\n", nil},
		{"none", -1, -1, "sleep_mode_enabled: false\n", []string{"sleep_start_time", "sleep_end_time"}},
	} {
		got := projectConfig(t, shared.WithDevice(NewDevice(&db.GetRenderDeviceRow{FriendlyID: "ABC123", SleepStart: tc.start, SleepEnd: tc.end})))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: .trmnlp.yml:\n%s\nwant it to hold:\n%s", tc.name, got, tc.want)
		}
		for _, a := range tc.absent {
			if strings.Contains(got, a) {
				t.Errorf("%s: .trmnlp.yml has %s:\n%s", tc.name, a, got)
			}
		}
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
	t.Parallel()
	dir := t.TempDir()
	writeDir(t, dir, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	config := func(p Project) string { return projectConfig(t, p) }
	shared := NewProject(dir, nil, "UTC", "", nil)
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

// TestInstallZip checks that a zip read from a stream installs like a zip
// file, records where it came from, and leaves no temporary file, also when
// the reader fails or the zip is refused.
func TestInstallZip(t *testing.T) {
	t.Parallel()
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
	rec, err := InstallZip(t.Context(), client, recipes, open("r.zip"), "", time.Unix(1000, 0))
	if err != nil || rec.Source != UploadSource || rec.InstalledAt != 1000 {
		t.Fatalf("InstallZip = %+v, %v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(Dir(recipes, rec.ID), "full.liquid")); err != nil {
		t.Error(err)
	}
	boom := errors.New("connection reset")
	if _, err := InstallZip(t.Context(), client, recipes, io.MultiReader(strings.NewReader("PK"), iotest.ErrReader(boom)), "", time.Now()); !errors.Is(err, boom) {
		t.Errorf("failing reader: %v", err)
	}
	if _, err := InstallZip(t.Context(), client, recipes, open("escape.zip"), "", time.Now()); !errors.Is(err, errkind.ErrInvalid) {
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
	t.Parallel()
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
	t.Parallel()
	for _, tc := range []struct {
		name    string
		entries []string
	}{
		{"same file twice", []string{"settings.yml", "full.liquid", "full.liquid"}},
		{"file then directory", []string{"settings.yml", "full.liquid", "assets", "assets/a.png"}},
		{"directory then file", []string{"settings.yml", "full.liquid", "assets/a.png", "assets"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
			_, err := InstallZip(t.Context(), client, recipes, &buf, "", time.Now())
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
	t.Parallel()
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
	_, err := InstallZip(t.Context(), client, recipes, bytes.NewReader(b), "", time.Now())
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// zipOf returns a zip file of the named files, in the order given, which
// can name one twice.
func zipOf(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f[0])
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, f[1])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInstallZipName checks the name a recipe gets: its settings.yml's, or
// else the uploaded file's name without a directory (of either kind) or an
// extension, or else the recorded source.
func TestInstallZipName(t *testing.T) {
	t.Parallel()
	const unnamed = "strategy: polling\nrefresh_interval: 60\n"
	for _, tc := range []struct {
		name, settings, filename, want string
	}{
		{"name in settings.yml wins", settingsYML, "other.zip", "Test Recipe"},
		{"file name", unnamed, "My Recipe.zip", "My Recipe"},
		{"Windows path", unnamed, `C:\dl\weather.zip`, "weather"},
		{"directories above", unnamed, "../../x.zip", "x"},
		{"no extension", unnamed, "dir/plain", "plain"},
		{"only the last extension", unnamed, "weather.v2.zip", "weather.v2"},
		{"space around", unnamed, "  spaced .zip", "spaced"},
		{"empty", unnamed, "", UploadSource},
		{"only an extension", unnamed, ".zip", UploadSource},
		{"only a directory", unnamed, "dir/", UploadSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			z := zipOf(t, [2]string{"settings.yml", tc.settings}, [2]string{"full.liquid", "x"})
			recipes := filepath.Join(t.TempDir(), "recipes")
			rec, err := InstallZip(t.Context(), openDB(t), recipes, bytes.NewReader(z), tc.filename, time.Now())
			if err != nil || rec.Name != tc.want || rec.Source != UploadSource {
				t.Errorf("InstallZip = %+v, %v; want name %q from %q", rec, err, tc.want, UploadSource)
			}
			// The file name is only text: nothing is made from it.
			if entries, err := os.ReadDir(recipes); err != nil || len(entries) != 1 || entries[0].Name() != strconv.FormatInt(rec.ID, 10) {
				t.Errorf("recipes dir = %v, %v; want only the recipe's directory", entries, err)
			}
		})
	}
}

// cutDirectory returns the zip with its end record changed to say the
// central directory starts ten bytes before the end of the file, so the
// directory runs off the end while the record still counts every entry.
func cutDirectory(t *testing.T, zipped []byte) []byte {
	t.Helper()
	// An end record with no comment is the last 22 bytes. The directory's
	// size is at byte 12 of it and its offset at byte 16.
	const endLen = 22
	if len(zipped) < endLen {
		t.Fatalf("zip of %d bytes has no end record", len(zipped))
	}
	out := bytes.Clone(zipped)
	end := out[len(out)-endLen:]
	// The directory ends where the end record starts, so its offset plus
	// its size plus 12 is ten bytes before the end of the file.
	size, offset := binary.LittleEndian.Uint32(end[12:]), binary.LittleEndian.Uint32(end[16:])
	binary.LittleEndian.PutUint32(end[12:], 0)
	binary.LittleEndian.PutUint32(end[16:], offset+size+endLen-10)
	return out
}

// TestInstallInvalidText checks that each way a recipe is refused as
// invalid input says so as a plain statement, without a nested Go error
// text, and keeps the cause where there is one.
func TestInstallInvalidText(t *testing.T) {
	t.Parallel()
	good := [2]string{"settings.yml", settingsYML}
	liquid := [2]string{"full.liquid", "x"}
	many := make([][2]string, 0, 2+maxRecipeFiles)
	many = append(many, good, liquid)
	for i := range maxRecipeFiles {
		many = append(many, [2]string{"f" + strconv.Itoa(i), "x"})
	}
	damaged := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, name := range []string{"settings.yml", "full.liquid"} {
			body := "<div>NEEDLE-PAYLOAD</div>"
			if name == "settings.yml" {
				body = settingsYML
			}
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
		b[bytes.Index(b, []byte("NEEDLE-PAYLOAD"))] ^= 0xff
		return b
	}()
	for _, tc := range []struct {
		name  string
		zip   []byte
		want  string
		cause error
	}{
		{"not a zip", []byte("plain text"), "the file is not a zip archive", zip.ErrFormat},
		{"zip directory runs off the end", cutDirectory(t, zipOf(t, good, liquid)), "the file is not a zip archive", io.ErrUnexpectedEOF},
		{"empty file", nil, "the file is empty", nil},
		{"no settings.yml", zipOf(t, liquid), "the zip has no settings.yml at its top, in src/, or in one top-level folder", nil},
		{"name leaves the directory", zipOf(t, good, liquid, [2]string{"../escape", "x"}), `the zip has a file name that leaves its directory: "../escape"`, nil},
		{"no full.liquid", zipOf(t, good), "the recipe has no full.liquid", nil},
		{"settings.yml is not YAML", zipOf(t, [2]string{"settings.yml", "name: [unclosed"}, liquid), "the recipe's settings.yml is not valid YAML", nil},
		{"the recipe's settings.yml is empty", zipOf(t, [2]string{"settings.yml", ""}, liquid), "the recipe's settings.yml is empty", io.EOF},
		{"too many files", zipOf(t, many...), "the recipe has more than 1000 files", nil},
		{"too large", zipOf(t, good, [2]string{"full.liquid", strings.Repeat("a", MaxRecipeBytes+1)}), "the recipe's files are over 8 MB in all", nil},
		{"same file twice", zipOf(t, good, liquid, liquid), "full.liquid clashes with another file of the same name in the recipe", nil},
		{"member fails its checksum", damaged, "full.liquid in the zip is damaged", zip.ErrChecksum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := InstallZip(t.Context(), openDB(t), filepath.Join(t.TempDir(), "recipes"), bytes.NewReader(tc.zip), "a.zip", time.Now())
			if !errors.Is(err, errkind.ErrInvalid) || err.Error() != tc.want {
				t.Fatalf("InstallZip = %v; want invalid input saying %q", err, tc.want)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("%v lost its cause %v", err, tc.cause)
			}
		})
	}
}

// TestProjectLocale checks that a project carries the user's locale under
// variables.trmnl.user, and that adding the device or the pushed data keeps
// it, so trmnlp, which merges variables over its own, still sees the locale.
// A project with no locale has no variables.
func TestProjectLocale(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDir(t, dir, map[string]string{"settings.yml": settingsYML, "full.liquid": "x"})
	config := func(p Project) string { return projectConfig(t, p) }
	const locale = `        user:
            locale: pt-BR
`
	p := NewProject(dir, nil, "Europe/Lisbon", "pt-BR", nil)
	const want = `time_zone: Europe/Lisbon
variables:
    trmnl:
` + locale
	if got := config(p); got != want {
		t.Errorf("shared project:\n%s\nwant:\n%s", got, want)
	}
	// The device's values go next to the locale, not in place of it.
	got := config(p.WithDevice(Device{FriendlyID: "ABC123"}))
	for _, part := range []string{locale, "        device:\n            friendly_id: ABC123\n"} {
		if !strings.Contains(got, part) {
			t.Errorf("per-device project lacks %q:\n%s", part, got)
		}
	}
	// So do the pushed data, and the two together.
	withData, err := p.WithData(`{"headline": "hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := config(withData); !strings.Contains(got, locale) || !strings.Contains(got, "headline: hi") {
		t.Errorf("project with data:\n%s", got)
	}
	both, err := p.WithDevice(Device{FriendlyID: "ABC123"}).WithData(`{"headline": "hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := config(both); !strings.Contains(got, locale) || !strings.Contains(got, "friendly_id: ABC123") || !strings.Contains(got, "headline: hi") {
		t.Errorf("project with the device and data:\n%s", got)
	}
	// WithDevice does not change the project it is called on.
	if strings.Contains(config(p), "device") {
		t.Errorf("WithDevice changed the project it was called on:\n%s", config(p))
	}
	if got := config(NewProject(dir, nil, "UTC", "", nil)); strings.Contains(got, "variables") || strings.Contains(got, "locale") {
		t.Errorf("a project with no locale has variables:\n%s", got)
	}
}

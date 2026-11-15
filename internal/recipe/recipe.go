// Package recipe installs TRMNL recipes and assembles the trmnlp project a
// build of a plugin instance sends to the renderer.
//
// A recipe is untrusted code. The server only copies its files and reads a
// few settings from its settings.yml; its Liquid, transforms and page
// scripts run in the renderer.
package recipe

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// Limits on an installed recipe, which keep a build's project well under
// what the renderer accepts.
const (
	// MaxRecipeBytes bounds the size of an installed recipe's files, and of
	// an uploaded zip.
	MaxRecipeBytes = 8 << 20
	maxRecipeFiles = 1000
)

// Errors that say why a request is refused, each of an errkind kind and
// wrapped with detail that is safe to show. Input that is not acceptable,
// such as a recipe that cannot be installed, wraps errkind.ErrInvalid.
var (
	// ErrNoRecipe means no recipe has the ID.
	ErrNoRecipe = errkind.New(errkind.ErrNotFound, "no such recipe")
	// ErrNoInstance means no plugin instance has the ID.
	ErrNoInstance = errkind.New(errkind.ErrNotFound, "no such instance")
)

// Refresh intervals for a recipe whose settings.yml gives none, and the
// shortest one allowed, which bounds how often the renderer runs.
const (
	DefaultRefresh = 15 * time.Minute
	MinRefresh     = 5 * time.Minute
)

// Settings are the parts of a recipe's settings.yml the server uses.
type Settings struct {
	Name string `yaml:"name"`
	// RefreshInterval is how often the recipe's data changes, in minutes.
	RefreshInterval int `yaml:"refresh_interval"`
}

// Refresh is how often an instance of the recipe is rendered.
func (s Settings) Refresh() time.Duration {
	if s.RefreshInterval <= 0 {
		return DefaultRefresh
	}
	return max(time.Duration(s.RefreshInterval)*time.Minute, MinRefresh)
}

// Dir is where the recipe with the given ID is installed under recipesDir.
func Dir(recipesDir string, id int64) string {
	return filepath.Join(recipesDir, strconv.FormatInt(id, 10))
}

// Install copies the recipe at src, a directory or a zip file, into
// recipesDir and records it. src may be the recipe itself, with
// settings.yml at its top, or a trmnlp project holding it in src/; a zip
// may also hold either inside one top-level directory, as GitHub's do.
func Install(ctx context.Context, client *db.Client, recipesDir, src string, now time.Time) (db.Recipe, error) {
	info, err := os.Stat(src)
	if err != nil {
		return db.Recipe{}, fmt.Errorf("recipe source: %w", err)
	}
	from := func(c *copier) error { return c.fromDir(src) }
	if !info.IsDir() {
		f, err := openFile(src)
		if err != nil {
			return db.Recipe{}, fmt.Errorf("recipe source: %w", err)
		}
		defer f.Close()
		// The size is the open file's, not the path's a moment ago.
		if info, err = f.Stat(); err != nil {
			return db.Recipe{}, fmt.Errorf("recipe source: %w", err)
		}
		from = func(c *copier) error { return c.fromZip(f, info.Size()) }
	}
	return installFrom(ctx, client, recipesDir, from, src, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)), now)
}

// openFile opens the file at name, which the admin gave on the command
// line. It follows links to the file, then opens it by its own name inside
// its own directory.
func openFile(name string) (*os.File, error) {
	name, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, err
	}
	return os.OpenInRoot(filepath.Dir(name), filepath.Base(name))
}

// UploadSource is the recorded source of a recipe installed from an upload,
// whose file is gone once it is installed.
const UploadSource = "upload"

// uploadName is the name an uploaded recipe gets when its settings.yml has
// none: the uploaded file's name without its directory and extension, or
// UploadSource if nothing is left. A browser may send a Windows path, so
// both / and \ end a directory. The name is only text and never reaches the
// filesystem.
func uploadName(filename string) string {
	if i := strings.LastIndexAny(filename, `/\`); i >= 0 {
		filename = filename[i+1:]
	}
	return cmp.Or(strings.TrimSpace(strings.TrimSuffix(filename, path.Ext(filename))), UploadSource)
}

// InstallZip installs the recipe in the zip file r streams, as Install does
// a zip file. A zip needs random access, so r is copied to a temporary file
// in recipesDir first, the zip is read from that open file, and the file is
// removed afterwards. Creating and writing the file can only fail on the
// server's side, unless the error is r's own. What reading it as a zip then
// reports is about the upload, and is invalid input. The caller
// bounds r, with http.MaxBytesReader for a request body, since this reads
// until it ends. filename is the name the file was uploaded with, or "". It
// only names a recipe whose settings.yml has no name.
func InstallZip(ctx context.Context, client *db.Client, recipesDir string, r io.Reader, filename string, now time.Time) (db.Recipe, error) {
	if err := os.MkdirAll(recipesDir, 0o750); err != nil {
		return db.Recipe{}, fmt.Errorf("create recipes dir: %w", err)
	}
	// A hidden name, so it is never taken for a recipe's directory.
	f, err := os.CreateTemp(recipesDir, ".upload-*.zip")
	if err != nil {
		return db.Recipe{}, fmt.Errorf("create upload file: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	size, err := io.Copy(f, r)
	if err != nil {
		return db.Recipe{}, fmt.Errorf("save upload: %w", err)
	}
	from := func(c *copier) error { return c.fromZip(f, size) }
	return installFrom(ctx, client, recipesDir, from, UploadSource, uploadName(filename), now)
}

// installFrom copies the recipe that from reads and records it as coming
// from source. A recipe whose settings.yml has no name is called fallback.
func installFrom(ctx context.Context, client *db.Client, recipesDir string, from func(*copier) error, source, fallback string, now time.Time) (db.Recipe, error) {
	if err := os.MkdirAll(recipesDir, 0o750); err != nil {
		return db.Recipe{}, fmt.Errorf("create recipes dir: %w", err)
	}
	tmp, err := os.MkdirTemp(recipesDir, ".install-")
	if err != nil {
		return db.Recipe{}, fmt.Errorf("create install dir: %w", err)
	}
	// Once renamed into place, tmp is gone and this does nothing.
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := copyRecipe(from, tmp); err != nil {
		return db.Recipe{}, err
	}
	settings, err := ReadSettings(tmp)
	if err != nil {
		return db.Recipe{}, err
	}
	if settings.Name == "" {
		settings.Name = fallback
	}
	// The row and the directory go in together: a rename that fails rolls
	// the row back. A crash between the rename and the commit leaves a
	// directory with no row, and recipe.id is reused, so a later install
	// would collide with it. Sweep removes such a directory at startup.
	var rec db.Recipe
	renamed := false
	err = client.InTx(ctx, func(q *db.Queries) error {
		var err error
		rec, err = q.CreateRecipe(ctx, db.CreateRecipeParams{
			Name:            settings.Name,
			Source:          source,
			RefreshInterval: int64(settings.Refresh() / time.Second),
			InstalledAt:     now.Unix(),
		})
		if err != nil {
			return fmt.Errorf("save recipe: %w", err)
		}
		if err := os.Rename(tmp, Dir(recipesDir, rec.ID)); err != nil {
			return fmt.Errorf("install recipe: %w", err)
		}
		renamed = true
		return nil
	})
	if err != nil {
		if renamed {
			// A failed commit rolled the row back, and a later recipe may
			// get its ID, so the directory must not stay.
			_ = os.RemoveAll(Dir(recipesDir, rec.ID))
		}
		return db.Recipe{}, err
	}
	return rec, nil
}

// staleTempAge is how old a temporary install or upload entry must be before
// Sweep removes it. A CLI install or upload may be running when the server
// starts, and its entry is new.
const staleTempAge = time.Hour

// Sweep removes what a crash left behind. It removes each directory in
// recipesDir named by an ID that has no recipe row, which would otherwise
// block the next install that is given the same ID, since an ID is reused
// after a rollback. It also removes the temporary entries of an install or
// an upload, in recipesDir and, when firmwareDir is not empty, in it, that are
// over an hour old.
//
// The directory pass runs in a transaction, which holds SQLite's write lock
// from its start, so it cannot run between another process's rename of an
// install (the CLI's) and its commit.
func Sweep(ctx context.Context, client *db.Client, recipesDir, firmwareDir string, now time.Time) error {
	entries, err := os.ReadDir(recipesDir)
	if errors.Is(err, fs.ErrNotExist) {
		entries, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("list recipes dir: %w", err)
	}
	err = client.InTx(ctx, func(q *db.Queries) error {
		rows, err := q.ListRecipes(ctx)
		if err != nil {
			return fmt.Errorf("list recipes: %w", err)
		}
		have := make(map[string]bool, len(rows))
		for _, r := range rows {
			have[filepath.Base(Dir(recipesDir, r.ID))] = true
		}
		for _, e := range entries {
			if !e.IsDir() || have[e.Name()] {
				continue
			}
			if id, err := strconv.ParseInt(e.Name(), 10, 64); err != nil || id <= 0 || strconv.FormatInt(id, 10) != e.Name() {
				continue
			}
			slog.Warn("removing a recipe directory with no recipe", "dir", e.Name())
			if err := os.RemoveAll(filepath.Join(recipesDir, e.Name())); err != nil {
				return fmt.Errorf("remove orphan recipe directory: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	cutoff := now.Add(-staleTempAge)
	for _, t := range []struct{ dir, pattern string }{
		{recipesDir, ".install-*"}, {recipesDir, ".upload-*.zip"}, {firmwareDir, ".upload-*.bin"},
	} {
		if t.dir == "" {
			continue
		}
		if err := removeOlder(t.dir, t.pattern, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// removeOlder removes the entries of dir that match pattern and were last
// changed before cutoff. A missing dir has none.
func removeOlder(dir, pattern string, cutoff time.Time) error {
	names, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return fmt.Errorf("list %s: %w", pattern, err)
	}
	for _, name := range names {
		info, err := os.Lstat(name)
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		slog.Warn("removing a stale temporary entry", "name", filepath.Base(name))
		if err := os.RemoveAll(name); err != nil {
			return fmt.Errorf("remove stale entry: %w", err)
		}
	}
	return nil
}

// ReadSettings reads the settings.yml of the recipe in dir.
func ReadSettings(dir string) (Settings, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Settings{}, fmt.Errorf("open recipe: %w", err)
	}
	defer root.Close()
	f, err := root.Open("settings.yml")
	if err != nil {
		return Settings{}, fmt.Errorf("open settings.yml: %w", err)
	}
	defer f.Close()
	var s Settings
	if err := yaml.NewDecoder(io.LimitReader(f, MaxRecipeBytes)).Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return Settings{}, errkind.Wrap(errkind.ErrInvalid, "the recipe's settings.yml is empty", err)
		}
		return Settings{}, errkind.Wrap(errkind.ErrInvalid, "the recipe's settings.yml is not valid YAML", err)
	}
	return s, nil
}

// copyRecipe copies the recipe's files into dst. from reads them from the
// source, a directory or a zip, into the copier it is given.
func copyRecipe(from func(*copier) error, dst string) error {
	out, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("open install dir: %w", err)
	}
	defer out.Close()
	c := &copier{out: out}
	if err := from(c); err != nil {
		return err
	}
	for _, name := range []string{"settings.yml", "full.liquid"} {
		if !c.seen[name] {
			return errkind.New(errkind.ErrInvalid, "the recipe has no "+name)
		}
	}
	return nil
}

// copier writes a recipe's files into out, within the size limits.
type copier struct {
	out   *os.Root
	bytes int64
	seen  map[string]bool
}

// fromDir copies the recipe in dir, or in dir/src.
func (c *copier) fromDir(dir string) error {
	in, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open recipe: %w", err)
	}
	defer in.Close()
	top := "."
	if _, err := in.Stat("settings.yml"); err != nil {
		top = "src"
		if _, err := in.Stat("src/settings.yml"); err != nil {
			return errkind.New(errkind.ErrInvalid, fmt.Sprintf("there is no settings.yml in %s or its src directory", dir))
		}
	}
	return fs.WalkDir(in.FS(), top, func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == top {
			return err
		}
		rel, ok := relative(top, name)
		if !ok {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := in.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		return c.add(rel, f)
	})
}

// fromZip copies the recipe in the zip file that r holds in its first size
// bytes: the files under the directory holding its shallowest settings.yml.
// Whatever reading the zip reports is about its contents, so it is invalid
// input. That is not only zip.ErrFormat: a directory that runs off the end
// of the file is a bare io.ErrUnexpectedEOF.
func (c *copier) fromZip(r io.ReaderAt, size int64) error {
	if size == 0 {
		return errkind.New(errkind.ErrInvalid, "the file is empty")
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return errkind.Wrap(errkind.ErrInvalid, "the file is not a zip archive", err)
	}
	top, depth := "", -1
	for _, f := range zr.File {
		name := path.Clean(f.Name)
		if !fs.ValidPath(name) {
			return errkind.New(errkind.ErrInvalid, fmt.Sprintf("the zip has a file name that leaves its directory: %q", f.Name))
		}
		if path.Base(name) != "settings.yml" {
			continue
		}
		dir := path.Dir(name)
		d := 0
		if dir != "." {
			d = strings.Count(dir, "/") + 1
		}
		if depth < 0 || d < depth {
			top, depth = dir, d
		}
	}
	if depth < 0 {
		return errkind.New(errkind.ErrInvalid, "the zip has no settings.yml at its top, in src/, or in one top-level folder")
	}
	for _, f := range zr.File {
		rel, ok := relative(top, path.Clean(f.Name))
		if !ok || !f.Mode().IsRegular() {
			continue
		}
		if err := c.addZipFile(rel, f); err != nil {
			return err
		}
	}
	return nil
}

func (c *copier) addZipFile(name string, f *zip.File) error {
	r, err := f.Open()
	if err != nil {
		return damaged(name, err)
	}
	defer r.Close()
	return c.add(name, invalidReader{r, name})
}

// damaged is the error for a zip member that cannot be read. It is invalid
// input: the zip is bad, whether from a failed checksum or a broken stream.
func damaged(name string, cause error) error {
	return errkind.Wrap(errkind.ErrInvalid, fmt.Sprintf("%s in the zip is damaged", name), cause)
}

// invalidReader marks the errors of reading the zip member name as invalid
// input: a bad checksum or a damaged stream is the upload's fault. Errors
// writing the file are not marked, since a full disk is the server's.
type invalidReader struct {
	r    io.Reader
	name string
}

func (r invalidReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = damaged(r.name, err)
	}
	return n, err
}

// relative returns name relative to top, and false for a name outside top
// or inside a hidden file or directory, such as .git or a macOS ._ file.
func relative(top, name string) (string, bool) {
	rel := name
	if top != "." {
		var ok bool
		if rel, ok = strings.CutPrefix(name, top+"/"); !ok {
			return "", false
		}
	}
	if rel == "." || rel == "" {
		return "", false
	}
	for part := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	return rel, true
}

// add copies one file into the recipe. os.Root refuses a name that would
// leave the install directory.
func (c *copier) add(name string, r io.Reader) error {
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	if len(c.seen) >= maxRecipeFiles {
		return errkind.New(errkind.ErrInvalid, fmt.Sprintf("the recipe has more than %d files", maxRecipeFiles))
	}
	c.seen[name] = true
	// A zip can name a file twice, or a path as both a file and a
	// directory. That is the recipe's fault, not the server's.
	if err := c.out.MkdirAll(path.Dir(name), 0o750); err != nil {
		return openError(name, err)
	}
	w, err := c.out.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return openError(name, err)
	}
	n, err := io.Copy(w, io.LimitReader(r, MaxRecipeBytes-c.bytes+1))
	c.bytes += n
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, errkind.ErrInvalid) {
		// Reading the file failed, which says why in its own words.
		return err
	}
	if err != nil {
		return fmt.Errorf("recipe file %s: %w", name, err)
	}
	if c.bytes > MaxRecipeBytes {
		return errkind.New(errkind.ErrInvalid, fmt.Sprintf("the recipe's files are over %d MB in all", MaxRecipeBytes>>20))
	}
	return nil
}

// openError is the error for a recipe file that could not be created. A file
// that exists already is invalid input.
func openError(name string, err error) error {
	if errors.Is(err, fs.ErrExist) {
		return errkind.New(errkind.ErrInvalid, fmt.Sprintf("%s clashes with another file of the same name in the recipe", name))
	}
	return fmt.Errorf("recipe file %s: %w", name, err)
}

// Project is what a build of a plugin instance sends: the recipe's files
// under src/, and a .trmnlp.yml with the instance's settings.
type Project struct {
	dir    string
	config trmnlpConfig
	// Env holds the field values, which the .trmnlp.yml names as
	// {{ env.FIELD_n }}. It goes in the build job, so no file holds a
	// secret.
	Env map[string]string
}

// trmnlpConfig is the .trmnlp.yml of a build.
type trmnlpConfig struct {
	TimeZone     string            `yaml:"time_zone,omitempty"`
	CustomFields map[string]string `yaml:"custom_fields,omitempty"`
	// FrameworkAssetHost and AssetOverrides send the framework's assets,
	// and those of the other cached hosts, over plain HTTP, so the proxy
	// can cache them.
	FrameworkAssetHost string            `yaml:"framework_asset_host,omitempty"`
	AssetOverrides     map[string]string `yaml:"asset_overrides,omitempty"`
	// Variables overrides the trmnl variables a recipe sees. trmnlp does
	// not run it through Liquid, so a value in it is only data.
	Variables *variables `yaml:"variables,omitempty"`
}

// variables is .trmnlp.yml's variables, which trmnlp merges over a
// template's data. It has the server's trmnl values, and the instance's
// pushed data at the top level next to them.
type variables struct {
	Trmnl *trmnlVariables `yaml:"trmnl,omitempty"`
	// Data is what a script pushed to the instance. Its shape is the
	// script's, so it is a map. It never has a trmnl key.
	Data map[string]any `yaml:",inline"`
}

// trmnlVariables is the part of the trmnl variables the server sets.
type trmnlVariables struct {
	Device Device `yaml:"device"`
}

// Device is the trmnl.device a per-device build shows the recipe: the
// fields TRMNL gives its recipes that the server knows, from the device's
// last wake. It never holds the MAC or the api key, since a recipe may send
// what it sees to the internet. A field the server does not know is left
// out, so trmnlp's own value for it shows.
type Device struct {
	FriendlyID string `yaml:"friendly_id"`
	// PercentCharged is the battery charge, 0 to 100, estimated from the
	// reported voltage.
	PercentCharged *float64 `yaml:"percent_charged,omitempty"`
	// WifiStrength is the signal quality, 0 to 100, estimated from the
	// reported RSSI.
	WifiStrength           *int   `yaml:"wifi_strength,omitempty"`
	FirmwareVersion        string `yaml:"firmware_version,omitempty"`
	RefreshIntervalSeconds int    `yaml:"refresh_interval_seconds,omitempty"`
}

// Battery voltages that count as empty and full, and RSSIs that count as no
// signal and a full one, for estimating a device's percentages. They are
// approximations: the firmware reports raw readings, not what TRMNL shows.
const (
	emptyVolts, fullVolts = 3.0, 4.2
	poorRSSI, goodRSSI    = -100, -50
)

// NewDevice returns the trmnl.device values for a device row.
func NewDevice(row *db.GetRenderDeviceRow) Device {
	d := Device{FriendlyID: row.FriendlyID, FirmwareVersion: row.FwVersion}
	if row.BatteryV != nil {
		pct := min(max((*row.BatteryV-emptyVolts)/(fullVolts-emptyVolts)*100, 0), 100)
		d.PercentCharged = &pct
	}
	if row.Rssi != nil {
		strength := int(min(max((*row.Rssi-poorRSSI)*100/(goodRSSI-poorRSSI), 0), 100))
		d.WifiStrength = &strength
	}
	if row.LastRefreshRateSent != nil {
		d.RefreshIntervalSeconds = int(*row.LastRefreshRateSent)
	}
	return d
}

// WithDevice returns the project with trmnl.device set to d.
func (p Project) WithDevice(d Device) Project {
	v := p.config.Variables.clone()
	v.Trmnl = &trmnlVariables{Device: d}
	p.config.Variables = v
	return p
}

// clone returns a copy of v, which may be nil, that a caller can set fields
// of without changing v. The copy is shallow: a caller replaces Trmnl and
// Data, and does not change what they point to.
func (v *variables) clone() *variables {
	if v == nil {
		return &variables{}
	}
	c := *v
	return &c
}

// deviceMarker is what a recipe's files contain when it reads the device's
// variables, in markup, a polling URL or a transform.
var deviceMarker = []byte("trmnl.device")

// UsesDevice reports whether any file of the recipe in dir mentions
// trmnl.device, so it needs a build per device. It streams each file.
func UsesDevice(dir string) (bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, fmt.Errorf("open recipe: %w", err)
	}
	defer root.Close()
	found := false
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || found || !d.Type().IsRegular() {
			return err
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		found, err = contains(f, deviceMarker)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("scan recipe: %w", err)
	}
	return found, nil
}

// contains reports whether r holds needle, reading it in chunks that overlap
// by one byte less than needle, so a match across a chunk edge is found.
func contains(r io.Reader, needle []byte) (bool, error) {
	buf := make([]byte, 32<<10)
	keep := 0
	for {
		n, err := r.Read(buf[keep:])
		if bytes.Contains(buf[:keep+n], needle) {
			return true, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		end := keep + n
		keep = min(len(needle)-1, end)
		copy(buf, buf[end-keep:end])
	}
}

// frameworkHost is the host the TRMNL framework's assets come from.
const frameworkHost = "trmnl.com"

// NewProject returns the project for an instance of the recipe in dir, with
// the given field values and time zone. cachedHosts are the hosts whose
// assets the proxy caches.
//
// Every field value, secret or not, travels in Env. trmnlp renders each
// custom_fields value as Liquid, so a value placed in the file would have
// any {{ or {% in it run as Liquid; a value from the environment is not.
func NewProject(dir string, fields map[string]string, timeZone string, cachedHosts []string) Project {
	p := Project{dir: dir, config: trmnlpConfig{TimeZone: timeZone}}
	keys := slices.Sorted(maps.Keys(fields))
	if len(keys) > 0 {
		p.Env = make(map[string]string, len(keys))
		p.config.CustomFields = make(map[string]string, len(keys))
	}
	for i, k := range keys {
		name := "FIELD_" + strconv.Itoa(i)
		p.Env[name] = fields[k]
		p.config.CustomFields[k] = "{{ env." + name + " }}"
	}
	if len(cachedHosts) > 0 {
		p.config.AssetOverrides = make(map[string]string, len(cachedHosts))
	}
	for _, h := range cachedHosts {
		p.config.AssetOverrides["https://"+h+"/"] = "http://" + h + "/"
		if h == frameworkHost {
			p.config.FrameworkAssetHost = "http://" + h
		}
	}
	return p
}

// Write streams the project into tw.
func (p Project) Write(tw *tar.Writer) error {
	if err := render.AddDir(tw, p.dir, "src"); err != nil {
		return err
	}
	// The config is a few hundred bytes, encoded to learn its size.
	var buf bytes.Buffer
	if err := yaml.NewEncoder(&buf).Encode(p.config); err != nil {
		return fmt.Errorf("encode .trmnlp.yml: %w", err)
	}
	return render.AddFile(tw, ".trmnlp.yml", int64(buf.Len()), &buf)
}

// InstanceParams are the settings of a new plugin instance.
type InstanceParams struct {
	RecipeID int64
	Name     string
	// Palette is the palette the instance asks for, or "" for the
	// device's own.
	Palette string
	// Fields are custom field values by keyname, stored as they are.
	Fields map[string]string
	// Secrets are custom field values stored sealed with the secrets key,
	// and never shown back.
	Secrets map[string]string
}

// MinSecretLen is the fewest characters a secret field value may have. A
// shorter value is easy to guess, so sealing it protects nothing, and
// redacting it from a build log would hide every place its characters
// appear. Such a value can be a plain field.
const MinSecretLen = 6

// CreateInstance records a new plugin instance. Secret values need key. An
// empty Name is the recipe's. It returns ErrNoRecipe for an unknown recipe,
// and an error wrapping errkind.ErrInvalid for an unknown palette, a field
// given both as a field and as a secret, or a secret shorter than
// MinSecretLen.
func CreateInstance(ctx context.Context, client *db.Client, key *secrets.Key, p InstanceParams, now time.Time) (db.PluginInstance, error) {
	if p.Palette != "" && !profile.KnownPalette(p.Palette) {
		return db.PluginInstance{}, fmt.Errorf("%w: unknown palette %q", errkind.ErrInvalid, p.Palette)
	}
	for k, v := range p.Secrets {
		if _, ok := p.Fields[k]; ok {
			return db.PluginInstance{}, fmt.Errorf("%w: field %q is given as both a field and a secret", errkind.ErrInvalid, k)
		}
		if utf8.RuneCountInString(v) < MinSecretLen {
			return db.PluginInstance{}, fmt.Errorf("%w: secret %q is shorter than %d characters; give it as a plain field", errkind.ErrInvalid, k, MinSecretLen)
		}
	}
	if len(p.Secrets) > 0 && key == nil {
		return db.PluginInstance{}, secrets.ErrNoKey
	}
	rec, err := client.Read.GetRecipe(ctx, p.RecipeID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PluginInstance{}, fmt.Errorf("%w: %d", ErrNoRecipe, p.RecipeID)
	}
	if err != nil {
		return db.PluginInstance{}, fmt.Errorf("get recipe: %w", err)
	}
	if p.Name == "" {
		p.Name = rec.Name
	}
	if p.Fields == nil {
		p.Fields = map[string]string{}
	}
	fields, err := json.Marshal(p.Fields)
	if err != nil {
		return db.PluginInstance{}, fmt.Errorf("encode fields: %w", err)
	}
	var plain []byte
	if len(p.Secrets) > 0 {
		if plain, err = json.Marshal(p.Secrets); err != nil {
			return db.PluginInstance{}, fmt.Errorf("encode secrets: %w", err)
		}
	}
	var inst db.PluginInstance
	err = client.InTx(ctx, func(q *db.Queries) error {
		var err error
		inst, err = q.CreateInstance(ctx, db.CreateInstanceParams{
			RecipeID:    p.RecipeID,
			Name:        p.Name,
			FieldValues: string(fields),
			Palette:     p.Palette,
			CreatedAt:   now.Unix(),
		})
		if err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
		if plain == nil {
			return nil
		}
		// The sealed value is bound to the instance ID, so it can be made
		// only once the row has one.
		if inst.SecretValues, err = key.Seal(plain, secretAD(inst.ID)); err != nil {
			return fmt.Errorf("seal secrets: %w", err)
		}
		if err := q.SetInstanceSecrets(ctx, db.SetInstanceSecretsParams{SecretValues: inst.SecretValues, ID: inst.ID}); err != nil {
			return fmt.Errorf("save secrets: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.PluginInstance{}, err
	}
	return inst, nil
}

// secretAD is the associated data an instance's secrets are sealed with, so
// a sealed value copied to another instance does not open.
func secretAD(id int64) []byte {
	return []byte("plugin_instance:" + strconv.FormatInt(id, 10))
}

// ErrInstanceInUse means an instance is in playlist items, which have to
// be removed before it can be.
var ErrInstanceInUse = errkind.New(errkind.ErrConflict, "instance is in a playlist")

// DeleteInstance removes an instance and its renders. It refuses while a
// playlist item shows the instance, with an error that wraps
// ErrInstanceInUse and names the items, since deleting the instance would
// silently delete them too.
func DeleteInstance(ctx context.Context, client *db.Client, id int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		items, err := q.ListInstanceItems(ctx, id)
		if err != nil {
			return fmt.Errorf("list items: %w", err)
		}
		if len(items) > 0 {
			ids := make([]string, len(items))
			for i, item := range items {
				ids[i] = strconv.FormatInt(item, 10)
			}
			return fmt.Errorf("%w, in item IDs %s; take them out with playlist remove first", ErrInstanceInUse, strings.Join(ids, ", "))
		}
		n, err := q.DeleteInstance(ctx, id)
		if err != nil {
			return fmt.Errorf("delete instance: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: %d", ErrNoInstance, id)
		}
		return nil
	})
}

// InstanceFields returns an instance's field values, secret ones included,
// which need key. secretValues is the value of each secret field, for a
// caller that must keep them out of what it stores or shows.
func InstanceFields(inst *db.PluginInstance, key *secrets.Key) (fields map[string]string, secretValues []string, err error) {
	fields, secret, err := splitFields(inst, key)
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(fields, secret)
	return fields, slices.Collect(maps.Values(secret)), nil
}

// PublicFields returns an instance's field values that are not secret, and
// the sorted names of the secret fields that are set, without their values.
// The names are an empty, non-nil slice when none are set. Reading them
// needs key. When the sealed secrets cannot be opened, because there is no
// key, the key changed, or they were sealed before they were bound to the
// instance's ID, the names are nil, which means unknown, and the instance
// is logged by ID. One unreadable instance then does not fail a list.
func PublicFields(inst *db.PluginInstance, key *secrets.Key) (fields map[string]string, secretNames []string, err error) {
	fields, secret, err := splitFields(inst, key)
	if errors.Is(err, errUnreadable) {
		if !errors.Is(err, secrets.ErrNoKey) {
			// The error is not logged: it comes from the sealed value.
			slog.Warn("instance secrets cannot be opened", "instance", inst.ID)
		}
		return fields, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return fields, append([]string{}, slices.Sorted(maps.Keys(secret))...), nil
}

// errUnreadable marks an error from opening an instance's sealed secrets. Its
// text is the start of the message the render path shows.
var errUnreadable = errors.New("secret fields")

// splitFields returns an instance's plain and secret field values. An
// error that wraps errUnreadable means the sealed secrets did not open.
func splitFields(inst *db.PluginInstance, key *secrets.Key) (fields, secret map[string]string, err error) {
	fields = map[string]string{}
	if err := json.Unmarshal([]byte(inst.FieldValues), &fields); err != nil {
		return nil, nil, fmt.Errorf("decode fields: %w", err)
	}
	if len(inst.SecretValues) == 0 {
		return fields, nil, nil
	}
	plain, err := key.Open(inst.SecretValues, secretAD(inst.ID))
	if err != nil {
		return fields, nil, fmt.Errorf("%w: %w", errUnreadable, err)
	}
	if err := json.Unmarshal(plain, &secret); err != nil {
		return nil, nil, fmt.Errorf("decode secret fields: %w", err)
	}
	return fields, secret, nil
}

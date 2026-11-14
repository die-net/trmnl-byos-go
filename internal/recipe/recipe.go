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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// Limits on an installed recipe, which keep a build's project well under
// what the renderer accepts.
const (
	maxRecipeBytes = 8 << 20
	maxRecipeFiles = 1000
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
	if err := os.MkdirAll(recipesDir, 0o750); err != nil {
		return db.Recipe{}, fmt.Errorf("create recipes dir: %w", err)
	}
	tmp, err := os.MkdirTemp(recipesDir, ".install-")
	if err != nil {
		return db.Recipe{}, fmt.Errorf("create install dir: %w", err)
	}
	// Once renamed into place, tmp is gone and this does nothing.
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := copyRecipe(src, tmp); err != nil {
		return db.Recipe{}, err
	}
	settings, err := ReadSettings(tmp)
	if err != nil {
		return db.Recipe{}, err
	}
	if settings.Name == "" {
		settings.Name = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	}
	rec, err := client.Write.CreateRecipe(ctx, db.CreateRecipeParams{
		Name:            settings.Name,
		Source:          src,
		RefreshInterval: int64(settings.Refresh() / time.Second),
		InstalledAt:     now.Unix(),
	})
	if err != nil {
		return db.Recipe{}, fmt.Errorf("save recipe: %w", err)
	}
	if err := os.Rename(tmp, Dir(recipesDir, rec.ID)); err != nil {
		if derr := client.Write.DeleteRecipe(ctx, rec.ID); derr != nil {
			err = errors.Join(err, derr)
		}
		return db.Recipe{}, fmt.Errorf("install recipe: %w", err)
	}
	return rec, nil
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
	if err := yaml.NewDecoder(io.LimitReader(f, maxRecipeBytes)).Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("read settings.yml: %w", err)
	}
	return s, nil
}

// copyRecipe copies the recipe's files from src into dst.
func copyRecipe(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("recipe source: %w", err)
	}
	out, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("open install dir: %w", err)
	}
	defer out.Close()
	c := &copier{out: out}
	if info.IsDir() {
		err = c.fromDir(src)
	} else {
		err = c.fromZip(src)
	}
	if err != nil {
		return err
	}
	if !c.seen["settings.yml"] || !c.seen["full.liquid"] {
		return errors.New("recipe needs settings.yml and full.liquid")
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
			return fmt.Errorf("no settings.yml in %s or its src directory", dir)
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

// fromZip copies the recipe in a zip file: the files under the directory
// holding its shallowest settings.yml.
func (c *copier) fromZip(file string) error {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return fmt.Errorf("open recipe zip: %w", err)
	}
	defer zr.Close()
	top, depth := "", -1
	for _, f := range zr.File {
		name := path.Clean(f.Name)
		if !fs.ValidPath(name) {
			return fmt.Errorf("recipe zip has an unsafe name %q", f.Name)
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
		return errors.New("recipe zip has no settings.yml")
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
		return fmt.Errorf("recipe zip %s: %w", name, err)
	}
	defer r.Close()
	return c.add(name, r)
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
		return fmt.Errorf("recipe has over %d files", maxRecipeFiles)
	}
	c.seen[name] = true
	if err := c.out.MkdirAll(path.Dir(name), 0o750); err != nil {
		return fmt.Errorf("recipe file %s: %w", name, err)
	}
	w, err := c.out.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return fmt.Errorf("recipe file %s: %w", name, err)
	}
	n, err := io.Copy(w, io.LimitReader(r, maxRecipeBytes-c.bytes+1))
	c.bytes += n
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("recipe file %s: %w", name, err)
	}
	if c.bytes > maxRecipeBytes {
		return fmt.Errorf("recipe is over %d bytes", maxRecipeBytes)
	}
	return nil
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

// CreateInstance records a new plugin instance. Secret values need key.
func CreateInstance(ctx context.Context, client *db.Client, key *secrets.Key, p InstanceParams, now time.Time) (db.PluginInstance, error) {
	if p.Fields == nil {
		p.Fields = map[string]string{}
	}
	fields, err := json.Marshal(p.Fields)
	if err != nil {
		return db.PluginInstance{}, fmt.Errorf("encode fields: %w", err)
	}
	var sealed []byte
	if len(p.Secrets) > 0 {
		plain, err := json.Marshal(p.Secrets)
		if err != nil {
			return db.PluginInstance{}, fmt.Errorf("encode secrets: %w", err)
		}
		if sealed, err = key.Seal(plain); err != nil {
			return db.PluginInstance{}, fmt.Errorf("seal secrets: %w", err)
		}
	}
	inst, err := client.Write.CreateInstance(ctx, db.CreateInstanceParams{
		RecipeID:     p.RecipeID,
		Name:         p.Name,
		FieldValues:  string(fields),
		SecretValues: sealed,
		Palette:      p.Palette,
		CreatedAt:    now.Unix(),
	})
	if err != nil {
		return db.PluginInstance{}, fmt.Errorf("save instance: %w", err)
	}
	return inst, nil
}

// InstanceFields returns an instance's field values, secret ones included,
// which need key.
func InstanceFields(inst *db.PluginInstance, key *secrets.Key) (map[string]string, error) {
	fields := map[string]string{}
	if err := json.Unmarshal([]byte(inst.FieldValues), &fields); err != nil {
		return nil, fmt.Errorf("decode fields: %w", err)
	}
	if len(inst.SecretValues) == 0 {
		return fields, nil
	}
	plain, err := key.Open(inst.SecretValues)
	if err != nil {
		return nil, fmt.Errorf("secret fields: %w", err)
	}
	var secret map[string]string
	if err := json.Unmarshal(plain, &secret); err != nil {
		return nil, fmt.Errorf("decode secret fields: %w", err)
	}
	maps.Copy(fields, secret)
	return fields, nil
}

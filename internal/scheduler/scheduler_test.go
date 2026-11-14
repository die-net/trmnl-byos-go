package scheduler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// fakeTrmnlp stands in for trmnlp behind the real shim. Each "PNG" names
// its layout, size, palette and first field value. It fails while
// FAIL_FILE exists, and fails if a secret reached the project's files.
const fakeTrmnlp = `#!/bin/sh
while [ $# -gt 0 ]; do
  case $1 in
    -d) dir=$2; shift ;;
    --width) w=$2; shift ;;
    --height) h=$2; shift ;;
    --palette) p=$2; shift ;;
  esac
  shift
done
test -f "$FAIL_FILE" && { echo "starting"; echo "data source said 401" >&2; exit 3; }
grep -rq hunter2 "$dir" && { echo "a secret is in the project" >&2; exit 4; }
mkdir -p "$dir/_build"
for v in full half_horizontal half_vertical quadrant; do
  printf '%s %sx%s %s %s' "$v" "$w" "$h" "$p" "$FIELD_0" > "$dir/_build/$v.png"
done
`

type harness struct {
	s        *Scheduler
	db       *db.Client
	images   *imagestore.Store
	builds   atomic.Int32
	failFile string
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{failFile: filepath.Join(dir, "fail"), now: time.Unix(1_800_000_000, 0)}
	script := filepath.Join(dir, "trmnlp")
	if err := os.WriteFile(script, []byte(fakeTrmnlp), 0o700); err != nil { //nolint:gosec // G306: the fake command must be executable.
		t.Fatal(err)
	}
	shim := render.NewShim(script, []string{"PATH=" + os.Getenv("PATH"), "FAIL_FILE=" + h.failFile}, 10*time.Second, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.builds.Add(1)
		shim.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	var err error
	if h.db, err = db.Open(t.Context(), filepath.Join(dir, "trmnl.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.db.Close() })
	if h.images, err = imagestore.New(filepath.Join(dir, "images")); err != nil {
		t.Fatal(err)
	}
	key, err := secrets.Parse("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if err != nil {
		t.Fatal(err)
	}
	h.s = New(Config{
		DB:         h.db,
		Images:     h.images,
		Renderer:   &render.Client{URL: srv.URL, HTTP: srv.Client()},
		RecipesDir: filepath.Join(dir, "recipes"),
		Secrets:    key,
		AssetHosts: []string{"trmnl.com"},
	})
	h.s.now = func() time.Time { return h.now }

	// A device on a playlist showing an instance of an hourly recipe, with a
	// secret field.
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"settings.yml": "name: Hourly\nrefresh_interval: 60\n", "full.liquid": "x"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := recipe.Install(t.Context(), h.db, h.s.cfg.RecipesDir, src, h.now)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := recipe.CreateInstance(t.Context(), h.db, key, recipe.InstanceParams{
		RecipeID: rec.ID, Name: "hourly", Secrets: map[string]string{"token": "hunter2"},
	}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := h.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "D8:3B:DA:12:34:56", ApiKeyHash: []byte{1}, FriendlyID: "ABC123", FwModel: "xiao_epaper_display", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := playlist.AddItem(t.Context(), h.db, "p", dev.ID, inst.ID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	return h
}

// render returns the one render row, for the OG's profile.
func (h *harness) render(t *testing.T) db.Render {
	t.Helper()
	p, err := profile.Resolve("xiao_epaper_display", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.db.Read.GetRender(t.Context(), db.GetRenderParams{
		InstanceID: 1, Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) renderDue(t *testing.T, wantBuilds int32) {
	t.Helper()
	if err := h.s.RenderDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := h.builds.Load(); n != wantBuilds {
		t.Fatalf("%d builds, want %d", n, wantBuilds)
	}
}

func TestRenderDue(t *testing.T) {
	h := newHarness(t)
	start := h.now

	h.renderDue(t, 1)
	r := h.render(t)
	if r.Error != "" || r.RenderedAt != start.Unix() {
		t.Fatalf("render %+v", r)
	}
	names, err := h.db.Read.ListReferencedImages(t.Context())
	if err != nil || len(names) != len(render.Layouts) {
		t.Fatalf("referenced images %v, %v", names, err)
	}
	// The secret reached the build through its environment.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/images/"+names[0]+".png", http.NoBody)
	req.SetPathValue("name", names[0]+".png")
	rec := httptest.NewRecorder()
	h.images.ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.HasSuffix(body, " hunter2") || !strings.Contains(body, "800x480") {
		t.Errorf("image %q", body)
	}

	h.now = start.Add(59 * time.Minute)
	h.renderDue(t, 1) // Not due before its hour.
	h.now = start.Add(time.Hour)
	h.renderDue(t, 2)
	good := h.render(t)

	// A failed build keeps the last good images, and is retried after
	// RetryAfter, not on every check.
	if err := os.WriteFile(h.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.now = start.Add(2 * time.Hour)
	h.renderDue(t, 3)
	r = h.render(t)
	if r.Error != "trmnlp: data source said 401" || r.RenderedAt != good.RenderedAt || r.AttemptedAt != h.now.Unix() {
		t.Fatalf("after a failure: %+v", r)
	}
	again, err := h.db.Read.ListReferencedImages(t.Context())
	if err != nil || !slices.Equal(again, names) {
		t.Errorf("images after a failure: %v, %v; want %v", again, err, names)
	}
	h.now = h.now.Add(RetryAfter - time.Second)
	h.renderDue(t, 3)
	h.now = h.now.Add(time.Second)
	h.renderDue(t, 4)

	if err := os.Remove(h.failFile); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(RetryAfter)
	h.renderDue(t, 5)
	if r := h.render(t); r.Error != "" || r.RenderedAt != h.now.Unix() {
		t.Fatalf("after recovering: %+v", r)
	}
}

func TestDescribe(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		log  string
		want string
	}{
		{"trmnlp failed", render.ErrBuildFailed, "starting\nboom\n\n", "trmnlp: boom"},
		{"no log", render.ErrBuildFailed, "", render.ErrBuildFailed.Error()},
		{"other", os.ErrDeadlineExceeded, "x", os.ErrDeadlineExceeded.Error()},
	} {
		if got := describe(tc.err, tc.log); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

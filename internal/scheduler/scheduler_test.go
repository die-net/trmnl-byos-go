package scheduler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// its layout, size, palette and first field value, and the friendly_id the
// project's trmnl.device has, if it has one, and the headline pushed to the
// instance, if there is one. It fails while
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
test -f "$LEAK_FILE" && { echo "fetching https://api.example/?key=$FIELD_0"; echo "GET https://api.example/?key=$FIELD_0 failed: 401" >&2; exit 3; }
test -f "$FAIL_FILE" && { echo "starting"; echo "data source said 401" >&2; exit 3; }
grep -rq hunter2 "$dir" && { echo "a secret is in the project" >&2; exit 4; }
dev=$(sed -n 's/^ *friendly_id: //p' "$dir/.trmnlp.yml")
hl=$(sed -n 's/^ *headline: //p' "$dir/.trmnlp.yml")
mkdir -p "$dir/_build"
for v in full half_horizontal half_vertical quadrant; do
  printf '%s%s %sx%s %s %s' "$v" "${dev:+@$dev}" "$w" "$h" "$p" "$FIELD_0${hl:+ hl=$hl}" > "$dir/_build/$v.png"
done
`

type harness struct {
	s        *Scheduler
	db       *db.Client
	images   *imagestore.Store
	builds   atomic.Int32
	failFile string
	// down, while set, makes the fake renderer answer 503 to every build.
	down atomic.Bool
	// leakFile, while it exists, makes the fake trmnlp fail with the
	// first field's value in its output.
	leakFile string
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{failFile: filepath.Join(dir, "fail"), leakFile: filepath.Join(dir, "leak"), now: time.Unix(1_800_000_000, 0)}
	script := filepath.Join(dir, "trmnlp")
	if err := os.WriteFile(script, []byte(fakeTrmnlp), 0o700); err != nil { //nolint:gosec // G306: the fake command must be executable.
		t.Fatal(err)
	}
	shim := render.NewShim(script, []string{"PATH=" + os.Getenv("PATH"), "FAIL_FILE=" + h.failFile, "LEAK_FILE=" + h.leakFile}, 10*time.Second, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.builds.Add(1)
		if h.down.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
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
	if _, _, err := playlist.AddItem(t.Context(), h.db, "p", dev.ID, inst.ID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	return h
}

// render returns the one render row, for the OG's profile.
func (h *harness) render(t *testing.T) db.ListRenderRowsRow {
	t.Helper()
	return h.renderOf(t, 1)
}

// renderOf returns the render row of instance, for the OG's profile.
func (h *harness) renderOf(t *testing.T, instance int64) db.ListRenderRowsRow {
	t.Helper()
	p := profile.Resolve("xiao_epaper_display", nil, nil)
	rows, err := h.db.Read.ListRenderRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.InstanceID == instance && r.Width == int64(p.Width) && r.Height == int64(p.Height) && r.Palette == p.RenderPalette("") {
			return r
		}
	}
	t.Fatalf("no render of instance %d in %+v", instance, rows)
	return db.ListRenderRowsRow{}
}

// drain builds one target per scan until nothing is due, as Run does.
func (h *harness) drain(t *testing.T) {
	t.Helper()
	for range 100 {
		more, err := h.s.RenderNext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("still building after 100 scans")
}

func (h *harness) renderDue(t *testing.T, wantBuilds int32) {
	t.Helper()
	h.drain(t)
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
	h.now = h.now.Add(db.RenderRetryAfter - time.Second)
	h.renderDue(t, 3)
	h.now = h.now.Add(time.Second)
	h.renderDue(t, 4)
	if got := h.render(t).Failures; got != 2 {
		t.Fatalf("failures = %d, want 2", got)
	}
	// A second failure in a row waits twice as long.
	h.now = h.now.Add(2*db.RenderRetryAfter - time.Second)
	h.renderDue(t, 4)

	if err := os.Remove(h.failFile); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Second)
	h.renderDue(t, 5)
	if r := h.render(t); r.Error != "" || r.RenderedAt != h.now.Unix() || r.Failures != 0 {
		t.Fatalf("after recovering: %+v", r)
	}
}

// installRecipe installs a recipe whose full.liquid is body and returns an
// instance of it.
func (h *harness) installRecipe(t *testing.T, body string) db.PluginInstance {
	t.Helper()
	src := t.TempDir()
	for name, content := range map[string]string{"settings.yml": "name: R\nrefresh_interval: 60\n", "full.liquid": body} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := recipe.Install(t.Context(), h.db, h.s.cfg.RecipesDir, src, h.now)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := recipe.CreateInstance(t.Context(), h.db, nil, recipe.InstanceParams{RecipeID: rec.ID, Name: "i"}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// screenBody returns the "PNG" the device gets for an instance.
func (h *harness) screenBody(t *testing.T, device, instance int64) string {
	t.Helper()
	p := profile.Resolve("xiao_epaper_display", nil, nil)
	row, err := h.db.Read.GetItemScreen(t.Context(), db.GetItemScreenParams{
		Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""), Layout: "full",
		DeviceID: device, InstanceID: instance,
	})
	if err != nil || row.Image == nil {
		t.Fatalf("screen for device %d: %+v, %v", device, row, err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/images/"+*row.Image+".png", http.NoBody)
	req.SetPathValue("name", *row.Image+".png")
	rec := httptest.NewRecorder()
	h.images.ServeHTTP(rec, req)
	return rec.Body.String()
}

// TestRenderPerDevice checks that a recipe reading trmnl.device is built
// once per device, each time with that device's values, while a recipe that
// does not is still built once for both.
func TestRenderPerDevice(t *testing.T) {
	h := newHarness(t)
	shared := int64(1) // The harness's instance of a recipe that ignores the device.
	perDevice := h.installRecipe(t, "<p>{{ trmnl.device.friendly_id }}</p>")
	second, err := h.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "D8:3B:DA:12:34:57", ApiKeyHash: []byte{2}, FriendlyID: "ABC124", FwModel: "xiao_epaper_display", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range []int64{1, second.ID} {
		for _, instance := range []int64{shared, perDevice.ID} {
			if _, _, err := playlist.AddItem(t.Context(), h.db, "p"+strconv.FormatInt(device, 10), device, instance, 15*time.Minute); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The second device's last wake reported a battery and a signal.
	err = h.db.Write.UpsertDeviceState(t.Context(), db.UpsertDeviceStateParams{
		DeviceID: second.ID, LastSeen: 1, BatteryV: new(3.6), Rssi: new(int64(-75)), LastRefreshRateSent: 900,
	})
	if err != nil {
		t.Fatal(err)
	}

	h.renderDue(t, 3)
	for device, want := range map[int64]string{1: "full@ABC123 ", second.ID: "full@ABC124 "} {
		if got := h.screenBody(t, device, perDevice.ID); !strings.HasPrefix(got, want) {
			t.Errorf("device %d got %q, want it to start %q", device, got, want)
		}
		if got := h.screenBody(t, device, shared); !strings.HasPrefix(got, "full 800x480") {
			t.Errorf("device %d got %q from the shared recipe, which should not see the device", device, got)
		}
	}
	h.renderDue(t, 3) // Nothing is due yet.

	// A device on the playlist later gets its own build, and the shared one
	// is left alone.
	third, err := h.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "D8:3B:DA:12:34:58", ApiKeyHash: []byte{3}, FriendlyID: "ABC125", FwModel: "xiao_epaper_display", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range []int64{shared, perDevice.ID} {
		if _, _, err := playlist.AddItem(t.Context(), h.db, "p3", third.ID, instance, 15*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	h.renderDue(t, 4)
	if got := h.screenBody(t, third.ID, perDevice.ID); !strings.HasPrefix(got, "full@ABC125 ") {
		t.Errorf("third device got %q", got)
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

func TestDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tgt := target{refresh: time.Hour}
	// A default lead is two 30 s builds (the target's, and one already running),
	// 30 s of wake slack and 30 s of check interval.
	const lead = 120 * time.Second
	good := func(age time.Duration) *db.Render {
		return &db.Render{RenderedAt: now.Add(-age).Unix(), AttemptedAt: now.Add(-age).Unix()}
	}
	// failedTimes is a render whose last n builds in a row failed, the
	// latest since the given time ago. failed is the first failure.
	failedTimes := func(since time.Duration, n int64) *db.Render {
		return &db.Render{RenderedAt: now.Add(-3 * time.Hour).Unix(), AttemptedAt: now.Add(-since).Unix(), Error: "boom", Failures: n}
	}
	failed := func(since time.Duration) *db.Render { return failedTimes(since, 1) }
	for _, tc := range []struct {
		name   string
		r      *db.Render
		wanted []time.Duration // Offsets from now.
		woken  bool
		// data is the updated_at of the instance's data, and built the
		// data_at its last render recorded. Both are 0 for none.
		data, built int64
		want        bool
		by          time.Duration // Offset of the expected sort key, if want and it is wanted.
	}{
		{name: "never rendered", r: nil, woken: true, want: true},
		{name: "never rendered and wanted", r: nil, wanted: []time.Duration{time.Hour}, woken: true, want: true, by: time.Hour},
		{name: "failure inside RetryAfter", r: failed(db.RenderRetryAfter - time.Second), wanted: []time.Duration{0}, woken: true},
		{name: "second failure inside twice RetryAfter", r: failedTimes(2*db.RenderRetryAfter-time.Second, 2), woken: true},
		{name: "second failure past twice RetryAfter", r: failedTimes(2*db.RenderRetryAfter, 2), woken: true, want: true},
		{name: "fifth failure inside the cap", r: failedTimes(time.Hour-time.Second, 5), woken: true},
		{name: "fifth failure at the cap", r: failedTimes(time.Hour, 5), woken: true, want: true},
		{name: "many failures stay at the cap", r: failedTimes(time.Hour, 500), woken: true, want: true},
		{name: "failure past RetryAfter", r: failed(db.RenderRetryAfter), wanted: []time.Duration{time.Hour}, woken: true, want: true, by: time.Hour},
		{name: "fresh now, stale by a wake far away", r: good(10 * time.Minute), wanted: []time.Duration{55 * time.Minute}, woken: true},
		{name: "stale at a wake before the lead", r: good(59 * time.Minute), wanted: []time.Duration{lead + time.Second}, woken: true},
		{name: "stale at a wake at the lead", r: good(59 * time.Minute), wanted: []time.Duration{lead}, woken: true, want: true, by: lead},
		{name: "wake inside the lead, render still fresh at it", r: good(10 * time.Minute), wanted: []time.Duration{time.Minute}, woken: true},
		{name: "second wake counts", r: good(59 * time.Minute), wanted: []time.Duration{time.Hour, lead}, woken: true, want: true, by: lead},
		{name: "device is late and the render is old", r: good(61 * time.Minute), wanted: []time.Duration{-10 * time.Minute}, woken: true, want: true, by: -10 * time.Minute},
		{name: "device is late and the render is fresh", r: good(59 * time.Minute), wanted: []time.Duration{-10 * time.Minute}, woken: true},
		{name: "old but wake far away", r: good(90 * time.Minute), wanted: []time.Duration{time.Hour}, woken: true},
		{name: "twice the interval", r: good(2 * time.Hour), wanted: []time.Duration{24 * time.Hour}, woken: true, want: true},
		{name: "twice the interval, nobody wants it", r: good(2 * time.Hour), woken: true, want: true},
		{name: "not yet twice the interval, nobody wants it", r: good(2*time.Hour - time.Second), woken: true},
		{name: "no device has woken, interval not passed", r: good(59 * time.Minute)},
		{name: "no device has woken, interval passed", r: good(time.Hour), want: true},
		{name: "data newer than the last build read", r: good(10 * time.Minute), data: 6, built: 5, woken: true, want: true},
		{name: "data and nothing read", r: good(10 * time.Minute), data: 6, woken: true, want: true},
		{name: "data newer, and wanted", r: good(10 * time.Minute), data: 6, built: 5, wanted: []time.Duration{time.Hour}, woken: true, want: true, by: time.Hour},
		{name: "build read the latest data", r: good(10 * time.Minute), data: 5, built: 5, woken: true},
		{name: "build read newer data than it has", r: good(10 * time.Minute), data: 4, built: 5, woken: true},
		{name: "data newer after a failed build, inside RetryAfter", r: failed(db.RenderRetryAfter - time.Second), data: 6, built: 5, woken: true},
		{name: "data newer after a failed build, past RetryAfter", r: failed(db.RenderRetryAfter), data: 6, built: 5, woken: true, want: true},
		{name: "data and never rendered", r: nil, data: 6, woken: true, want: true},
		{name: "a device has woken but wants nothing", r: good(time.Hour), woken: true},
		{
			name: "a long build lengthens the lead", r: &db.Render{RenderedAt: now.Add(-59 * time.Minute).Unix(), DurationMs: 120_000},
			wanted: []time.Duration{3 * time.Minute}, woken: true, want: true, by: 3 * time.Minute,
		},
		{name: "the default lead is shorter", r: good(59 * time.Minute), wanted: []time.Duration{3 * time.Minute}, woken: true},
	} {
		wanted := make([]time.Time, 0, len(tc.wanted))
		for _, d := range tc.wanted {
			wanted = append(wanted, now.Add(d))
		}
		if tc.r != nil {
			r := *tc.r
			r.DataAt = tc.built
			tc.r = &r
		}
		got, by := due(tgt, tc.r, wanted, tc.woken, tc.data, now)
		if got != tc.want {
			t.Errorf("%s: due is %v, want %v", tc.name, got, tc.want)
		}
		wantBy := time.Time{}
		if tc.want && tc.by != 0 {
			wantBy = now.Add(tc.by)
		}
		if !by.Equal(wantBy) {
			t.Errorf("%s: by is %v, want %v", tc.name, by, wantBy)
		}
	}
}

// wake records that the device last woke at the harness's time and was told
// to sleep for rate.
func (h *harness) wake(t *testing.T, device int64, rate time.Duration) {
	t.Helper()
	err := h.db.Write.UpsertDeviceState(t.Context(), db.UpsertDeviceStateParams{
		DeviceID: device, LastSeen: h.now.Unix(), LastRefreshRateSent: int64(rate / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRenderAheadOfWake checks that a build of a render that is fresh now
// but will be stale at the device's next wake starts inside the lead before
// that wake, not when the refresh interval passes.
func TestRenderAheadOfWake(t *testing.T) {
	h := newHarness(t)
	start := h.now
	h.renderDue(t, 1)

	// The device wakes 10 minutes later and sleeps until 65 minutes after
	// the first build, when that render is over the hour.
	h.now = start.Add(10 * time.Minute)
	h.wake(t, 1, 55*time.Minute)

	h.now = start.Add(time.Hour)
	h.renderDue(t, 1) // The old rule would build here.
	h.now = start.Add(65*time.Minute - 121*time.Second)
	h.renderDue(t, 1) // Just before the lead of 120 s.
	h.now = start.Add(65*time.Minute - 120*time.Second)
	h.renderDue(t, 2)
	if r := h.render(t); r.RenderedAt != h.now.Unix() {
		t.Errorf("rendered at %d, want %d", r.RenderedAt, h.now.Unix())
	}

	// The render is fresh again, so the same wake does not build it again.
	h.now = start.Add(65 * time.Minute)
	h.renderDue(t, 2)

	// A device that never wakes again still gets a rebuild every interval,
	// since its wake time is long past.
	h.now = start.Add(65*time.Minute + time.Hour)
	h.renderDue(t, 3)
}

// TestRenderAheadOfWakeFailure checks that a failed build is retried only
// after RetryAfter even when a wake is near.
func TestRenderAheadOfWakeFailure(t *testing.T) {
	h := newHarness(t)
	start := h.now
	h.renderDue(t, 1)
	h.wake(t, 1, time.Hour+time.Minute)
	if err := os.WriteFile(h.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.now = start.Add(time.Hour)
	h.renderDue(t, 2)
	h.now = h.now.Add(db.RenderRetryAfter - time.Second)
	h.renderDue(t, 2)
	h.now = h.now.Add(time.Second)
	h.renderDue(t, 3)
}

// TestCandidatesWanted checks whose wakes want which target: a shared
// target is wanted by every device that will show it, and a per-device one
// only by its own device.
func TestCandidatesWanted(t *testing.T) {
	h := newHarness(t)
	shared := int64(1)
	perDevice := h.installRecipe(t, "<p>{{ trmnl.device.friendly_id }}</p>")
	second, err := h.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "D8:3B:DA:12:34:57", ApiKeyHash: []byte{2}, FriendlyID: "ABC124", FwModel: "xiao_epaper_display", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	third, err := h.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "D8:3B:DA:12:34:58", ApiKeyHash: []byte{3}, FriendlyID: "ABC125", FwModel: "xiao_epaper_display", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Devices 1 and 3 show playlist p, the shared instance. Device 2 shows
	// q2 first, which holds only the per-device one; device 1 is also on q1
	// with it, but p comes first.
	for _, device := range []int64{1, second.ID} {
		item, _, err := playlist.AddItem(t.Context(), h.db, "q"+strconv.FormatInt(device, 10), device, perDevice.ID, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if device == second.ID {
			err = h.db.Write.LinkPlaylistDevice(t.Context(), db.LinkPlaylistDeviceParams{DeviceID: device, PlaylistID: item.PlaylistID, Priority: -1})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := playlist.AddItem(t.Context(), h.db, "p", third.ID, shared, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	// They woke at the harness's time and will wake in 10, 20 and 30 minutes.
	h.wake(t, 1, 10*time.Minute)
	h.wake(t, second.ID, 20*time.Minute)
	h.wake(t, third.ID, 30*time.Minute)

	cands, err := h.s.candidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ instance, device int64 }
	got := map[key][]time.Time{}
	for _, c := range cands {
		if !c.woken {
			t.Errorf("%+v is not woken", c.target)
		}
		got[key{c.instance, c.device}] = c.wanted
	}
	want := map[key][]time.Time{
		{shared, 0}:               {h.now.Add(10 * time.Minute), h.now.Add(30 * time.Minute)},
		{perDevice.ID, second.ID}: {h.now.Add(20 * time.Minute)},
		{perDevice.ID, 1}:         nil,
	}
	if len(got) != len(want) {
		t.Errorf("targets %v, want %v", got, want)
	}
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("target %+v wanted at %v, want %v", k, got[k], w)
		}
	}
}

// TestDueTargetsOrder checks that targets wanted by a wake build first,
// soonest first, ahead of those due only because they are old.
func TestDueTargetsOrder(t *testing.T) {
	h := newHarness(t)
	start := h.now
	other := h.installRecipe(t, "y")
	if _, _, err := playlist.AddItem(t.Context(), h.db, "p", 0, other.ID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 2)

	// Instance 1 is old with no wake wanting it (the device shows the other
	// instance next), and the other is wanted by a wake soon.
	h.now = start.Add(30 * time.Minute)
	cur := db.SetDeviceCursorParams{DeviceID: 1}
	items, err := h.db.Read.ListDeviceItems(t.Context(), 1)
	if err != nil || len(items) != 2 {
		t.Fatalf("items %v, %v", items, err)
	}
	cur.ItemID, cur.ItemStartedAt = items[0].ID, h.now.Unix()
	if err := h.db.Write.SetDeviceCursor(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	h.wake(t, 1, 15*time.Minute) // The next wake shows the second item.
	h.now = start.Add(2 * time.Hour)
	got, err := h.s.dueTargets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	instances := make([]int64, 0, len(got))
	for _, g := range got {
		instances = append(instances, g.instance)
	}
	if want := []int64{other.ID, 1}; !slices.Equal(instances, want) {
		t.Errorf("due %v, want %v", instances, want)
	}
}

// push stores data for the harness's instance as a push at the harness's
// time would.
func (h *harness) push(t *testing.T, data string) {
	t.Helper()
	_, err := recipe.PushData(t.Context(), h.db, recipe.PushParams{
		InstanceID: 1, Add: mustData(t, data),
	}, h.now)
	if err != nil {
		t.Fatal(err)
	}
}

func mustData(t *testing.T, s string) recipe.Data {
	t.Helper()
	d, err := recipe.ParseData(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestRenderOnPush checks that pushed data reaches the build, and makes
// the target due at the next check although its interval has not passed. The
// clock never moves between a push and the build that follows, so a build
// that read the latest data must not be due again even in the same second.
func TestRenderOnPush(t *testing.T) {
	h := newHarness(t)
	h.renderDue(t, 1)
	if got := h.screenBody(t, 1, 1); strings.Contains(got, "hl=") {
		t.Fatalf("image before a push: %q", got)
	}
	if r := h.render(t); r.DataAt != 0 {
		t.Fatalf("data_at %d before a push", r.DataAt)
	}

	h.now = h.now.Add(time.Minute)
	h.renderDue(t, 1) // Nothing is due yet.
	h.push(t, `{"headline": "{{ env.FIELD_0 }}"}`)
	h.renderDue(t, 2)
	if got := h.screenBody(t, 1, 1); !strings.HasSuffix(got, ` hl='{{ env.FIELD_0 }}'`) {
		t.Errorf("image after a push: %q", got)
	}
	if r := h.render(t); r.DataAt != h.now.Unix() {
		t.Errorf("data_at %d, want %d", r.DataAt, h.now.Unix())
	}
	// The build read the latest data, so it is not due again, with the clock
	// frozen or moving.
	for range 3 {
		h.renderDue(t, 2)
	}
	h.now = h.now.Add(time.Minute)
	h.renderDue(t, 2)
}

// TestRenderOnPushSameSecond checks that a push in the same second as a
// build that already read the data is newer, and is built, and that the
// build after it is not repeated.
func TestRenderOnPushSameSecond(t *testing.T) {
	h := newHarness(t)
	h.renderDue(t, 1)
	h.now = h.now.Add(time.Minute)
	h.push(t, `{"headline": "one"}`)
	h.renderDue(t, 2)
	// Another push in the same second, after that build read the data.
	h.push(t, `{"headline": "two"}`)
	h.renderDue(t, 3)
	if got := h.screenBody(t, 1, 1); !strings.HasSuffix(got, " hl=two") {
		t.Errorf("image after the second push: %q", got)
	}
	h.renderDue(t, 3)
	// The stored time rose, so the second push is not the first's second.
	row, err := h.db.Read.GetInstanceData(t.Context(), 1)
	if err != nil || row.UpdatedAt != h.now.Unix()+1 {
		t.Errorf("updated_at %d, %v, want %d", row.UpdatedAt, err, h.now.Unix()+1)
	}
	if r := h.render(t); r.DataAt != row.UpdatedAt {
		t.Errorf("data_at %d, want %d", r.DataAt, row.UpdatedAt)
	}
}

// TestRenderOnPushAfterFailure checks that a failed build still waits
// RetryAfter when data is pushed after it.
func TestRenderOnPushAfterFailure(t *testing.T) {
	h := newHarness(t)
	h.renderDue(t, 1)
	if err := os.WriteFile(h.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	h.push(t, `{"headline": "one"}`)
	h.renderDue(t, 2) // The build fails.

	// Data that arrives after the failure waits for RetryAfter.
	h.now = h.now.Add(time.Minute)
	h.push(t, `{"headline": "two"}`)
	h.renderDue(t, 2)
	h.now = h.now.Add(db.RenderRetryAfter - time.Minute - time.Second)
	h.renderDue(t, 2)
	if err := os.Remove(h.failFile); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Second)
	h.renderDue(t, 3)
	if got := h.screenBody(t, 1, 1); !strings.HasSuffix(got, " hl=two") {
		t.Errorf("image after the retry: %q", got)
	}
	h.renderDue(t, 3)
}

// TestRenderMissingRecipeDir checks that a target whose recipe directory is
// gone records a failure the device can show, retries only after RetryAfter,
// and does not stop the other targets.
func TestRenderMissingRecipeDir(t *testing.T) {
	h := newHarness(t)
	other := h.installRecipe(t, "y")
	if _, _, err := playlist.AddItem(t.Context(), h.db, "p", 0, other.ID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(recipe.Dir(h.s.cfg.RecipesDir, 1)); err != nil {
		t.Fatal(err)
	}
	failure := func() db.GetItemScreenRow {
		t.Helper()
		p := profile.Resolve("xiao_epaper_display", nil, nil)
		row, err := h.db.Read.GetItemScreen(t.Context(), db.GetItemScreenParams{
			Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""), Layout: "full", DeviceID: 1, InstanceID: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	h.renderDue(t, 1) // Only the other recipe is built.
	row := failure()
	if row.Error == nil || !strings.Contains(*row.Error, "recipe 1") || row.Image != nil {
		t.Fatalf("missing directory: %+v", row)
	}
	if got := h.screenBody(t, 1, other.ID); got == "" {
		t.Error("the other target did not render")
	}

	// Not retried until RetryAfter, and then recorded again.
	h.now = h.now.Add(db.RenderRetryAfter - time.Second)
	h.renderDue(t, 1)
	if at := failure().AttemptedAt; at == nil || *at != h.now.Add(-(db.RenderRetryAfter-time.Second)).Unix() {
		t.Fatalf("retried early: attempted at %v", at)
	}
	h.now = h.now.Add(time.Second)
	h.renderDue(t, 1)
	if at := failure().AttemptedAt; at == nil || *at != h.now.Unix() {
		t.Fatalf("not retried after RetryAfter: attempted at %v", at)
	}
}

// TestRenderRedactsSecrets checks that the value of a secret field never
// reaches the stored error or log, though a transform or a failed fetch
// printed it, and that the rest of the line is kept.
func TestRenderRedactsSecrets(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.leakFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 1)
	r, err := h.db.Read.GetRenderStatus(t.Context(), h.render(t).ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Error, "hunter2") || strings.Contains(r.Log, "hunter2") {
		t.Fatalf("secret stored: error %q, log %q", r.Error, r.Log)
	}
	if want := "trmnlp: GET https://api.example/?key=[redacted] failed: 401"; r.Error != want {
		t.Errorf("error = %q, want %q", r.Error, want)
	}
	if !strings.Contains(r.Log, "fetching https://api.example/?key=[redacted]\n") {
		t.Errorf("log = %q", r.Log)
	}
}

// TestRedact checks that a secret is hidden as it was written, and in the
// two URL-encoded forms trmnlp prints in a failed poll's warning.
func TestRedact(t *testing.T) {
	const field = `s3cr/t+key=a b&"x"`
	for _, tc := range []struct {
		name, log string
		values    []string
		want      string
	}{
		{
			name:   "query-encoded url as trmnlp prints it",
			log:    "warning: HTTP 404 from http://host:8080/x?k=s3cr%2Ft%2Bkey%3Da+b%26%22x%22",
			values: []string{field},
			want:   "warning: HTTP 404 from http://host:8080/x?k=[redacted]",
		},
		{
			name:   "raw value in a url",
			log:    `warning: Malformed JSON from url: http://host:8080/x?k=s3cr/t+key=a b&"x"`,
			values: []string{field},
			want:   "warning: Malformed JSON from url: http://host:8080/x?k=[redacted]",
		},
		{
			name:   "path-encoded form",
			log:    "HTTP 404 from http://host/s3cr%2Ft+key=a%20b&%22x%22/data",
			values: []string{field},
			want:   "HTTP 404 from http://host/[redacted]/data",
		},
		{
			name:   "a longer value holds a shorter one",
			log:    "key=abc%2Fdef and abc",
			values: []string{"abc", "abc/def"},
			want:   "key=[redacted] and [redacted]",
		},
		{name: "empty value", log: "unchanged", values: []string{""}, want: "unchanged"},
		{name: "no values", log: "unchanged", want: "unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redact(tc.log, tc.values); got != tc.want {
				t.Errorf("redact = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunStops checks that Run, once it is running and has built what was
// due, returns when its context ends, so a server can close the database
// after waiting for it.
func TestRunStops(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.s.Run(ctx)
		close(done)
	}()
	// Wait for the loop to build the due target, which shows it is running.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := h.db.Read.ListRenderRows(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].RenderedAt != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not build the due render")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("Run returned before its context ended")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// TestBackoffDoubles runs a recipe that keeps failing through the
// scheduler and checks that each retry waits twice as long as the one
// before, up to an hour, and that a success starts the count over.
func TestBackoffDoubles(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	builds := int32(0)
	// The first build fails at once. After it, each wait is checked to be
	// one second short of the delay, then to end at it.
	h.renderDue(t, 1)
	builds++
	for i, wait := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		if got := h.render(t).Failures; got != int64(i+1) {
			t.Fatalf("after %d failures the row says %d", i+1, got)
		}
		h.now = h.now.Add(wait - time.Second)
		h.renderDue(t, builds)
		h.now = h.now.Add(time.Second)
		builds++
		h.renderDue(t, builds)
	}
	if got := h.render(t).Failures; got != 7 {
		t.Fatalf("failures = %d, want 7", got)
	}

	// A success resets it, so the next failure waits the first delay again.
	if err := os.Remove(h.failFile); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Hour)
	builds++
	h.renderDue(t, builds)
	if r := h.render(t); r.Error != "" || r.Failures != 0 {
		t.Fatalf("after recovering: %+v", r)
	}
	if err := os.WriteFile(h.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(2 * time.Hour)
	builds++
	h.renderDue(t, builds)
	h.now = h.now.Add(db.RenderRetryAfter - time.Second)
	h.renderDue(t, builds)
	h.now = h.now.Add(time.Second)
	builds++
	h.renderDue(t, builds)
	if got := h.render(t).Failures; got != 2 {
		t.Fatalf("failures after recovering and failing twice = %d, want 2", got)
	}
}

// TestOutageDoesNotCountAsFailure checks that a renderer that cannot be
// reached is recorded but leaves failures as it was, so the retry comes
// after the delay for the old count, and that a build failure after the
// outage still counts.
func TestOutageDoesNotCountAsFailure(t *testing.T) {
	h := newHarness(t)
	builds := int32(0)

	// With no prior failure, several outages in a row keep failures at 0
	// and retry after 5 minutes each time.
	h.down.Store(true)
	for i := range 4 {
		builds++
		h.renderDue(t, builds)
		r := h.render(t)
		if r.Failures != 0 || !strings.Contains(r.Error, "renderer unavailable") || r.AttemptedAt != h.now.Unix() {
			t.Fatalf("outage %d: %+v", i+1, r)
		}
		h.now = h.now.Add(db.RenderRetryAfter - time.Second)
		h.renderDue(t, builds)
		h.now = h.now.Add(time.Second)
	}

	// Two real failures raise the count to 2.
	h.down.Store(false)
	if err := os.WriteFile(h.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		builds++
		h.renderDue(t, builds)
		if got := h.render(t).Failures; got != int64(i+1) {
			t.Fatalf("failures = %d, want %d", got, i+1)
		}
		h.now = h.now.Add(retryDelay(int64(i + 1)))
	}

	// An outage now keeps 2, and the next try is 10 minutes after it.
	h.down.Store(true)
	for range 3 {
		builds++
		h.renderDue(t, builds)
		if got := h.render(t).Failures; got != 2 {
			t.Fatalf("failures after an outage = %d, want 2", got)
		}
		h.now = h.now.Add(10*time.Minute - time.Second)
		h.renderDue(t, builds)
		h.now = h.now.Add(time.Second)
	}

	// A build that runs and fails again counts.
	h.down.Store(false)
	builds++
	h.renderDue(t, builds)
	if r := h.render(t); r.Failures != 3 || strings.Contains(r.Error, "unavailable") {
		t.Fatalf("after a real failure: %+v", r)
	}
}

func TestRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		failures int64
		want     time.Duration
	}{
		{0, 5 * time.Minute}, // A row from before failures were counted.
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 20 * time.Minute},
		{4, 40 * time.Minute},
		{5, time.Hour},
		{6, time.Hour},
		{1 << 62, time.Hour},
	} {
		if got := retryDelay(tc.failures); got != tc.want {
			t.Errorf("retryDelay(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// TestRenderPruning checks that a render no device can show any more is
// deleted, so its images lose their reference and expire, and that a render
// a device can still show is kept.
func TestRenderPruning(t *testing.T) {
	h := newHarness(t)
	other := h.installRecipe(t, "y")
	item, _, err := playlist.AddItem(t.Context(), h.db, "p", 0, other.ID, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 2)
	renders := func() int {
		t.Helper()
		rows, err := h.db.Read.ListRenderKeys(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	images := func() int {
		t.Helper()
		names, err := h.db.Read.ListReferencedImages(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return len(names)
	}
	if renders() != 2 {
		t.Fatalf("%d renders, want 2", renders())
	}

	// A device that is now of an unknown model, which takes the size it
	// reports, does not show the 800x480 renders. The next scan drops them.
	imagesBefore := images()
	h.now = h.now.Add(time.Minute)
	if err := h.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: "mystery", ID: 1}); err != nil {
		t.Fatal(err)
	}
	err = h.db.Write.UpsertDeviceState(t.Context(), db.UpsertDeviceStateParams{
		DeviceID: 1, LastSeen: h.now.Unix(), LastRefreshRateSent: 900, Width: new(int64(400)), Height: new(int64(300)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.dueTargets(t.Context()); err != nil {
		t.Fatal(err)
	}
	keys, err := h.db.Read.ListRenderKeys(t.Context())
	if err != nil || len(keys) != 0 {
		t.Fatalf("renders after a size change: %+v, %v; want none left at the old size", keys, err)
	}
	if images() != 0 || imagesBefore == 0 {
		t.Errorf("%d images still referenced, want 0 (had %d)", images(), imagesBefore)
	}

	// Back to the first model, and with the second instance taken off the
	// playlist, only the first instance's render is left.
	if err := h.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: "xiao_epaper_display", ID: 1}); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	h.renderDue(t, 4)
	if renders() != 2 {
		t.Fatalf("%d renders, want 2", renders())
	}
	if err := playlist.DeleteItem(t.Context(), h.db, item.ID); err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 4)
	keys, err = h.db.Read.ListRenderKeys(t.Context())
	if err != nil || len(keys) != 1 || keys[0].InstanceID != 1 {
		t.Fatalf("renders after removing an item: %+v, %v; want only instance 1's", keys, err)
	}
	if got := images(); got != len(render.Layouts) {
		t.Errorf("%d images referenced, want %d", got, len(render.Layouts))
	}
}

// TestPruneKeepsRendersOfUnscannedRecipe checks that a recipe whose files
// cannot be read at a scan, so that it is not known to be per device, does
// not lose its per-device renders and their images. Its devices keep their
// last good screens until the recipe can be scanned again.
func TestPruneKeepsRendersOfUnscannedRecipe(t *testing.T) {
	h := newHarness(t)
	per := h.installRecipe(t, "<p>{{ trmnl.device.friendly_id }}</p>")
	if _, _, err := playlist.AddItem(t.Context(), h.db, "p2", 1, per.ID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 2)
	perDeviceRows := func() int {
		t.Helper()
		keys, err := h.db.Read.ListRenderKeys(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, k := range keys {
			if k.InstanceID == per.ID && k.DeviceID != 0 {
				n++
			}
		}
		return n
	}
	if perDeviceRows() != 1 {
		t.Fatalf("%d per-device renders, want 1", perDeviceRows())
	}
	imagesBefore, err := h.db.Read.ListReferencedImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// A restarted scheduler has not scanned the recipe, and cannot now.
	if err := os.RemoveAll(recipe.Dir(h.s.cfg.RecipesDir, per.RecipeID)); err != nil {
		t.Fatal(err)
	}
	h.s = New(h.s.cfg)
	h.s.now = func() time.Time { return h.now }
	if _, err := h.s.dueTargets(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := perDeviceRows(); got != 1 {
		t.Fatalf("%d per-device renders after a failed scan, want 1", got)
	}
	if after, err := h.db.Read.ListReferencedImages(t.Context()); err != nil || !slices.Equal(after, imagesBefore) {
		t.Fatalf("images after a failed scan: %v, %v; want %v", after, err, imagesBefore)
	}
}

// TestRebuildAfterRenderDeleted checks that a render the device API deleted
// because its image was missing is built again at the next check, with no
// wait for its refresh interval.
func TestRebuildAfterRenderDeleted(t *testing.T) {
	h := newHarness(t)
	h.renderDue(t, 1)
	p := profile.Resolve("xiao_epaper_display", nil, nil)
	row, err := h.db.Read.GetItemScreen(t.Context(), db.GetItemScreenParams{
		Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""), Layout: "full", DeviceID: 1, InstanceID: 1,
	})
	if err != nil || row.RenderID == nil || row.Image == nil {
		t.Fatalf("screen %+v, %v", row, err)
	}
	if due, err := h.s.dueTargets(t.Context()); err != nil || len(due) != 0 {
		t.Fatalf("due before the delete: %+v, %v", due, err)
	}
	if err := h.db.Write.DeleteRenderWithImage(t.Context(), db.DeleteRenderWithImageParams{ID: *row.RenderID, Image: *row.Image}); err != nil {
		t.Fatal(err)
	}
	if due, err := h.s.dueTargets(t.Context()); err != nil || len(due) != 1 || due[0].instance != 1 {
		t.Fatalf("due after the delete: %+v, %v; want instance 1", due, err)
	}
	h.renderDue(t, 2)
}

// TestRenderTimesOut checks that a renderer that never answers fails the
// build at its deadline, so the target is recorded as failed and retried
// later instead of the scheduler waiting on it forever.
func TestRenderTimesOut(t *testing.T) {
	h := newHarness(t)
	stuck := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Read the project, then never answer. The server notices the
		// client's disconnect once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(stuck.Close)
	h.s.cfg.Renderer = &render.Client{URL: stuck.URL, HTTP: stuck.Client()}
	h.s.buildTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := h.s.RenderNext(t.Context())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RenderNext waited on a renderer that never answers")
	}
	if r := h.render(t); !strings.Contains(r.Error, "deadline exceeded") || r.Failures != 0 {
		t.Fatalf("render after a timeout: %+v", r)
	}
}

// renderedAt returns when the render of instance (the harness's device
// profile, shared) was last built.
func (h *harness) renderedAt(t *testing.T, instance int64) int64 {
	t.Helper()
	return h.renderOf(t, instance).RenderedAt
}

// TestRenderNextUrgentFirst checks that a pass builds one target and the next
// pass looks again, so a target that becomes urgent while others are queued
// is built before the older ones that were due first.
func TestRenderNextUrgentFirst(t *testing.T) {
	h := newHarness(t)
	start := h.now
	second := h.installRecipe(t, "y")
	third := h.installRecipe(t, "z")
	for _, in := range []db.PluginInstance{second, third} {
		if _, _, err := playlist.AddItem(t.Context(), h.db, "p", 0, in.ID, 15*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	h.renderDue(t, 3)

	// All three are over two intervals old, and no device wants any of them.
	h.now = start.Add(3 * time.Hour)
	more, err := h.s.RenderNext(t.Context())
	if err != nil || !more || h.builds.Load() != 4 {
		t.Fatalf("first pass: more %v, %d builds, %v; want one build", more, h.builds.Load(), err)
	}
	if got := h.renderedAt(t, 1); got != h.now.Unix() {
		t.Fatalf("the first pass built instance 1 at %d, want %d", got, h.now.Unix())
	}

	// The device now is 14.5 minutes into the second item and wakes in 90
	// seconds, so the third instance is wanted inside its lead. It is the
	// newest of the targets still queued, and it must go first.
	items, err := h.db.Read.ListDeviceItems(t.Context(), 1)
	if err != nil || len(items) != 3 {
		t.Fatalf("items %v, %v", items, err)
	}
	err = h.db.Write.SetDeviceCursor(t.Context(), db.SetDeviceCursorParams{
		DeviceID: 1, ItemID: items[1].ID, ItemStartedAt: h.now.Add(-14*time.Minute - 30*time.Second).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.wake(t, 1, 90*time.Second)
	if more, err = h.s.RenderNext(t.Context()); err != nil || !more || h.builds.Load() != 5 {
		t.Fatalf("second pass: more %v, %d builds, %v", more, h.builds.Load(), err)
	}
	if got3, got2 := h.renderedAt(t, third.ID), h.renderedAt(t, second.ID); got3 != h.now.Unix() || got2 == h.now.Unix() {
		t.Fatalf("second pass: instance 3 built at %d, instance 2 at %d, now %d; want 3 built and 2 not", got3, got2, h.now.Unix())
	}
	if more, err = h.s.RenderNext(t.Context()); err != nil || !more || h.renderedAt(t, second.ID) != h.now.Unix() {
		t.Fatalf("third pass: more %v, %v", more, err)
	}
	if more, err = h.s.RenderNext(t.Context()); err != nil || more || h.builds.Load() != 6 {
		t.Fatalf("fourth pass: more %v, %d builds, %v; want nothing due", more, h.builds.Load(), err)
	}
}

// TestCursorIsNotAWake checks that a device that has a playlist cursor but
// has never called /api/display has not woken: it is not "woken" for the
// rule that rebuilds a render nobody has fetched, and it has no wake time
// for the scheduler to build ahead of.
func TestCursorIsNotAWake(t *testing.T) {
	h := newHarness(t)
	items, err := h.db.Read.ListDeviceItems(t.Context(), 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("items %v, %v", items, err)
	}
	err = h.db.Write.SetDeviceCursor(t.Context(), db.SetDeviceCursorParams{DeviceID: 1, ItemID: items[0].ID, ItemStartedAt: h.now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	wakes, err := h.db.Read.ListDeviceWakes(t.Context())
	if err != nil || len(wakes) != 0 {
		t.Fatalf("wakes %+v, %v; want none", wakes, err)
	}
	cands, err := h.s.candidates(t.Context())
	if err != nil || len(cands) != 1 || cands[0].woken || len(cands[0].wanted) != 0 {
		t.Fatalf("candidates %+v, %v; want one that has not woken and is not wanted", cands, err)
	}

	// Once the device reports a wake, it has woken, and its cursor is read.
	h.wake(t, 1, 15*time.Minute)
	if cands, err = h.s.candidates(t.Context()); err != nil || len(cands) != 1 || !cands[0].woken || len(cands[0].wanted) != 1 {
		t.Fatalf("candidates after a wake %+v, %v", cands, err)
	}
}

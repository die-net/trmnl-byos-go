package deviceapi

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

// playlistDevice registers testMAC with a playlist of two instances of an
// hourly recipe, ten minutes each, and returns its key and the instances.
func (ts *testServer) playlistDevice(t *testing.T) (key string, first, second int64) {
	t.Helper()
	key = ts.register(t)
	ctx := t.Context()
	dev, err := ts.db.Read.GetDeviceByMAC(ctx, testMAC)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := ts.db.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 3600, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	var ids [2]int64
	for i, name := range []string{"Weather", "News"} {
		inst, err := ts.db.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: rec.ID, Name: name, FieldValues: "{}", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := playlist.AddItem(ctx, ts.db, "p", dev.ID, inst.ID, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		ids[i] = inst.ID
	}
	return key, ids[0], ids[1]
}

// saveRender records a render of an instance at the OG's profile: a good
// one with a real PNG when failure is empty, and a failed one otherwise. It
// returns the good render's image name.
func (ts *testServer) saveRender(t *testing.T, instance int64, failure string) string {
	t.Helper()
	ctx := t.Context()
	p, err := profile.Resolve("xiao_epaper_display", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	params := db.SaveRenderFailureParams{
		InstanceID: instance, Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""),
		AttemptedAt: ts.now.Unix(), Error: failure,
	}
	if failure != "" {
		if err := ts.db.Write.SaveRenderFailure(ctx, params); err != nil {
			t.Fatal(err)
		}
		return ""
	}
	var png bytes.Buffer
	if err := screen.Render(&png, screen.Profile{Width: p.Width, Height: p.Height, Levels: 2}, []screen.Line{{Text: "render", Size: 40}}); err != nil {
		t.Fatal(err)
	}
	name, size, err := ts.srv.images.Put(&png)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ts.db.Write.SaveRenderSuccess(ctx, db.SaveRenderSuccessParams{
		InstanceID: instance, Width: params.Width, Height: params.Height, Palette: params.Palette,
		RenderedAt: ts.now.Unix(), AttemptedAt: ts.now.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.db.Write.SaveRenderImage(ctx, db.SaveRenderImageParams{RenderID: id, Layout: "full", Image: name, Bytes: size}); err != nil {
		t.Fatal(err)
	}
	return name
}

// TestDisplayPlaylist walks a device through its playlist: a screen while
// the first render is under way, the render, the next item when the first
// one's time is up, a failed render, a button press, and a render too old
// to show.
func TestDisplayPlaylist(t *testing.T) {
	ts := newTestServer(t)
	key, first, second := ts.playlistDevice(t)
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"
	press := displayHeaders(testMAC, key)
	t0 := ts.now

	wake := func(h map[string]string) (string, float64) {
		t.Helper()
		m := ts.getJSON(t, "/api/display", h)
		ts.fetchImage(t, m["image_url"].(string), m["filename"].(string))
		return m["filename"].(string), m["refresh_rate"].(float64)
	}

	pending, rate := wake(timer)
	if rate != pendingRefresh {
		t.Fatalf("before the first render: refresh %v, want %d", rate, pendingRefresh)
	}
	cur, err := ts.db.Read.GetDeviceCursor(t.Context(), 1)
	if err != nil || cur.ItemID == nil || *cur.ItemStartedAt != t0.Unix() {
		t.Fatalf("cursor %+v, %v", cur, err)
	}

	img := ts.saveRender(t, first, "")
	ts.now = t0.Add(time.Minute)
	if name, r := wake(timer); name != img || r != 9*60 {
		t.Fatalf("rendered: %s, refresh %v; want %s until the item ends in 540 s", name, r, img)
	}

	ts.saveRender(t, second, "trmnlp: data source said 401")
	ts.now = t0.Add(10 * time.Minute)
	failed, rate := wake(timer)
	if failed == pending || failed == img || rate != float64(retryRefresh) {
		t.Fatalf("failed render: %s, refresh %v", failed, rate)
	}

	ts.now = t0.Add(11 * time.Minute)
	if name, r := wake(press); name != img || r != 10*60 {
		t.Fatalf("after a press: %s, refresh %v; want the first item again for its full 600 s", name, r)
	}

	// Nine 20-minute turns later the first item is up again, but its render
	// is over three hours old: two refresh intervals and an hour.
	ts.now = t0.Add(11*time.Minute + 3*time.Hour)
	stale, rate := wake(timer)
	if stale == img || stale == failed || rate != float64(retryRefresh) {
		t.Fatalf("stale render: %s, refresh %v", stale, rate)
	}

	refs, err := ts.db.Read.ListReferencedImages(t.Context())
	if err != nil || !slices.Contains(refs, img) {
		t.Fatalf("referenced images %v, %v; want the render's image kept", refs, err)
	}
	targets, err := ts.db.Read.ListRenderTargets(t.Context())
	if err != nil || len(targets) != 2 || targets[0].Width == nil || *targets[0].Width != 800 {
		t.Fatalf("render targets %+v, %v; want the reported width saved", targets, err)
	}
}

func TestClip(t *testing.T) {
	long := string(bytes.Repeat([]byte("é"), 100))
	for _, tc := range []struct{ in, want string }{
		{"short", "short"},
		{"first line\nsecond", "first line"},
		{long, long[:2*(maxLineRunes-1)] + "…"},
	} {
		if got := clip(tc.in); got != tc.want {
			t.Errorf("clip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

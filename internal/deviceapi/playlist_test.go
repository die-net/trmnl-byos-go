package deviceapi

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
		if _, _, err := playlist.AddItem(ctx, ts.db, "p", dev.ID, inst.ID, 10*time.Minute); err != nil {
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
	return ts.saveDeviceRender(t, instance, 0, "render", failure)
}

// saveDeviceRender is saveRender for one device's render, or the shared
// render when device is 0, with text drawn in the image.
func (ts *testServer) saveDeviceRender(t *testing.T, instance, device int64, text, failure string) string {
	t.Helper()
	ctx := t.Context()
	p := profile.Resolve("xiao_epaper_display", nil, nil)
	params := db.SaveRenderFailureParams{
		InstanceID: instance, DeviceID: device, Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""),
		AttemptedAt: ts.now.Unix(), Error: failure,
	}
	if failure != "" {
		if err := ts.db.Write.SaveRenderFailure(ctx, params); err != nil {
			t.Fatal(err)
		}
		return ""
	}
	var png bytes.Buffer
	if err := screen.Render(&png, screen.Profile{Width: p.Width, Height: p.Height, Levels: 2}, []screen.Line{{Text: text, Size: 40}}); err != nil {
		t.Fatal(err)
	}
	name, size, err := ts.srv.images.Put(&png)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ts.db.Write.SaveRenderSuccess(ctx, db.SaveRenderSuccessParams{
		InstanceID: instance, DeviceID: device, Width: params.Width, Height: params.Height, Palette: params.Palette,
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
	if err != nil || cur.ItemStartedAt != t0.Unix() {
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
	// The screen says how old the render is in whole hours, so a wake a
	// minute later gets the same image, not a new file per wake.
	ts.now = ts.now.Add(time.Minute)
	if again, _ := wake(timer); again != stale {
		t.Errorf("out of date screen a minute later is %s, want the same %s", again, stale)
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

// TestDisplayPerDeviceRender checks that two devices showing one instance
// each get the render built for them, and that a device whose render is not
// built yet does not get the other's.
func TestDisplayPerDeviceRender(t *testing.T) {
	ts := newTestServer(t)
	keyA, instance, _ := ts.playlistDevice(t)
	devA, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	const macB = "D8:3B:DA:12:34:57"
	ts.openJoinWindow(t)
	m := ts.getJSON(t, "/api/setup", setupHeaders(macB))
	keyB, _ := m["api_key"].(string)
	devB, err := ts.db.Read.GetDeviceByMAC(t.Context(), macB)
	if err != nil || keyB == "" {
		t.Fatalf("second device: %v, %v", m, err)
	}
	if _, _, err := playlist.AddItem(t.Context(), ts.db, "p", devB.ID, instance, 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	wake := func(mac, key string) string {
		t.Helper()
		h := displayHeaders(mac, key)
		h["Update-Source"] = "timer" // A press would move on to the playlist's next item.
		return ts.getJSON(t, "/api/display", h)["filename"].(string)
	}
	imgA := ts.saveDeviceRender(t, instance, devA.ID, "device A", "")
	if got := wake(testMAC, keyA); got != imgA {
		t.Fatalf("device A got %s, want its render %s", got, imgA)
	}
	// B has no render of its own yet: it must get the "Rendering" screen,
	// not A's image.
	if got := wake(macB, keyB); got == imgA {
		t.Fatal("device B got device A's render")
	}
	imgB := ts.saveDeviceRender(t, instance, devB.ID, "device B", "")
	if imgA == imgB {
		t.Fatal("the two renders are the same image")
	}
	if got := wake(macB, keyB); got != imgB {
		t.Fatalf("device B got %s, want its render %s", got, imgB)
	}
	if got := wake(testMAC, keyA); got != imgA {
		t.Fatalf("device A got %s, want %s", got, imgA)
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

// TestScreenCacheIsBounded checks that the table of drawn system screens
// does not grow without limit when the text keeps changing.
func TestScreenCacheIsBounded(t *testing.T) {
	ts := newTestServer(t)
	sp := screen.Profile{Width: 80, Height: 40, Levels: 2}
	for i := range maxScreens + 20 {
		if _, err := ts.srv.screen(sp, []screen.Line{{Text: strconv.Itoa(i), Size: 20}}); err != nil {
			t.Fatal(err)
		}
	}
	ts.srv.mu.Lock()
	defer ts.srv.mu.Unlock()
	if n := len(ts.srv.screens); n > maxScreens {
		t.Errorf("%d cached screens, want at most %d", n, maxScreens)
	}
}

// TestDisplayKeepsRenderImage checks that showing a render's image marks it
// used. A row can be replaced between the device's read and its fetch, which
// leaves the image unnamed, and expiry must not delete it from under the
// device because it was last stored long ago.
func TestDisplayKeepsRenderImage(t *testing.T) {
	ts := newTestServer(t)
	key, first, _ := ts.playlistDevice(t)
	img := ts.saveRender(t, first, "")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(ts.imagesDir, img[:2], img[2:]+".png"), old, old); err != nil {
		t.Fatal(err)
	}
	h := displayHeaders(testMAC, key)
	h["Update-Source"] = "timer"
	if m := ts.getJSON(t, "/api/display", h); m["filename"] != img {
		t.Fatalf("shown %v, want the render's image %s", m["filename"], img)
	}
	// What expiry would do if no row named the image.
	n, err := ts.srv.images.Expire(map[string]bool{}, time.Now().Add(-24*time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("expiry deleted %d images, %v; want the shown image kept", n, err)
	}
}

// TestDisplayMissingRenderImage checks that a render whose image is gone
// from the store is treated as a render that does not exist yet: the device
// gets a screen that is stored, not a name that 404s, and the render row
// is deleted so the scheduler builds it again. A row replaced by a newer
// build, which names a different image, is not deleted.
func TestDisplayMissingRenderImage(t *testing.T) {
	ts := newTestServer(t)
	key, first, _ := ts.playlistDevice(t)
	img := ts.saveRender(t, first, "")
	if err := os.Remove(filepath.Join(ts.imagesDir, img[:2], img[2:]+".png")); err != nil {
		t.Fatal(err)
	}

	m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))
	name, _ := m["filename"].(string)
	if name == "" || name == img || m["refresh_rate"] != float64(pendingRefresh) {
		t.Fatalf("response = %v; want a stored screen other than %s, refresh %d", m, img, pendingRefresh)
	}
	ts.fetchImage(t, m["image_url"].(string), name)
	rows, err := ts.db.Read.ListRenderRows(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("render rows after a missing image: %+v, %v; want none, so the scheduler rebuilds", rows, err)
	}

	// A build that replaced the row's image after it was read is kept.
	img = ts.saveRender(t, first, "")
	rows, err = ts.db.Read.ListRenderRows(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("render rows: %+v, %v", rows, err)
	}
	err = ts.db.Write.SaveRenderImage(t.Context(), db.SaveRenderImageParams{RenderID: rows[0].ID, Layout: "full", Image: "newer", Bytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.db.Write.DeleteRenderWithImage(t.Context(), db.DeleteRenderWithImageParams{ID: rows[0].ID, Image: img}); err != nil {
		t.Fatal(err)
	}
	if rows, err := ts.db.Read.ListRenderRows(t.Context()); err != nil || len(rows) != 1 {
		t.Fatalf("render rows after a conditional delete of an old image: %+v, %v; want the row kept", rows, err)
	}
}

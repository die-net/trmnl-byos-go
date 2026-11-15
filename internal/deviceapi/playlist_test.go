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

// scheduleWindow sets the schedule of the playlist named name to a window
// every day, in minutes after midnight.
func (ts *testServer) scheduleWindow(t *testing.T, name string, start, end int64) {
	t.Helper()
	pl, err := ts.db.Read.GetPlaylistByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	s := playlist.Always
	s.WindowStart, s.WindowEnd = start, end
	if err := playlist.SetPlaylistSchedule(t.Context(), ts.db, pl.ID, s.Full()); err != nil {
		t.Fatal(err)
	}
}

// nothingScheduled is the name of the "Nothing scheduled now" screen the
// test device gets.
func (ts *testServer) nothingScheduled(t *testing.T) string {
	t.Helper()
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	p := profile.Resolve("xiao_epaper_display", nil, nil)
	name, _, err := ts.srv.screenSized(screenProfile(p), []screen.Line{
		{Text: "Nothing scheduled now", Size: 44, Bold: true},
		{Text: dev.FriendlyID, Size: 28},
		{Text: "None of this display's playlists is scheduled for now.", Size: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// TestDisplaySchedule walks a device past the end of its playlist's window:
// the refresh is cut to the minute the window closes, and the wake that
// follows gets the "Nothing scheduled now" screen, not the last item, with
// the usual refresh since nothing changes soon.
func TestDisplaySchedule(t *testing.T) {
	ts := newTestServer(t)
	ts.now = time.Date(2026, 11, 13, 12, 0, 0, 0, testZone)
	key, first, second := ts.playlistDevice(t)
	img := ts.saveRender(t, first, "")
	ts.saveRender(t, second, "")
	// Open from 09:00 to 12:05.
	ts.scheduleWindow(t, "p", 9*60, 12*60+5)
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"

	// The item has 600 s left and the usual rate is 900, but the window
	// closes at 12:05:00, so the device is woken then.
	m := ts.getJSON(t, "/api/display", timer)
	if m["filename"] != img || m["refresh_rate"] != float64(300) {
		t.Fatalf("in the window: %v; want the first item's image and refresh 300", m)
	}
	// A wake 20 s early still chooses after the window closed.
	ts.now = ts.now.Add(280 * time.Second)
	m = ts.getJSON(t, "/api/display", timer)
	closed := ts.nothingScheduled(t)
	if m["filename"] != closed || m["refresh_rate"] != float64(900) {
		t.Fatalf("after the window: %v; want the Nothing scheduled now screen %s and refresh 900", m, closed)
	}
	ts.fetchImage(t, m["image_url"].(string), closed)

	// A wake at 12:05 that finds the window about to open, at 12:08, is cut
	// to it.
	ts.now = time.Date(2026, 11, 13, 12, 5, 0, 0, testZone)
	ts.scheduleWindow(t, "p", 12*60+5+3, 14*60)
	m = ts.getJSON(t, "/api/display", timer)
	if m["filename"] != closed || m["refresh_rate"] != float64(180) {
		t.Fatalf("before the window opens: %v; want the same screen and refresh 180", m)
	}
}

// TestDisplayHiddenItem checks that a hidden item is never shown, the next
// shown one is, and a playlist whose items are all hidden shows the
// "Nothing scheduled now" screen.
func TestDisplayHiddenItem(t *testing.T) {
	ts := newTestServer(t)
	key, first, second := ts.playlistDevice(t)
	imgFirst, imgSecond := ts.saveRender(t, first, ""), ts.saveRender(t, second, "")
	items, err := ts.db.Read.ListPlaylistItemIDs(t.Context(), 1)
	if err != nil || len(items) != 2 {
		t.Fatalf("items %v, %v", items, err)
	}
	hide := func(id int64, hidden bool) {
		t.Helper()
		if err := playlist.UpdateItem(t.Context(), ts.db, id, playlist.ItemUpdate{Hidden: &hidden}); err != nil {
			t.Fatal(err)
		}
	}
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"
	press := displayHeaders(testMAC, key)

	hide(items[0], true)
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != imgSecond {
		t.Fatalf("with the first item hidden: %v; want the second's image %s", m, imgSecond)
	}
	// A press on the only shown item stays on it.
	if m := ts.getJSON(t, "/api/display", press); m["filename"] != imgSecond {
		t.Fatalf("press with the first item hidden: %v; want the second's image", m)
	}
	// The cursor is on the item that is now hidden. It moves to the shown one.
	hide(items[0], false)
	hide(items[1], true)
	ts.now = ts.now.Add(time.Minute)
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != imgFirst {
		t.Fatalf("with the second item hidden under the cursor: %v; want the first's image %s", m, imgFirst)
	}
	hide(items[0], true)
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != ts.nothingScheduled(t) {
		t.Fatalf("with every item hidden: %v; want the Nothing scheduled now screen", m)
	}
}

// TestDisplayDefaultPlaylist checks that when a scheduled playlist is not
// active, the device shows the next playlist, which is the unscheduled
// default, and goes back when the schedule opens.
func TestDisplayDefaultPlaylist(t *testing.T) {
	ts := newTestServer(t)
	ts.now = time.Date(2026, 11, 13, 12, 0, 0, 0, testZone)
	key, first, _ := ts.playlistDevice(t)
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := ts.db.Write.CreateRecipe(t.Context(), db.CreateRecipeParams{Name: "r2", Source: "s", RefreshInterval: 3600, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := ts.db.Write.CreateInstance(t.Context(), db.CreateInstanceParams{RecipeID: rec.ID, Name: "Default", FieldValues: "{}", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Playlist "p" has the lower ID, so it is tried first.
	if _, _, err := playlist.AddItem(t.Context(), ts.db, "fallback", dev.ID, inst.ID, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	imgP := ts.saveRender(t, first, "")
	ts.scheduleWindow(t, "p", 12*60, 13*60)
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"

	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != imgP {
		t.Fatalf("in the window: %v; want playlist p's image %s", m, imgP)
	}
	ts.now = time.Date(2026, 11, 13, 14, 0, 0, 0, testZone)
	// Renders are made again, since they would be too old by now.
	ts.saveRender(t, first, "")
	imgDefault := ts.saveRender(t, inst.ID, "")
	m := ts.getJSON(t, "/api/display", timer)
	if m["filename"] != imgDefault {
		t.Fatalf("after the window: %v; want the default playlist's image %s", m, imgDefault)
	}
	// The window opens tomorrow at 12:00, 22 hours away, so the usual rate holds.
	if m["refresh_rate"] != float64(600) {
		t.Errorf("refresh = %v, want the default item's 600", m["refresh_rate"])
	}
}

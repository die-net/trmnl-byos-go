package deviceapi

import (
	"testing"
	"time"
	_ "time/tzdata" // So the zones the tests name load on any host.

	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

// sleepingScreen is the name of the "Sleeping" screen the test device gets
// for a window that ends at wake, as HH:MM.
func (ts *testServer) sleepingScreen(t *testing.T, wake string) string {
	t.Helper()
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
	name, _, err := ts.srv.screenSized(screenProfile(p), []screen.Line{
		{Text: "Sleeping", Size: 44, Bold: true},
		{Text: dev.FriendlyID, Size: 28},
		{Text: "Wakes at " + wake + ".", Size: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// TestDisplaySleep walks a device through its sleep window, read in its own
// time zone: before it, the refresh is cut to the window's start. Inside it,
// the device gets the Sleeping screen and a refresh to the window's end, a
// button press included, and its playlist cursor does not move. After it,
// the playlist shows again. The server's zone is UTC+5:30 and the device's is
// Tokyo, UTC+9, so a window of 15:00 to 17:30 is closed at 12:00 on the
// server's clock, and open at 15:30 on the device's.
func TestDisplaySleep(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	// 14:55 in Tokyo, which is 11:25 on the server's clock.
	ts.now = time.Date(2026, 11, 13, 14, 55, 0, 0, time.FixedZone("tokyo", 9*3600))
	key, first, second := ts.playlistDevice(t)
	imgFirst := ts.saveRenderAt(t, first, 0, "first", "", "Asia/Tokyo", "en")
	imgSecond := ts.saveRenderAt(t, second, 0, "second", "", "Asia/Tokyo", "en")
	sleeping := ts.sleepingScreen(t, "17:30")
	if sleeping == imgFirst || sleeping == imgSecond {
		t.Fatal("the Sleeping screen is a playlist render")
	}
	tokyo, window := "Asia/Tokyo", "15:00-17:30"
	ts.setDevice(t, device.Change{TimeZone: &tokyo, Sleep: &window})
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"
	press := displayHeaders(testMAC, key)
	press["Update-Source"] = "button"
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	cursor := func() (int64, int64) {
		t.Helper()
		cur, err := ts.db.Read.GetDeviceCursor(t.Context(), dev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return cur.ItemID, cur.ItemStartedAt
	}

	// Before the window: the first item, whose 10 minutes are cut to the 5
	// until the device sleeps.
	m := ts.getJSON(t, "/api/display", timer)
	if m["filename"] != imgFirst || m["refresh_rate"] != float64(5*60) {
		t.Fatalf("before the window: %v; want the first item and a refresh of 300 s to its start", m)
	}
	itemBefore, startedBefore := cursor()

	// Inside it, at 15:30 Tokyo: asleep until 17:30, two hours on.
	ts.now = ts.now.Add(35 * time.Minute)
	m = ts.getJSON(t, "/api/display", timer)
	if m["filename"] != sleeping || m["refresh_rate"] != float64(2*3600) {
		t.Fatalf("in the window: %v; want the Sleeping screen and a refresh of 7200 s", m)
	}
	// A press shows the same screen and sleeps as long. A wake a minute
	// before the end sleeps only until it.
	if m = ts.getJSON(t, "/api/display", press); m["filename"] != sleeping || m["refresh_rate"] != float64(2*3600) {
		t.Errorf("a press in the window: %v; want the Sleeping screen and 7200 s", m)
	}
	if item, started := cursor(); item != itemBefore || started != startedBefore {
		t.Errorf("the cursor moved in the window: item %d started %d, was item %d started %d", item, started, itemBefore, startedBefore)
	}
	ts.now = ts.now.Add(2*time.Hour - time.Minute)
	if m = ts.getJSON(t, "/api/display", timer); m["filename"] != sleeping || m["refresh_rate"] != float64(60) {
		t.Errorf("a minute before the end: %v; want the Sleeping screen and 60 s", m)
	}
	if item, started := cursor(); item != itemBefore || started != startedBefore {
		t.Errorf("the cursor moved late in the window: item %d started %d", item, started)
	}

	// After it, the playlist shows again. The first item's 10 minutes are
	// long up, so the cursor moves on to the second.
	ts.now = ts.now.Add(time.Minute)
	m = ts.getJSON(t, "/api/display", timer)
	if m["filename"] != imgSecond || m["refresh_rate"] == nil {
		t.Errorf("after the window: %v; want the second item's render", m)
	}
	if item, _ := cursor(); item == itemBefore {
		t.Error("the cursor did not move after the window")
	}
	// The device sleeps again at 15:00 tomorrow, which is far off, so the
	// refresh is the rest of the item. The items alternate every 10 minutes
	// from 14:55, so the second started at 17:25.
	if m["refresh_rate"] != float64(5*60) {
		t.Errorf("after the window: refresh %v, want the 300 s the item has left", m["refresh_rate"])
	}

	// Clearing the window wakes the device into its playlist at its next wake.
	none := ""
	ts.setDevice(t, device.Change{Sleep: &none})
	ts.now = time.Date(2026, 11, 13, 15, 40, 0, 0, time.FixedZone("tokyo", 9*3600))
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] == sleeping {
		t.Errorf("a device with no window got the Sleeping screen: %v", m)
	}
}

// TestDisplaySleepNoPlaylist checks that a device with no playlist items
// still gets the Sleeping screen in its window, and is woken at its start.
func TestDisplaySleepNoPlaylist(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.now = time.Date(2026, 11, 13, 12, 0, 0, 0, testZone)
	key := ts.register(t)
	window := "12:10-14:00"
	ts.setDevice(t, device.Change{Sleep: &window})
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"

	// Awake at 12:00, the refresh is cut from the usual 900 s to the 10
	// minutes until it sleeps.
	if m := ts.getJSON(t, "/api/display", timer); m["refresh_rate"] != float64(10*60) {
		t.Errorf("before the window: %v; want a refresh of 600 s", m)
	}
	ts.now = ts.now.Add(55 * time.Minute)
	m := ts.getJSON(t, "/api/display", timer)
	if m["filename"] != ts.sleepingScreen(t, "14:00") || m["refresh_rate"] != float64(65*60) {
		t.Errorf("in the window: %v; want the Sleeping screen and 3900 s", m)
	}
}

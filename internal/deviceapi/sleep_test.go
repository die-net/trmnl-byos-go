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
// a timer wake gets the Sleeping screen and a refresh to the window's end,
// and its playlist cursor does not move. A press there gets what the device
// would be shown awake, and its next timer wake sleeps again. After it, the
// playlist shows again. The server's zone is UTC+5:30 and the device's is
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
	// A timer wake in the window leaves the cursor where it was.
	if item, started := cursor(); item != itemBefore || started != startedBefore {
		t.Errorf("the cursor moved in the window: item %d started %d, was item %d started %d", item, started, itemBefore, startedBefore)
	}
	// A press wakes the device for one screen: what it would be shown awake,
	// the second item, as a press moves on, with the second item's 10
	// minutes and not the window's end.
	m = ts.getJSON(t, "/api/display", press)
	if m["filename"] != imgSecond || m["refresh_rate"] != float64(10*60) {
		t.Errorf("a press in the window: %v; want the second item and 600 s", m)
	}
	itemPressed, startedPressed := cursor()
	if itemPressed == itemBefore {
		t.Error("a press in the window did not move the cursor")
	}
	// Its next timer wake, still in the window, puts it back to sleep, to
	// the window's end, and leaves the cursor.
	ts.now = ts.now.Add(10 * time.Minute)
	if m = ts.getJSON(t, "/api/display", timer); m["filename"] != sleeping || m["refresh_rate"] != float64(2*3600-10*60) {
		t.Errorf("the timer wake after a press: %v; want the Sleeping screen and 6600 s", m)
	}
	// A wake a minute before the end sleeps only until it.
	ts.now = ts.now.Add(2*time.Hour - 11*time.Minute)
	if m = ts.getJSON(t, "/api/display", timer); m["filename"] != sleeping || m["refresh_rate"] != float64(60) {
		t.Errorf("a minute before the end: %v; want the Sleeping screen and 60 s", m)
	}
	if item, started := cursor(); item != itemPressed || started != startedPressed {
		t.Errorf("a timer wake moved the cursor in the window: item %d started %d", item, started)
	}

	// After it, the playlist shows again, with the cursor moved on from
	// where the press left it, as the item's time is long up.
	ts.now = ts.now.Add(time.Minute)
	m = ts.getJSON(t, "/api/display", timer)
	if f := m["filename"]; f != imgFirst && f != imgSecond {
		t.Errorf("after the window: %v; want a playlist item's render", m)
	}
	if item, started := cursor(); item == itemPressed && started == startedPressed {
		t.Error("the cursor did not move after the window")
	}
	// The device sleeps again at 15:00 tomorrow, which is far off, so the
	// refresh is the rest of the item. The items alternate every 10 minutes
	// from the press at 15:30, so one starts at 17:30 with all of its 600 s.
	if m["refresh_rate"] != float64(10*60) {
		t.Errorf("after the window: refresh %v, want the 600 s of an item that starts now", m["refresh_rate"])
	}

	// Clearing the window wakes the device into its playlist at its next wake.
	none := ""
	ts.setDevice(t, device.Change{Sleep: &none})
	ts.now = time.Date(2026, 11, 13, 15, 40, 0, 0, time.FixedZone("tokyo", 9*3600))
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] == sleeping {
		t.Errorf("a device with no window got the Sleeping screen: %v", m)
	}
}

// TestDisplaySleepAtWindowEnd checks a timer wake that comes early, just
// before the window's end, with a clock that moves between reads, as a real
// one does. The device is shown the Sleeping screen and told to sleep only
// to the window's end. Reading the clock a second time, past the end, to
// work out the sleep would have told it to sleep until the next window, most
// of a day, with the Sleeping screen up.
func TestDisplaySleepAtWindowEnd(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key, first, _ := ts.playlistDevice(t)
	ts.saveRender(t, first, "")
	window := "23:00-06:30"
	ts.setDevice(t, device.Change{Sleep: &window})
	sleeping := ts.sleepingScreen(t, "06:30")
	// 06:29:29.93 on the server's clock, 40 ms later at each read.
	clock := time.Date(2026, 11, 13, 6, 29, 29, 930000000, testZone)
	ts.srv.now = func() time.Time {
		t := clock
		clock = clock.Add(40 * time.Millisecond)
		return t.UTC()
	}
	h := displayHeaders(testMAC, key)
	h["Update-Source"] = "timer"
	m := ts.getJSON(t, "/api/display", h)
	if m["filename"] != sleeping {
		t.Fatalf("%v; want the Sleeping screen, as the wake plus the slack is before 06:30", m)
	}
	if r, _ := m["refresh_rate"].(float64); r < 1 || r > 60 {
		t.Errorf("refresh_rate %v; want the seconds to 06:30, about 30", m["refresh_rate"])
	}
}

// TestDisplaySleepNoPlaylist checks that a device with no playlist items
// still gets the Sleeping screen in its window on a timer wake, and is woken
// at its start. A press in the window gets the Connected screen it gets
// awake, with the usual refresh.
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
	m := ts.getJSON(t, "/api/display", timer)
	if m["refresh_rate"] != float64(10*60) {
		t.Errorf("before the window: %v; want a refresh of 600 s", m)
	}
	connected := m["filename"]
	ts.now = ts.now.Add(55 * time.Minute)
	m = ts.getJSON(t, "/api/display", timer)
	if m["filename"] != ts.sleepingScreen(t, "14:00") || m["refresh_rate"] != float64(65*60) {
		t.Errorf("in the window: %v; want the Sleeping screen and 3900 s", m)
	}
	press := displayHeaders(testMAC, key)
	press["Update-Source"] = "button"
	if m := ts.getJSON(t, "/api/display", press); m["filename"] != connected || m["refresh_rate"] != float64(900) {
		t.Errorf("a press in the window: %v; want the Connected screen %v and 900 s", m, connected)
	}
}

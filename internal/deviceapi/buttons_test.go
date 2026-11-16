package deviceapi

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // So the zones the tests name load on any host.

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

// buttonDevice is testMAC registered with a playlist of three items of ten
// minutes each, each with a render of its own.
type buttonDevice struct {
	key string
	// devID is the device's ID, items are the playlist item IDs in order, and
	// images are the names of their renders.
	devID  int64
	items  []int64
	images []string
}

func (ts *testServer) buttonDevice(t *testing.T) buttonDevice {
	t.Helper()
	ctx := t.Context()
	b := buttonDevice{key: ts.register(t)}
	dev, err := ts.db.Read.GetDeviceByMAC(ctx, testMAC)
	if err != nil {
		t.Fatal(err)
	}
	b.devID = dev.ID
	rec, err := ts.db.Write.CreateRecipe(ctx, db.CreateRecipeParams{Name: "r", Source: "s", RefreshInterval: 3600, InstalledAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"One", "Two", "Three"} {
		inst, err := ts.db.Write.CreateInstance(ctx, db.CreateInstanceParams{RecipeID: rec.ID, Name: name, FieldValues: "{}", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := addItemTo(ctx, ts.db, "p", dev.FriendlyID, inst.ID); err != nil {
			t.Fatal(err)
		}
		b.images = append(b.images, ts.saveDeviceRender(t, inst.ID, 0, name, ""))
	}
	if b.items, err = ts.db.Read.ListPlaylistItemIDs(ctx, 1); err != nil || len(b.items) != 3 {
		t.Fatalf("items %v, %v", b.items, err)
	}
	return b
}

// cursorAt puts the device's cursor on item i, started a minute ago.
func (ts *testServer) cursorAt(t *testing.T, b buttonDevice, i int) {
	t.Helper()
	err := ts.db.Write.SetDeviceCursor(t.Context(), db.SetDeviceCursorParams{DeviceID: b.devID, ItemID: b.items[i], ItemStartedAt: ts.now.Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
}

// cursor returns the index in b.items of the item the device's cursor is
// on and when it started.
func (ts *testServer) cursor(t *testing.T, b buttonDevice) (int, int64) {
	t.Helper()
	cur, err := ts.db.Read.GetDeviceCursor(t.Context(), b.devID)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range b.items {
		if id == cur.ItemID {
			return i, cur.ItemStartedAt
		}
	}
	t.Fatalf("cursor on item %d, which is not in %v", cur.ItemID, b.items)
	return -1, 0
}

// TestDisplayButtonCommand checks that a button press moves the cursor as the
// device's command says, from each place in a playlist, with the new item's
// start time now, that a hidden item is stepped over, that a timer wake
// leaves the cursor alone whatever the command, and that the default is
// next.
func TestDisplayButtonCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		hide    int // An item to hide, or -1.
		from    int
		want    int
	}{
		{"", -1, 0, 1},
		{"", -1, 2, 0},
		{"next", -1, 1, 2},
		{"next", 1, 0, 2},
		{"previous", -1, 0, 2},
		{"previous", -1, 1, 0},
		{"previous", -1, 2, 1},
		{"previous", 1, 2, 0},
		{"previous", 1, 0, 2},
		{"first", -1, 0, 0},
		{"first", -1, 1, 0},
		{"first", -1, 2, 0},
		{"first", 0, 2, 1},
		{"refresh", -1, 0, 0},
		{"refresh", -1, 1, 1},
		{"refresh", -1, 2, 2},
	} {
		t.Run(fmt.Sprintf("%s from %d hiding %d", tc.command, tc.from, tc.hide), func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			b := ts.buttonDevice(t)
			ts.setDevice(t, device.Change{ButtonCommand: &tc.command})
			if tc.hide >= 0 {
				hidden := true
				if err := playlist.UpdateItem(t.Context(), ts.db, b.items[tc.hide], playlist.ItemUpdate{Hidden: &hidden}); err != nil {
					t.Fatal(err)
				}
			}
			ts.cursorAt(t, b, tc.from)
			m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, b.key))
			if m["filename"] != b.images[tc.want] {
				t.Errorf("press shows %v, want item %d's render", m["filename"], tc.want)
			}
			if i, started := ts.cursor(t, b); i != tc.want || started != ts.now.Unix() {
				t.Errorf("cursor on item %d started %d, want item %d started %d", i, started, tc.want, ts.now.Unix())
			}
			// The device sleeps for the ten minutes the new item has.
			if m["refresh_rate"] != float64(600) {
				t.Errorf("refresh_rate = %v, want 600", m["refresh_rate"])
			}
		})
	}

	// A timer wake does not apply the command.
	ts := newTestServer(t)
	b := ts.buttonDevice(t)
	command := "first"
	ts.setDevice(t, device.Change{ButtonCommand: &command})
	ts.cursorAt(t, b, 2)
	timer := displayHeaders(testMAC, b.key)
	timer["Update-Source"] = "timer"
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != b.images[2] {
		t.Errorf("timer wake shows %v, want item 2's render", m["filename"])
	}
	if i, _ := ts.cursor(t, b); i != 2 {
		t.Errorf("timer wake moved the cursor to item %d", i)
	}
}

// identifyScreen is the name of the Identify screen of the test device.
func (ts *testServer) identifyScreen(t *testing.T, name string) string {
	t.Helper()
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	lines := []screen.Line{{Text: "Identify", Size: 44, Bold: true}, {Text: dev.FriendlyID, Size: 64, Bold: true}}
	if name != "" {
		lines = append(lines, screen.Line{Text: name, Size: 28})
	}
	lines = append(lines, screen.Line{Text: testMAC, Size: 22})
	p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
	img, err := ts.srv.screen(screenProfile(p), lines)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// lastImage is the image the device's state says it was last sent, or "".
func (ts *testServer) lastImage(t *testing.T, b buttonDevice) string {
	t.Helper()
	var img string
	if err := ts.db.Reader.QueryRowContext(t.Context(), `SELECT last_image FROM device_state WHERE device_id = ?`, b.devID).Scan(&img); err != nil {
		t.Fatal(err)
	}
	return img
}

// lastRate returns the device's stored last_refresh_rate_sent, the interval
// the firmware keeps until an answer it applies a refresh_rate from.
func (ts *testServer) lastRate(t *testing.T, b buttonDevice) float64 {
	t.Helper()
	st, err := ts.db.Read.GetDeviceStatus(t.Context(), b.devID)
	if err != nil || st.LastRefreshRateSent == nil {
		t.Fatalf("device state: %+v, %v", st, err)
	}
	return float64(*st.LastRefreshRateSent)
}

// TestDisplaySpecialFunction checks the answer to each special function a
// device can ask for, with true for the configured one and with a name, and
// that every normal answer carries the configured function, rewind by
// default. Each answer but sleep sends and records the interval the device
// stored from its last normal answer, since the firmware applies no other.
func TestDisplaySpecialFunction(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	b := ts.buttonDevice(t)
	name := "Kitchen"
	ts.setDevice(t, device.Change{Name: &name})

	// A normal wake sends the default function, and saves what it showed.
	ts.cursorAt(t, b, 1)
	timer := displayHeaders(testMAC, b.key)
	timer["Update-Source"] = "timer"
	m := ts.getJSON(t, "/api/display", timer)
	if m["special_function"] != "rewind" || m["action"] != nil || m["filename"] != b.images[1] {
		t.Fatalf("normal answer: %v; want special_function rewind, no action, item 1's render", m)
	}
	if got := ts.lastImage(t, b); got != b.images[1] {
		t.Fatalf("last image %q, want item 1's render", got)
	}

	// Item 1 started a minute ago and lasts 10, so the device stored 540 s.
	if m["refresh_rate"] != float64(540) {
		t.Fatalf("normal answer: %v; want refresh 540", m)
	}
	// stored is the interval the device keeps, so a special function other
	// than sleep is answered and recorded with it.
	var stored float64
	ask := func(value string) map[string]any {
		t.Helper()
		stored = ts.lastRate(t, b)
		h := displayHeaders(testMAC, b.key) // A button wake, as a double-click is.
		h["special_function"] = value
		return ts.getJSON(t, "/api/display", h)
	}
	kept := func(name string, got map[string]any) {
		t.Helper()
		if got["refresh_rate"] != stored {
			t.Errorf("%s: refresh %v, want %v, the interval the device stored", name, got["refresh_rate"], stored)
		}
		if r := ts.lastRate(t, b); r != stored {
			t.Errorf("%s: recorded refresh %v, want %v", name, r, stored)
		}
	}
	// Without an image, the device keeps what it shows and the cursor stays.
	for _, tc := range []struct {
		ask, action string
	}{
		{"true", "rewind"}, // The default, rewind.
		{"rewind", "rewind"},
		{"add_wifi", "add_wifi"},
		{"send_to_me", "send_to_me"}, // The device redraws its last image.
	} {
		got := ask(tc.ask)
		if got["status"] != float64(0) || got["action"] != tc.action || got["image_url"] != nil || got["filename"] != nil ||
			got["special_function"] != "rewind" {
			t.Errorf("%s: %v; want action %s, no image and special_function rewind", tc.ask, got, tc.action)
		}
		kept(tc.ask, got)
		if i, started := ts.cursor(t, b); i != 1 || started != ts.now.Add(-time.Minute).Unix() {
			t.Errorf("%s moved the cursor to item %d started %d", tc.ask, i, started)
		}
		if got := ts.lastImage(t, b); got != b.images[1] {
			t.Errorf("%s: last image %q, want item 1's render, kept", tc.ask, got)
		}
	}

	// guest_mode and toggle_hold, from the TRMNL X's menu, are answered as a
	// timer wake with the action guest_mode, which the firmware needs to draw
	// the image: the screen the device shows, with the 540 s left of item 1,
	// which the firmware applies. The cursor stays, though a double-click
	// comes as a button wake.
	for _, v := range []string{"guest_mode", "toggle_hold"} {
		got := ask(v)
		if got["action"] != "guest_mode" || got["filename"] != b.images[1] || got["image_url"] == nil ||
			got["refresh_rate"] != float64(540) || got["special_function"] != "rewind" {
			t.Errorf("%s: %v; want action guest_mode, item 1's render and 540 s", v, got)
		}
		if i, started := ts.cursor(t, b); i != 1 || started != ts.now.Add(-time.Minute).Unix() {
			t.Errorf("%s moved the cursor to item %d started %d", v, i, started)
		}
		if r := ts.lastRate(t, b); r != 540 {
			t.Errorf("%s: recorded refresh %v, want the 540 s sent", v, r)
		}
	}

	// A function that is not known, and none, are normal wakes.
	for _, v := range []string{"bogus", "none", "false", ""} {
		got := ask(v)
		if got["action"] != nil || got["filename"] != b.images[2] {
			t.Errorf("special_function %q: %v; want a normal press to item 2", v, got)
		}
		ts.cursorAt(t, b, 1)
	}

	// Identify draws the same screen every time, and leaves the cursor.
	want := ts.identifyScreen(t, "Kitchen")
	for _, v := range []string{"identify", "IDENTIFY"} {
		got := ask(v)
		if got["action"] != "identify" || got["filename"] != want || got["image_url"] == nil || got["special_function"] != "rewind" {
			t.Errorf("%s: %v; want the Identify screen %s", v, got, want)
		}
		kept(v, got)
		ts.fetchImage(t, got["image_url"].(string), want)
		if i, _ := ts.cursor(t, b); i != 1 {
			t.Errorf("%s moved the cursor to item %d", v, i)
		}
	}
	if got := ts.lastImage(t, b); got != want {
		t.Errorf("last image %q, want the Identify screen", got)
	}
	empty := ""
	ts.setDevice(t, device.Change{Name: &empty})
	if got := ask("identify"); got["filename"] != ts.identifyScreen(t, "") || got["filename"] == want {
		t.Errorf("identify with no name: %v; want a screen without it", got)
	}

	// Restart playlist moves the cursor to the first item and shows it.
	ts.cursorAt(t, b, 2)
	m = ask("restart_playlist")
	if m["action"] != "restart_playlist" || m["filename"] != b.images[0] || m["image_url"] == nil || m["special_function"] != "rewind" {
		t.Errorf("restart_playlist: %v; want item 0's render", m)
	}
	if i, started := ts.cursor(t, b); i != 0 || started != ts.now.Unix() {
		t.Errorf("restart_playlist left the cursor on item %d started %d", i, started)
	}
	kept("restart_playlist", m)

	// The configured function is sent on every answer, and a normal answer
	// is what the device saves it from, so true asks for it from then on.
	configured := "identify"
	ts.setDevice(t, device.Change{SpecialFunction: &configured})
	if got := ask("add_wifi"); got["action"] != "add_wifi" || got["special_function"] != "identify" {
		t.Errorf("add_wifi with identify configured: %v", got)
	}
	if got := ts.getJSON(t, "/api/display", timer); got["special_function"] != "identify" || got["action"] != nil {
		t.Errorf("normal answer with identify configured: %v", got)
	}
	if got := ask("true"); got["action"] != "identify" || got["special_function"] != "identify" {
		t.Errorf("true after a normal answer sent identify: %v", got)
	}
}

// TestDisplaySpecialFunctionSaved checks that true is answered with the
// function the device saved from its last normal answer, not one configured
// since, which the firmware has not seen: with its action, the interval the
// device stored, the cursor where it was and the last image kept. Once a
// normal answer sends the new function, true asks for that. With none
// recorded as sent, true runs a function the server does not know, which
// draws nothing, so it gets no image and the cursor stays, even at an item's
// end.
func TestDisplaySpecialFunctionSaved(t *testing.T) {
	t.Parallel()
	for _, since := range []string{"sleep", "restart_playlist", "identify", "add_wifi", "none"} {
		t.Run(since, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			ts.now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
			b := ts.buttonDevice(t)
			ts.cursorAt(t, b, 1)
			timer := displayHeaders(testMAC, b.key)
			timer["Update-Source"] = "timer"
			if m := ts.getJSON(t, "/api/display", timer); m["special_function"] != "rewind" || m["refresh_rate"] != float64(540) {
				t.Fatalf("normal answer: %v; want rewind and 540 s", m)
			}
			ts.setDevice(t, device.Change{SpecialFunction: &since})
			double := displayHeaders(testMAC, b.key)
			double["special_function"] = "true"
			unchanged := func(what string, m map[string]any) {
				t.Helper()
				if m["refresh_rate"] != float64(540) || ts.lastRate(t, b) != 540 {
					t.Errorf("%s: refresh %v, recorded %v; want the 540 s the device keeps", what, m["refresh_rate"], ts.lastRate(t, b))
				}
				if i, started := ts.cursor(t, b); i != 1 || started != ts.now.Add(-time.Minute).Unix() {
					t.Errorf("%s moved the cursor to item %d started %d", what, i, started)
				}
			}

			m := ts.getJSON(t, "/api/display", double)
			if m["action"] != "rewind" || m["image_url"] != nil || m["special_function"] != since {
				t.Errorf("true with %s configured since: %v; want action rewind and no image", since, m)
			}
			unchanged("true", m)
			if got := ts.lastImage(t, b); got != b.images[1] {
				t.Errorf("last image %q, want item 1's render, kept", got)
			}
			// The firmware saves no function from an answer to a double-click
			// or a named function, though each carries the new one, so the
			// next true still runs rewind.
			for i, v := range []string{"true", "add_wifi", "true"} {
				double["special_function"] = v
				want := v
				if v == "true" {
					want = "rewind"
				}
				if got := ts.getJSON(t, "/api/display", double); got["action"] != want {
					t.Errorf("wake %d, %s, with %s configured since: %v; want action %s", i, v, since, got, want)
				}
			}
			double["special_function"] = "true"

			// A normal answer sends the new function, which the device saves.
			ts.getJSON(t, "/api/display", timer)
			ts.cursorAt(t, b, 1)
			m = ts.getJSON(t, "/api/display", double)
			if since == "none" {
				if m["action"] != nil || m["image_url"] != nil || m["filename"] != nil {
					t.Errorf("true with none sent: %v; want no action and no image", m)
				}
				unchanged("true with none sent", m)
				if got := ts.lastImage(t, b); got != b.images[1] {
					t.Errorf("true with none sent: last image %q, want item 1's render, kept", got)
				}
				// Five seconds before item 1 ends, inside the slack a timer
				// wake advances in, the cursor still stays.
				started := ts.now.Add(-10*time.Minute + 5*time.Second).Unix()
				if err := ts.db.Write.SetDeviceCursor(t.Context(), db.SetDeviceCursorParams{DeviceID: b.devID, ItemID: b.items[1], ItemStartedAt: started}); err != nil {
					t.Fatal(err)
				}
				m = ts.getJSON(t, "/api/display", double)
				if i, at := ts.cursor(t, b); i != 1 || at != started || m["image_url"] != nil {
					t.Errorf("true with none sent at item 1's end: %v; cursor on item %d started %d, want item 1 started %d and no image", m, i, at, started)
				}
				if got := ts.lastImage(t, b); got != b.images[1] {
					t.Errorf("true with none sent at item 1's end: last image %q, want item 1's render, kept", got)
				}
			} else if m["action"] != since {
				t.Errorf("true after a normal answer sent %s: %v", since, m)
			}
		})
	}
}

// TestDisplayFirmwareOnlyAtNormalWake checks that a wake with a
// special_function header is offered no firmware, so an update never starts
// from a double-click or the TRMNL X menu, and that such wakes do not use up
// the canary's offers, which the next normal wakes get.
func TestDisplayFirmwareOnlyAtNormalWake(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	none := "none"
	if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{UpdatesEnabled: ptr(true), SpecialFunction: &none}); err != nil {
		t.Fatal(err)
	}
	// Records the version, and none as the function sent.
	ts.getJSON(t, "/api/display", firmwareDisplayHeaders(key, "1.8.17"))
	fw := ts.addFirmware(t, []byte("image"))
	if _, err := firmware.StartRollout(t.Context(), ts.db, fw.ID, testMAC, ts.now); err != nil {
		t.Fatal(err)
	}
	// true gets no action, and bogus is answered as a normal wake, but the
	// firmware runs neither as one.
	for _, v := range []string{"true", "bogus", "identify", "guest_mode", "rewind", "true", "bogus"} {
		h := firmwareDisplayHeaders(key, "1.8.17")
		h["special_function"] = v
		if m := ts.getJSON(t, "/api/display", h); m["update_firmware"] == true || m["firmware_url"] != nil {
			t.Errorf("%s: offered firmware: %v", v, m)
		}
	}
	for i := 1; i <= firmware.MaxOffers; i++ {
		if m := ts.getJSON(t, "/api/display", firmwareDisplayHeaders(key, "1.8.17")); m["update_firmware"] != true {
			t.Fatalf("normal wake %d: %v; want an offer", i, m)
		}
	}
}

// TestDisplaySpecialFunctionSleep checks the refresh_rate of the sleep
// function: the time to the next end of the device's sleep window in its own
// zone, whether or not it is in the window now, and 8 hours with no window.
func TestDisplaySpecialFunctionSleep(t *testing.T) {
	t.Parallel()
	tokyo := time.FixedZone("tokyo", 9*3600)
	for _, tc := range []struct {
		name   string
		window string
		now    time.Time
		want   int64
	}{
		{"no window", "", time.Date(2026, 11, 13, 12, 0, 0, 0, tokyo), 8 * 3600},
		{"before the window", "23:00-06:30", time.Date(2026, 11, 13, 12, 0, 0, 0, tokyo), 18*3600 + 30*60},
		{"in the window before midnight", "23:00-06:30", time.Date(2026, 11, 13, 23, 30, 0, 0, tokyo), 7 * 3600},
		{"in the window after midnight", "23:00-06:30", time.Date(2026, 11, 14, 2, 0, 0, 0, tokyo), 4*3600 + 30*60},
		{"at the end", "23:00-06:30", time.Date(2026, 11, 14, 6, 30, 0, 0, tokyo), 24 * 3600},
		{"a window within a day", "13:00-14:00", time.Date(2026, 11, 13, 13, 15, 0, 0, tokyo), 45 * 60},
		{"after a window within a day", "13:00-14:00", time.Date(2026, 11, 13, 15, 0, 0, 0, tokyo), 23 * 3600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			ts.now = tc.now
			key := ts.register(t)
			zone, sleepFn := "Asia/Tokyo", "sleep"
			c := device.Change{TimeZone: &zone, SpecialFunction: &sleepFn}
			if tc.window != "" {
				c.Sleep = &tc.window
			}
			ts.setDevice(t, c)
			h := displayHeaders(testMAC, key)
			h["special_function"] = "true"
			m := ts.getJSON(t, "/api/display", h)
			if m["action"] != "sleep" || m["refresh_rate"] != float64(tc.want) || m["image_url"] != nil || m["special_function"] != "sleep" {
				t.Errorf("%v; want action sleep with refresh_rate %d and no image", m, tc.want)
			}
		})
	}
}

// TestDisplaySpecialFunctionAsleep checks that a restart_playlist in the
// device's sleep window wakes it for one screen, as a press does: it is
// shown the first item, with the cursor there. The firmware applies no
// refresh_rate to it, so the answer sends and records the Sleeping answer's
// rate, which is what the device sleeps from the double-click.
func TestDisplaySpecialFunctionAsleep(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.now = time.Date(2026, 11, 13, 23, 30, 0, 0, testZone)
	b := ts.buttonDevice(t)
	window := "23:00-06:30"
	ts.setDevice(t, device.Change{Sleep: &window})
	ts.cursorAt(t, b, 2)
	timer := displayHeaders(testMAC, b.key)
	timer["Update-Source"] = "timer"
	m := ts.getJSON(t, "/api/display", timer)
	if m["filename"] != ts.sleepingScreen(t, "06:30") || m["refresh_rate"] != float64(7*3600) {
		t.Fatalf("the timer wake in the window: %v; want the Sleeping screen and 25200 s", m)
	}

	ts.now = ts.now.Add(10 * time.Minute)
	h := displayHeaders(testMAC, b.key)
	h["special_function"] = "restart_playlist"
	m = ts.getJSON(t, "/api/display", h)
	if m["action"] != "restart_playlist" || m["filename"] != b.images[0] || m["refresh_rate"] != float64(7*3600) {
		t.Errorf("%v; want the first item and the Sleeping answer's 25200 s", m)
	}
	if i, started := ts.cursor(t, b); i != 0 || started != ts.now.Unix() {
		t.Errorf("the cursor is at item %d started %d, want item 0 started now", i, started)
	}
	if r := ts.lastRate(t, b); r != float64(7*3600) {
		t.Errorf("recorded refresh %v, want the 25200 s the device keeps", r)
	}
}

// TestDisplaySpecialFunctionBlocked checks that a blocked device that asks
// for a special function gets the Blocked answer, with no action.
func TestDisplaySpecialFunctionBlocked(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	if _, err := device.Block(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
		t.Fatal(err)
	}
	h := displayHeaders(testMAC, key)
	h["special_function"] = "true"
	for _, v := range []string{"true", "identify", "sleep", "rewind"} {
		h["special_function"] = v
		m := ts.getJSON(t, "/api/display", h)
		if m["action"] != nil || m["refresh_rate"] != float64(blockedRefresh) || m["special_function"] != "none" || m["image_url"] == nil {
			t.Errorf("%s: %v; want the Blocked answer with no action", v, m)
		}
	}
}

// failStatus is a db.DBTX that fails every GetDeviceStatus and passes every
// other query to the database.
type failStatus struct{ db.DBTX }

// QueryRowContext fails GetDeviceStatus by running it with a context that
// is already canceled.
func (f failStatus) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.HasPrefix(query, "-- name: GetDeviceStatus ") {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		ctx = canceled
	}
	return f.DBTX.QueryRowContext(ctx, query, args...)
}

// TestDisplayFailedKeepsSpecialFunction checks that a failure inside
// /api/display for a known device carries the special function recorded as
// sent, or the configured one with none recorded, so the firmware does not
// save none and turn its double-click off, and saves what the server
// expects. One before the device is known sends none.
func TestDisplayFailedKeepsSpecialFunction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, ts *testServer)
		// want is the function sent with none recorded. With rewind
		// recorded, a known device is sent rewind.
		want string
	}{
		{"render screen", func(t *testing.T, ts *testServer) {
			if err := os.RemoveAll(ts.imagesDir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ts.imagesDir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "identify"},
		{"save state", func(t *testing.T, ts *testServer) {
			_, err := ts.db.Writer.ExecContext(t.Context(),
				`CREATE TRIGGER fail_state BEFORE INSERT ON device_state BEGIN SELECT RAISE(ABORT, 'test failure'); END`)
			if err != nil {
				t.Fatal(err)
			}
		}, "identify"},
		// What the device saved is not known, so it is sent none.
		{"get device state", func(_ *testing.T, ts *testServer) {
			ts.db.Read = db.New(failStatus{ts.db.Reader})
		}, "none"},
		// The device cannot be found, so the answer is for an unknown one.
		{"database", func(_ *testing.T, ts *testServer) { _ = ts.db.Close() }, "none"},
	} {
		for _, recorded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s recorded %v", tc.name, recorded), func(t *testing.T) {
				t.Parallel()
				ts := newTestServer(t)
				key := ts.register(t)
				want := tc.want
				if recorded {
					// A normal answer records rewind, the default, as sent.
					ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))
					if want != device.FuncNone {
						want = "rewind"
					}
				}
				configured := "identify"
				ts.setDevice(t, device.Change{SpecialFunction: &configured})
				tc.breakIt(t, ts)
				m := ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))
				if m["refresh_rate"] != float64(retryRefresh) || m["image_url"] != nil || m["special_function"] != want {
					t.Errorf("%v; want the failure answer with special_function %s", m, want)
				}
			})
		}
	}
}

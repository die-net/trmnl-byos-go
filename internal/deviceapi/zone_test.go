package deviceapi

import (
	"testing"
	"time"
	_ "time/tzdata" // So the zones the tests name load on any host.

	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// setDevice changes the test device's settings.
func (ts *testServer) setDevice(t *testing.T, c device.Change) {
	t.Helper()
	if _, err := device.Update(t.Context(), ts.db, testMAC, c); err != nil {
		t.Fatal(err)
	}
}

// TestDisplayDeviceZone checks that a device's schedules are read in its own
// time zone, not the server's: a playlist window that is closed on the
// server's clock but open on the device's is shown, and the reverse, and
// the render it is shown is the one made for its zone. The server's zone is
// UTC+5:30 and the device's is Tokyo, UTC+9, so at 12:00 on the server's
// clock it is 15:30 in Tokyo.
func TestDisplayDeviceZone(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.now = time.Date(2026, 11, 13, 12, 0, 0, 0, testZone)
	key, first, second := ts.playlistDevice(t)
	imgServer := ts.saveRenderAt(t, first, 0, "server zone", "", testZone.String(), "en")
	imgTokyo := ts.saveRenderAt(t, first, 0, "tokyo", "", "Asia/Tokyo", "en")
	ts.saveRender(t, second, "")
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"
	shown := func() string {
		t.Helper()
		m := ts.getJSON(t, "/api/display", timer)
		name, _ := m["filename"].(string)
		return name
	}
	tokyo, none := "Asia/Tokyo", ""
	closed := ts.nothingScheduled(t)

	// Open from 12:00 to 13:00 on the server's clock, which is 15:30 in Tokyo.
	ts.scheduleWindow(t, 12*60, 13*60)
	if got := shown(); got != imgServer {
		t.Fatalf("no zone, window open on the server's clock: shown %q, want the server zone's render %q", got, imgServer)
	}
	ts.setDevice(t, device.Change{TimeZone: &tokyo})
	if got := shown(); got != closed {
		t.Fatalf("Tokyo, window closed on its clock: shown %q, want the Nothing scheduled now screen %q", got, closed)
	}

	// Open from 15:00 to 16:00: open in Tokyo, closed on the server's clock.
	ts.scheduleWindow(t, 15*60, 16*60)
	if got := shown(); got != imgTokyo {
		t.Fatalf("Tokyo, window open on its clock: shown %q, want Tokyo's render %q", got, imgTokyo)
	}
	ts.setDevice(t, device.Change{TimeZone: &none})
	if got := shown(); got != closed {
		t.Fatalf("no zone, window closed on the server's clock: shown %q, want the Nothing scheduled now screen %q", got, closed)
	}

	// The wake is timed by the device's clock too. In Tokyo the window
	// closes at 16:00, 30 minutes from 15:30.
	ts.setDevice(t, device.Change{TimeZone: &tokyo})
	m := ts.getJSON(t, "/api/display", timer)
	if m["filename"] != imgTokyo || m["refresh_rate"] != float64(600) {
		t.Errorf("Tokyo: %v; want Tokyo's render and the item's 600 s", m)
	}
	ts.scheduleWindow(t, 15*60, 15*60+35)
	ts.now = ts.now.Add(time.Minute)
	if m := ts.getJSON(t, "/api/display", timer); m["refresh_rate"] != float64(4*60) {
		t.Errorf("window closing at 15:35 in Tokyo, now 15:31: %v; want refresh 240", m)
	}
}

// TestDisplayDeviceLocale checks that a device is shown the render made for
// its locale, or the server's locale setting while it has none, and never a
// render made for another locale, and that a change of either takes effect
// at the next wake.
func TestDisplayDeviceLocale(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key, first, _ := ts.playlistDevice(t)
	zone := testZone.String()
	images := map[string]string{}
	for _, locale := range []string{"en", "de", "fr"} {
		images[locale] = ts.saveRenderAt(t, first, 0, locale, "", zone, locale)
	}
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"
	shown := func() string {
		t.Helper()
		m := ts.getJSON(t, "/api/display", timer)
		name, _ := m["filename"].(string)
		return name
	}
	for _, tc := range []struct {
		name   string
		device *string // The device's locale, nil to leave it.
		server string  // The server's locale setting, "" to leave it.
		want   string  // The locale of the render shown.
	}{
		{"neither sets one", nil, "", "en"},
		{"the device's", new("de"), "", "de"},
		{"the server's, which the device does not override", new(""), "fr", "fr"},
		{"the device's over the server's", new("de"), "", "de"},
		{"the server's again", new(""), "", "fr"},
		{"the server's default again", nil, "en", "en"},
	} {
		if tc.device != nil {
			ts.setDevice(t, device.Change{Locale: tc.device})
		}
		if tc.server != "" {
			if err := settings.Set(t.Context(), ts.db, settings.KeyLocale, tc.server, ts.now); err != nil {
				t.Fatal(err)
			}
		}
		if got := shown(); got != images[tc.want] {
			t.Errorf("%s: shown %q, want the %s render %q", tc.name, got, tc.want, images[tc.want])
		}
	}

	// A locale with no render gets the Rendering screen, not another
	// locale's render.
	ts.setDevice(t, device.Change{Locale: new("ja")})
	got := shown()
	for locale, name := range images {
		if got == name {
			t.Errorf("a device with locale ja was shown the %s render", locale)
		}
	}
}

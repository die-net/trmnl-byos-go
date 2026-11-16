package adminui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// storeZone stores a time zone and locale for the test device without a
// check, as a value that stopped applying is.
func storeZone(t *testing.T, tu *testUI, zone, locale string) {
	t.Helper()
	err := tu.db.Write.UpdateDeviceSettings(t.Context(), db.UpdateDeviceSettingsParams{Name: "Kitchen", TimeZone: zone, Locale: locale, ID: 1})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDeviceZoneOnPage checks the zone and locale the page says are in use,
// the line that names the zones, and the form's fields.
func TestDeviceZoneOnPage(t *testing.T) {
	t.Parallel()
	const note = `<p class="rel zone-note" id="zone-note" data-device-zone=`
	for _, tc := range []struct {
		name         string
		zone, locale string
		// serverLocale is the server's locale setting, or "" for its
		// default.
		serverLocale string
		want         []string
	}{
		{
			name: "nothing set",
			want: []string{
				`<dt>Time zone</dt><dd>TST <span class="rel">(the server&#39;s)</span></dd>`, `<dt>Locale</dt><dd>en <span class="rel">(the server&#39;s)</span></dd>`,
				// The device is in the server's zone, so the line is hidden
				// unless the script finds the browser in another.
				note + `"TST" hidden>The times above are in TST, the server's time zone. Schedules are read in this device's time zone, TST.</p>`,
				`<input id="time_zone" name="time_zone" type="text" value="" autocomplete="off" autocapitalize="none" spellcheck="false" placeholder="TST"`,
				"Leave empty for the server's, TST.", `<option value="" selected>The server&#39;s: en</option>`, `<option value="pt-BR">pt-BR</option>`,
			},
		},
		{
			name: "zone and locale set", zone: "Europe/Berlin", locale: "de", serverLocale: "fr",
			want: []string{
				`<dt>Time zone</dt><dd>Europe/Berlin</dd>`, `<dt>Locale</dt><dd>de</dd>`,
				note + `"Europe/Berlin">The times above are in TST, the server's time zone. Schedules are read in this device's time zone, Europe/Berlin.</p>`,
				`name="time_zone" type="text" value="Europe/Berlin"`, `<option value="">The server&#39;s: fr</option>`, `<option value="de" selected>de</option>`,
			},
		},
		{
			name: "stored values that no longer apply", zone: "Nowhere/Gone", locale: "xx",
			want: []string{
				`<dt>Time zone</dt><dd>TST <span class="rel">(the server&#39;s, since &#34;Nowhere/Gone&#34; does not load on this server)</span></dd>`,
				`<dt>Locale</dt><dd>en <span class="rel">(the server&#39;s, since &#34;xx&#34; is not a locale the server has)</span></dd>`,
				`name="time_zone" type="text" value="Nowhere/Gone"`, `<option value="xx" selected>xx (not a locale the server has)</option>`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			if tc.zone != "" || tc.locale != "" {
				storeZone(t, tu, tc.zone, tc.locale)
			}
			if tc.serverLocale != "" {
				if err := settings.Set(t.Context(), tu.db, settings.KeyLocale, tc.serverLocale, tu.now); err != nil {
					t.Fatal(err)
				}
			}
			body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
		})
	}
}

func TestDeviceZoneSave(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// zone and locale are stored before the post, without a check.
		zone, locale string
		form         url.Values
		status       int
		wantZone     string
		wantLocale   string
		body         []string
	}{
		{name: "zone and locale", form: url.Values{"time_zone": {" Europe/Berlin "}, "locale": {"de"}}, status: 303, wantZone: "Europe/Berlin", wantLocale: "de"},
		{name: "back to the server's", zone: "Europe/Berlin", locale: "de", form: url.Values{}, status: 303},
		{
			name: "zone that does not load", form: url.Values{"name": {"Hall"}, "time_zone": {"Mars/<Olympus>"}, "locale": {"de"}}, status: 400,
			body: []string{`role="alert">Time zone &#34;Mars/&lt;Olympus&gt;&#34;`, `value="Mars/&lt;Olympus&gt;"`, `value="Hall"`, `<option value="de" selected>`},
		},
		{
			name: "Local", form: url.Values{"time_zone": {"Local"}}, status: 400,
			body: []string{"is the server&#39;s own zone, so leave the zone empty instead"},
		},
		{
			name: "locale the server does not have", form: url.Values{"locale": {"xx"}}, status: 400,
			body: []string{`role="alert">Locale &#34;xx&#34; is not one of`, `<option value="xx" selected>xx (not a locale the server has)</option>`},
		},
		{
			// The form posts stored values back as they are, which must
			// not block a rename.
			name: "rename with stored values that no longer apply", zone: "Nowhere/Gone", locale: "xx",
			form: url.Values{"name": {"Hall"}, "time_zone": {"Nowhere/Gone"}, "locale": {"xx"}}, status: 303, wantZone: "Nowhere/Gone", wantLocale: "xx",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			if tc.zone != "" || tc.locale != "" {
				storeZone(t, tu, tc.zone, tc.locale)
			}
			w := tu.post(t, tu.signIn(t), "/devices/ABC123", withState(t, tu, tc.form))
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			body := w.Body.String()
			for _, want := range tc.body {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "<Olympus>") {
				t.Error("a posted value was not escaped")
			}
			wantZone, wantLocale, wantName := tc.wantZone, tc.wantLocale, "Kitchen"
			if tc.status != http.StatusSeeOther {
				wantZone, wantLocale = tc.zone, tc.locale
			} else if tc.form.Has("name") {
				wantName = tc.form.Get("name")
			} else {
				wantName = ""
			}
			if dev := testDevice(t, tu); dev.TimeZone != wantZone || dev.Locale != wantLocale || dev.Name != wantName {
				t.Errorf("stored zone %q, locale %q, name %q; want %q, %q, %q", dev.TimeZone, dev.Locale, dev.Name, wantZone, wantLocale, wantName)
			}
		})
	}
}

// TestDeviceZoneStale checks that a stale save says what zone and locale
// are stored now.
func TestDeviceZoneStale(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	form := withState(t, tu, url.Values{"name": {"Kitchen"}, "time_zone": {"Europe/Berlin"}, "locale": {"de"}})
	zone, loc := "Asia/Tokyo", "ja"
	if _, err := device.Update(t.Context(), tu.db, testMAC, device.Change{TimeZone: &zone, Locale: &loc}); err != nil {
		t.Fatal(err)
	}
	w := tu.post(t, tu.signIn(t), "/devices/ABC123", form)
	want := `Stored now: the time zone is &#34;Asia/Tokyo&#34;, the locale is &#34;ja&#34;. Save again to use the values below.`
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), want) {
		t.Fatalf("status %d, want 409 with %q: %s", w.Code, want, w.Body)
	}
	if !strings.Contains(w.Body.String(), `name="time_zone" type="text" value="Europe/Berlin"`) || !strings.Contains(w.Body.String(), `<option value="de" selected>`) {
		t.Errorf("the page lost the typed zone or locale:\n%s", w.Body)
	}
	if dev := testDevice(t, tu); dev.TimeZone != "Asia/Tokyo" || dev.Locale != "ja" {
		t.Errorf("a stale save changed the device: %+v", dev)
	}
}

// TestDeviceZoneStaleZoneOnly checks the clause for a zone that changed
// alone, and for one that was cleared.
func TestDeviceZoneStaleZoneOnly(t *testing.T) {
	t.Parallel()
	stored := deviceForm{Name: "Kitchen", Zone: "Asia/Tokyo"}
	for _, tc := range []struct {
		form deviceForm
		want string
	}{
		{deviceForm{Name: "Kitchen", Zone: "Europe/Berlin"}, ` Stored now: the time zone is "Asia/Tokyo". Save again to use the values below.`},
		{deviceForm{Name: "Kitchen", Zone: "Asia/Tokyo"}, " The values below are what is stored now."},
	} {
		if got := settingsChangedText(&tc.form, &stored); got != settingsChanged+tc.want {
			t.Errorf("zone %q: %q, want %q", tc.form.Zone, got, settingsChanged+tc.want)
		}
	}
	cleared := deviceForm{Name: "Kitchen"}
	if got, want := settingsChangedText(&stored, &cleared), settingsChanged+" Stored now: the time zone is the server's. Save again to use the values below."; got != want {
		t.Errorf("cleared zone: %q, want %q", got, want)
	}
}

// TestDeviceSettingsLeftOut checks that a post that leaves a setting out
// keeps it, as a form from a release without the field does. Only the
// checkbox is read as off when it is left out, as a browser posts it.
func TestDeviceSettingsLeftOut(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	zone, loc, pin, model, palette, on := "Europe/Berlin", "de", "1.8.19", "og_bwry", "color-4bwry", true
	_, err := device.Update(t.Context(), tu.db, testMAC, device.Change{
		TimeZone: &zone, Locale: &loc, FwPin: &pin, Profile: &model, Palette: &palette, UpdatesEnabled: &on,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := tu.signIn(t)
	form := withState(t, tu, url.Values{"name": {"Hall"}})
	for _, name := range []string{"fw_pin", "profile", "palette", "time_zone", "locale"} {
		form.Del(name)
	}
	if w := tu.post(t, c, "/devices/ABC123", form); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	dev := testDevice(t, tu)
	if dev.Name != "Hall" || dev.TimeZone != zone || dev.Locale != loc || dev.FwPin != pin || dev.ProfileOverride != model || dev.PaletteOverride != palette {
		t.Errorf("a setting the post left out changed: %+v", dev)
	}
	if dev.UpdatesEnabled != 0 {
		t.Errorf("updates_enabled = %d, want 0 for a box left out", dev.UpdatesEnabled)
	}
	// A refused post shows what it left out as stored, so saving the page
	// that comes back keeps it too.
	form = withState(t, tu, url.Values{"name": {strings.Repeat("x", 65)}})
	for _, name := range []string{"fw_pin", "profile", "palette", "time_zone", "locale"} {
		form.Del(name)
	}
	w := tu.post(t, c, "/devices/ABC123", form)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
	}
	for _, want := range []string{`name="time_zone" type="text" value="Europe/Berlin"`, `<option value="de" selected>`, `<option value="og_bwry" selected>`, `<option value="color-4bwry" selected>`, `<option value="1.8.19" selected>`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("the refused page lacks the stored %q:\n%s", want, w.Body)
		}
	}
}

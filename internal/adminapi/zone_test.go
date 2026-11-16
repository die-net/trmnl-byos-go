package adminapi

import (
	"testing"
	"time"
	_ "time/tzdata" // So the zones the tests name load on any host.
)

// TestPatchDeviceZoneLocale checks the device's time zone and locale: what
// is set and what is in use, which follow the server's zone and locale
// setting while the device sets none, that a refused field changes nothing,
// the other fields included, and that an empty value clears one.
func TestPatchDeviceZoneLocale(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.srv.cfg.Zone = time.FixedZone("TST", 3600)
	ta.addDevice(t)
	path := "/api/v1/devices/" + testFriend

	var d deviceJSON
	ta.call(t, "GET", path, "", 200, &d)
	if d.TimeZone != "" || d.TimeZoneInUse != "TST" || d.Locale != "" || d.LocaleInUse != "en" {
		t.Fatalf("new device %+v", d)
	}
	old := d.SettingsState
	ta.call(t, "PATCH", path, `{"time_zone": "Asia/Tokyo", "locale": "de", "settings_state": "`+old+`"}`, 200, &d)
	if d.TimeZone != "Asia/Tokyo" || d.TimeZoneInUse != "Asia/Tokyo" || d.Locale != "de" || d.LocaleInUse != "de" || d.SettingsState == old {
		t.Errorf("after setting both %+v", d)
	}
	if list := ta.devicesList(t); len(list) != 1 || list[0].TimeZoneInUse != "Asia/Tokyo" || list[0].LocaleInUse != "de" || list[0].Locale != "de" {
		t.Errorf("list %+v", list)
	}

	// A field left out is kept, and the server's locale setting does not
	// touch a device that sets its own.
	ta.call(t, "PATCH", "/api/v1/settings", `{"locale": "fr"}`, 200, nil)
	ta.call(t, "PATCH", path, `{"name": "Hall"}`, 200, &d)
	if d.Name != "Hall" || d.TimeZone != "Asia/Tokyo" || d.Locale != "de" || d.LocaleInUse != "de" {
		t.Errorf("after another field %+v", d)
	}

	// Refused values change nothing.
	good := d.SettingsState
	for _, tc := range []struct{ name, body, contains string }{
		{"unknown zone", `{"name": "New", "time_zone": "Nowhere/Land"}`, `time zone "Nowhere/Land"`},
		{"Local", `{"name": "New", "time_zone": "Local"}`, `"Local" is the server's own zone`},
		{"zone with a path", `{"name": "New", "time_zone": "../etc/localtime"}`, "time zone"},
		{"unknown locale", `{"name": "New", "locale": "xx"}`, `locale "xx" is not one of`},
		{"locale in the wrong case", `{"name": "New", "locale": "DE"}`, "is not one of"},
		{"good zone, bad locale", `{"time_zone": "Europe/Paris", "locale": "xx"}`, "is not one of"},
		{"zone as a number", `{"time_zone": 5}`, "bad request body"},
	} {
		ta.wantError(t, "PATCH", path, tc.body, 400, tc.contains)
		var got deviceJSON
		ta.call(t, "GET", path, "", 200, &got)
		if got.SettingsState != good || got.Name != "Hall" || got.TimeZone != "Asia/Tokyo" || got.Locale != "de" {
			t.Errorf("%s: a refused patch changed the device: %+v", tc.name, got)
		}
	}

	// Empty clears each, and the device follows the server again.
	ta.call(t, "PATCH", path, `{"time_zone": "", "locale": ""}`, 200, &d)
	if d.TimeZone != "" || d.TimeZoneInUse != "TST" || d.Locale != "" || d.LocaleInUse != "fr" {
		t.Errorf("after clearing %+v", d)
	}
}

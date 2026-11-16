package adminapi

import "testing"

// TestPatchDeviceSleep checks the device's sleep window through the API: it
// is set, shown in the device and the list, kept by a change that leaves it
// out, cleared with an empty value, and a refused one changes nothing, the
// other fields included.
func TestPatchDeviceSleep(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	path := "/api/v1/devices/" + testFriend

	var d deviceJSON
	ta.call(t, "GET", path, "", 200, &d)
	if d.Sleep != "" {
		t.Fatalf("new device sleeps %q", d.Sleep)
	}
	old := d.SettingsState
	ta.call(t, "PATCH", path, `{"sleep": "23:00-06:30", "settings_state": "`+old+`"}`, 200, &d)
	if d.Sleep != "23:00-06:30" || d.SettingsState == old {
		t.Errorf("after setting it %+v", d)
	}
	if list := ta.devicesList(t); len(list) != 1 || list[0].Sleep != "23:00-06:30" {
		t.Errorf("list %+v", list)
	}
	ta.call(t, "PATCH", path, `{"name": "Hall"}`, 200, &d)
	if d.Name != "Hall" || d.Sleep != "23:00-06:30" {
		t.Errorf("after another field %+v", d)
	}

	good := d.SettingsState
	for _, tc := range []struct{ name, body, contains string }{
		{"bad format", `{"name": "New", "sleep": "late"}`, "is not a window"},
		{"hour out of range", `{"name": "New", "sleep": "24:00-06:00"}`, "is not a time"},
		{"minute out of range", `{"name": "New", "sleep": "23:60-06:00"}`, "is not a time"},
		{"start equal to end", `{"name": "New", "sleep": "06:00-06:00"}`, "starts and ends at the same time"},
		{"a number", `{"sleep": 5}`, "bad request body"},
	} {
		ta.wantError(t, "PATCH", path, tc.body, 400, tc.contains)
		var got deviceJSON
		ta.call(t, "GET", path, "", 200, &got)
		if got.SettingsState != good || got.Name != "Hall" || got.Sleep != "23:00-06:30" {
			t.Errorf("%s: a refused patch changed the device: %+v", tc.name, got)
		}
	}

	ta.call(t, "PATCH", path, `{"sleep": ""}`, 200, &d)
	if d.Sleep != "" {
		t.Errorf("after clearing %+v", d)
	}
}

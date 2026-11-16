package adminapi

import "testing"

// TestPatchDeviceButtons checks the button command and double-click function
// through the API: both show as set and as in effect, with the defaults for
// empty, are kept by a change that leaves them out, are cleared with an empty
// value, and a refused one changes nothing, the other fields included.
func TestPatchDeviceButtons(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ta.addDevice(t)
	path := "/api/v1/devices/" + testFriend

	var d deviceJSON
	ta.call(t, "GET", path, "", 200, &d)
	if d.ButtonCommand != "" || d.ButtonCommandInUse != "next" || d.SpecialFunction != "" || d.SpecialFunctionInUse != "rewind" {
		t.Fatalf("new device %+v", d)
	}
	old := d.SettingsState
	ta.call(t, "PATCH", path, `{"button_command": "previous", "special_function": "identify", "settings_state": "`+old+`"}`, 200, &d)
	if d.ButtonCommand != "previous" || d.ButtonCommandInUse != "previous" || d.SpecialFunction != "identify" || d.SpecialFunctionInUse != "identify" || d.SettingsState == old {
		t.Errorf("after setting them %+v", d)
	}
	if list := ta.devicesList(t); len(list) != 1 || list[0].ButtonCommand != "previous" || list[0].SpecialFunctionInUse != "identify" {
		t.Errorf("list %+v", list)
	}
	ta.call(t, "PATCH", path, `{"name": "Hall"}`, 200, &d)
	if d.Name != "Hall" || d.ButtonCommand != "previous" || d.SpecialFunction != "identify" {
		t.Errorf("after another field %+v", d)
	}

	good := d.SettingsState
	for _, tc := range []struct{ name, body, contains string }{
		{"bad command", `{"name": "New", "button_command": "sideways"}`, "is not one of"},
		{"none is not a command", `{"name": "New", "button_command": "none"}`, "is not one of"},
		{"bad function", `{"name": "New", "special_function": "dance"}`, "is not one of"},
		{"a cloud function", `{"name": "New", "special_function": "guest_mode"}`, "is not one of"},
		{"a number", `{"button_command": 5}`, "bad request body"},
	} {
		ta.wantError(t, "PATCH", path, tc.body, 400, tc.contains)
		var got deviceJSON
		ta.call(t, "GET", path, "", 200, &got)
		if got.SettingsState != good || got.Name != "Hall" || got.ButtonCommand != "previous" || got.SpecialFunction != "identify" {
			t.Errorf("%s: a refused patch changed the device: %+v", tc.name, got)
		}
	}

	ta.call(t, "PATCH", path, `{"button_command": "", "special_function": ""}`, 200, &d)
	if d.ButtonCommand != "" || d.ButtonCommandInUse != "next" || d.SpecialFunction != "" || d.SpecialFunctionInUse != "rewind" {
		t.Errorf("after clearing %+v", d)
	}
}

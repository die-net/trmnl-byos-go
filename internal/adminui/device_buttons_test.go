package adminui

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// setButtons stores a sleep window, button command and special function for
// the test device through device.Update.
func setButtons(t *testing.T, tu *testUI, sleep, button, function string) {
	t.Helper()
	if _, err := device.Update(t.Context(), tu.db, testMAC, device.Change{Sleep: &sleep, ButtonCommand: &button, SpecialFunction: &function}); err != nil {
		t.Fatal(err)
	}
}

// TestDeviceButtonFields checks the sleep window, button and double-click
// fields the settings form shows for what is stored.
func TestDeviceButtonFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		sleep, button, function string
		want                    []string
	}{
		{
			name: "nothing set",
			want: []string{
				`<input id="sleep_start" name="sleep_start" type="time" value="" aria-describedby="sleep-hint">`,
				`<input id="sleep_end" name="sleep_end" type="time" value="" aria-describedby="sleep-hint">`,
				`<option value="" selected>Default: show the next item</option>`, `<option value="next">Show the next item</option>`,
				`<option value="previous">Show the previous item</option>`, `<option value="first">Show the first item</option>`,
				`<option value="refresh">Refresh the current item</option>`,
				`<option value="" selected>Default: show the last screen</option>`, `<option value="none">Do nothing</option>`,
				`<option value="identify">Show which device this is</option>`, `<option value="restart_playlist">Go back to the first item</option>`,
				`<option value="rewind">Show the last screen</option>`, `<option value="sleep">Go to sleep</option>`,
				`<option value="add_wifi">Open Wi-Fi setup</option>`,
				`Go to sleep lasts until the sleep window next ends, or for ` + spanText(device.FuncSleepSeconds) + ` when there is no window.`,
			},
		},
		{
			name: "all set", sleep: "23:00-06:30", button: "previous", function: "none",
			want: []string{
				`name="sleep_start" type="time" value="23:00"`, `name="sleep_end" type="time" value="06:30"`,
				`<option value="">Default: show the next item</option>`, `<option value="previous" selected>Show the previous item</option>`,
				`<option value="">Default: show the last screen</option>`, `<option value="none" selected>Do nothing</option>`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			setButtons(t, tu, tc.sleep, tc.button, tc.function)
			body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
		})
	}
}

// TestCommandOptions checks the choices of a command list: the default
// first, every value by its label or its name, and a stored value that is no
// longer a choice listed and selected as what it is.
func TestCommandOptions(t *testing.T) {
	t.Parallel()
	labels := map[string]string{"a": "Do A", "b": "Do B"}
	for _, tc := range []struct {
		name, selected string
		want           []choiceOption
	}{
		{"default", "", []choiceOption{{Label: "Default: do b", Selected: true}, {Value: "a", Label: "Do A"}, {Value: "b", Label: "Do B"}, {Value: "c", Label: "c"}}},
		{"a value", "c", []choiceOption{{Label: "Default: do b"}, {Value: "a", Label: "Do A"}, {Value: "b", Label: "Do B"}, {Value: "c", Label: "c", Selected: true}}},
		{
			"a dropped value", "gone",
			[]choiceOption{
				{Label: "Default: do b"},
				{Value: "a", Label: "Do A"},
				{Value: "b", Label: "Do B"},
				{Value: "c", Label: "c"},
				{Value: "gone", Label: "gone (not a choice)", Selected: true},
			},
		},
	} {
		if got := commandOptions([]string{"a", "b", "c"}, labels, "b", tc.selected); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// Every value the domain takes has a label.
	for _, v := range device.ButtonCommands {
		if buttonLabels[v] == "" {
			t.Errorf("button command %q has no label", v)
		}
	}
	for _, v := range device.SpecialFunctions {
		if functionLabels[v] == "" {
			t.Errorf("special function %q has no label", v)
		}
	}
}

// TestDeviceButtonSave checks what a save of the sleep window, button and
// double-click fields stores, and that a refused one changes nothing and
// keeps what was typed.
func TestDeviceButtonSave(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// sleep, button and function are stored before the post.
		sleep, button, function string
		form                    url.Values
		status                  int
		// wantSleep, wantButton and wantFunction are stored after it.
		wantSleep, wantButton, wantFunction string
		body                                []string
	}{
		{
			name: "all three", form: url.Values{"sleep_start": {"23:00"}, "sleep_end": {"06:30"}, "button_command": {"refresh"}, "special_function": {"sleep"}},
			status: http.StatusSeeOther, wantSleep: "23:00-06:30", wantButton: "refresh", wantFunction: "sleep",
		},
		{
			name: "cleared", sleep: "23:00-06:30", button: "first", function: "identify", form: url.Values{},
			status: http.StatusSeeOther,
		},
		{
			name: "window with one end", sleep: "01:00-02:00", form: url.Values{"sleep_start": {"23:00"}},
			status: http.StatusBadRequest, wantSleep: "01:00-02:00",
			body: []string{
				`role="alert">Give the sleep window both a start and an end, or leave both empty.`,
				`name="sleep_start" type="time" value="23:00"`, `name="sleep_end" type="time" value=""`,
			},
		},
		{
			name: "window of no time", form: url.Values{"sleep_start": {"23:00"}, "sleep_end": {"23:00"}, "button_command": {"first"}},
			status: http.StatusBadRequest,
			body: []string{
				`role="alert">Sleep &#34;23:00-23:00&#34; starts and ends at the same time.`,
				`name="sleep_start" type="time" value="23:00"`, `<option value="first" selected>`,
			},
		},
		{
			name: "time that is not one", form: url.Values{"sleep_start": {"25:00"}, "sleep_end": {"06:00"}},
			status: http.StatusBadRequest, body: []string{`role="alert">&#34;25:00&#34; is not a time as HH:MM, 00:00 to 23:59.`},
		},
		{
			name: "button that is not one", button: "first", form: url.Values{"button_command": {"jump"}},
			status: http.StatusBadRequest, wantButton: "first", body: []string{`role="alert">`, `<option value="jump" selected>jump (not a choice)</option>`},
		},
		{
			name: "function that is not one", form: url.Values{"special_function": {"send_to_me"}},
			status: http.StatusBadRequest, body: []string{`role="alert">`, `<option value="send_to_me" selected>send_to_me (not a choice)</option>`},
		},
		{
			name: "window with only an end", form: url.Values{"sleep_start": {"  "}, "sleep_end": {"06:30"}},
			status: http.StatusBadRequest,
			body:   []string{`role="alert">Give the sleep window both a start and an end, or leave both empty.`, `name="sleep_start" type="time" value=""`, `name="sleep_end" type="time" value="06:30"`},
		},
		{
			name: "only white space is no window", sleep: "01:00-02:00", form: url.Values{"sleep_start": {" "}, "sleep_end": {" "}},
			status: http.StatusSeeOther,
		},
		{
			name: "time with seconds", form: url.Values{"sleep_start": {"07:00:30"}, "sleep_end": {"08:00"}},
			status: http.StatusBadRequest, body: []string{`role="alert">&#34;07:00:30&#34; is not a time as HH:MM, 00:00 to 23:59.`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			setButtons(t, tu, tc.sleep, tc.button, tc.function)
			tc.form.Set("name", "Kitchen")
			w := tu.post(t, tu.signIn(t), "/devices/ABC123", withState(t, tu, tc.form))
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			dev := testDevice(t, tu)
			if got := device.FormatSleep(dev.SleepStart, dev.SleepEnd); got != tc.wantSleep || dev.ButtonCommand != tc.wantButton || dev.SpecialFunction != tc.wantFunction {
				t.Errorf("stored sleep %q, button %q, function %q; want %q, %q, %q", got, dev.ButtonCommand, dev.SpecialFunction, tc.wantSleep, tc.wantButton, tc.wantFunction)
			}
			for _, want := range tc.body {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("page lacks %q:\n%s", want, w.Body)
				}
			}
		})
	}
}

// TestDeviceButtonStale checks that a stale save says what the sleep window,
// button and double-click are now.
func TestDeviceButtonStale(t *testing.T) {
	t.Parallel()
	stored := deviceForm{Name: "Kitchen", SleepStart: "23:00", SleepEnd: "06:30", Function: "none"}
	for _, tc := range []struct {
		form, stored deviceForm
		want         string
	}{
		{
			deviceForm{Name: "Kitchen", SleepStart: "22:00", SleepEnd: "06:30", Button: "first"},
			stored,
			` Stored now: the sleep window is 23:00 to 06:30, the button press is "Default: show the next item", the double-click is "Do nothing". Save again to use the values below.`,
		},
		{
			deviceForm{Name: "Kitchen", SleepStart: "22:00", SleepEnd: "06:30"},
			deviceForm{Name: "Kitchen", Button: "refresh"},
			` Stored now: there is no sleep window, the button press is "Refresh the current item". Save again to use the values below.`,
		},
	} {
		if got := settingsChangedText(&tc.form, &tc.stored); got != settingsChanged+tc.want {
			t.Errorf("got %q, want %q", got, settingsChanged+tc.want)
		}
	}
}

// TestDeviceButtonDropped checks that a stored button command the server no
// longer has is listed as what it is, and that saving the form with it
// unchanged keeps it and saves the rest.
func TestDeviceButtonDropped(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	dev := testDevice(t, tu)
	err := tu.db.Write.UpdateDeviceSettings(t.Context(), db.UpdateDeviceSettingsParams{
		Name: "Kitchen", SleepStart: -1, SleepEnd: -1, ButtonCommand: "jump", ID: dev.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := tu.signIn(t)
	body := tu.get(t, c, "/devices/ABC123").Body.String()
	if want := `<option value="jump" selected>jump (not a choice)</option>`; !strings.Contains(body, want) {
		t.Fatalf("page lacks %q:\n%s", want, body)
	}
	w := tu.post(t, c, "/devices/ABC123", withState(t, tu, url.Values{"name": {"Hall"}, "button_command": {"jump"}}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if dev := testDevice(t, tu); dev.Name != "Hall" || dev.ButtonCommand != "jump" {
		t.Errorf("after the save: %+v", dev)
	}
}

// TestDeviceButtonStalePage checks that a save of these fields from a page
// that showed other settings is refused, changes nothing, and comes back
// with what is stored now and the typed values.
func TestDeviceButtonStalePage(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	form := withState(t, tu, url.Values{"name": {"Kitchen"}, "sleep_start": {"22:00"}, "sleep_end": {"07:00"}, "button_command": {"first"}, "special_function": {"identify"}})
	// Another page saves first.
	setButtons(t, tu, "23:00-06:30", "refresh", "")
	w := tu.post(t, tu.signIn(t), "/devices/ABC123", form)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body)
	}
	dev := testDevice(t, tu)
	if got := device.FormatSleep(dev.SleepStart, dev.SleepEnd); got != "23:00-06:30" || dev.ButtonCommand != "refresh" || dev.SpecialFunction != "" {
		t.Fatalf("a stale save changed the device: %+v", dev)
	}
	wantInOrder(t, w.Body.String(), `id="settings"`,
		`Stored now: the sleep window is 23:00 to 06:30, the button press is &#34;Refresh the current item&#34;, the double-click is &#34;Default: show the last screen&#34;. Save again to use the values below.`,
		`name="settings_state" value="`+device.SettingsState(&dev)+`"`,
		`name="sleep_start" type="time" value="22:00"`, `name="sleep_end" type="time" value="07:00"`,
		`<option value="first" selected>`, `<option value="identify" selected>`)
}

// TestDeviceButtonLeftOut checks that a post without the sleep window,
// button and double-click fields keeps all three, and that a post with one
// end of the window is still read as a window with one end.
func TestDeviceButtonLeftOut(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	setButtons(t, tu, "23:00-06:30", "refresh", "none")
	c := tu.signIn(t)
	left := func(form url.Values, keep ...string) url.Values {
		form = withState(t, tu, form)
		for _, name := range []string{"sleep_start", "sleep_end", "button_command", "special_function"} {
			if !slices.Contains(keep, name) {
				form.Del(name)
			}
		}
		return form
	}
	if w := tu.post(t, c, "/devices/ABC123", left(url.Values{"name": {"Hall"}})); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	stored := func() (string, string, string) {
		dev := testDevice(t, tu)
		return device.FormatSleep(dev.SleepStart, dev.SleepEnd), dev.ButtonCommand, dev.SpecialFunction
	}
	if sleep, button, function := stored(); sleep != "23:00-06:30" || button != "refresh" || function != "none" {
		t.Errorf("settings the post left out changed: %q, %q, %q", sleep, button, function)
	}
	// One end posted empty is a window with one end, not a setting left out.
	w := tu.post(t, c, "/devices/ABC123", left(url.Values{"name": {"Hall"}, "sleep_end": {"07:00"}}, "sleep_end"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Give the sleep window both a start and an end") {
		t.Fatalf("status %d, want 400 for a window with one end: %s", w.Code, w.Body)
	}
	// The refused page shows what the post left out as stored.
	for _, want := range []string{`<option value="refresh" selected>`, `<option value="none" selected>`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("the refused page lacks the stored %q:\n%s", want, w.Body)
		}
	}
	if sleep, _, _ := stored(); sleep != "23:00-06:30" {
		t.Errorf("a refused window changed the stored one to %q", sleep)
	}
}

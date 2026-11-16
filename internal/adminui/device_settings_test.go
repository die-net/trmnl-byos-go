package adminui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// newProfileUI returns a test server with one device, which reports the
// firmware model "og" and so the registry model og_plus.
func newProfileUI(t *testing.T) *testUI {
	t.Helper()
	tu := newTestUI(t, Config{}, true)
	addDevice(t, tu, testMAC, "ABC123", "Kitchen", 0)
	if err := tu.db.Write.UpdateDeviceFirmware(t.Context(), db.UpdateDeviceFirmwareParams{FwModel: "og", ID: 1}); err != nil {
		t.Fatal(err)
	}
	return tu
}

// setProfile stores a profile and palette for the test device.
func setProfile(t *testing.T, tu *testUI, model, palette string) {
	t.Helper()
	if _, err := device.Update(t.Context(), tu.db, testMAC, device.Change{Profile: &model, Palette: &palette}); err != nil {
		t.Fatal(err)
	}
}

// testDevice returns the test device as stored.
func testDevice(t *testing.T, tu *testUI) db.Device {
	t.Helper()
	dev, err := device.FindDevice(t.Context(), tu.db.Read, testMAC)
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func TestDeviceProfileFields(t *testing.T) {
	t.Parallel()
	const hint = "It changes what is drawn: the size and palettes of this device's screens. The image size limit never goes above the one for the model the device reports, and firmware updates stay on that model."
	for _, tc := range []struct {
		name           string
		model, palette string
		want           []string
		notWant        []string
	}{
		{
			name: "no override",
			want: []string{
				`<form method="post" action="/devices/ABC123#settings">`, `<label for="profile">Panel profile</label>`,
				`<option value="" selected>As the device reports: TRMNL OG (2-bit) (og_plus)</option>`,
				`<option value="og_bwry">TRMNL OG (B/W/R/Y) (og_bwry)</option>`, hint,
				`<label for="palette">Palette</label>`, `<option value="" selected>Model&#39;s default: Black &amp; White (1-bit)</option>`,
				`<option value="gray-4">4 Grays (2-bit)</option>`, "These are the palettes of TRMNL OG (2-bit) (og_plus).",
			},
			notWant: []string{`<option value="color-4bwry"`},
		},
		{
			name: "profile and palette set", model: "og_bwry", palette: "color-4bwry",
			want: []string{
				`<option value="">As the device reports: TRMNL OG (2-bit) (og_plus)</option>`, `<option value="og_bwry" selected>`,
				`<option value="">Model&#39;s default: Color (4 colors: black, red, white, yellow)</option>`,
				`<option value="color-4bwry" selected>`, "These are the palettes of TRMNL OG (B/W/R/Y) (og_bwry).",
			},
			notWant: []string{`<option value="gray-4"`},
		},
		{
			name: "palette of the reported model", palette: "gray-4",
			want: []string{`<option value="gray-4" selected>4 Grays (2-bit)</option>`, `<option value="">Model&#39;s default`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			if tc.model != "" || tc.palette != "" {
				setProfile(t, tu, tc.model, tc.palette)
			}
			body := tu.get(t, tu.signIn(t), "/devices/ABC123").Body.String()
			dev := testDevice(t, tu)
			tc.want = append(tc.want, `<input type="hidden" name="settings_state" value="`+device.SettingsState(&dev)+`">`)
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(body, bad) {
					t.Errorf("page has %q", bad)
				}
			}
		})
	}
}

// TestProfileOptionsOrder checks that the reported model is first and the
// registry's models follow by label.
func TestProfileOptionsOrder(t *testing.T) {
	t.Parallel()
	opts := profileOptions("og", "gone_model")
	if opts[0].Value != "" || opts[0].Selected {
		t.Errorf("first option = %+v, want the reported model, not selected", opts[0])
	}
	var selected []string
	for i := 1; i < len(opts); i++ {
		if i > 1 && strings.ToLower(opts[i-1].Label) > strings.ToLower(opts[i].Label) {
			t.Errorf("%q is before %q", opts[i-1].Label, opts[i].Label)
		}
		if opts[i].Selected {
			selected = append(selected, opts[i].Value+"="+opts[i].Label)
		}
	}
	// A stored model the registry no longer has is listed as its name.
	if len(selected) != 1 || selected[0] != "gone_model=gone_model" {
		t.Errorf("selected = %v, want the stored model alone", selected)
	}
	if first := profileOptions("nope", "")[0]; first.Label != "As the device reports: unknown model" || !first.Selected {
		t.Errorf("first option for an unknown firmware model = %+v", first)
	}
}

func TestDeviceProfileSave(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// model and palette are stored before the post.
		model, palette string
		form           url.Values
		status         int
		wantModel      string
		wantPalette    string
		body           []string
	}{
		{name: "profile with its default palette", form: url.Values{"profile": {"og_bwry"}}, status: 303, wantModel: "og_bwry"},
		{
			name: "profile and one of its palettes", model: "og_bwry",
			form: url.Values{"profile": {"og_bwry"}, "palette": {"color-4bwry"}}, status: 303, wantModel: "og_bwry", wantPalette: "color-4bwry",
		},
		{name: "palette of the reported model", form: url.Values{"palette": {"gray-4"}}, status: 303, wantPalette: "gray-4"},
		{name: "back to what the device reports", model: "og_bwry", palette: "color-4bwry", form: url.Values{}, status: 303},
		{
			// The palette list was for the old profile. The page the
			// refusal sends lists the new profile's palettes, with its
			// default chosen, so saving it works.
			name: "palette the new profile does not take", palette: "gray-4",
			form: url.Values{"name": {"Hall"}, "profile": {"og_bwry"}, "palette": {"gray-4"}}, status: 400, wantPalette: "gray-4",
			body: []string{
				`role="alert">Palette &#34;gray-4&#34; is not one of og_bwry&#39;s palettes.`, `value="Hall"`, `<option value="og_bwry" selected>`,
				`<option value="" selected>Model&#39;s default: Color (4 colors: black, red, white, yellow)</option>`, `<option value="color-4bwry">`,
			},
		},
		{
			name: "profile that is not a model", form: url.Values{"profile": {"<nope>"}}, status: 400,
			body: []string{`Profile &#34;&lt;nope&gt;&#34; is not a model in the registry.`, `<option value="&lt;nope&gt;" selected>&lt;nope&gt;</option>`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			if tc.model != "" || tc.palette != "" {
				setProfile(t, tu, tc.model, tc.palette)
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
			if strings.Contains(body, "<nope>") {
				t.Error("a posted value was not escaped")
			}
			wantModel, wantPalette := tc.wantModel, tc.wantPalette
			if tc.status != http.StatusSeeOther {
				wantModel = tc.model
			}
			if dev := testDevice(t, tu); dev.ProfileOverride != wantModel || dev.PaletteOverride != wantPalette || (tc.status != http.StatusSeeOther && dev.Name != "Kitchen") {
				t.Errorf("stored profile %q, palette %q, name %q; want %q, %q", dev.ProfileOverride, dev.PaletteOverride, dev.Name, wantModel, wantPalette)
			}
		})
	}
}

// TestDeviceSettingsStale checks that a save from a page that showed other
// settings than are stored now saves nothing, says what is stored, and
// sends a page whose own save works.
func TestDeviceSettingsStale(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	c := tu.signIn(t)
	form := withState(t, tu, url.Values{"name": {"Hall"}, "updates_enabled": {"1"}, "profile": {"og_bwry"}})
	// Another page saves first.
	setProfile(t, tu, "", "gray-4")
	w := tu.post(t, c, "/devices/ABC123", form)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	dev := testDevice(t, tu)
	if dev.Name != "Kitchen" || dev.ProfileOverride != "" || dev.PaletteOverride != "gray-4" || dev.UpdatesEnabled != 0 {
		t.Fatalf("a stale save changed the device: %+v", dev)
	}
	wantInOrder(t, body, `id="settings"`,
		`role="alert">This device&#39;s settings changed after the page loaded, so nothing was saved. Stored now: the name is &#34;Kitchen&#34;, firmware updates are off, the profile is as the device reports, the palette is &#34;gray-4&#34;. Save again to use the values below.`,
		`name="settings_state" value="`+device.SettingsState(&dev)+`"`, `value="Hall"`, ` checked`, `<option value="og_bwry" selected>`)
	// The page's own form saves.
	again := url.Values{"name": {"Hall"}, "updates_enabled": {"1"}, "profile": {"og_bwry"}, "settings_state": {device.SettingsState(&dev)}}
	if w := tu.post(t, c, "/devices/ABC123", again); w.Code != http.StatusSeeOther {
		t.Fatalf("second save: status %d: %s", w.Code, w.Body)
	}
	if dev := testDevice(t, tu); dev.Name != "Hall" || dev.ProfileOverride != "og_bwry" || dev.PaletteOverride != "" || dev.UpdatesEnabled != 1 {
		t.Errorf("after the second save: %+v", dev)
	}
}

func TestDeviceSettingsStaleSameValues(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	form := withState(t, tu, url.Values{"name": {"Kitchen"}, "palette": {"gray-4"}})
	setProfile(t, tu, "", "gray-4")
	w := tu.post(t, tu.signIn(t), "/devices/ABC123", form)
	if want := "so nothing was saved. The values below are what is stored now."; w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), want) {
		t.Errorf("status %d, want 409 with %q: %s", w.Code, want, w.Body)
	}
}

func TestDeviceSettingsNoState(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	w := tu.post(t, tu.signIn(t), "/devices/ABC123", url.Values{"name": {"Hall"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `role="alert">`+reloadText) || !strings.Contains(w.Body.String(), `value="Hall"`) {
		t.Errorf("status %d, want 400 with the typed name: %s", w.Code, w.Body)
	}
	if dev := testDevice(t, tu); dev.Name != "Kitchen" {
		t.Errorf("a post with no state renamed the device to %q", dev.Name)
	}
}

// TestDeviceSettingsReissuedID checks that a settings page for a deleted
// device changes nothing of a newer device with its friendly ID.
func TestDeviceSettingsReissuedID(t *testing.T) {
	t.Parallel()
	tu := newProfileUI(t)
	form := withState(t, tu, url.Values{"name": {"Hall"}})
	deleteNow(t, tu)
	addDevice(t, tu, "D8:3B:DA:00:00:09", "ABC123", "Newer", 0)
	w := tu.post(t, tu.signIn(t), "/devices/ABC123", form)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body)
	}
	dev, err := device.FindDevice(t.Context(), tu.db.Read, "ABC123")
	if err != nil || dev.Name != "Newer" {
		t.Errorf("the newer device: %+v, %v", dev, err)
	}
}

// TestDeviceSettingsStaleAfterRefusal checks that a form refused for a bad
// value keeps the state it posted, so that fixing the value and saving
// again from a stale page is still refused as stale.
func TestDeviceSettingsStaleAfterRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		bad  url.Values
	}{
		{"name too long", url.Values{"name": {strings.Repeat("n", 65)}}},
		{"pin that is not a version", url.Values{"name": {"Hall"}, "fw_pin": {"latest"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			c := tu.signIn(t)
			form := withState(t, tu, tc.bad)
			old := form.Get("settings_state")
			// Another page saves first.
			setProfile(t, tu, "", "gray-4")
			w := tu.post(t, c, "/devices/ABC123", form)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `name="settings_state" value="`+old+`"`) {
				t.Fatalf("status %d, want 400 with the posted state: %s", w.Code, w.Body)
			}
			// The corrected form, with the state that page carries.
			w = tu.post(t, c, "/devices/ABC123", url.Values{"name": {"Hall"}, "settings_state": {old}})
			if w.Code != http.StatusConflict {
				t.Fatalf("corrected save: status %d, want 409: %s", w.Code, w.Body)
			}
			if dev := testDevice(t, tu); dev.Name != "Kitchen" || dev.PaletteOverride != "gray-4" {
				t.Errorf("the stale page overwrote the device: %+v", dev)
			}
		})
	}
}

// TestDeviceSettingsStalePin checks that a refused stale save does not call
// a pin changed when it is the stored one written another way.
func TestDeviceSettingsStalePin(t *testing.T) {
	t.Parallel()
	stored := deviceForm{Name: "Kitchen", Pin: "1.8.19"}
	for _, tc := range []struct {
		pin, want string
	}{
		{"1.08.19", settingsChanged + " The values below are what is stored now."},
		{"1.8.20", settingsChanged + ` Stored now: the firmware pin is "1.8.19". Save again to use the values below.`},
		{"latest", settingsChanged + ` Stored now: the firmware pin is "1.8.19". Save again to use the values below.`},
	} {
		if got := settingsChangedText(&deviceForm{Name: "Kitchen", Pin: tc.pin}, &stored); got != tc.want {
			t.Errorf("pin %q: %q, want %q", tc.pin, got, tc.want)
		}
	}
}

// TestDeviceStoredOverrideIgnored checks that a stored profile or palette
// that no longer applies is said to be ignored, is still what the form
// posts, and does not block a save of the other settings.
func TestDeviceStoredOverrideIgnored(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// profile and palette are stored without a check, as after the
		// registry dropped a model or the device reported another one.
		profile, palette string
		want             []string
	}{
		{
			name: "profile the registry lacks", profile: "gone_model",
			want: []string{
				`<p class="note" id="profile-problem">Profile &#34;gone_model&#34; is not a model in the registry. A setting that does not apply is ignored until it is changed below.</p>`,
				`<option value="gone_model" selected>gone_model</option>`, "These are the palettes of TRMNL OG (2-bit) (og_plus).",
			},
		},
		{
			name: "palette the model does not take", palette: "color-4bwry",
			want: []string{
				`Palette &#34;color-4bwry&#34; is not one of og_plus&#39;s palettes. A setting that does not apply is ignored until it is changed below.`,
				`<option value="color-4bwry" selected>Color (4 colors: black, red, white, yellow) (not a palette of this model)</option>`,
				`<option value="">Model&#39;s default: Black &amp; White (1-bit)</option>`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newProfileUI(t)
			err := tu.db.Write.UpdateDeviceSettings(t.Context(), db.UpdateDeviceSettingsParams{Name: "Kitchen", ProfileOverride: tc.profile, PaletteOverride: tc.palette, ID: 1})
			if err != nil {
				t.Fatal(err)
			}
			c := tu.signIn(t)
			body := tu.get(t, c, "/devices/ABC123").Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q:\n%s", want, body)
				}
			}
			// A rename that posts the stored profile and palette back, as
			// the page's form does, saves and leaves them as they are.
			form := withState(t, tu, url.Values{"name": {"Hall"}, "profile": {tc.profile}, "palette": {tc.palette}})
			if w := tu.post(t, c, "/devices/ABC123", form); w.Code != http.StatusSeeOther {
				t.Fatalf("rename: status %d: %s", w.Code, w.Body)
			}
			if dev := testDevice(t, tu); dev.Name != "Hall" || dev.ProfileOverride != tc.profile || dev.PaletteOverride != tc.palette {
				t.Errorf("after the rename: %+v", dev)
			}
			// Choosing what the device reports and the default clears it.
			if w := tu.post(t, c, "/devices/ABC123", withState(t, tu, url.Values{"name": {"Hall"}})); w.Code != http.StatusSeeOther {
				t.Fatalf("clear: status %d: %s", w.Code, w.Body)
			}
			if body := tu.get(t, c, "/devices/ABC123").Body.String(); strings.Contains(body, "profile-problem") {
				t.Errorf("the page still flags an override after it was cleared:\n%s", body)
			}
		})
	}
}

package adminui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// storedSettings returns the settings as they are stored, by key.
func storedSettings(t *testing.T, tu *testUI) map[string]settings.Setting {
	t.Helper()
	list, err := settings.List(t.Context(), tu.db.Read)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]settings.Setting{}
	for _, st := range list {
		out[st.Key] = st
	}
	return out
}

func TestSettingsPage(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	w := tu.get(t, c, "/settings")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h1>Settings</h1>",
		`id="refresh_seconds"`, "<h2>Refresh interval</h2>", "<code>trmnl-byos password</code>",
		`id="min_password_length"`, "<h2>Minimum password length</h2>",
		`<code>refresh_seconds</code></p>`, `<code>min_password_length</code>`,
		`name="value" type="number" min="1" max="31536000" step="1" inputmode="numeric" required value="900"`,
		`min="1" max="1024" step="1" inputmode="numeric" required value="12"`,
		"Allowed: 1 to 31536000 seconds. Currently 15 min. This is the default.",
		"Allowed: 1 to 1024 bytes. This is the default.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "Reset to default") {
		t.Error("an unset setting has a reset button")
	}
	// The panels are in the order the package lists the settings.
	wantInOrder(t, body, `id="refresh_seconds"`, `id="min_password_length"`)

	if w := tu.post(t, c, "/settings", url.Values{"key": {"refresh_seconds"}, "value": {"600"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("save status %d", w.Code)
	}
	body = tu.get(t, c, "/settings").Body.String()
	for _, want := range []string{`required value="600"`, "Allowed: 1 to 31536000 seconds. Currently 10 min. The default is 900.", "Reset to default", `action="/settings/reset"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page after a save lacks %q", want)
		}
	}
	if n := strings.Count(body, "Reset to default"); n != 1 {
		t.Errorf("%d reset buttons, want 1", n)
	}
	for _, bad := range []string{"<script", "style=", "<style", " onclick=", " onsubmit="} {
		if strings.Contains(body, bad) {
			t.Errorf("page has inline %q, which the CSP refuses", bad)
		}
	}
}

func TestSaveSetting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key, value string
		want       int64
	}{
		{"refresh_seconds", "1800", 1800},
		{"min_password_length", "20", 20},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			w := tu.post(t, tu.signIn(t), "/settings", url.Values{"key": {tc.key}, "value": {tc.value}})
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings#"+tc.key {
				t.Fatalf("status %d to %q, want 303 to /settings#%s", w.Code, w.Header().Get("Location"), tc.key)
			}
			if st := storedSettings(t, tu)[tc.key]; !st.Set || st.Value != tc.want {
				t.Errorf("stored %+v, want %d set", st, tc.want)
			}
		})
	}
}

func TestSaveSettingRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key, value string
		// bounds is the range the page says the value must be in.
		bounds string
	}{
		{"not a number", "refresh_seconds", "soon", "1 to 31536000"},
		{"fraction", "refresh_seconds", "1.5", "1 to 31536000"},
		{"below min", "refresh_seconds", "0", "1 to 31536000"},
		{"negative", "min_password_length", "-3", "1 to 1024"},
		{"above max", "min_password_length", "1025", "1 to 1024"},
		{"empty", "min_password_length", "", "1 to 1024"},
	} {
		// The page words the refusal itself, with the setting's bounds.
		want := `<p class="error" role="alert" id="error-` + tc.key + `">Enter a whole number from ` + tc.bounds + `.</p>`
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			w := tu.post(t, tu.signIn(t), "/settings", url.Values{"key": {tc.key}, "value": {tc.value}})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
			body := w.Body.String()
			// The message is in the panel of the setting, above its form.
			_, panel, _ := strings.Cut(body, `id="`+tc.key+`"`)
			panel, _, _ = strings.Cut(panel, "</li>")
			if !strings.Contains(panel, want) {
				t.Errorf("panel lacks the message %q: %s", want, panel)
			}
			if strings.Index(panel, `role="alert"`) > strings.Index(panel, `name="value"`) {
				t.Error("message is after the input")
			}
			if tc.value != "" && !strings.Contains(panel, `required value="`+tc.value+`"`) {
				t.Errorf("panel does not keep the typed value %q: %s", tc.value, panel)
			}
			if strings.Count(body, `role="alert"`) != 1 {
				t.Errorf("%d messages, want 1", strings.Count(body, `role="alert"`))
			}
			for key, st := range storedSettings(t, tu) {
				if st.Set {
					t.Errorf("%s was stored after a refusal", key)
				}
			}
		})
	}
}

func TestSettingBadKey(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/settings", "/settings/reset"} {
		for _, tc := range []struct {
			name string
			form url.Values
		}{
			{"unknown key", url.Values{"key": {"join_until"}, "value": {"5"}}},
			{"missing key", url.Values{"value": {"5"}}},
		} {
			t.Run(path+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				tu := newTestUI(t, Config{}, true)
				w := tu.post(t, tu.signIn(t), path, tc.form)
				body := w.Body.String()
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status %d, want 400: %s", w.Code, body)
				}
				// One message, at the top of the page and in no panel.
				wantInOrder(t, body, `<p class="error" role="alert">Choose a setting.</p>`, `id="refresh_seconds"`)
				if n := strings.Count(body, `role="alert"`); n != 1 {
					t.Errorf("%d messages, want 1", n)
				}
				for key, st := range storedSettings(t, tu) {
					if st.Set {
						t.Errorf("%s was stored", key)
					}
				}
			})
		}
	}
}

func TestResetSetting(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	if err := settings.Set(t.Context(), tu.db, "refresh_seconds", "300", tu.now); err != nil {
		t.Fatal(err)
	}
	w := tu.post(t, c, "/settings/reset", url.Values{"key": {"refresh_seconds"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings#refresh_seconds" {
		t.Fatalf("status %d to %q, want 303 to /settings#refresh_seconds", w.Code, w.Header().Get("Location"))
	}
	if st := storedSettings(t, tu)["refresh_seconds"]; st.Set || st.Value != st.Default {
		t.Errorf("after reset %+v, want the default and not set", st)
	}
}

// TestResetSettingUnset checks that resetting a setting that has no stored
// value, such as from a page that is out of date, is not an error.
func TestResetSettingUnset(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	w := tu.post(t, tu.signIn(t), "/settings/reset", url.Values{"key": {"min_password_length"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings#min_password_length" {
		t.Fatalf("status %d to %q, want 303 to /settings#min_password_length", w.Code, w.Header().Get("Location"))
	}
}

// TestSaveSettingRefusedKeepsStored checks a refused save of a setting that
// has a stored value: the stored value stays, its panel keeps the reset
// button and marks the input, and the other panel shows its own value.
func TestSaveSettingRefusedKeepsStored(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	if err := settings.Set(t.Context(), tu.db, "refresh_seconds", "300", tu.now); err != nil {
		t.Fatal(err)
	}
	w := tu.post(t, tu.signIn(t), "/settings", url.Values{"key": {"refresh_seconds"}, "value": {"0"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
	wantInOrder(t, w.Body.String(), `id="refresh_seconds"`,
		`<p class="error" role="alert" id="error-refresh_seconds">Enter a whole number from 1 to 31536000.</p>`,
		`required value="0" aria-describedby="error-refresh_seconds hint-refresh_seconds" aria-invalid="true">`,
		`Currently 5 min. The default is 900.`, "Reset to default",
		`id="min_password_length"`, `required value="12" aria-describedby="hint-min_password_length">`)
	if st := storedSettings(t, tu)["refresh_seconds"]; !st.Set || st.Value != 300 {
		t.Errorf("stored %+v after a refusal, want 300", st)
	}
}

// putBadSetting stores a value the settings package would refuse.
func putBadSetting(t *testing.T, tu *testUI, key, value string) {
	t.Helper()
	if err := tu.db.Write.SetSetting(t.Context(), db.SetSettingParams{Key: key, Value: value, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
}

// badMessage is what a panel says about a stored value that is not usable.
const badMessage = `The stored value &#34;&lt;b&gt;abc&#34;`

func TestSettingsBadRow(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	putBadSetting(t, tu, "refresh_seconds", `<b>abc"`)
	w := tu.get(t, c, "/settings")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	body := w.Body.String()
	// The message is escaped, in the bad setting's panel only, above its
	// form, and the input holds the default. The hint does not say the
	// value is the default, and the reset button is there.
	wantInOrder(t, body, `id="refresh_seconds"`,
		`<p class="error" role="alert" id="error-refresh_seconds">`+badMessage,
		`is not usable, so the default is in effect. Save a value or reset to the default.</p>`,
		`required value="900" aria-describedby="error-refresh_seconds hint-refresh_seconds">`,
		`The default is 900.`, "Reset to default",
		`id="min_password_length"`, `required value="12" aria-describedby="hint-min_password_length">`, `This is the default.`)
	if n := strings.Count(body, `role="alert"`); n != 1 {
		t.Errorf("%d messages, want 1", n)
	}
	if n := strings.Count(body, "Reset to default"); n != 1 {
		t.Errorf("%d reset buttons, want 1", n)
	}
	if strings.Contains(body, "<b>abc") || strings.Contains(body, `aria-invalid`) {
		t.Error("the stored text is not escaped, or the input is marked invalid")
	}
}

func TestSettingsBadRowRepaired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path string
		form       url.Values
	}{
		{"reset", "/settings/reset", url.Values{"key": {"refresh_seconds"}}},
		{"save", "/settings", url.Values{"key": {"refresh_seconds"}, "value": {"600"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			c := tu.signIn(t)
			putBadSetting(t, tu, "refresh_seconds", "abc")
			w := tu.post(t, c, tc.path, tc.form)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings#refresh_seconds" {
				t.Fatalf("status %d to %q, want 303 to /settings#refresh_seconds", w.Code, w.Header().Get("Location"))
			}
			w = tu.get(t, c, "/settings")
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `role="alert"`) || strings.Contains(w.Body.String(), "not usable") {
				t.Errorf("status %d; the page still has a message: %s", w.Code, w.Body)
			}
			if st := storedSettings(t, tu)["refresh_seconds"]; st.Bad != nil {
				t.Errorf("still bad: %+v", st)
			}
		})
	}
}

func TestSettingsBadRowRefusedSave(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	putBadSetting(t, tu, "refresh_seconds", "abc")
	w := tu.post(t, tu.signIn(t), "/settings", url.Values{"key": {"refresh_seconds"}, "value": {"0"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
	body := w.Body.String()
	// Only the refusal, not the stored-value message too.
	wantInOrder(t, body, `id="refresh_seconds"`,
		`<p class="error" role="alert" id="error-refresh_seconds">Enter a whole number from 1 to 31536000.</p>`,
		`required value="0" aria-describedby="error-refresh_seconds hint-refresh_seconds" aria-invalid="true">`, "Reset to default")
	if n := strings.Count(body, `role="alert"`); n != 1 || strings.Contains(body, "not usable") {
		t.Errorf("%d messages, want only the refusal: %s", n, body)
	}
	if st := storedSettings(t, tu)["refresh_seconds"]; st.Bad == nil || *st.Bad != "abc" {
		t.Errorf("stored %+v, want the bad row left alone", st)
	}
}

func TestSettingsSignedOut(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/settings"},
		{"POST", "/settings"},
		{"POST", "/settings/reset"},
	} {
		body := url.Values{"key": {"refresh_seconds"}, "value": {"5"}}.Encode()
		w := tu.do(t, tc.method, tc.path, body)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Errorf("%s %s: status %d to %q, want 303 to /login", tc.method, tc.path, w.Code, w.Header().Get("Location"))
		}
	}
	if st := storedSettings(t, tu)["refresh_seconds"]; st.Set {
		t.Error("a signed-out post stored a setting")
	}
}

func TestSettingsFormTooBig(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	c := tu.signIn(t)
	big := url.Values{"key": {"refresh_seconds"}, "value": {strings.Repeat("1", maxFormBody)}}
	for _, path := range []string{"/settings", "/settings/reset"} {
		w := tu.post(t, c, path, big)
		// The page is shown again with the reason, and nothing is stored.
		if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `role="alert"`) || !strings.Contains(w.Body.String(), `id="refresh_seconds"`) {
			t.Errorf("POST %s: status %d, body %s; want 413 and the page with a message", path, w.Code, w.Body)
		}
	}
}

func TestSettingsNav(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	body := tu.get(t, tu.signIn(t), "/settings").Body.String()
	if strings.Count(body, `aria-current="page"`) != 1 || !strings.Contains(body, `<a href="/settings" aria-current="page">`) {
		t.Error("the page does not mark Settings as the current page")
	}
}

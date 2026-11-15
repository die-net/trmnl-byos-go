package adminui

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// helpPart is a run of a setting's help text, which is shown as code when
// Code is set.
type helpPart struct {
	Text string
	Code bool
}

// settingText is what the settings page says about one setting, which the
// settings package does not know.
type settingText struct {
	// label is the panel's heading.
	label string
	// help is one or two sentences about the setting.
	help []helpPart
	// unit names what the value counts, such as "seconds".
	unit string
	// seconds is whether the value is a number of seconds, which the panel
	// also shows in words when it is a minute or more.
	seconds bool
}

// settingTexts has the text for each setting, by key. A setting with no
// entry is shown under its key, with no help.
var settingTexts = map[string]settingText{
	settings.KeyRefreshSeconds: {
		label:   "Refresh interval",
		help:    []helpPart{{Text: "How long a device sleeps between wakes when nothing else needs it sooner. A change applies at each device's next wake."}},
		unit:    "seconds",
		seconds: true,
	},
	settings.KeyMinPasswordLength: {
		label: "Minimum password length",
		help: []helpPart{
			{Text: "The shortest admin password that can be set. A shorter password set earlier still signs in. Set the password with "},
			{Text: "trmnl-byos password", Code: true},
			{Text: " on the server."},
		},
		unit: "bytes",
	},
}

// settingView is a setting as the settings page shows it.
type settingView struct {
	// Key names the setting, and Label is its heading.
	Key, Label string
	Help       []helpPart
	// Unit names what the value counts.
	Unit string
	// Value is the text of the input: the value in effect, or what was
	// typed if that was refused.
	Value string
	// Words is the value in effect in hours, minutes and seconds, or "".
	Words string
	// Range says what values are allowed, and State that the value is the
	// default, or what the default is.
	Range, State string
	// Min and Max bound the input.
	Min, Max int64
	// CanReset is whether the setting has a stored value to remove.
	CanReset bool
	// Error is why a save of this setting was refused, or "".
	Error string
	// Bad says the stored value is not usable, so the default is in
	// effect, or is "". It is shown only when Error is "".
	Bad string
}

// newSettingView returns the view of a setting.
func newSettingView(st *settings.Setting) settingView {
	text, ok := settingTexts[st.Key]
	label := st.Key
	if ok {
		label = text.label
	}
	v := settingView{
		Key: st.Key, Label: label, Help: text.help, Unit: text.unit,
		Value: strconv.FormatInt(st.Value, 10), Min: st.Min, Max: st.Max, CanReset: st.Set,
	}
	if st.Bad != nil {
		v.Bad = `The stored value "` + *st.Bad + `" is not usable, so the default is in effect. Save a value or reset to the default.`
	}
	if text.seconds && st.Value >= 60 {
		v.Words = spanText(st.Value)
	}
	v.Range = fmt.Sprintf("Allowed: %d to %d", st.Min, st.Max)
	if text.unit != "" {
		v.Range += " " + text.unit
	}
	v.Range += "."
	if st.Set {
		v.State = fmt.Sprintf("The default is %d.", st.Default)
	} else {
		v.State = "This is the default."
	}
	return v
}

// settingsPage shows the settings.
func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, "", "", "")
}

// renderSettings sends the settings page with the status. msg is shown at
// the top. If refused is not "", it is the key of a setting whose save was
// refused as not a whole number in its bounds: its panel says so, with the
// bounds the settings package reports, and its input shows typed.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, msg, refused, typed string) {
	list, err := settings.List(r.Context(), s.db.Read)
	if err != nil {
		serverError(w, r, "list settings", err)
		return
	}
	views := make([]settingView, len(list))
	for i := range list {
		views[i] = newSettingView(&list[i])
		if refused != "" && list[i].Key == refused {
			views[i].Error = fmt.Sprintf("Enter a whole number from %d to %d.", list[i].Min, list[i].Max)
			views[i].Value = typed
		}
	}
	s.render(w, r, status, "settings", &pageData{Nav: "settings", Heading: "Settings", Error: msg, Settings: views})
}

// saveSetting stores the value the form gives for the setting it names.
func (s *Server) saveSetting(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, func(status int, msg string) { s.renderSettings(w, r, status, msg, "", "") }) {
		return
	}
	key, value := r.PostForm.Get("key"), r.PostForm.Get("value")
	err := settings.Set(r.Context(), s.db, key, value, s.now())
	switch {
	case err == nil:
		http.Redirect(w, r, "/settings#"+key, http.StatusSeeOther)
	case errors.Is(err, settings.ErrUnknownKey):
		s.renderSettings(w, r, http.StatusBadRequest, "Choose a setting.", "", "")
	case errors.Is(err, errkind.ErrInvalid):
		s.renderSettings(w, r, http.StatusBadRequest, "", key, value)
	default:
		serverError(w, r, "save setting", err)
	}
}

// resetSetting removes the stored value of the setting the form names, so
// it goes back to its default.
func (s *Server) resetSetting(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, func(status int, msg string) { s.renderSettings(w, r, status, msg, "", "") }) {
		return
	}
	key := r.PostForm.Get("key")
	err := settings.Reset(r.Context(), s.db, key)
	switch {
	case err == nil:
		http.Redirect(w, r, "/settings#"+key, http.StatusSeeOther)
	case errors.Is(err, settings.ErrUnknownKey):
		s.renderSettings(w, r, http.StatusBadRequest, "Choose a setting.", "", "")
	default:
		serverError(w, r, "reset setting", err)
	}
}

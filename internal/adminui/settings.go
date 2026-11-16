package adminui

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/die-net/trmnl-byos-go/internal/db"
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
	settings.KeyLocale: {
		label: "Locale",
		help:  []helpPart{{Text: "The language and formats screens are rendered in, unless a device sets its own. A change makes each affected screen render again."}},
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

// choiceOption is one value a choice setting may have, as an option of its
// select.
type choiceOption struct {
	// Value is the code that is saved, and Label is the option's text.
	Value, Label string
	// Selected is whether this is the value in effect.
	Selected bool
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
	// Range says what values are allowed, or is "" for a choice setting,
	// and State that the value is the default, or what the default is.
	Range, State string
	// Min and Max bound the input.
	Min, Max int64
	// Choices are the options of the select, for a setting with a fixed set
	// of values. It is nil for a setting that is a whole number.
	Choices []choiceOption
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
	if st.Choices != nil {
		v.Value = st.Text
		v.Choices = make([]choiceOption, len(st.Choices))
		for i, c := range st.Choices {
			v.Choices[i] = choiceOption{Value: c, Label: c, Selected: c == st.Text}
			if c == st.DefaultText {
				v.Choices[i].Label += " (default)"
			}
		}
	}
	if st.Bad != nil {
		v.Bad = `The stored value "` + *st.Bad + `" is not usable, so the default is in effect. Save a value or reset to the default.`
	}
	if text.seconds && st.Value >= 60 {
		v.Words = spanText(st.Value)
	}
	if st.Choices == nil {
		v.Range = fmt.Sprintf("Allowed: %d to %d", st.Min, st.Max)
		if text.unit != "" {
			v.Range += " " + text.unit
		}
		v.Range += "."
	}
	if st.Set {
		v.State = "The default is " + st.DefaultText + "."
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
// refused as not a whole number in its bounds, or not one of its choices: its
// panel says so, and for a number its input shows typed. The
// settings are read in one snapshot, so two changed together are never shown
// half changed.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, msg, refused, typed string) {
	var list []settings.Setting
	err := s.db.ReadTx(r.Context(), func(q *db.Queries) error {
		var err error
		list, err = settings.List(r.Context(), q)
		return err
	})
	if err != nil {
		serverError(w, r, "list settings", err)
		return
	}
	views := make([]settingView, 0, len(list))
	for i := range list {
		v := newSettingView(&list[i])
		if refused != "" && list[i].Key == refused {
			if list[i].Choices != nil {
				// A value outside the list cannot be shown, so the select
				// keeps the value in effect.
				v.Error = "Choose one of the listed values."
			} else {
				v.Error = fmt.Sprintf("Enter a whole number from %d to %d.", list[i].Min, list[i].Max)
				v.Value = typed
			}
		}
		views = append(views, v)
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

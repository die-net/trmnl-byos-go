package adminapi

import (
	"net/http"
	"strconv"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// settingsJSON is the settings that can be changed while the server runs,
// with the value in effect for each.
type settingsJSON struct {
	// RefreshSeconds is how long a device sleeps between wakes.
	RefreshSeconds int64 `json:"refresh_seconds"`
	// MinPasswordLength is the fewest bytes a new admin password may have.
	MinPasswordLength int64 `json:"min_password_length"`
	// Locale is the locale renders are made for, unless a device sets its
	// own. It is a string, one of trmnlp's locales.
	Locale string `json:"locale"`
	// Bad has the stored text of each setting whose stored value is not
	// usable, for which the number above is the default. It is left out
	// when none is.
	Bad *settingsBad `json:"bad,omitempty"`
}

// settingsBad is the stored text of the settings whose stored value is not
// usable. A pointer is nil for a setting that is fine, and the text can be
// empty.
type settingsBad struct {
	RefreshSeconds    *string `json:"refresh_seconds,omitempty"`
	MinPasswordLength *string `json:"min_password_length,omitempty"`
	Locale            *string `json:"locale,omitempty"`
}

// settingsPatch is the body of a settings change. A field left out keeps
// its value.
type settingsPatch struct {
	RefreshSeconds    *int64  `json:"refresh_seconds"`
	MinPasswordLength *int64  `json:"min_password_length"`
	Locale            *string `json:"locale"`
}

// getSettings answers with every setting. settings.List reads each one
// separately, so they are read in one snapshot, and two settings changed
// together are never shown half changed.
func (s *Server) getSettings(_ http.ResponseWriter, r *http.Request) (settingsJSON, error) {
	var list []settings.Setting
	err := s.db.ReadTx(r.Context(), func(q *db.Queries) error {
		var err error
		list, err = settings.List(r.Context(), q)
		return err
	})
	if err != nil {
		return settingsJSON{}, err
	}
	var out settingsJSON
	var bad settingsBad
	for i := range list {
		st := &list[i]
		switch st.Key {
		case settings.KeyRefreshSeconds:
			out.RefreshSeconds, bad.RefreshSeconds = st.Value, st.Bad
		case settings.KeyMinPasswordLength:
			out.MinPasswordLength, bad.MinPasswordLength = st.Value, st.Bad
		case settings.KeyLocale:
			out.Locale, bad.Locale = st.Text, st.Bad
		}
	}
	if bad != (settingsBad{}) {
		out.Bad = &bad
	}
	return out, nil
}

func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) (settingsJSON, error) {
	var req settingsPatch
	if err := decode(w, r, &req, false); err != nil {
		return settingsJSON{}, err
	}
	var changes []settings.Change
	for _, c := range []struct {
		key string
		val *int64
	}{
		{settings.KeyRefreshSeconds, req.RefreshSeconds},
		{settings.KeyMinPasswordLength, req.MinPasswordLength},
	} {
		if c.val != nil {
			changes = append(changes, settings.Change{Key: c.key, Value: strconv.FormatInt(*c.val, 10)})
		}
	}
	if req.Locale != nil {
		changes = append(changes, settings.Change{Key: settings.KeyLocale, Value: *req.Locale})
	}
	if err := settings.SetMany(r.Context(), s.db, changes, s.now()); err != nil {
		return settingsJSON{}, err
	}
	return s.getSettings(w, r)
}

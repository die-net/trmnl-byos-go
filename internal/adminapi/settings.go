package adminapi

import (
	"net/http"
	"strconv"

	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// settingsJSON is the settings that can be changed while the server runs,
// with the value in effect for each.
type settingsJSON struct {
	// RefreshSeconds is how long a device sleeps between wakes.
	RefreshSeconds int64 `json:"refresh_seconds"`
	// MinPasswordLength is the fewest bytes a new admin password may have.
	MinPasswordLength int64 `json:"min_password_length"`
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
}

// settingsPatch is the body of a settings change. A field left out keeps
// its value.
type settingsPatch struct {
	RefreshSeconds    *int64 `json:"refresh_seconds"`
	MinPasswordLength *int64 `json:"min_password_length"`
}

func (s *Server) getSettings(_ http.ResponseWriter, r *http.Request) (settingsJSON, error) {
	list, err := settings.List(r.Context(), s.db.Read)
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
	if err := settings.SetMany(r.Context(), s.db, changes, s.now()); err != nil {
		return settingsJSON{}, err
	}
	return s.getSettings(w, r)
}

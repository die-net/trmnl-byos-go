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
}

// settingsPatch is the body of a settings change. A field left out keeps
// its value.
type settingsPatch struct {
	RefreshSeconds    *int64 `json:"refresh_seconds"`
	MinPasswordLength *int64 `json:"min_password_length"`
}

func (s *Server) getSettings(_ http.ResponseWriter, r *http.Request) (settingsJSON, error) {
	ctx := r.Context()
	refresh, err := settings.RefreshInterval(ctx, s.db.Read)
	if err != nil {
		return settingsJSON{}, err
	}
	minLen, err := settings.MinPasswordLength(ctx, s.db.Read)
	if err != nil {
		return settingsJSON{}, err
	}
	return settingsJSON{RefreshSeconds: int64(refresh.Seconds()), MinPasswordLength: int64(minLen)}, nil
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

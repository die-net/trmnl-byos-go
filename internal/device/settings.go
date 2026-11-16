package device

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/locale"
	"github.com/die-net/trmnl-byos-go/internal/profile"
)

// ErrSettingsChanged means an update was given the SettingsState a page
// showed, and the device's settings are no longer what it showed, or the
// device is another one now. Nothing was changed.
var ErrSettingsChanged = errkind.New(errkind.ErrConflict, "the device's settings have changed")

// Change is a change to a device's settings. A nil field is left as it is.
type Change struct {
	// Name is the device's name, checked by CleanName. Empty means none.
	Name *string
	// UpdatesEnabled turns on or off offering the device firmware.
	UpdatesEnabled *bool
	// FwPin is a firmware version the device stays on, as
	// firmware.CleanPin returns it, or empty for none. The device package
	// cannot parse firmware versions, so the caller does.
	FwPin *string
	// Profile is the registry model the device's panel is, or empty for the
	// one its Model header gives.
	Profile *string
	// Palette is the palette the device renders with when an instance asks
	// for none, one of its model's palettes, or empty for the model's
	// default.
	Palette *string
	// TimeZone is the IANA time zone the device's schedules are read in and
	// its renders are made for, or empty for the server's. It is checked by
	// time.LoadLocation, and Local is refused, since it names the server
	// process's zone, which the empty value already is.
	TimeZone *string
	// Locale is the locale the device's renders are made for, one of
	// locale.Codes, or empty for the server's locale setting.
	Locale *string
	// Sleep is the window in which the device is shown no playlist item,
	// written HH:MM-HH:MM in the device's time zone, as ParseSleep takes it,
	// or empty for none, which is always awake.
	Sleep *string
	// Seen is the SettingsState the caller read, from a page or an API client.
	// Nil means none was sent, as from the command line, and is not checked.
	Seen *string
}

// SettingsState is a digest of the device's ID and its settings, which a
// page shows and posts back as Change.Seen. The ID is in it so a page for a
// deleted device cannot change a newer one that got the same friendly ID.
// Each part is framed by its length, so no two states encode alike.
func SettingsState(dev *db.Device) string {
	h := sha256.New()
	var n [8]byte
	put := func(s string) {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	for _, s := range []string{
		strconv.FormatInt(dev.ID, 10), strconv.FormatInt(dev.UpdatesEnabled, 10),
		dev.Name, dev.FwPin, dev.ProfileOverride, dev.PaletteOverride, dev.TimeZone, dev.Locale,
		FormatSleep(dev.SleepStart, dev.SleepEnd),
	} {
		put(s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Update changes the settings of the device named by its friendly ID or MAC
// and returns the device. Every field is checked before anything is
// written, and the check and the write are one transaction, so a refused
// change changes nothing. A profile must be a registry model, and a palette
// one of the palettes of the device's model: its new profile, else its
// stored one, else the one its Model header gives. A profile change that
// leaves the stored palette outside the new model's palettes is refused
// unless the change sets the palette too. A time zone must load, and a
// locale must be one of locale.Codes. A sleep window must parse, as
// ParseSleep says. Each of the profile, palette, zone, locale and sleep
// window is checked only when the change changes it from what is
// stored, so a stored one that stopped applying does not block a change of
// the others, nor a form that posts it back as it is.
func Update(ctx context.Context, client *db.Client, device string, c Change) (db.Device, error) {
	var name string
	if c.Name != nil {
		var err error
		if name, err = CleanName(*c.Name); err != nil {
			return db.Device{}, err
		}
	}
	var dev db.Device
	err := client.InTx(ctx, func(q *db.Queries) error {
		var err error
		if dev, err = FindDevice(ctx, q, device); err != nil {
			return err
		}
		if c.Seen != nil && *c.Seen != SettingsState(&dev) {
			return fmt.Errorf("%w: %s", ErrSettingsChanged, dev.FriendlyID)
		}
		if c.Name != nil {
			dev.Name = name
		}
		if c.UpdatesEnabled != nil {
			dev.UpdatesEnabled = 0
			if *c.UpdatesEnabled {
				dev.UpdatesEnabled = 1
			}
		}
		if c.FwPin != nil {
			dev.FwPin = *c.FwPin
		}
		// A profile or palette is checked only when the change changes it
		// from what is stored. A stored one can stop resolving without a
		// change, when the registry drops the model or the device reports
		// another one. Resolve then ignores it, and the device JSON says so,
		// but it must not block a change of the other settings, nor a form
		// that posts it back as it is.
		profileChanged := c.Profile != nil && *c.Profile != dev.ProfileOverride
		paletteChanged := c.Palette != nil && *c.Palette != dev.PaletteOverride
		if profileChanged {
			dev.ProfileOverride = *c.Profile
		}
		if paletteChanged {
			dev.PaletteOverride = *c.Palette
		}
		modelProblem, paletteProblem := ProfileOverride(&dev).Check(dev.FwModel)
		if profileChanged && modelProblem != "" {
			return fmt.Errorf("%w: %s", errkind.ErrInvalid, modelProblem)
		}
		if (profileChanged || paletteChanged) && paletteProblem != "" {
			return fmt.Errorf("%w: %s", errkind.ErrInvalid, paletteProblem)
		}
		if c.TimeZone != nil && *c.TimeZone != dev.TimeZone {
			if err := checkZone(*c.TimeZone); err != nil {
				return err
			}
			dev.TimeZone = *c.TimeZone
		}
		if c.Locale != nil && *c.Locale != dev.Locale {
			if *c.Locale != "" && !locale.Valid(*c.Locale) {
				return fmt.Errorf("%w: locale %q is not one of %s", errkind.ErrInvalid, *c.Locale, strings.Join(locale.Codes(), ", "))
			}
			dev.Locale = *c.Locale
		}
		if c.Sleep != nil && *c.Sleep != FormatSleep(dev.SleepStart, dev.SleepEnd) {
			start, end, err := ParseSleep(*c.Sleep)
			if err != nil {
				return err
			}
			dev.SleepStart, dev.SleepEnd = start, end
		}
		err = q.UpdateDeviceSettings(ctx, db.UpdateDeviceSettingsParams{
			Name: dev.Name, UpdatesEnabled: dev.UpdatesEnabled, FwPin: dev.FwPin,
			ProfileOverride: dev.ProfileOverride, PaletteOverride: dev.PaletteOverride,
			TimeZone: dev.TimeZone, Locale: dev.Locale, SleepStart: dev.SleepStart, SleepEnd: dev.SleepEnd, ID: dev.ID,
		})
		if err != nil {
			return fmt.Errorf("update device: %w", err)
		}
		return nil
	})
	return dev, err
}

// ParseSleep parses a sleep window written HH:MM-HH:MM, such as 23:00-06:30,
// into its start and end in minutes after midnight, 0 to 1439. An end before
// the start wraps past midnight. The empty string is no window, and gives -1
// for both. A bad format, an hour or minute out of range, and a start equal
// to the end, which would be a window of no time or of all of it, return an
// error wrapping errkind.ErrInvalid.
func ParseSleep(s string) (start, end int64, err error) {
	if s == "" {
		return -1, -1, nil
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("%w: sleep %q is not a window as HH:MM-HH:MM", errkind.ErrInvalid, s)
	}
	if start, err = parseClock(from); err != nil {
		return 0, 0, err
	}
	if end, err = parseClock(to); err != nil {
		return 0, 0, err
	}
	if start == end {
		return 0, 0, fmt.Errorf("%w: sleep %q starts and ends at the same time", errkind.ErrInvalid, s)
	}
	return start, end, nil
}

// parseClock parses HH:MM, 00:00 to 23:59, into minutes after midnight.
func parseClock(s string) (int64, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, fmt.Errorf("%w: %q is not a time as HH:MM, 00:00 to 23:59", errkind.ErrInvalid, s)
	}
	return int64(t.Hour()*60 + t.Minute()), nil
}

// FormatSleep is the inverse of ParseSleep: it writes a stored sleep window
// as HH:MM-HH:MM, or empty for none, which is a negative start.
func FormatSleep(start, end int64) string {
	if start < 0 || end < 0 {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", start/60, start%60, end/60, end%60)
}

// ProfileOverride is the profile override stored for a device.
func ProfileOverride(dev *db.Device) profile.Override {
	return profile.Override{Model: dev.ProfileOverride, Palette: dev.PaletteOverride}
}

// OverrideProblem says why a device's stored profile or palette override
// is ignored when its firmware reports fwModel, as a sentence, or "" when
// it applies or there is none. A stored override can stop applying when
// the registry drops its model or the device reports a model that does not
// take its palette. Renders and /api/display then use what the firmware
// reports, so the device is never stuck, and this is what the device's
// JSON shows so a page can flag it. /api/display passes the request's Model
// header, which it resolves the profile from, and a page passes the stored
// fw_model.
func OverrideProblem(dev *db.Device, fwModel string) string {
	model, palette := ProfileOverride(dev).Check(fwModel)
	switch {
	case model != "" && palette != "":
		return model + ", and " + palette
	case model != "":
		return model
	}
	return palette
}

// zones caches the locations loaded by name, so a request does not read the
// time zone database again. Only names that load are kept.
var zones sync.Map // string to *time.Location

// warned holds the stored values that did not load or are not allowed, so
// each is logged once and not at every request.
var warned sync.Map // string to struct{}

// loadZone returns the location named name, which is not empty. It refuses
// Local, which time.LoadLocation takes to mean the server process's zone.
func loadZone(name string) (*time.Location, error) {
	if v, ok := zones.Load(name); ok {
		return v.(*time.Location), nil
	}
	if name == "Local" {
		return nil, errors.New(`"Local" is the server's own zone, so leave the zone empty instead`)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Store(name, loc)
	return loc, nil
}

// checkZone returns an error of kind errkind.ErrInvalid if name is not empty
// and does not name a time zone.
func checkZone(name string) error {
	if name == "" {
		return nil
	}
	if _, err := loadZone(name); err != nil {
		return fmt.Errorf("%w: time zone %q: %w", errkind.ErrInvalid, name, err)
	}
	return nil
}

// warnOnce logs msg, with the stored value, the first time it sees the
// value under kind.
func warnOnce(kind, value, msg string) {
	if _, seen := warned.LoadOrStore(kind+" "+value, struct{}{}); !seen {
		slog.Warn(msg, kind, value)
	}
}

// ZoneOf returns the time zone in effect for a device whose time_zone
// setting is name: the zone it names, or server when name is empty. A name
// that no longer loads, which a change of the host's time zone database can
// cause, gives server, with one warning.
func ZoneOf(name string, server *time.Location) *time.Location {
	if name == "" {
		return server
	}
	loc, err := loadZone(name)
	if err != nil {
		warnOnce("time_zone", name, "device: stored time zone does not load; using the server's")
		return server
	}
	return loc
}

// LocaleOf returns the locale in effect for a device whose locale setting
// is code: code, or server, the server's locale setting, when code is empty.
// A code that is not one of locale.Codes, which a release that drops a
// locale can cause, gives server, with one warning.
func LocaleOf(code, server string) string {
	if code == "" {
		return server
	}
	if !locale.Valid(code) {
		warnOnce("locale", code, "device: stored locale is not one of the locales; using the server's")
		return server
	}
	return code
}

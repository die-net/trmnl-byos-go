package device

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
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
	// Seen is the SettingsState a page showed, or nil from the command line
	// and the API, which are not checked.
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
		dev.Name, dev.FwPin, dev.ProfileOverride, dev.PaletteOverride,
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
// unless the change sets the palette too. A change of neither does not
// check the palette, so a stored one that stopped fitting does not block
// it.
func Update(ctx context.Context, client *db.Client, device string, c Change) (db.Device, error) {
	var name string
	if c.Name != nil {
		var err error
		if name, err = CleanName(*c.Name); err != nil {
			return db.Device{}, err
		}
	}
	if c.Profile != nil && *c.Profile != "" {
		if _, ok := profile.ModelByName(*c.Profile); !ok {
			return db.Device{}, fmt.Errorf("%w: profile %q is not a model in the registry", errkind.ErrInvalid, *c.Profile)
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
		if c.Profile != nil {
			dev.ProfileOverride = *c.Profile
		}
		if c.Palette != nil {
			dev.PaletteOverride = *c.Palette
		}
		// Only a change of the profile or the palette checks the palette. A
		// stored one can stop fitting without either, when the device
		// reports another model or the registry changes, and Resolve then
		// ignores it, so it must not block a change of the other settings.
		if c.Profile != nil || c.Palette != nil {
			if err := checkPalette(&dev); err != nil {
				return err
			}
		}
		err = q.UpdateDeviceSettings(ctx, db.UpdateDeviceSettingsParams{
			Name: dev.Name, UpdatesEnabled: dev.UpdatesEnabled, FwPin: dev.FwPin,
			ProfileOverride: dev.ProfileOverride, PaletteOverride: dev.PaletteOverride, ID: dev.ID,
		})
		if err != nil {
			return fmt.Errorf("update device: %w", err)
		}
		return nil
	})
	return dev, err
}

// checkPalette returns an error of kind errkind.ErrInvalid if the device's
// palette override is not one of its model's palettes.
func checkPalette(dev *db.Device) error {
	if dev.PaletteOverride == "" {
		return nil
	}
	p := profile.Resolve(dev.FwModel, nil, nil, profile.Override{Model: dev.ProfileOverride})
	if !slices.Contains(p.Palettes, dev.PaletteOverride) {
		model := p.Model
		if model == "" {
			model = "an unknown model"
		}
		return fmt.Errorf("%w: palette %q is not one of %s's palettes", errkind.ErrInvalid, dev.PaletteOverride, model)
	}
	return nil
}

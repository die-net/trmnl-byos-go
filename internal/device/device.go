// Package device finds devices and sets what they show, and holds the
// setting that opens the window in which a new device may join. It has no
// HTTP code, so the device protocol, the admin API, the firmware rollout and
// the command line all use it.
package device

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// settingJoinUntil is the setting key holding when the join window closes,
// in Unix seconds.
const settingJoinUntil = "join_until"

// Screen overrides that show the gray calibration screen, with dither dots
// of 1x1 and 2x2 pixels.
const (
	ScreenCalibrate    = "calibrate"
	ScreenCalibrate2x2 = "calibrate-2x2"
)

// calibrationDots is the dither dot size of each calibration override.
var calibrationDots = map[string]int{ScreenCalibrate: 1, ScreenCalibrate2x2: 2}

// CalibrationDot returns the dither dot size of a calibration screen
// override, and false if the override is not a calibration screen.
func CalibrationDot(override string) (int, bool) {
	dot, ok := calibrationDots[override]
	return dot, ok
}

// ErrNoDevice means no registered device matches.
var ErrNoDevice = errkind.New(errkind.ErrNotFound, "no such device")

// DefaultCalibration is how long a device shows the calibration screen when
// no time is given.
const DefaultCalibration = 30 * time.Minute

// Calibrate makes the device named by its friendly ID or MAC show the
// calibration screen with dither dots of dot pixels, 1 or 2, from its next
// wake until window after now. It returns the device and when the screen
// ends, and an error of kind errkind.ErrInvalid for a dot size or window it
// does not take.
func Calibrate(ctx context.Context, client *db.Client, device string, dot int, window time.Duration, now time.Time) (db.Device, time.Time, error) {
	override := ""
	for name, d := range calibrationDots {
		if d == dot {
			override = name
		}
	}
	if override == "" {
		return db.Device{}, time.Time{}, fmt.Errorf("%w: dot size %d is not 1 or 2", errkind.ErrInvalid, dot)
	}
	if window <= 0 {
		return db.Device{}, time.Time{}, fmt.Errorf("%w: duration %v is not positive", errkind.ErrInvalid, window)
	}
	until := now.Add(window)
	dev, err := ShowScreen(ctx, client, device, override, until)
	return dev, until, err
}

// ShowScreen makes the device named by its friendly ID or MAC show the
// named system screen, such as ScreenCalibrate, until the given time, then
// go back to its usual screen. It returns the device.
func ShowScreen(ctx context.Context, client *db.Client, device, screenName string, until time.Time) (db.Device, error) {
	var dev db.Device
	err := client.InTx(ctx, func(q *db.Queries) error {
		var err error
		if dev, err = FindDevice(ctx, q, device); err != nil {
			return err
		}
		dev.ScreenOverride, dev.ScreenOverrideUntil = screenName, until.Unix()
		err = q.SetScreenOverride(ctx, db.SetScreenOverrideParams{
			ScreenOverride:      screenName,
			ScreenOverrideUntil: until.Unix(),
			ID:                  dev.ID,
		})
		if err != nil {
			return fmt.Errorf("set screen: %w", err)
		}
		return nil
	})
	return dev, err
}

// FindDevice returns the device named by its friendly ID or MAC.
func FindDevice(ctx context.Context, q *db.Queries, device string) (db.Device, error) {
	var dev db.Device
	var err error
	if mac, ok := ParseMAC(device); ok {
		dev, err = q.GetDeviceByMAC(ctx, mac)
	} else {
		dev, err = q.GetDeviceByFriendlyID(ctx, strings.ToUpper(device))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return db.Device{}, fmt.Errorf("%w: %s", ErrNoDevice, device)
	}
	if err != nil {
		return db.Device{}, fmt.Errorf("get device: %w", err)
	}
	return dev, nil
}

// MaxNameBytes is the longest a device's name may be.
const MaxNameBytes = 64

// CleanName returns name without the space around it, or an error of kind
// errkind.ErrInvalid if it is too long, is not UTF-8, or has a control
// character such as a newline. An empty name is valid and means none.
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case len(name) > MaxNameBytes:
		return "", fmt.Errorf("%w: name is over %d bytes", errkind.ErrInvalid, MaxNameBytes)
	case !utf8.ValidString(name):
		return "", fmt.Errorf("%w: name is not valid UTF-8", errkind.ErrInvalid)
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("%w: name has a control character", errkind.ErrInvalid)
	}
	return name, nil
}

// SetName gives the device named by its friendly ID or MAC a name, checked
// by CleanName, and returns the device. An empty name clears it.
func SetName(ctx context.Context, client *db.Client, device, name string) (db.Device, error) {
	name, err := CleanName(name)
	if err != nil {
		return db.Device{}, err
	}
	var dev db.Device
	err = client.InTx(ctx, func(q *db.Queries) error {
		var err error
		if dev, err = FindDevice(ctx, q, device); err != nil {
			return err
		}
		dev.Name = name
		if err := q.SetDeviceName(ctx, db.SetDeviceNameParams{Name: name, ID: dev.ID}); err != nil {
			return fmt.Errorf("set name: %w", err)
		}
		return nil
	})
	return dev, err
}

// SetJoinWindow lets devices with unknown MACs register through /api/setup
// until the given time. A time in the past closes the window.
func SetJoinWindow(ctx context.Context, client *db.Client, until time.Time) error {
	err := client.Write.SetSetting(ctx, db.SetSettingParams{
		Key:       settingJoinUntil,
		Value:     strconv.FormatInt(until.Unix(), 10),
		UpdatedAt: time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("set join window: %w", err)
	}
	return nil
}

// JoinUntil returns when the join window closes, or the zero time if it was
// never opened. A time in the past means it is closed.
func JoinUntil(ctx context.Context, q *db.Queries) (time.Time, error) {
	v, err := q.GetSetting(ctx, settingJoinUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get join window: %w", err)
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse join window %q: %w", v, err)
	}
	return time.Unix(sec, 0), nil
}

// ParseMAC returns a MAC address in the firmware's form, AA:BB:CC:DD:EE:FF.
func ParseMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return strings.ToUpper(hw.String()), true
}

// Health is how a device's last contact compares with when it should have
// woken.
type Health int

// The values of Health.
const (
	// NeverSeen is a device that has not contacted the server since it
	// registered.
	NeverSeen Health = iota
	// Online is a device that woke within twice its refresh interval.
	Online
	// Late is a device that has been quiet for more than twice its refresh
	// interval.
	Late
)

// lateFactor is how many refresh intervals a device may stay quiet before it
// is late. One missed wake does not make it so.
const lateFactor = 2

// StatusHealth says whether a device is online. lastSeen is its last contact
// in Unix seconds, or nil if it never made one, and refreshSeconds is the
// refresh_rate the server last sent it. A device with no recorded interval
// is late once it has been seen, since there is nothing to compare with.
func StatusHealth(lastSeen, refreshSeconds *int64, now time.Time) Health {
	if lastSeen == nil {
		return NeverSeen
	}
	if refreshSeconds != nil && now.Unix()-*lastSeen <= lateFactor**refreshSeconds {
		return Online
	}
	return Late
}

// ListStatus returns every device with what it last reported, ordered by
// name. It is one query, however many devices there are.
func ListStatus(ctx context.Context, q *db.Queries) ([]db.ListDeviceStatusRow, error) {
	rows, err := q.ListDeviceStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	return rows, nil
}

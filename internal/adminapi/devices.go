package adminapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// deviceJSON is a device and what it last reported. It has no api_key or
// its hash. A field the device never reported is null.
type deviceJSON struct {
	ID           string   `json:"id"`
	MAC          string   `json:"mac"`
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	Firmware     string   `json:"firmware"`
	CreatedAt    int64    `json:"created_at"`
	LastSeen     *int64   `json:"last_seen"`
	BatteryV     *float64 `json:"battery_v"`
	RSSI         *int64   `json:"rssi"`
	WakeTimeMs   *int64   `json:"wake_time_ms"`
	UpdateSource *string  `json:"update_source"`
	// UpdatesEnabled is whether the device may be offered firmware.
	UpdatesEnabled bool `json:"updates_enabled"`
	// FWPin is the firmware version the device stays on, or "" for none.
	FWPin string `json:"fw_pin"`
}

func newDevice(d *db.Device, lastSeen *int64, battery *float64, rssi, wakeMs *int64, source *string) deviceJSON {
	return deviceJSON{
		ID: d.FriendlyID, MAC: d.Mac, Name: d.Name, Model: d.FwModel, Firmware: d.FwVersion, CreatedAt: d.CreatedAt,
		LastSeen: lastSeen, BatteryV: battery, RSSI: rssi, WakeTimeMs: wakeMs, UpdateSource: source,
		UpdatesEnabled: d.UpdatesEnabled != 0, FWPin: d.FwPin,
	}
}

func (s *Server) listDevices(_ http.ResponseWriter, r *http.Request) ([]deviceJSON, error) {
	rows, err := device.ListStatus(r.Context(), s.db.Read)
	if err != nil {
		return nil, err
	}
	out := make([]deviceJSON, 0, len(rows))
	for i := range rows {
		x := &rows[i]
		out = append(out, newDevice(&x.Device, x.LastSeen, x.BatteryV, x.Rssi, x.WakeTimeMs, x.UpdateSource))
	}
	return out, nil
}

// getDevice finds the device and reads its status in one snapshot, so a
// device deleted between the two reads is not found rather than an error.
func (s *Server) getDevice(_ http.ResponseWriter, r *http.Request) (deviceJSON, error) {
	ctx := r.Context()
	var out deviceJSON
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		dev, err := device.FindDevice(ctx, q, r.PathValue("id"))
		if err != nil {
			return err
		}
		out, err = deviceStatus(ctx, q, dev.ID)
		return err
	})
	return out, err
}

// deviceStatus returns a device and what it last reported, read through q.
// A device that is gone, as after a delete that came in just after a write
// the caller made, wraps device.ErrNoDevice.
func deviceStatus(ctx context.Context, q *db.Queries, id int64) (deviceJSON, error) {
	x, err := q.GetDeviceStatus(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return deviceJSON{}, fmt.Errorf("%w: %d", device.ErrNoDevice, id)
	}
	if err != nil {
		return deviceJSON{}, fmt.Errorf("get device status: %w", err)
	}
	return newDevice(&x.Device, x.LastSeen, x.BatteryV, x.Rssi, x.WakeTimeMs, x.UpdateSource), nil
}

// calibrateRequest is the body of a calibrate request. A field left out
// takes its default.
type calibrateRequest struct {
	// Dot is the dither dot size in pixels, 1 or 2. Default 1.
	Dot int `json:"dot"`
	// ForSeconds is how long the device shows the screen. Default
	// device.DefaultCalibration.
	ForSeconds int64 `json:"for_seconds"`
}

// calibrateResponse says when the calibration screen ends.
type calibrateResponse struct {
	Device string `json:"device"`
	Dot    int    `json:"dot"`
	Until  int64  `json:"until"`
}

func (s *Server) calibrate(w http.ResponseWriter, r *http.Request) (calibrateResponse, error) {
	req := calibrateRequest{Dot: 1}
	if err := decode(w, r, &req, true); err != nil {
		return calibrateResponse{}, err
	}
	window := device.DefaultCalibration
	if req.ForSeconds != 0 {
		var err error
		if window, err = duration(req.ForSeconds); err != nil {
			return calibrateResponse{}, err
		}
	}
	dev, until, err := device.Calibrate(r.Context(), s.db, r.PathValue("id"), req.Dot, window, s.now())
	if err != nil {
		return calibrateResponse{}, err
	}
	return calibrateResponse{Device: dev.FriendlyID, Dot: req.Dot, Until: until.Unix()}, nil
}

// joinJSON is the state of the join window.
type joinJSON struct {
	// Open is whether new devices can register now.
	Open bool `json:"open"`
	// Until is when the window closes, in Unix seconds, or 0 if it never
	// opened.
	Until int64 `json:"until"`
}

// joinRequest is the body of a join window change.
type joinRequest struct {
	// ForSeconds is how long the window stays open from now. 0 closes it.
	ForSeconds *int64 `json:"for_seconds"`
}

func (s *Server) getJoin(_ http.ResponseWriter, r *http.Request) (joinJSON, error) {
	until, err := device.JoinUntil(r.Context(), s.db.Read)
	if err != nil {
		return joinJSON{}, err
	}
	return s.join(until), nil
}

func (s *Server) putJoin(w http.ResponseWriter, r *http.Request) (joinJSON, error) {
	var req joinRequest
	if err := decode(w, r, &req, false); err != nil {
		return joinJSON{}, err
	}
	if req.ForSeconds == nil {
		return joinJSON{}, fmt.Errorf("%w: for_seconds is required", errBadBody)
	}
	window, err := duration(*req.ForSeconds)
	if err != nil {
		return joinJSON{}, err
	}
	until := s.now().Add(window)
	if err := device.SetJoinWindow(r.Context(), s.db, until); err != nil {
		return joinJSON{}, err
	}
	return s.join(until), nil
}

func (s *Server) join(until time.Time) joinJSON {
	if until.IsZero() {
		return joinJSON{}
	}
	return joinJSON{Open: until.After(s.now()), Until: until.Unix()}
}

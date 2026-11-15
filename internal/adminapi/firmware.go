package adminapi

import (
	"fmt"
	"mime"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
	"github.com/die-net/trmnl-byos-go/internal/upload"
)

// firmwareJSON is a firmware build in the catalog and its rollout.
type firmwareJSON struct {
	ID         int64  `json:"id"`
	Model      string `json:"model"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	Chip       string `json:"chip"`
	IDFVersion string `json:"idf_version"`
	Bytes      int64  `json:"bytes"`
	Source     string `json:"source"`
	UploadedAt int64  `json:"uploaded_at"`
	// Rollout is null for a build that was never rolled out.
	Rollout *rolloutJSON `json:"rollout"`
}

// rolloutJSON is where a build's rollout stands.
type rolloutJSON struct {
	// State is canary while only the canary is offered the build, open
	// once the canary has reported its version and every device of the
	// model is offered it, stalled when the canary was offered it the most
	// times it is and does not report its version, so nothing is offered
	// until the rollout is restarted or canceled, or canceled.
	State string `json:"state"`
	// Canary is the canary device's friendly ID, and CanaryVersion the
	// firmware version it last reported.
	Canary        string `json:"canary"`
	CanaryVersion string `json:"canary_version"`
	StartedAt     int64  `json:"started_at"`
	// LastOfferedAt is when the canary was last offered the build, in Unix
	// seconds, or 0 if never.
	LastOfferedAt int64 `json:"last_offered_at"`
}

func newFirmware(e *firmware.Entry) firmwareJSON {
	out := firmwareJSON{
		ID: e.ID, Model: e.FwModel, Version: e.Version, SHA256: e.Sha256, Chip: e.Chip,
		IDFVersion: e.IdfVersion, Bytes: e.Bytes, Source: e.Source, UploadedAt: e.UploadedAt,
	}
	if e.Stage != "" {
		out.Rollout = &rolloutJSON{State: e.Stage, Canary: e.Canary, CanaryVersion: e.CanaryVersion, StartedAt: e.StartedAt, LastOfferedAt: e.CanaryLastOfferedAt}
	}
	return out
}

func (s *Server) listFirmware(_ http.ResponseWriter, r *http.Request) ([]firmwareJSON, error) {
	rows, err := firmware.List(r.Context(), s.db.Read)
	if err != nil {
		return nil, err
	}
	out := make([]firmwareJSON, 0, len(rows))
	for i := range rows {
		out = append(out, newFirmware(&rows[i]))
	}
	return out, nil
}

// firmwareDeviceJSON is one device's standing for a build.
type firmwareDeviceJSON struct {
	Device  string `json:"device"`
	Name    string `json:"name"`
	Model   string `json:"model"`
	Version string `json:"version"`
	// Canary is whether the device is the build's rollout canary.
	Canary bool  `json:"canary"`
	Offers int64 `json:"offers"`
	// LastOfferedAt is when the device was last offered the build, in Unix
	// seconds, or 0 if never.
	LastOfferedAt int64 `json:"last_offered_at"`
	// Reason is a code for why the device would not be offered the build
	// now, or "" when it would, and Why a sentence saying so.
	Reason string `json:"reason"`
	Why    string `json:"why"`
	// CanBeCanary is whether the device could be the build's canary now.
	CanBeCanary bool `json:"can_be_canary"`
}

// listFirmwareDevices lists the devices of the build's model and whether
// each would be offered the build now.
func (s *Server) listFirmwareDevices(_ http.ResponseWriter, r *http.Request) ([]firmwareDeviceJSON, error) {
	id, err := pathID(r, firmware.ErrNoFirmware)
	if err != nil {
		return nil, err
	}
	rows, err := firmware.DeviceOffers(r.Context(), s.db.Read, s.cfg.FirmwareDir, id)
	if err != nil {
		return nil, err
	}
	out := make([]firmwareDeviceJSON, len(rows))
	for i := range rows {
		o := &rows[i]
		out[i] = firmwareDeviceJSON{
			Device: o.FriendlyID, Name: o.Name, Model: o.Model, Version: o.Version, Canary: o.Canary,
			Offers: o.Offers, LastOfferedAt: o.LastOfferedAt, Reason: string(o.Reason), Why: o.Why, CanBeCanary: o.CanBeCanary,
		}
	}
	return out, nil
}

// addFirmware adds the firmware file in the request body, for the model in
// the query and, if it is given, the version. A body that is cut off is the
// client's error, not the server's.
func (s *Server) addFirmware(w http.ResponseWriter, r *http.Request) (firmwareJSON, error) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/octet-stream" {
		return firmwareJSON{}, &statusError{status: http.StatusUnsupportedMediaType, msg: "send the firmware as a raw file with Content-Type application/octet-stream"}
	}
	upload.AllowSlow(w)
	q := r.URL.Query()
	body := upload.Reader(http.MaxBytesReader(w, r.Body, firmware.MaxUploadBytes()))
	fw, err := firmware.Add(r.Context(), s.db, s.cfg.FirmwareDir, q.Get("model"), q.Get("version"), body, s.now())
	if err != nil {
		return firmwareJSON{}, err
	}
	return newFirmware(&firmware.Entry{Firmware: fw}), nil
}

// rolloutRequest is the body of a rollout start.
type rolloutRequest struct {
	// Canary is the friendly ID or MAC of the device that gets the build
	// first.
	Canary string `json:"canary"`
}

func (s *Server) startRollout(w http.ResponseWriter, r *http.Request) (firmwareJSON, error) {
	id, err := pathID(r, firmware.ErrNoFirmware)
	if err != nil {
		return firmwareJSON{}, err
	}
	var req rolloutRequest
	if err := decode(w, r, &req, false); err != nil {
		return firmwareJSON{}, err
	}
	if req.Canary == "" {
		return firmwareJSON{}, fmt.Errorf("%w: canary is required", errBadBody)
	}
	e, err := firmware.StartRollout(r.Context(), s.db, id, req.Canary, s.now())
	if err != nil {
		return firmwareJSON{}, err
	}
	return newFirmware(&e), nil
}

func (s *Server) cancelRollout(r *http.Request) error {
	id, err := pathID(r, firmware.ErrNoFirmware)
	if err != nil {
		return err
	}
	return firmware.CancelRollout(r.Context(), s.db, id)
}

// devicePatch is the body of a device change. A field left out is not
// changed.
type devicePatch struct {
	// Name is the device's name, or "" for none.
	Name *string `json:"name"`
	// UpdatesEnabled turns on or off offering the device firmware.
	UpdatesEnabled *bool `json:"updates_enabled"`
	// FWPin is a firmware version the device stays on, or "" for none.
	FWPin *string `json:"fw_pin"`
}

func (s *Server) patchDevice(w http.ResponseWriter, r *http.Request) (deviceJSON, error) {
	var req devicePatch
	if err := decode(w, r, &req, false); err != nil {
		return deviceJSON{}, err
	}
	// Check the name before the policy is written, so a bad name changes
	// nothing. SetPolicy checks the pin the same way.
	if req.Name != nil {
		if _, err := device.CleanName(*req.Name); err != nil {
			return deviceJSON{}, err
		}
	}
	id := r.PathValue("id")
	dev, err := firmware.SetPolicy(r.Context(), s.db, id, firmware.Policy{UpdatesEnabled: req.UpdatesEnabled, Pin: req.FWPin})
	if err != nil {
		return deviceJSON{}, err
	}
	if req.Name != nil {
		if _, err := device.SetName(r.Context(), s.db, id, *req.Name); err != nil {
			return deviceJSON{}, err
		}
	}
	return s.deviceStatus(r.Context(), dev.ID)
}

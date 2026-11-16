package adminapi

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/device"
)

// blockedJSON is a blocked MAC.
type blockedJSON struct {
	MAC string `json:"mac"`
	// BlockedAt is when the MAC was first blocked, in Unix seconds.
	BlockedAt int64 `json:"blocked_at"`
	// Device is the friendly ID of the registered device that has the MAC,
	// or null if none does.
	Device *string `json:"device"`
	// Name is that device's name, or null likewise.
	Name *string `json:"name"`
}

func (s *Server) deleteDevice(r *http.Request) error {
	_, err := device.Delete(r.Context(), s.db, r.PathValue("id"), s.now())
	return err
}

func (s *Server) listBlocked(_ http.ResponseWriter, r *http.Request) ([]blockedJSON, error) {
	rows, err := device.ListBlocked(r.Context(), s.db.Read)
	if err != nil {
		return nil, err
	}
	out := make([]blockedJSON, 0, len(rows))
	for i := range rows {
		x := &rows[i]
		out = append(out, blockedJSON{MAC: x.Mac, BlockedAt: x.BlockedAt, Device: x.FriendlyID, Name: x.Name})
	}
	return out, nil
}

// putBlocked blocks the MAC in the path, a MAC or a registered device's
// friendly ID. It answers with the block, and the device that has the MAC,
// which it looks up after the block, since a device is not part of one.
func (s *Server) putBlocked(_ http.ResponseWriter, r *http.Request) (blockedJSON, error) {
	ctx := r.Context()
	b, err := device.Block(ctx, s.db, r.PathValue("id"), s.now())
	if err != nil {
		return blockedJSON{}, err
	}
	out := blockedJSON{MAC: b.Mac, BlockedAt: b.BlockedAt}
	dev, err := s.db.Read.GetDeviceByMAC(ctx, b.Mac)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return blockedJSON{}, fmt.Errorf("get device: %w", err)
	default:
		out.Device, out.Name = &dev.FriendlyID, &dev.Name
	}
	return out, nil
}

func (s *Server) deleteBlocked(r *http.Request) error {
	_, err := device.Unblock(r.Context(), s.db, r.PathValue("id"))
	return err
}

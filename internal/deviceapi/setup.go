package deviceapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

type setupResponse struct {
	Status     int    `json:"status"`
	APIKey     string `json:"api_key,omitempty"`
	FriendlyID string `json:"friendly_id,omitempty"`
	// ImageURL is shown during setup. Empty for a registered device, which
	// then draws its own screen with the friendly ID.
	ImageURL string `json:"image_url"`
	Message  string `json:"message"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	debugRequest(r)
	mac, ok := device.ParseMAC(r.Header.Get("ID"))
	if !ok {
		http.Error(w, "missing or bad ID header", http.StatusBadRequest)
		return
	}
	now := s.now()
	// Every setup gets a new key. Only its hash is stored, so a device that
	// runs setup again, having lost its credentials, cannot be given its old
	// key back, and the old key stops working. Setup needs the join window
	// even for a known MAC: anything on the LAN can see a MAC, so a MAC
	// alone must not be enough to take over a device.
	key := rand.Text()
	var dev db.Device
	registered, blocked, known := false, false, false
	err := s.db.InTx(ctx, func(q *db.Queries) error {
		registered, blocked, known = false, false, false
		// A registered device's refusal screen is drawn with its profile
		// override.
		var err error
		dev, err = q.GetDeviceByMAC(ctx, mac)
		switch {
		case err == nil:
			known = true
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("get device: %w", err)
		}
		// A blocked MAC is refused even while the join window is open.
		standing, err := q.GetMACStanding(ctx, mac)
		if err != nil {
			return fmt.Errorf("get mac standing: %w", err)
		}
		if standing.Blocked != 0 {
			blocked = true
			return nil
		}
		until, err := device.JoinUntil(ctx, q)
		if err != nil || !now.Before(until) {
			return err
		}
		// A deleted device's MAC registers as a new device, and its
		// tombstone goes, since there is no old key left to reset.
		if standing.Deleted != 0 {
			if err := q.DeleteDeviceTombstone(ctx, mac); err != nil {
				return fmt.Errorf("delete tombstone: %w", err)
			}
		}
		if known {
			if err := q.SetDeviceAPIKeyHash(ctx, db.SetDeviceAPIKeyHashParams{ApiKeyHash: hashKey(key), ID: dev.ID}); err != nil {
				return fmt.Errorf("set key: %w", err)
			}
		} else if dev, err = createDevice(ctx, q, mac, hashKey(key), r.Header.Get("Model"), r.Header.Get("FW-Version"), now); err != nil {
			return err
		}
		registered = true
		return nil
	})
	if err != nil {
		slog.Error("setup", "mac", mac, "err", err)
		http.Error(w, "setup failed", http.StatusInternalServerError)
		return
	}
	if !registered {
		resp := setupResponse{Status: statusSetupUnknown, Message: "MAC not registered"}
		if blocked {
			slog.Info("setup refused: MAC blocked", "mac", mac)
			resp.Message = "MAC blocked"
		} else {
			slog.Info("setup refused: join window closed", "mac", mac)
		}
		var o profile.Override
		if known {
			o = overrideOf(&dev)
		}
		name, err := systemScreen(r, o, func(sp screen.Profile) (string, error) {
			if blocked {
				return s.blockedScreen(sp, mac, false)
			}
			return s.screen(sp, []screen.Line{
				{Text: "Not registered", Size: 44, Bold: true},
				{Text: mac, Size: 28},
				{Text: "Open the join window on the server.", Size: 22},
				{Text: "This display tries again every 5 minutes.", Size: 18},
			})
		})
		if err != nil {
			// The firmware shows its own message when there is no image.
			slog.Error("setup: render screen", "mac", mac, "err", err)
		} else {
			resp.ImageURL = s.imageURL(r, name)
		}
		writeJSON(w, resp)
		return
	}
	slog.Info("setup", "mac", mac, "friendly_id", dev.FriendlyID, "model", dev.FwModel)
	writeJSON(w, setupResponse{Status: statusSetupOK, APIKey: key, FriendlyID: dev.FriendlyID})
}

// createDevice registers a device with a new friendly ID and the hash of its
// API key.
func createDevice(ctx context.Context, q *db.Queries, mac string, keyHash []byte, model, version string, now time.Time) (db.Device, error) {
	for range 10 {
		id := friendlyID()
		exists, err := q.FriendlyIDExists(ctx, id)
		if err != nil {
			return db.Device{}, fmt.Errorf("check friendly id: %w", err)
		}
		if exists != 0 {
			continue
		}
		dev, err := q.CreateDevice(ctx, db.CreateDeviceParams{
			Mac:        mac,
			ApiKeyHash: keyHash,
			FriendlyID: id,
			FwModel:    model,
			FwVersion:  version,
			CreatedAt:  now.Unix(),
		})
		if err != nil {
			return db.Device{}, fmt.Errorf("create device: %w", err)
		}
		return dev, nil
	}
	return db.Device{}, errors.New("create device: no free friendly id")
}

// friendlyAlphabet has 32 symbols, so a random byte maps to one without
// bias, and leaves out 0, 1, I and O, which are easy to misread.
const friendlyAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// friendlyID returns a random 6-character ID for people to tell devices
// apart by.
func friendlyID() string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	for i := range b {
		b[i] = friendlyAlphabet[b[i]%byte(len(friendlyAlphabet))]
	}
	return string(b[:])
}

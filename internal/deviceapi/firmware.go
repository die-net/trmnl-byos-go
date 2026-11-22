package deviceapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
)

// offerFirmware returns the firmware build the device should install, or
// nil. A failure to decide is logged and offers nothing: a device not told
// to update loses nothing, and one told wrongly can be bricked. model and
// version are what the device sent this time, before they are saved.
func (s *Server) offerFirmware(ctx context.Context, dev *db.Device, model, version string) *db.Firmware {
	if s.cfg.FirmwareDir == "" {
		return nil
	}
	fw, err := firmware.OfferAndRecord(ctx, s.db, s.cfg.FirmwareDir, dev, model, version, s.now())
	if err != nil {
		if !errkind.Ended(ctx, err) {
			slog.ErrorContext(ctx, "display: choose firmware", "mac", dev.Mac, "err", err)
		}
		return nil
	}
	return fw
}

// firmwareWriteTimeout is how long a firmware download may take. It is
// longer than the server's usual write timeout, since the file is
// megabytes and a device on weak Wi-Fi is slow.
const firmwareWriteTimeout = 10 * time.Minute

// firmware serves a firmware file that is in an active rollout. The
// device's firmware sends no credentials when it downloads firmware_url, so
// the route is open, like the images. The hash in the path, and serving
// only builds in a rollout, are what limit it. The file is not secret.
func (s *Server) firmware(w http.ResponseWriter, r *http.Request) {
	debugRequest(r)
	sha, ok := firmware.SHAFromName(r.PathValue("name"))
	if !ok || s.cfg.FirmwareDir == "" {
		http.NotFound(w, r)
		return
	}
	f, fw, err := firmware.Open(r.Context(), s.db.Read, s.cfg.FirmwareDir, sha)
	if errors.Is(err, firmware.ErrNoFirmware) {
		http.NotFound(w, r)
		return
	}
	if requestEnded(r, "firmware download", err) {
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "firmware download", "sha256", sha, "err", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if err := http.NewResponseController(w).SetWriteDeadline(s.now().Add(firmwareWriteTimeout)); err != nil {
		slog.Warn("firmware download: extend write deadline", "err", err)
	}
	slog.Info("firmware download", "model", fw.FwModel, "version", fw.Version, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f)
}

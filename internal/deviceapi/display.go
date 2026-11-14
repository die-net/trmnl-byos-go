package deviceapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

type displayResponse struct {
	Status   int    `json:"status"`
	ImageURL string `json:"image_url,omitempty"`
	// Filename is the image's content hash. The firmware skips the
	// download when it has a file by that name, and the repaint when that
	// image is already on the panel.
	Filename string `json:"filename,omitempty"`
	// RefreshRate is seconds until the next wake. The firmware reads it as
	// a number, never a string.
	RefreshRate     int64  `json:"refresh_rate"`
	ResetFirmware   bool   `json:"reset_firmware"`
	UpdateFirmware  bool   `json:"update_firmware"`
	SpecialFunction string `json:"special_function,omitempty"`
}

// displayFailed answers a /api/display request that failed inside the
// server. The firmware treats an HTTP error as a failed connection: it
// retries five times in a few seconds, then sleeps five minutes, and shows
// an unrelated Wi-Fi error after an hour of that. A status 0 answer with no
// image_url makes it keep its screen and sleep for refresh_rate, so this
// sends that, with the scheduler's retry interval. Status 500 is never
// sent, since it erases the device's credentials.
func displayFailed(w http.ResponseWriter, mac, step string, err error) {
	slog.Error("display: "+step, "mac", mac, "err", err)
	writeJSON(w, displayResponse{Status: statusOK, RefreshRate: retryRefresh, SpecialFunction: "none"})
}

func (s *Server) display(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	debugRequest(r)
	dev, ok, err := s.authenticate(ctx, r)
	if err != nil {
		displayFailed(w, r.Header.Get("ID"), "authenticate", err)
		return
	}
	if !ok {
		stale, err := s.staleKey(ctx, r)
		if err != nil {
			displayFailed(w, r.Header.Get("ID"), "check for a stale key", err)
			return
		}
		if stale {
			slog.Warn("display: registered MAC with a stale key; resetting the device", "mac", r.Header.Get("ID"))
			writeJSON(w, displayResponse{Status: statusOK, ResetFirmware: true, RefreshRate: int64(s.cfg.RefreshRate / time.Second)})
			return
		}
		slog.Warn("display: unknown device or bad token", "mac", r.Header.Get("ID"))
		writeJSON(w, displayResponse{Status: statusNotRegistered})
		return
	}

	model, version := r.Header.Get("Model"), r.Header.Get("FW-Version")
	p := profile.Resolve(model, headerInt(r, "Width"), headerInt(r, "Height"))
	sp := screenProfile(p)
	choice := screenChoice{refresh: int64(s.cfg.RefreshRate / time.Second)}
	dot, calibrating := device.CalibrationDot(dev.ScreenOverride)
	if left := dev.ScreenOverrideUntil - s.now().Unix(); calibrating && left > 0 && sp.Levels > 2 {
		choice.name, choice.size, err = s.cached(fmt.Sprintf("calibration %d %+v", dot, sp), func(w io.Writer) error { return screen.Calibration(w, sp, dot) })
		// Wake when the override ends, so the usual screen comes back on time.
		choice.refresh = min(choice.refresh, left)
	} else {
		choice, err = s.playlistScreen(ctx, dev, p, sp, playlist.Pressed(r.Header.Get("Update-Source")), choice.refresh)
		if err == nil && !choice.shown {
			choice.name, choice.size, err = s.screenSized(sp, []screen.Line{
				{Text: "Connected", Size: 44, Bold: true},
				{Text: dev.FriendlyID, Size: 64, Bold: true},
				{Text: "Nothing is scheduled for this display yet.", Size: 22},
				{Swatches: true, Size: 36},
				{Text: fmt.Sprintf("Each box above should be a different shade, %d in all.", sp.Levels), Size: 16},
				{Text: model + " · firmware " + version, Size: 18},
			})
		}
	}
	if err == nil && !p.Fits(choice.size) {
		// The firmware would fail the download on every wake and keep
		// showing its old screen, so it gets a small screen saying why.
		slog.Warn("display: image too large for the device", "mac", dev.Mac, "image", choice.name, "bytes", choice.size, "limit", p.ImageSizeLimit)
		choice.name, err = s.tooLarge(sp, choice.size, p.ImageSizeLimit)
		choice.refresh = tooLargeRefresh
	}
	if err != nil {
		displayFailed(w, dev.Mac, "render screen", err)
		return
	}

	err = s.db.InTx(ctx, func(q *db.Queries) error {
		if (model != "" && model != dev.FwModel) || (version != "" && version != dev.FwVersion) {
			// An empty header leaves the stored value alone in the query.
			if err := q.UpdateDeviceFirmware(ctx, db.UpdateDeviceFirmwareParams{FwModel: model, FwVersion: version, ID: dev.ID}); err != nil {
				return fmt.Errorf("update firmware: %w", err)
			}
		}
		err := q.UpsertDeviceState(ctx, db.UpsertDeviceStateParams{
			DeviceID:            dev.ID,
			LastSeen:            s.now().Unix(),
			BatteryV:            headerFloat(r, "Battery-Voltage"),
			Rssi:                headerInt(r, "RSSI"),
			WakeTimeMs:          headerInt(r, "Wake-Time"),
			UpdateSource:        headerString(r, "Update-Source"),
			LastRefreshRateSent: choice.refresh,
			LastImage:           choice.name,
			Width:               headerInt(r, "Width"),
			Height:              headerInt(r, "Height"),
		})
		if err != nil {
			return fmt.Errorf("save device state: %w", err)
		}
		return nil
	})
	if err != nil {
		displayFailed(w, dev.Mac, "save state", err)
		return
	}

	resp := displayResponse{
		Status:          statusOK,
		ImageURL:        s.imageURL(r, choice.name),
		Filename:        choice.name,
		RefreshRate:     choice.refresh,
		SpecialFunction: "none",
	}
	writeJSON(w, resp)
}

// Refresh rates, in seconds, for a device whose playlist item has no
// image yet: soon while its first render is under way, and at the
// scheduler's retry interval (db.RenderRetryAfter) after a render failed.
const (
	pendingRefresh = 60
	retryRefresh   = int64(db.RenderRetryAfter / time.Second)
)

// staleAfter is how old an instance's last good render may be before the
// device shows that it is out of date instead: two refresh intervals and
// an hour, which allows for a failed build or two.
func staleAfter(refresh time.Duration) time.Duration {
	return 2*refresh + time.Hour
}

// screenChoice is what a device is shown on one wake.
type screenChoice struct {
	// name is the image's name, and size its size in bytes.
	name string
	size int64
	// refresh is the seconds until the device wakes again.
	refresh int64
	// shown is false when the device's playlist has no items, so nothing
	// was chosen.
	shown bool
}

// playlistScreen picks what a device shows from its playlist, moving its
// cursor first. The refresh rate it returns is refresh, shortened to wake the
// device when the item ends or a render is ready.
func (s *Server) playlistScreen(ctx context.Context, dev db.Device, p profile.Profile, sp screen.Profile, pressed bool, refresh int64) (screenChoice, error) {
	item, started, ok, err := s.advance(ctx, dev.ID, pressed)
	if err != nil || !ok {
		return screenChoice{refresh: refresh}, err
	}
	now := s.now()
	if item.Duration > 0 {
		left := started.Add(time.Duration(item.Duration) * time.Second).Sub(now)
		refresh = min(refresh, max(int64(left/time.Second), 1))
	}
	row, err := s.db.Read.GetItemScreen(ctx, db.GetItemScreenParams{
		Width:      int64(p.Width),
		Height:     int64(p.Height),
		Palette:    p.RenderPalette(item.Palette),
		Layout:     "full",
		DeviceID:   dev.ID,
		InstanceID: item.InstanceID,
	})
	if err != nil {
		return screenChoice{refresh: refresh}, fmt.Errorf("get item screen: %w", err)
	}
	failure := ""
	if row.Error != nil {
		failure = clip(*row.Error)
	}
	if row.Image != nil && row.RenderedAt != nil && *row.RenderedAt != 0 && now.Sub(time.Unix(*row.RenderedAt, 0)) <= staleAfter(time.Duration(row.RefreshInterval)*time.Second) {
		// Mark the image used, as the system screens do. A render can
		// replace the row before the device fetches, and then no row names
		// the image, which may have been stored long ago.
		_, stored, err := s.images.Refresh(*row.Image)
		if err != nil {
			return screenChoice{refresh: refresh}, err
		}
		if stored {
			return screenChoice{name: *row.Image, size: *row.Bytes, refresh: refresh, shown: true}, nil
		}
		// The image is gone, so the device would fetch a 404 on every wake.
		// Delete the render, as if it did not exist yet, so the scheduler
		// builds it again. The delete holds only while the row still names
		// this image, since a build may have replaced it.
		slog.Warn("render image is missing from the image store; deleting the render", "image", *row.Image, "device", dev.ID)
		if err := s.db.Write.DeleteRenderWithImage(ctx, db.DeleteRenderWithImageParams{ID: *row.RenderID, Image: *row.Image}); err != nil {
			return screenChoice{refresh: refresh}, fmt.Errorf("delete render: %w", err)
		}
		row.Image, failure = nil, ""
	}
	var lines []screen.Line
	switch {
	case row.Image == nil || row.RenderedAt == nil || *row.RenderedAt == 0:
		if failure == "" {
			lines = []screen.Line{
				{Text: "Rendering", Size: 44, Bold: true},
				{Text: row.Name, Size: 28},
				{Text: "This display checks again in a minute.", Size: 20},
			}
			refresh = min(refresh, pendingRefresh)
			break
		}
		lines = []screen.Line{
			{Text: "Could not render", Size: 44, Bold: true},
			{Text: row.Name, Size: 28},
			{Text: failure, Size: 18},
		}
		refresh = min(refresh, retryRefresh)
	default:
		// The age is in whole hours so the screen, and so its image name,
		// stays the same between wakes and the device does not repaint.
		hours := int(now.Sub(time.Unix(*row.RenderedAt, 0)) / time.Hour)
		unit := "hours"
		if hours == 1 {
			unit = "hour"
		}
		lines = []screen.Line{
			{Text: "Out of date", Size: 44, Bold: true},
			{Text: row.Name, Size: 28},
			{Text: fmt.Sprintf("Last rendered over %d %s ago.", hours, unit), Size: 20},
		}
		if failure != "" {
			lines = append(lines, screen.Line{Text: failure, Size: 18})
		}
		refresh = min(refresh, retryRefresh)
	}
	name, size, err := s.screenSized(sp, lines)
	return screenChoice{name: name, size: size, refresh: refresh, shown: true}, err
}

// advance moves a device's playlist cursor for this wake and returns the
// item it now shows and when that started. ok is false when the device's
// playlist has no items.
func (s *Server) advance(ctx context.Context, deviceID int64, pressed bool) (item db.ListDeviceItemsRow, started time.Time, ok bool, err error) {
	err = s.db.InTx(ctx, func(q *db.Queries) error {
		items, err := q.ListDeviceItems(ctx, deviceID)
		if err != nil {
			return fmt.Errorf("list playlist items: %w", err)
		}
		cursor, err := q.GetDeviceCursor(ctx, deviceID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get cursor: %w", err)
		}
		var cur *playlist.Cursor
		if err == nil {
			cur = &playlist.Cursor{ItemID: cursor.ItemID, Started: time.Unix(cursor.ItemStartedAt, 0)}
		}
		pl := make([]playlist.Item, len(items))
		for i := range items {
			pl[i] = playlist.Item{ID: items[i].ID, Duration: time.Duration(items[i].Duration) * time.Second}
		}
		i, start := playlist.Advance(pl, cur, s.now(), pressed)
		if i < 0 {
			return nil
		}
		item, started, ok = items[i], start, true
		if cur != nil && cur.ItemID == item.ID && cur.Started.Equal(started) {
			return nil
		}
		err = q.SetDeviceCursor(ctx, db.SetDeviceCursorParams{
			DeviceID:      deviceID,
			ItemID:        item.ID,
			ItemStartedAt: started.Unix(),
		})
		if err != nil {
			return fmt.Errorf("set cursor: %w", err)
		}
		return nil
	})
	return item, started, ok, err
}

// maxLineRunes bounds a line of text from elsewhere, such as an error, on a
// system screen, which does not wrap.
const maxLineRunes = 72

// clip cuts s to its first line and maxLineRunes runes.
func clip(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > maxLineRunes {
		return string(r[:maxLineRunes-1]) + "…"
	}
	return s
}

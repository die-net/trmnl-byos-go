// Package deviceapi serves the endpoints TRMNL firmware calls: /api/setup,
// /api/display and /api/log, plus the images /api/display points at.
//
// The firmware reads a JSON status field, not the HTTP status, so /api/setup
// and /api/display answer HTTP 200 with the outcome in the body.
package deviceapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/scheduler"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

// Statuses in a response's JSON status field, as the firmware reads them.
const (
	// statusOK is what /api/display sends for a normal response. The
	// firmware ignores a response whose status is 200.
	statusOK = 0
	// statusSetupOK is what /api/setup sends for a registered device.
	statusSetupOK = 200
	// statusNotRegistered tells /api/display the device is unknown. The
	// device keeps its credentials and Wi-Fi settings, shows its own setup
	// screen, and polls again soon. Status 500 would wipe both, the same as
	// reset_firmware, so it is never sent.
	statusNotRegistered = 202
	// statusSetupUnknown tells /api/setup the MAC is not registered. The
	// device shows the response's image and tries again in 5 minutes.
	statusSetupUnknown = 404
)

const (
	// maxLogBody bounds an /api/log request body.
	maxLogBody = 1 << 20
	// keepLogs is how many log entries are kept per device.
	keepLogs = 1000
	// settingJoinUntil is the setting key holding when the join window
	// closes, in Unix seconds.
	settingJoinUntil = "join_until"
)

// Config is how the device API is set up.
type Config struct {
	// PublicURL is the base URL devices reach this server at, such as
	// http://192.168.1.10:8080. Empty uses the scheme and Host of each
	// request.
	PublicURL string
	// RefreshRate is how long a device sleeps between wakes.
	RefreshRate time.Duration
}

// Server handles device requests.
type Server struct {
	db     *db.Client
	images *imagestore.Store
	cfg    Config
	now    func() time.Time
	// resolve finds a device's profile: profile.Resolve.
	resolve func(fwModel string, width, height *int64) (profile.Profile, error)

	mu sync.Mutex
	// screens maps a system screen's profile and text to its image name,
	// so a screen is drawn once, not on every wake. It is drawn again only
	// if image expiry deleted it.
	screens map[string]string
}

// New returns a Server that keeps state in client and images in images.
func New(client *db.Client, images *imagestore.Store, cfg Config) *Server {
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	return &Server{db: client, images: images, cfg: cfg, now: time.Now, resolve: profile.Resolve, screens: map[string]string{}}
}

// Register adds the device endpoints to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", s.setup)
	mux.HandleFunc("GET /api/display", s.display)
	mux.HandleFunc("POST /api/log", s.log)
	mux.Handle("GET /images/{name}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		debugRequest(r)
		s.images.ServeHTTP(w, r)
	}))
}

// Screen overrides that show the gray calibration screen, with dither dots
// of 1x1 and 2x2 pixels.
const (
	ScreenCalibrate    = "calibrate"
	ScreenCalibrate2x2 = "calibrate-2x2"
)

// calibrationDots is the dither dot size of each calibration override.
var calibrationDots = map[string]int{ScreenCalibrate: 1, ScreenCalibrate2x2: 2}

// ErrNoDevice means no registered device matches.
var ErrNoDevice = errors.New("no such device")

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
	if mac, ok := parseMAC(device); ok {
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

func joinUntil(ctx context.Context, q *db.Queries) (time.Time, error) {
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
	mac, ok := parseMAC(r.Header.Get("ID"))
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
	registered := false
	err := s.db.InTx(ctx, func(q *db.Queries) error {
		until, err := joinUntil(ctx, q)
		if err != nil || !now.Before(until) {
			return err
		}
		dev, err = q.GetDeviceByMAC(ctx, mac)
		switch {
		case err == nil:
			if err := q.SetDeviceAPIKeyHash(ctx, db.SetDeviceAPIKeyHashParams{ApiKeyHash: hashKey(key), ID: dev.ID}); err != nil {
				return fmt.Errorf("set key: %w", err)
			}
		case errors.Is(err, sql.ErrNoRows):
			dev, err = createDevice(ctx, q, mac, hashKey(key), r.Header.Get("Model"), r.Header.Get("FW-Version"), now)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("get device: %w", err)
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
		slog.Info("setup refused: join window closed", "mac", mac)
		resp := setupResponse{Status: statusSetupUnknown, Message: "MAC not registered"}
		name, err := s.systemScreen(r, func(sp screen.Profile) (string, error) {
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

func (s *Server) display(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	debugRequest(r)
	dev, ok, err := s.authenticate(ctx, r)
	if err != nil {
		slog.Error("display: authenticate", "err", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		stale, err := s.staleKey(ctx, r)
		if err != nil {
			slog.Error("display: check for a stale key", "err", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
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
	p, err := s.resolve(model, headerInt(r, "Width"), headerInt(r, "Height"))
	if err != nil {
		slog.Error("display: resolve profile", "mac", dev.Mac, "err", err)
		http.Error(w, "profile failed", http.StatusInternalServerError)
		return
	}
	sp, err := screenProfile(p)
	if err != nil {
		slog.Error("display: screen profile", "mac", dev.Mac, "err", err)
		http.Error(w, "profile failed", http.StatusInternalServerError)
		return
	}
	refresh := int64(s.cfg.RefreshRate / time.Second)
	var name string
	var size int64
	dot, calibrating := calibrationDots[dev.ScreenOverride]
	if left := dev.ScreenOverrideUntil - s.now().Unix(); calibrating && left > 0 && sp.Levels > 2 {
		name, size, err = s.cached(fmt.Sprintf("calibration %d %+v", dot, sp), func(w io.Writer) error { return screen.Calibration(w, sp, dot) })
		// Wake when the override ends, so the usual screen comes back on time.
		refresh = min(refresh, left)
	} else {
		var shown bool
		name, size, refresh, shown, err = s.playlistScreen(ctx, dev, p, sp, playlist.Pressed(r.Header.Get("Update-Source")), refresh)
		if err == nil && !shown {
			name, size, err = s.screenSized(sp, []screen.Line{
				{Text: "Connected", Size: 44, Bold: true},
				{Text: dev.FriendlyID, Size: 64, Bold: true},
				{Text: "Nothing is scheduled for this display yet.", Size: 22},
				{Swatches: true, Size: 36},
				{Text: fmt.Sprintf("Each box above should be a different shade, %d in all.", sp.Levels), Size: 16},
				{Text: model + " · firmware " + version, Size: 18},
			})
		}
	}
	if err == nil && !p.Fits(size) {
		// The firmware would fail the download on every wake and keep
		// showing its old screen, so it gets a small screen saying why.
		slog.Warn("display: image too large for the device", "mac", dev.Mac, "image", name, "bytes", size, "limit", p.ImageSizeLimit)
		name, err = s.tooLarge(sp, size, p.ImageSizeLimit)
		refresh = tooLargeRefresh
	}
	if err != nil {
		slog.Error("display: render screen", "mac", dev.Mac, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	err = s.db.InTx(ctx, func(q *db.Queries) error {
		if (model != "" && model != dev.FwModel) || (version != "" && version != dev.FwVersion) {
			if model == "" {
				model = dev.FwModel
			}
			if version == "" {
				version = dev.FwVersion
			}
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
			LastRefreshRateSent: refresh,
			LastImage:           name,
			Width:               headerInt(r, "Width"),
			Height:              headerInt(r, "Height"),
		})
		if err != nil {
			return fmt.Errorf("save device state: %w", err)
		}
		return nil
	})
	if err != nil {
		slog.Error("display", "mac", dev.Mac, "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	writeJSON(w, displayResponse{
		Status:          statusOK,
		ImageURL:        s.imageURL(r, name),
		Filename:        name,
		RefreshRate:     refresh,
		SpecialFunction: "none",
	})
}

// Refresh rates, in seconds, for a device whose playlist item has no
// image yet: soon while its first render is under way, and at the
// scheduler's retry interval after a render failed.
const (
	pendingRefresh = 60
	retryRefresh   = int64(scheduler.RetryAfter / time.Second)
)

// staleAfter is how old an instance's last good render may be before the
// device shows that it is out of date instead: two refresh intervals and
// an hour, which allows for a failed build or two.
func staleAfter(refresh time.Duration) time.Duration {
	return 2*refresh + time.Hour
}

// playlistScreen picks what a device shows from its playlist, moving its
// cursor first. It returns the image, its size, and the refresh rate,
// shortened to wake the device when the item ends or a render is ready.
// shown is false when the device's playlist has no items.
func (s *Server) playlistScreen(ctx context.Context, dev db.Device, p profile.Profile, sp screen.Profile, pressed bool, refresh int64) (name string, size, newRefresh int64, shown bool, err error) {
	item, started, ok, err := s.advance(ctx, dev.ID, pressed)
	if err != nil || !ok {
		return "", 0, refresh, false, err
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
		InstanceID: item.InstanceID,
	})
	if err != nil {
		return "", 0, refresh, false, fmt.Errorf("get item screen: %w", err)
	}
	failure := ""
	if row.Error != nil {
		failure = clip(*row.Error)
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
	case now.Sub(time.Unix(*row.RenderedAt, 0)) > staleAfter(time.Duration(row.RefreshInterval)*time.Second):
		age := now.Sub(time.Unix(*row.RenderedAt, 0)).Truncate(time.Minute)
		lines = []screen.Line{
			{Text: "Out of date", Size: 44, Bold: true},
			{Text: row.Name, Size: 28},
			{Text: "Last rendered " + age.String() + " ago.", Size: 20},
		}
		if failure != "" {
			lines = append(lines, screen.Line{Text: failure, Size: 18})
		}
		refresh = min(refresh, retryRefresh)
	default:
		return *row.Image, *row.Bytes, refresh, true, nil
	}
	name, size, err = s.screenSized(sp, lines)
	return name, size, refresh, true, err
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
		if cursor.ItemID != nil && cursor.ItemStartedAt != nil {
			cur = &playlist.Cursor{ItemID: *cursor.ItemID, Started: time.Unix(*cursor.ItemStartedAt, 0)}
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
			LastSeen:      s.now().Unix(),
			ItemID:        &item.ID,
			ItemStartedAt: new(started.Unix()),
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

// logEntry is the part of a firmware log entry that gets its own columns.
// The whole entry is kept as sent.
type logEntry struct {
	CreatedAt int64  `json:"created_at"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	debugRequest(r)
	dev, ok, err := s.authenticate(ctx, r)
	if err != nil {
		slog.Error("log: authenticate", "err", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown device or bad token", http.StatusUnauthorized)
		return
	}
	var body struct {
		Logs []json.RawMessage `json:"logs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLogBody)).Decode(&body); err != nil {
		http.Error(w, "bad log body", http.StatusBadRequest)
		return
	}
	now := s.now().Unix()
	err = s.db.InTx(ctx, func(q *db.Queries) error {
		for _, raw := range body.Logs {
			// An entry whose fields have unexpected types is still kept
			// whole in detail, with whatever columns did parse.
			var e logEntry
			_ = json.Unmarshal(raw, &e)
			err := q.InsertDeviceLog(ctx, db.InsertDeviceLogParams{
				DeviceID:   dev.ID,
				ReceivedAt: now,
				DeviceAt:   e.CreatedAt,
				Level:      e.Level,
				Message:    e.Message,
				Detail:     string(raw),
			})
			if err != nil {
				return fmt.Errorf("save log entry: %w", err)
			}
		}
		if err := q.TrimDeviceLog(ctx, db.TrimDeviceLogParams{DeviceID: dev.ID, Keep: keepLogs}); err != nil {
			return fmt.Errorf("trim log: %w", err)
		}
		return nil
	})
	if err != nil {
		slog.Error("log", "mac", dev.Mac, "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authenticate finds the device whose API key is the Access-Token header,
// by the key's hash. Looking up a hash leaks nothing useful through timing,
// unlike looking up or comparing the key itself. The ID header is optional,
// since some non-TRMNL clients leave it out, but when present it must be
// the device's MAC. ok is false for an unknown key or a mismatched ID.
func (s *Server) authenticate(ctx context.Context, r *http.Request) (dev db.Device, ok bool, err error) {
	token := r.Header.Get("Access-Token")
	if token == "" {
		return db.Device{}, false, nil
	}
	dev, err = s.db.Read.GetDeviceByAPIKeyHash(ctx, hashKey(token))
	if errors.Is(err, sql.ErrNoRows) {
		return db.Device{}, false, nil
	}
	if err != nil {
		return db.Device{}, false, fmt.Errorf("get device: %w", err)
	}
	if id := r.Header.Get("ID"); id != "" {
		if mac, valid := parseMAC(id); !valid || mac != dev.Mac {
			return db.Device{}, false, nil
		}
	}
	return dev, true, nil
}

// staleKey reports whether a request that failed authenticate comes from a
// registered MAC holding a key that is no longer its key, because the MAC
// was paired again since. Such a device would otherwise keep its old key
// and never run setup again, so it is told to reset, which erases its key,
// Wi-Fi settings and server URL and starts its setup portal. A request with
// no token, no ID, or an unknown MAC is not stale: it may come from a
// server that lost its database, and resetting then would wipe every
// display at once.
func (s *Server) staleKey(ctx context.Context, r *http.Request) (bool, error) {
	if r.Header.Get("Access-Token") == "" {
		return false, nil
	}
	mac, ok := parseMAC(r.Header.Get("ID"))
	if !ok {
		return false, nil
	}
	_, err := s.db.Read.GetDeviceByMAC(ctx, mac)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get device: %w", err)
	}
	return true, nil
}

// hashKey is how a device API key is stored and looked up. The key is 128
// random bits, so a fast hash is enough: there is nothing to brute-force.
func hashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// tooLargeRefresh is how long a device showing the "too large" screen
// sleeps, so it picks up a fix soon.
const tooLargeRefresh = 300

// tooLarge renders the screen a device gets instead of an image over its
// size limit.
func (s *Server) tooLarge(sp screen.Profile, size, limit int64) (string, error) {
	return s.screen(sp, []screen.Line{
		{Text: "Image too large", Size: 44, Bold: true},
		{Text: fmt.Sprintf("This screen is %d KB; this display takes at most %d KB.", (size+1023)/1024, limit/1024), Size: 20},
		{Text: "Try a palette with fewer grays for it on the server.", Size: 18},
	})
}

// screenProfile is what system screens are drawn for on a device: its size
// and its richest gray palette, with the palette's measured light if known.
func screenProfile(p profile.Profile) (screen.Profile, error) {
	levels, err := p.GrayLevels()
	if err != nil {
		return screen.Profile{}, err
	}
	sp := screen.Profile{Width: p.Width, Height: p.Height, Levels: levels}
	if len(p.Light) == levels {
		sp.Light = p.Light
	}
	return sp, nil
}

// systemScreen resolves the requesting device's profile from its headers
// and draws a system screen for it with draw.
func (s *Server) systemScreen(r *http.Request, draw func(screen.Profile) (string, error)) (string, error) {
	p, err := s.resolve(r.Header.Get("Model"), headerInt(r, "Width"), headerInt(r, "Height"))
	if err != nil {
		return "", err
	}
	sp, err := screenProfile(p)
	if err != nil {
		return "", err
	}
	return draw(sp)
}

// screen renders a system screen of text lines, stores it, and returns its
// image name.
func (s *Server) screen(p screen.Profile, lines []screen.Line) (string, error) {
	name, _, err := s.screenSized(p, lines)
	return name, err
}

// screenSized is screen, also returning the image's size in bytes.
func (s *Server) screenSized(p screen.Profile, lines []screen.Line) (string, int64, error) {
	return s.cached(fmt.Sprintf("lines %+v %+v", p, lines), func(w io.Writer) error { return screen.Render(w, p, lines) })
}

// cached returns the name and size of the image render draws for key. It
// draws the image straight into the image store the first time key is seen,
// and again only if the stored image is gone. Using it marks it used, so
// image expiry keeps it.
func (s *Server) cached(key string, render func(io.Writer) error) (string, int64, error) {
	s.mu.Lock()
	name, ok := s.screens[key]
	s.mu.Unlock()
	if ok {
		size, stored, err := s.images.Refresh(name)
		if err != nil || stored {
			return name, size, err
		}
	}
	w, err := s.images.Create()
	if err != nil {
		return "", 0, err
	}
	if err := render(w); err != nil {
		w.Abort()
		return "", 0, err
	}
	name, size, err := w.Commit()
	if err != nil {
		return "", 0, err
	}
	s.mu.Lock()
	s.screens[key] = name
	s.mu.Unlock()
	return name, size, nil
}

// imageURL is the absolute URL a device fetches the named image from. The
// firmware cannot follow a relative one.
func (s *Server) imageURL(r *http.Request, name string) string {
	base := s.cfg.PublicURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/images/" + name + ".png"
}

// parseMAC returns a MAC address in the firmware's form, AA:BB:CC:DD:EE:FF.
func parseMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return strings.ToUpper(hw.String()), true
}

func headerString(r *http.Request, name string) *string {
	v := r.Header.Get(name)
	if v == "" {
		return nil
	}
	return &v
}

func headerInt(r *http.Request, name string) *int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get(name)), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

func headerFloat(r *http.Request, name string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(r.Header.Get(name)), 64)
	if err != nil {
		return nil
	}
	return &v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}

// debugRequest logs a device request's headers at debug level, without its
// Access-Token, so real requests can be captured for tests.
func debugRequest(r *http.Request) {
	if !slog.Default().Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	attrs := make([]any, 0, len(r.Header)+2)
	attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
	for name, vals := range r.Header {
		if name == "Access-Token" {
			continue
		}
		attrs = append(attrs, slog.String(name, strings.Join(vals, ", ")))
	}
	slog.Debug("device request", attrs...)
}

package deviceapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
	"github.com/die-net/trmnl-byos-go/internal/settings"
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
	RefreshRate   int64 `json:"refresh_rate"`
	ResetFirmware bool  `json:"reset_firmware"`
	// UpdateFirmware tells the device to install the firmware at
	// FirmwareURL. It is only ever true with a URL.
	UpdateFirmware bool   `json:"update_firmware"`
	FirmwareURL    string `json:"firmware_url,omitempty"`
	// SpecialFunction is what the device's next double-click asks for. The
	// firmware saves it from every normal answer, so an answer that leaves it
	// out or sends none turns the double-click off until the next one.
	SpecialFunction string `json:"special_function,omitempty"`
	// Action names the function the device asked for with a special_function
	// header. The firmware does nothing useful with an answer to one that
	// has none.
	Action string `json:"action,omitempty"`
}

// displayFailed answers a /api/display request that failed inside the
// server. The firmware treats an HTTP error as a failed connection: it
// retries five times in a few seconds, then sleeps five minutes, and shows
// an unrelated Wi-Fi error after an hour of that. A status 0 answer with no
// image_url makes it keep its screen and sleep for refresh_rate, so this
// sends that, with the scheduler's retry interval. Status 500 is never
// sent, since it erases the device's credentials. special is the special
// function to send, which the firmware saves: a known device's configured
// one, so a failure does not turn its double-click off, and none for a
// request that came from no known device, which reads nothing. A request
// whose context ended has no one to answer, so it is not logged as an error
// and gets no answer.
func displayFailed(w http.ResponseWriter, r *http.Request, mac, step, special string, err error) {
	if requestEnded(r, "display: "+step, err) {
		return
	}
	slog.ErrorContext(r.Context(), "display: "+step, "mac", mac, "err", err)
	writeJSON(w, r, displayResponse{Status: statusOK, RefreshRate: retryRefresh, SpecialFunction: special})
}

// refreshRate returns the seconds a device sleeps between wakes, from the
// refresh_seconds setting, read now so a change applies at the next wake. A
// setting that cannot be read is logged and the default is used, since the
// device still needs an answer.
func (s *Server) refreshRate(ctx context.Context) int64 {
	d, err := settings.RefreshInterval(ctx, s.db.Read)
	if err != nil && !errkind.Ended(ctx, err) {
		slog.ErrorContext(ctx, "display: read the refresh interval; using the default", "err", err)
	}
	return int64(d / time.Second)
}

func (s *Server) display(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	debugRequest(r)
	c, err := s.identify(ctx, r)
	if err != nil {
		displayFailed(w, r, r.Header.Get("ID"), "authenticate", device.FuncNone, err)
		return
	}
	switch {
	case c.blocked():
		slog.Warn("display: blocked MAC", "mac", c.mac)
		var o profile.Override
		if c.ok {
			o = device.ProfileOverride(&c.dev)
		}
		name, err := systemScreen(r, o, func(sp screen.Profile) (string, error) { return s.blockedScreen(sp, c.mac, true) })
		if err != nil {
			displayFailed(w, r, c.mac, "render screen", device.FuncNone, err)
			return
		}
		writeJSON(w, r, displayResponse{Status: statusOK, ImageURL: s.imageURL(r, name), Filename: name, RefreshRate: blockedRefresh, SpecialFunction: device.FuncNone})
		return
	case c.reset(r):
		slog.Warn("display: MAC deleted or paired again; resetting the device", "mac", c.mac, "deleted", c.standing.Deleted != 0)
		writeJSON(w, r, displayResponse{Status: statusOK, ResetFirmware: true, RefreshRate: s.refreshRate(ctx)})
		return
	case !c.ok:
		slog.Warn("display: unknown device or bad token", "mac", r.Header.Get("ID"))
		writeJSON(w, r, displayResponse{Status: statusNotRegistered})
		return
	}
	dev := c.dev
	model, version := r.Header.Get("Model"), r.Header.Get("FW-Version")
	// The override is checked against the model this request reports, which
	// is the one Resolve applies it to. The stored fw_model is updated only
	// later in the request, and not at all for an empty Model header.
	if problem := device.OverrideProblem(&dev, model); problem != "" {
		// Resolve falls back to what the firmware reports, so the device
		// is not stuck. The device's JSON shows the problem for as long as
		// it lasts, so it is logged once per process, not at every wake.
		if _, seen := s.overrideWarned.LoadOrStore(fmt.Sprintf("%d %s", dev.ID, problem), struct{}{}); !seen {
			slog.Warn("display: ignoring the device's profile override", "mac", dev.Mac, "problem", problem)
		}
	}

	p := profile.Resolve(model, headerInt(r, "Width"), headerInt(r, "Height"), device.ProfileOverride(&dev))
	sp := screenProfile(p)
	special := device.SpecialFunctionOf(dev.SpecialFunction)
	// The firmware sends a special_function header only to ask for one. On
	// a wake without it, and only then, it saves the answer's
	// special_function, which a double-click later runs.
	normal := strings.TrimSpace(r.Header.Get("special_function")) == ""
	// sent is the function the device saved: the one recorded as sent in the
	// last answer to a normal wake, or the configured one when none was
	// recorded. A double-click sends true and runs it, whatever is configured
	// now, and the firmware acts only on an answer with its action.
	st, err := s.db.Read.GetDeviceStatus(ctx, dev.ID)
	if err != nil {
		// What the device saved is not known, so this sends none, which
		// the next normal answer replaces, rather than a function the
		// record may not match.
		displayFailed(w, r, dev.Mac, "get device state", device.FuncNone, err)
		return
	}
	sent := special
	if st.LastSpecialFunctionSent != nil && *st.LastSpecialFunctionSent != "" {
		sent = *st.LastSpecialFunctionSent
	}
	fn := requestedFunction(r, sent)
	// A true that names no function, because the device saved none as far
	// as the server knows, still runs whatever the device saved. That draws
	// nothing for an answer without its action, so the answer has no image
	// and the cursor does not move, even at an item's end, since moving it
	// would skip an item the device never shows.
	saved := fn == "" && strings.EqualFold(strings.TrimSpace(r.Header.Get("special_function")), "true")
	// The TRMNL X's guest_mode and toggle_hold are answered as a timer wake,
	// with the screen the device would show now and its usual refresh. The
	// firmware draws that image and applies the refresh, so the device looks
	// unchanged and its menu, which covers the screen, is cleared.
	guest := fn == funcGuestMode || fn == funcToggleHold
	// The firmware applies a refresh_rate to a normal answer, to sleep and
	// to guest_mode. For any other special function it keeps the interval it
	// stored from an earlier answer, counted from this wake, so that interval
	// is what the device sleeps, and what is recorded for the scheduler to
	// predict its next wake.
	keepRate := saved || (fn != "" && fn != device.FuncSleep && !guest)
	// A press moves the cursor as the device's button command says. A
	// restart_playlist moves it to the first item shown. Any other function
	// leaves the cursor alone, since it does not show a new item.
	command := ""
	if playlist.Pressed(r.Header.Get("Update-Source")) && fn == "" && !saved {
		command = device.ButtonCommandOf(dev.ButtonCommand)
	}
	if fn == device.FuncRestartPlaylist {
		command = device.ButtonFirst
	}
	choice := screenChoice{refresh: s.refreshRate(ctx)}
	dot, calibrating := device.CalibrationDot(dev.ScreenOverride)
	left := dev.ScreenOverrideUntil - s.now().Unix()
	switch {
	case (fn != "" && fn != device.FuncRestartPlaylist && !guest) || saved:
		choice, err = s.functionScreen(dev, sp, fn, choice.refresh)
	case (fn == "" || guest) && calibrating && left > 0 && sp.Levels > 2:
		choice.name, choice.size, err = s.cached(fmt.Sprintf("calibration %d %+v", dot, sp), func(w io.Writer) error { return screen.Calibration(w, sp, dot) })
		// Wake when the override ends, so the usual screen comes back on time.
		choice.refresh = min(choice.refresh, left)
	default:
		choice, err = s.playlistScreen(ctx, dev, p, sp, command, choice.refresh)
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
	// A failure answer to a normal wake is saved by the firmware too, so it
	// sends the function recorded as sent, which this request does not
	// change, and the record stays what the device holds.
	if err != nil {
		displayFailed(w, r, dev.Mac, "render screen", sent, err)
		return
	}

	// record is the function the device saves from this answer, or "", which
	// keeps the record, for an answer it saves none from.
	record := ""
	if normal {
		record = special
	}
	err = s.db.InTx(ctx, func(q *db.Queries) error {
		if (model != "" && model != dev.FwModel) || (version != "" && version != dev.FwVersion) {
			// An empty header leaves the stored value alone in the query.
			// model and version stay as sent, for the firmware offer.
			if err := q.UpdateDeviceFirmware(ctx, db.UpdateDeviceFirmwareParams{FwModel: model, FwVersion: version, ID: dev.ID}); err != nil {
				return fmt.Errorf("update firmware: %w", err)
			}
		}
		if keepRate {
			latest, err := q.GetDeviceStatus(ctx, dev.ID)
			if err != nil {
				return fmt.Errorf("get device state: %w", err)
			}
			// A device with no state yet has had no answer to store a
			// rate from, so the one chosen here is the best guess. The
			// state is read again here, under the write lock.
			if latest.LastRefreshRateSent != nil {
				choice.refresh = *latest.LastRefreshRateSent
			}
		}
		err := q.UpsertDeviceState(ctx, db.UpsertDeviceStateParams{
			DeviceID:                dev.ID,
			LastSeen:                s.now().Unix(),
			BatteryV:                headerFloat(r, "Battery-Voltage"),
			Rssi:                    headerInt(r, "RSSI"),
			WakeTimeMs:              headerInt(r, "Wake-Time"),
			UpdateSource:            headerString(r, "Update-Source"),
			LastRefreshRateSent:     choice.refresh,
			LastImage:               choice.name,
			LastSpecialFunctionSent: record,
			Width:                   headerInt(r, "Width"),
			Height:                  headerInt(r, "Height"),
		})
		if err != nil {
			return fmt.Errorf("save device state: %w", err)
		}
		return nil
	})
	if err != nil {
		displayFailed(w, r, dev.Mac, "save state", sent, err)
		return
	}
	// A device's first wake, or one that reports another size or model,
	// gives it render targets it did not have, which are due now.
	if st.LastSeen == nil || !samePtr(st.Width, headerInt(r, "Width")) || !samePtr(st.Height, headerInt(r, "Height")) ||
		(model != "" && model != dev.FwModel) {
		s.db.Changed()
	}

	resp := displayResponse{
		Status:          statusOK,
		RefreshRate:     choice.refresh,
		SpecialFunction: special,
		Action:          functionAction(fn),
	}
	if choice.name != "" {
		resp.ImageURL, resp.Filename = s.imageURL(r, choice.name), choice.name
	}
	// The firmware would take an offer in any answer, but an update never
	// starts from a double-click or the TRMNL X menu, which ask for
	// something else. The next wake without a special_function header gets
	// the offer.
	if normal {
		if fw := s.offerFirmware(ctx, &dev, model, version); fw != nil {
			slog.Info("display: offering firmware", "mac", dev.Mac, "model", model, "from", version, "to", fw.Version)
			resp.UpdateFirmware, resp.FirmwareURL = true, s.baseURL(r)+"/firmware/"+firmware.FileName(fw.Sha256)
		}
	}
	writeJSON(w, r, resp)
}

// The special functions that are TRMNL cloud features, which only the TRMNL
// X sends, from its menu. This server has no use for them, so it answers
// each so that the device looks unchanged and the menu is cleared.
const (
	funcSendToMe   = "send_to_me"
	funcGuestMode  = "guest_mode"
	funcToggleHold = "toggle_hold"
)

// cloudFunctions are the TRMNL cloud features a device may ask for.
var cloudFunctions = []string{funcSendToMe, funcGuestMode, funcToggleHold}

// requestedFunction returns the special function a request asks for, or ""
// for a normal wake. The firmware sends the header as true to run the
// function it saved, which is saved, and as a function's name to ask for
// that one, as the TRMNL X menu and touch bar do. A value that names no
// function, such as none, is a normal wake.
func requestedFunction(r *http.Request, saved string) string {
	v := strings.ToLower(strings.TrimSpace(r.Header.Get("special_function")))
	if v == "true" {
		v = saved
	}
	if v != device.FuncNone && (slices.Contains(device.SpecialFunctions, v) || slices.Contains(cloudFunctions, v)) {
		return v
	}
	return ""
}

// functionAction is the action field that answers the special function fn,
// or "" for a normal wake. The firmware acts on an answer only when the
// action is the one it expects: the function's own name, except toggle_hold,
// which it takes as guest_mode.
func functionAction(fn string) string {
	if fn == funcToggleHold {
		return funcGuestMode
	}
	return fn
}

// functionScreen answers a special function that does not show a playlist
// screen: fn is anything but restart_playlist, guest_mode and toggle_hold,
// which pick a screen as a wake does. identify draws a screen that says
// which device this is. The others send no image, and the device keeps what
// it shows: rewind redraws its own previous image, send_to_me its last one,
// add_wifi has opened its portal already, and sleep sets how long the device
// sleeps. fn is "" for a true that names no function, which gets no image
// either. refresh is the usual seconds to the next wake.
func (s *Server) functionScreen(dev db.Device, sp screen.Profile, fn string, refresh int64) (screenChoice, error) {
	switch fn {
	case device.FuncIdentify:
		lines := []screen.Line{
			{Text: "Identify", Size: 44, Bold: true},
			{Text: dev.FriendlyID, Size: 64, Bold: true},
		}
		if dev.Name != "" {
			lines = append(lines, screen.Line{Text: dev.Name, Size: 28})
		}
		lines = append(lines, screen.Line{Text: dev.Mac, Size: 22})
		name, size, err := s.screenSized(sp, lines)
		return screenChoice{name: name, size: size, refresh: refresh, shown: true}, err
	case device.FuncSleep:
		zone := device.ZoneOf(dev.TimeZone, s.cfg.Zone)
		return screenChoice{refresh: sleepRefresh(dev.SleepEnd, s.now().In(zone))}, nil
	default:
		return screenChoice{refresh: refresh}, nil
	}
}

// sleepRefresh returns the seconds from now, which is in the device's time
// zone, to the next end of a sleep window that ends at end, in minutes after
// midnight, whether or not now is inside the window. It is
// device.FuncSleepSeconds for end below 0, which is no window.
func sleepRefresh(end int64, now time.Time) int64 {
	if end < 0 {
		return device.FuncSleepSeconds
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), int(end/60), int(end%60), 0, 0, now.Location())
	if !at.After(now) {
		at = time.Date(now.Year(), now.Month(), now.Day()+1, int(end/60), int(end%60), 0, 0, now.Location())
	}
	// Round up, so the device does not wake just before the end.
	return max(int64((at.Sub(now)+time.Second-1)/time.Second), 1)
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
	// shown is false when the device has no playlist items, so nothing was
	// chosen.
	shown bool
}

// maxSleep is the longest a device is told to sleep to wake at the end of
// its sleep window, in seconds: a day and an hour, for a window just under a
// day long and the hour a DST change adds.
const maxSleep = 25 * 60 * 60

// untilChange shortens refresh, in seconds, to wake a device when what it is
// shown changes: its sleep starts or ends, or a schedule of its playlists
// opens or closes. awake is its awake schedule. This wake chose at now plus
// the slack a wake may come early, so the search starts there. The device is
// woken at the change itself, as at the end of an item, so a wake up to the
// slack early still chooses after it.
func untilChange(lists []playlist.List, awake playlist.Schedule, now time.Time, refresh int64) int64 {
	at := now.Add(playlist.WakeSlack)
	next, ok := playlist.NextChange(lists, awake, at, now.Add(time.Duration(refresh)*time.Second))
	if !ok {
		return refresh
	}
	// Round up, so the device is not woken just before the change.
	secs := (next.Sub(now) + time.Second - 1) / time.Second
	return min(refresh, max(int64(secs), 1))
}

// playlistScreen picks what a device shows from its playlists, moving its
// cursor first as command says, which is a button command, or empty for a
// timer wake. Its schedules and sleep window are read in the device's time
// zone, and it is shown the render made for that zone and its locale. A
// device in its sleep window is shown a screen that says so, with the cursor
// where it was, and sleeps until the window ends. A button press or a
// restart_playlist in the window is the exception: it is shown what it would
// be shown awake. A press applies the refresh it is sent, so its next timer
// wake comes in the window and puts it back to sleep. The firmware applies
// no refresh to a restart_playlist, so the device keeps the screen until
// its stored interval runs out, counted from the double-click, which can be
// past the window's end.
// The refresh rate it returns is refresh, shortened to wake the device when
// the item ends, a schedule or its sleep changes or a render is ready.
func (s *Server) playlistScreen(ctx context.Context, dev db.Device, p profile.Profile, sp screen.Profile, command string, refresh int64) (screenChoice, error) {
	// The locale is part of the render's key, so a default in place of a
	// setting that cannot be read would find no render and send the
	// Rendering screen over a good image. The error is returned, so the
	// device keeps its screen and tries again. It is read before the cursor
	// moves, so a wake that shows nothing new does not move it.
	serverLocale, err := settings.Locale(ctx, s.db.Read)
	if err != nil {
		return screenChoice{refresh: refresh}, fmt.Errorf("read the locale setting: %w", err)
	}
	// The clock is read once, so whether the device is asleep, what it is
	// shown and how long it sleeps all agree. A second read could fall past
	// the window's end after the first chose the Sleeping screen, and then
	// the device would be told to sleep until the next window.
	now := s.now().In(device.ZoneOf(dev.TimeZone, s.cfg.Zone))
	awake := playlist.Awake(dev.SleepStart, dev.SleepEnd)
	if command != "" && !awake.Active(now.Add(playlist.WakeSlack)) {
		// A press in the sleep window, or a restart_playlist, wakes the
		// device for this screen: it is shown what it would show awake, as
		// the command says.
		awake = playlist.Always
	}
	pick, err := s.advance(ctx, dev.ID, now, awake, command)
	if err != nil {
		return screenChoice{refresh: refresh}, err
	}
	if pick.asleep {
		// The text is constant for a window, so the image name is too. The
		// device sleeps to the window's end and not for the usual refresh,
		// since waking it earlier would only show this screen again.
		name, size, err := s.screenSized(sp, []screen.Line{
			{Text: "Sleeping", Size: 44, Bold: true},
			{Text: dev.FriendlyID, Size: 28},
			{Text: "Wakes at " + playlist.FormatClock(dev.SleepEnd) + ".", Size: 20},
		})
		return screenChoice{name: name, size: size, refresh: untilChange(pick.lists, awake, now, maxSleep), shown: true}, err
	}
	refresh = untilChange(pick.lists, awake, now, refresh)
	if len(pick.lists) == 0 {
		return screenChoice{refresh: refresh}, nil
	}
	if !pick.ok {
		// Nothing is scheduled, so there is no cursor to move. The text is
		// constant, so the image name is too.
		name, size, err := s.screenSized(sp, []screen.Line{
			{Text: "Nothing scheduled now", Size: 44, Bold: true},
			{Text: dev.FriendlyID, Size: 28},
			{Text: "None of this display's playlists is scheduled for now.", Size: 20},
		})
		return screenChoice{name: name, size: size, refresh: refresh, shown: true}, err
	}
	item, started := pick.item, pick.started
	if item.Duration > 0 {
		left := started.Add(time.Duration(item.Duration) * time.Second).Sub(now)
		refresh = min(refresh, max(int64(left/time.Second), 1))
	}
	row, err := s.db.Read.GetItemScreen(ctx, db.GetItemScreenParams{
		Width:      int64(p.Width),
		Height:     int64(p.Height),
		Palette:    p.RenderPalette(item.Palette),
		TimeZone:   now.Location().String(),
		Locale:     device.LocaleOf(dev.Locale, serverLocale),
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
	// The latest counted failure was of settings newer than the images', so
	// the images show what the instance is no longer set to, and the device
	// is told why instead, with that failure's error. A failure of only newer
	// data keeps the images. An unreachable renderer is not counted, and says
	// nothing about the settings, so it changes neither what is shown nor
	// the error.
	failed := deref(row.FailedSettingsAt) > deref(row.RenderedSettingsAt) && deref(row.FailedError) != ""
	if failed {
		failure = clip(*row.FailedError)
	}
	if !failed && row.Image != nil && row.RenderedAt != nil && *row.RenderedAt != 0 && now.Sub(time.Unix(*row.RenderedAt, 0)) <= staleAfter(time.Duration(row.RefreshInterval)*time.Second) {
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
		// The target has no render now, so it is due.
		s.db.Changed()
		row.Image, failure = nil, ""
	}
	var lines []screen.Line
	switch {
	case failed || row.Image == nil || row.RenderedAt == nil || *row.RenderedAt == 0:
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

// samePtr reports whether a and b are both nil or point to equal values.
func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// deref returns what p points to, or the zero value for nil, as a column of
// a LEFT JOIN that matched no row is.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// pick is what advance chose for a device.
type pick struct {
	// lists are the device's playlists in the order they are tried. It is
	// empty when the device has no playlist items.
	lists []playlist.List
	// asleep is true when the device is in its sleep window at this wake, so
	// nothing is chosen and the cursor is not moved.
	asleep bool
	// ok is true when one of lists is scheduled now and has an item to
	// show. item is then the item to show, and started is when it started.
	ok      bool
	item    db.ListDeviceItemsRow
	started time.Time
}

// advance moves a device's playlist cursor for this wake, as playlist.Advance
// does for command, and returns the item it now shows. The playlist is the
// first that is scheduled at the wake, now, plus the slack a timer wake may
// come early, read in now's location, and has an item that is shown then. A
// device that is not awake then, by its awake schedule, is shown nothing, and
// its cursor stays where it is.
func (s *Server) advance(ctx context.Context, deviceID int64, now time.Time, awake playlist.Schedule, command string) (pick, error) {
	var out pick
	err := s.db.InTx(ctx, func(q *db.Queries) error {
		out = pick{}
		rows, err := q.ListDeviceItems(ctx, deviceID)
		if err != nil {
			return fmt.Errorf("list playlist items: %w", err)
		}
		lists, groups := playlist.DeviceLists(rows)
		out.lists = lists
		out.asleep = !awake.Active(now.Add(playlist.WakeSlack))
		c := playlist.Choose(lists, awake, now.Add(playlist.WakeSlack))
		if c < 0 {
			return nil
		}
		cursor, err := q.GetDeviceCursor(ctx, deviceID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get cursor: %w", err)
		}
		var cur *playlist.Cursor
		if err == nil {
			cur = &playlist.Cursor{ItemID: cursor.ItemID, Started: time.Unix(cursor.ItemStartedAt, 0)}
		}
		i, start := playlist.Advance(lists[c].Items(now.Add(playlist.WakeSlack)), cur, now, command)
		if i < 0 {
			return nil
		}
		out.item, out.started, out.ok = groups[c][i], start, true
		if cur != nil && cur.ItemID == out.item.ID && cur.Started.Equal(start) {
			return nil
		}
		err = q.SetDeviceCursor(ctx, db.SetDeviceCursorParams{
			DeviceID:      deviceID,
			ItemID:        out.item.ID,
			ItemStartedAt: start.Unix(),
		})
		if err != nil {
			return fmt.Errorf("set cursor: %w", err)
		}
		return nil
	})
	return out, err
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

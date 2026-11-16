package adminui

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
	"github.com/die-net/trmnl-byos-go/internal/locale"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

const (
	// missing is shown in place of a value the device has not reported.
	missing = "–"

	// timeLayout is how an absolute time is shown, in the server's zone.
	timeLayout = "2 Jan 2006 15:04 MST"
	// clockLayout is how a time later today is shown, in the server's zone.
	clockLayout = "15:04 MST"

	// joinWindow is how long the Open button leaves the join window open.
	joinWindow = 10 * time.Minute
)

// deviceView is a device as the devices page and the top of its page show
// it.
type deviceView struct {
	// Name is the device's name, or its friendly ID if it has none.
	Name string
	// Href is the path of the device's page.
	Href string
	// Detail is the line under the name: the friendly ID, unless it is
	// already the name, and the firmware model. It may be "".
	Detail string
	// Badge is the status label, and BadgeClass the CSS class that colors it.
	Badge      string
	BadgeClass string
	// LastSeenText is the relative time, such as "4 min ago", or "Never".
	LastSeenText string
	// LastSeenISO and LastSeenTitle are the time as RFC 3339 and as text
	// for a tooltip. Both are "" for a device never seen.
	LastSeenISO   string
	LastSeenTitle string
	Battery       string
	RSSI          string
	Firmware      string
	// Image is the name of the image the device was last sent, or "" if it
	// has none yet. Width and Height are the panel size the device
	// reported, or 0 if it has not.
	Image         string
	Width, Height int64
	// Lazy is whether the image loads only when it is near the screen,
	// which suits a list and not the one image a page is about.
	Lazy bool
}

// The ways a time is written, which the page's script follows when it
// writes the time again in the browser's zone.
const (
	// timeFull is the day and the time of day, as timeLayout.
	timeFull = "full"
	// timeClock is the time of day alone, for a time later today.
	timeClock = "clock"
	// timeDay is the day alone.
	timeDay = "day"
)

// timeView is a time as a page shows it: a time element with the instant in
// UTC and, as its text, the time in the server's zone. The text is what a
// browser without the script shows. The script writes it again in the
// browser's zone.
type timeView struct {
	// ISO is the time as RFC 3339, or "" if it is not known, when Text is
	// missing.
	ISO string
	// Text is the time in the server's zone.
	Text string
	// Kind is how Text is written: timeFull, timeClock or timeDay.
	Kind string
	// Rel is how long ago it was, such as "4 min ago", or "".
	Rel string
}

// playlistRow is one item of a device's playlist.
type playlistRow struct {
	// Name is the instance's name.
	Name string
	// Duration is how long the item shows.
	Duration string
}

// deviceListRow is a playlist a device is linked to, as the device page
// lists it.
type deviceListRow struct {
	// ID is the playlist's ID, which the move forms post.
	ID int64
	// Name is the playlist's name, and Href the path of its card on the
	// playlists page.
	Name, Href string
	// Schedule is the playlist's schedule in words.
	Schedule string
	// Up and Down are the place the playlist moves to, and CanUp and
	// CanDown whether it has one.
	Up, Down       int64
	CanUp, CanDown bool
}

// pinOption is a choice in the firmware pin list.
type pinOption struct {
	Value    string
	Selected bool
}

// profileOption is a choice in the profile list.
type profileOption struct {
	// Value is the registry model's name, or "" for the reported one.
	Value    string
	Label    string
	Selected bool
}

// localeOption is a choice in the locale list.
type localeOption struct {
	// Value is the locale's code, or "" for the server's.
	Value string
	// Label is the option's text.
	Label string
	// Selected is whether it is the form's locale.
	Selected bool
}

// deviceForm is the settings form's values.
type deviceForm struct {
	// Name is the device's name, or "" for none.
	Name string
	// Updates is whether the device may be offered firmware.
	Updates bool
	// Pin is the version the device stays on, or "" for none.
	Pin string
	// Pins are the versions the Pin list offers.
	Pins []pinOption
	// Profile is the registry model set for the device's panel, or "" for
	// the one its firmware reports, and Profiles the choices.
	Profile  string
	Profiles []profileOption
	// Palette is the palette the device renders with when an instance asks
	// for none, or "" for the model's default. Palettes are the choices,
	// which are the palettes of PaletteModel, the model Profile gives.
	Palette      string
	Palettes     []paletteOption
	PaletteModel string
	// Zone is the IANA time zone set for the device, or "" for the
	// server's, and ServerZone the server's, which an empty Zone uses.
	Zone, ServerZone string
	// Locale is the locale set for the device, or "" for the server's, and
	// Locales the choices.
	Locale  string
	Locales []localeOption
	// Seen is the device.SettingsState of the settings stored when the page
	// was made, which the form posts back.
	Seen string
	// Device is the row ID of the device the page showed, which the form
	// posts back. A friendly ID can pass to a newer device once its device
	// is deleted, and this is how a refused save tells that from a change
	// to the same device's settings.
	Device int64
	// Problem says why the stored profile or palette is ignored, as
	// device.OverrideProblem does, or is "".
	Problem string
}

// deviceDetail is a device as its own page shows it.
type deviceDetail struct {
	View deviceView
	// ID is the friendly ID, which the page's path and form use.
	ID    string
	MAC   string
	Model string
	// LastSeen, NextWake and Registered are times in the server's zone.
	LastSeen   timeView
	NextWake   timeView
	Registered timeView
	WakeTime   string
	WakeCause  string
	PanelSize  string
	Form       deviceForm
	Playlist   []playlistRow
	// Linked are the playlists the device is linked to, in the order they
	// are tried. Order is their IDs in that order, comma-separated, which
	// each move form sends back so a move from a stale page is refused.
	// MoveAction is the path the forms that move one post to, and
	// LinkedError is why a move was refused, or "".
	Linked      []deviceListRow
	Order       string
	MoveAction  string
	LinkedError string
	// Blocked is whether the device's MAC is blocked, and RemoveError why a
	// block or unblock was refused, shown in the Remove panel, or "".
	Blocked     bool
	RemoveError string
	// Zone and Locale are the time zone and locale in use for the device.
	Zone, Locale inUseView
	// ZoneNote tells the reader which zone the page's times are in and
	// which the device is in.
	ZoneNote zoneNote
	// Seen is the standing the page shows, which the block form posts back.
	Seen standingView
	// Held are the rollouts that offer their build to no device while this
	// device, their canary, is blocked: those it has not yet reported the
	// version of.
	Held []rolloutRef
}

// inUseView is a time zone or locale in use for a device.
type inUseView struct {
	// Value is the one in use.
	Value string
	// Note says where it comes from when it is not the device's own, such
	// as "the server's", or is "".
	Note string
}

// zoneNote is the line that says which zone a device page's times are in
// and which the device is in. It is written for the server's zone, and the
// page's script writes it again for the browser's.
type zoneNote struct {
	// Page is the zone the times are written in: the server's.
	Page string
	// Device is the zone in use for the device.
	Device string
	// Same is whether the two keep the same time, when the line is left
	// out.
	Same bool
}

// sameZone reports whether two zones keep the same time: at now and at two
// more times of the year, so two names for one zone, such as UTC and
// Etc/UTC, are the same, and two zones that agree only for part of the year
// are not. The page's script decides the same way for the browser's zone.
func sameZone(a, b *time.Location, now time.Time) bool {
	if a.String() == b.String() {
		return true
	}
	for k := range 3 {
		t := now.Add(time.Duration(k) * 120 * 24 * time.Hour)
		_, offA := t.In(a).Zone()
		_, offB := t.In(b).Zone()
		if offA != offB {
			return false
		}
	}
	return true
}

// inUse describes the value in use for a device setting: stored when it is
// the one in use, the server's when stored is empty, and the server's with
// a reason when stored does not apply.
func inUse(stored, used, reason string) inUseView {
	switch {
	case stored == "":
		return inUseView{Value: used, Note: "the server's"}
	case stored != used:
		return inUseView{Value: used, Note: "the server's, since " + strconv.Quote(stored) + " " + reason}
	}
	return inUseView{Value: used}
}

// joinView is the state of the join window.
type joinView struct {
	// Open is whether new devices can register now.
	Open bool
	// Until is when an open window closes.
	Until timeView
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	s.renderDevices(w, r, http.StatusOK, "", blockForm{})
}

// blockedView is a blocked MAC as the Blocked devices panel lists it.
type blockedView struct {
	// MAC is the blocked MAC.
	MAC string
	// Name is the name of the device that has the MAC, or its friendly ID if
	// it has no name, and Href the path of its page. Both are "" when no
	// registered device has the MAC.
	Name, Href string
	// Deleted is whether the MAC is a deleted device's, which no device has.
	Deleted bool
	// Unblock is the path of the page that asks whether to unblock it.
	Unblock string
	// At is when the MAC was blocked.
	At timeView
}

// blockForm is the block form of the Blocked devices panel.
type blockForm struct {
	// MAC is the text in the MAC input.
	MAC string
	// Error is why the last post was refused, shown in the panel, or "".
	Error string
	// Invalid is whether MAC is what the error is about, which marks the
	// input.
	Invalid bool
}

// renderDevices sends the devices page, with an error message above the
// list if msg is not "", and bf as the Blocked devices panel's form. The list,
// the join window and the blocked MACs are read in one snapshot.
func (s *Server) renderDevices(w http.ResponseWriter, r *http.Request, status int, msg string, bf blockForm) {
	ctx := r.Context()
	var rows []db.ListDeviceStatusRow
	var blocked []db.ListBlockedMACsRow
	var until time.Time
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if rows, err = device.ListStatus(ctx, q); err != nil {
			return err
		}
		if until, err = device.JoinUntil(ctx, q); err != nil {
			return err
		}
		blocked, err = device.ListBlocked(ctx, q)
		return err
	})
	if err != nil {
		serverError(w, r, "read devices", err)
		return
	}
	now := s.now()
	views := make([]deviceView, 0, len(rows))
	for i := range rows {
		v := s.newDeviceView(&rows[i], now)
		v.Lazy = true
		views = append(views, v)
	}
	join := joinView{Open: until.After(now)}
	if join.Open {
		join.Until = s.timeAt(until.Unix(), now, false)
		join.Until.Text, join.Until.Kind = s.untilText(until, now)
	}
	blockedViews := make([]blockedView, len(blocked))
	for i := range blocked {
		b := &blocked[i]
		v := blockedView{MAC: b.Mac, Deleted: b.Deleted != 0, Unblock: "/blocklist/" + b.Mac, At: s.timeAt(b.BlockedAt, now, false)}
		if b.FriendlyID != nil {
			v.Href = "/devices/" + *b.FriendlyID
			name := ""
			if b.Name != nil {
				name = *b.Name
			}
			v.Name = deviceLabel(name, *b.FriendlyID)
		}
		blockedViews[i] = v
	}
	s.render(w, r, status, "devices", &pageData{Nav: "devices", Heading: "Devices", Error: msg, Devices: views, Join: join, Blocked: blockedViews, BlockForm: bf})
}

// joinWindowForm opens or closes the join window, as the action field of
// the form says.
func (s *Server) joinWindowForm(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, func(status int, msg string) { s.renderDevices(w, r, status, msg, blockForm{}) }) {
		return
	}
	now := s.now()
	var until time.Time
	switch r.PostForm.Get("action") {
	case "open":
		until = now.Add(joinWindow)
	case "close":
		until = now
	default:
		s.renderDevices(w, r, http.StatusBadRequest, "Choose Open or Close.", blockForm{})
		return
	}
	if err := device.SetJoinWindow(r.Context(), s.db, until); err != nil {
		slog.Error("admin ui: set join window", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

func (s *Server) newDeviceView(x *db.ListDeviceStatusRow, now time.Time) deviceView {
	v := deviceView{
		Name:         x.Device.Name,
		Href:         "/devices/" + x.Device.FriendlyID,
		LastSeenText: "Never",
		Battery:      missing,
		RSSI:         missing,
		Firmware:     missing,
	}
	var detail []string
	if v.Name == "" {
		v.Name = x.Device.FriendlyID
	} else {
		detail = append(detail, x.Device.FriendlyID)
	}
	if x.Device.FwModel != "" {
		detail = append(detail, x.Device.FwModel)
	}
	v.Detail = strings.Join(detail, " · ")
	if x.Device.FwVersion != "" {
		v.Firmware = x.Device.FwVersion
	}
	if x.BatteryV != nil {
		v.Battery = strconv.FormatFloat(*x.BatteryV, 'f', 2, 64) + " V"
	}
	if x.Rssi != nil {
		v.RSSI = strconv.FormatInt(*x.Rssi, 10) + " dBm"
	}
	switch health := device.StatusHealth(x.LastSeen, x.LastRefreshRateSent, now); {
	case x.Blocked != 0:
		v.Badge, v.BadgeClass = "Blocked", "badge-warn"
	case health == device.Online:
		v.Badge, v.BadgeClass = "Online", "badge-ok"
	case health == device.Late:
		v.Badge, v.BadgeClass = "Late", "badge-warn"
	default:
		v.Badge, v.BadgeClass = "Never seen", "badge-neutral"
	}
	if x.LastSeen != nil {
		t := time.Unix(*x.LastSeen, 0)
		v.LastSeenText = relativeTime(now.Sub(t))
		v.LastSeenISO = t.UTC().Format(time.RFC3339)
		v.LastSeenTitle = s.formatTime(t)
	}
	if x.LastImage != nil {
		v.Image = *x.LastImage
	}
	if x.Width != nil && x.Height != nil && *x.Width > 0 && *x.Height > 0 {
		v.Width, v.Height = *x.Width, *x.Height
	}
	return v
}

// formatTime returns t in the server's zone, with the zone's abbreviation.
func (s *Server) formatTime(t time.Time) string {
	return t.In(s.zone).Format(timeLayout)
}

// untilText says when until is, for the join window, and how it is written.
// The window usually closes within minutes, so a time later today is given
// as the time alone.
func (s *Server) untilText(until, now time.Time) (text, kind string) {
	u, n := until.In(s.zone), now.In(s.zone)
	if u.Year() == n.Year() && u.YearDay() == n.YearDay() {
		return u.Format(clockLayout), timeClock
	}
	return u.Format(timeLayout), timeFull
}

// timeAt returns the time given in Unix seconds, with how long ago it was
// if rel is set.
func (s *Server) timeAt(unix int64, now time.Time, rel bool) timeView {
	t := time.Unix(unix, 0)
	v := timeView{ISO: t.UTC().Format(time.RFC3339), Text: s.formatTime(t), Kind: timeFull}
	if rel {
		v.Rel = strings.ToLower(relativeTime(now.Sub(t)))
	}
	return v
}

// dayAt returns the day of the time given in Unix seconds.
func (s *Server) dayAt(unix int64) timeView {
	t := time.Unix(unix, 0)
	return timeView{ISO: t.UTC().Format(time.RFC3339), Text: t.In(s.zone).Format(dayLayout), Kind: timeDay}
}

// relativeTime describes how long ago something was, rounded down to a
// whole minute, hour or day. A time in the future, from clock skew, reads as
// just now.
func relativeTime(ago time.Duration) string {
	switch {
	case ago < time.Minute:
		return "Just now"
	case ago < time.Hour:
		return fmt.Sprintf("%d min ago", ago/time.Minute)
	case ago < 24*time.Hour:
		return fmt.Sprintf("%d h ago", ago/time.Hour)
	default:
		return fmt.Sprintf("%d d ago", ago/(24*time.Hour))
	}
}

// itemDuration describes how long a playlist item shows.
func itemDuration(seconds int64) string {
	if seconds <= 0 {
		return "Until a button press"
	}
	return spanText(seconds)
}

// spanText describes a number of seconds as hours, minutes and seconds,
// such as "1 h 30 min".
func spanText(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	var parts []string
	if h := d / time.Hour; h > 0 {
		parts = append(parts, fmt.Sprintf("%d h", h))
	}
	if m := d % time.Hour / time.Minute; m > 0 {
		parts = append(parts, fmt.Sprintf("%d min", m))
	}
	if sec := d % time.Minute / time.Second; sec > 0 {
		parts = append(parts, fmt.Sprintf("%d s", sec))
	}
	return strings.Join(parts, " ")
}

// wakeDuration describes how long a device's last wake took.
func wakeDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}
	return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64) + " s"
}

// noDevice is the not-found page for a device.
var noDevice = notFoundView{Nav: "devices", What: "No such device", BackHref: "/devices", BackName: "Devices"}

// deviceFromPath returns the device the path names. A device that does not
// exist gets the not-found page, and any other failure a plain error, and
// both return false.
func (s *Server) deviceFromPath(w http.ResponseWriter, r *http.Request) (db.Device, bool) {
	dev, err := device.FindDevice(r.Context(), s.db.Read, r.PathValue("id"))
	switch {
	case err == nil:
		return dev, true
	case errors.Is(err, device.ErrNoDevice):
		s.renderNotFound(w, r, noDevice)
	default:
		slog.Error("admin ui: find device", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
	}
	return db.Device{}, false
}

func (s *Server) deviceDetail(w http.ResponseWriter, r *http.Request) {
	s.renderDevice(w, r, http.StatusOK, r.PathValue("id"), nil, "", "", "")
}

// updateDevice saves the settings form. It checks the name and the pin
// before writing either, as the JSON API does, so a bad value changes
// nothing.
func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceFromPath(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r, func(status int, msg string) { s.renderDevice(w, r, status, dev.FriendlyID, nil, msg, "", "") }) {
		return
	}
	form := deviceForm{
		Name:    r.PostForm.Get("name"),
		Updates: r.PostForm.Get("updates_enabled") != "",
		Pin:     r.PostForm.Get("fw_pin"),
		Profile: r.PostForm.Get("profile"),
		Palette: r.PostForm.Get("palette"),
		Zone:    strings.TrimSpace(r.PostForm.Get("time_zone")),
		Locale:  r.PostForm.Get("locale"),
		Seen:    r.PostForm.Get("settings_state"),
	}
	seenDevice, err := strconv.ParseInt(r.PostForm.Get("seen_device"), 10, 64)
	form.Device = seenDevice
	if form.Seen == "" || err != nil {
		// The post says nothing of what its page showed, so its values
		// cannot be checked against what is stored. The page sent back has
		// the stored settings, not the posted ones, so its Save cannot
		// write over settings the sender never saw.
		s.renderDevice(w, r, http.StatusBadRequest, dev.FriendlyID, nil, reloadText, "", "")
		return
	}
	err = s.saveDevice(r, dev.FriendlyID, &form)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+dev.FriendlyID, http.StatusSeeOther)
	case errors.Is(err, device.ErrNoDevice):
		s.renderNotFound(w, r, noDevice)
	case errors.Is(err, device.ErrSettingsChanged):
		// The page says what is stored now, and its form has the typed
		// values and the current state, so saving again applies them. If
		// the friendly ID is another device's now, renderDevice drops the
		// typed values instead.
		s.renderDevice(w, r, http.StatusConflict, dev.FriendlyID, &form, settingsChanged, "", "")
	case errors.Is(err, errkind.ErrInvalid):
		msg := strings.TrimPrefix(err.Error(), errkind.ErrInvalid.Error()+": ")
		s.renderDevice(w, r, http.StatusBadRequest, dev.FriendlyID, &form, capitalize(msg)+".", "", "")
	default:
		slog.Error("admin ui: save device", "device", dev.FriendlyID, "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
	}
}

// settingsChanged is what a refused save of the settings form says. The
// page then lists what is stored now where it differs from the form.
const settingsChanged = "This device's settings changed after the page loaded, so nothing was saved."

// deviceReplaced is what a refused save says when the device its page
// showed was deleted and a newer device got its friendly ID. The page then
// has the newer device's settings, never the typed ones, so that saving
// again cannot put the deleted device's settings on it.
const deviceReplaced = "The device this page showed was deleted, and a new device has its ID now, so nothing was saved. The values below are the new device's."

// saveDevice stores the form in one transaction, so a refused field changes
// nothing, and a form whose page showed other settings than are stored now
// is refused. It is what the JSON API's PATCH does with every field and the
// settings state set.
func (s *Server) saveDevice(r *http.Request, id string, form *deviceForm) error {
	pin, err := firmware.CleanPin(form.Pin)
	if err != nil {
		return err
	}
	_, err = device.Update(r.Context(), s.db, id, device.Change{
		Name: &form.Name, UpdatesEnabled: &form.Updates, FwPin: &pin, Profile: &form.Profile, Palette: &form.Palette,
		TimeZone: &form.Zone, Locale: &form.Locale, Seen: &form.Seen,
	})
	return err
}

// capitalize returns msg with its first letter in upper case.
func capitalize(msg string) string {
	if msg == "" {
		return msg
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// renderDevice sends the page of the device that ref names, a friendly ID or
// a MAC. A device that does not exist gets the not-found page. A nil form
// shows the device's stored settings, and a form is what was submitted. msg
// is an error to show above the settings form, listMsg one to show in the
// playlists panel, and removeMsg one to show in the Remove panel, or "". The
// page's reads, the device's own row among them, are one snapshot, so the
// playlist it would show and the playlists it lists agree.
func (s *Server) renderDevice(w http.ResponseWriter, r *http.Request, status int, ref string, form *deviceForm, msg, listMsg, removeMsg string) {
	ctx := r.Context()
	var row db.GetDeviceStatusRow
	var items []db.ListDeviceItemsRow
	var linked []db.Playlist
	var catalog []firmware.Entry
	var st device.Standing
	var serverLocale string
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		d, err := device.FindDevice(ctx, q, ref)
		if err != nil {
			return err
		}
		if row, err = q.GetDeviceStatus(ctx, d.ID); err != nil {
			return fmt.Errorf("get device status: %w", err)
		}
		if st, err = device.ReadStanding(ctx, q, d.Mac); err != nil {
			return err
		}
		if serverLocale, err = settings.Locale(ctx, q); err != nil {
			return err
		}
		if items, err = playlist.ListDeviceItems(ctx, q, d.ID); err != nil {
			return err
		}
		if linked, err = q.ListDevicePlaylists(ctx, d.ID); err != nil {
			return fmt.Errorf("list device playlists: %w", err)
		}
		if catalog, err = firmware.List(ctx, q); err != nil {
			return fmt.Errorf("list firmware: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, device.ErrNoDevice) || errors.Is(err, sql.ErrNoRows):
		s.renderNotFound(w, r, noDevice)
		return
	case err != nil:
		serverError(w, r, "read device page", err)
		return
	}
	dev := &row.Device
	now := s.now().In(s.zone)
	d := &deviceDetail{
		View:        s.newDeviceView((*db.ListDeviceStatusRow)(&row), now),
		ID:          dev.FriendlyID,
		MAC:         dev.Mac,
		Model:       orMissing(dev.FwModel),
		LastSeen:    timeView{Text: missing},
		NextWake:    timeView{Text: missing},
		Registered:  s.timeAt(dev.CreatedAt, now, false),
		WakeTime:    missing,
		WakeCause:   missing,
		PanelSize:   missing,
		Playlist:    []playlistRow{},
		MoveAction:  "/devices/" + dev.FriendlyID + "/playlists#playlists",
		LinkedError: listMsg,
		Blocked:     st.Blocked,
		RemoveError: removeMsg,
		Seen:        newStandingView(&st),
	}
	zone := device.ZoneOf(dev.TimeZone, s.zone).String()
	d.Zone = inUse(dev.TimeZone, zone, "does not load on this server")
	d.Locale = inUse(dev.Locale, device.LocaleOf(dev.Locale, serverLocale), "is not a locale the server has")
	d.ZoneNote = zoneNote{Page: s.zone.String(), Device: zone, Same: sameZone(s.zone, device.ZoneOf(dev.TimeZone, s.zone), now)}
	order := make([]int64, len(linked))
	for i := range linked {
		order[i] = linked[i].ID
	}
	d.Order = joinIDs(order)
	for i := range linked {
		pl := &linked[i]
		d.Linked = append(d.Linked, deviceListRow{
			ID: pl.ID, Name: pl.Name, Href: "/playlists#playlist-" + strconv.FormatInt(pl.ID, 10), Schedule: s.describeSchedule(scheduleOf(pl)),
			Up: int64(i) - 1, Down: int64(i) + 1, CanUp: i > 0, CanDown: i < len(linked)-1,
		})
	}
	d.View.Firmware = orMissing(dev.FwVersion)
	if row.LastSeen != nil {
		d.LastSeen = s.timeAt(*row.LastSeen, now, true)
		if row.LastRefreshRateSent != nil {
			d.NextWake = s.timeAt(*row.LastSeen+*row.LastRefreshRateSent, now, false)
		}
	}
	if row.WakeTimeMs != nil {
		d.WakeTime = wakeDuration(*row.WakeTimeMs)
	}
	if row.UpdateSource != nil {
		d.WakeCause = orMissing(*row.UpdateSource)
	}
	if d.View.Width > 0 {
		d.PanelSize = fmt.Sprintf("%d × %d", d.View.Width, d.View.Height)
	}
	// The playlist the device would show at its next wake, none if it is
	// asleep then, and its items that are shown then. Its schedules and sleep
	// window are read in its own time zone.
	at := now.Add(playlist.WakeSlack).In(device.ZoneOf(dev.TimeZone, s.zone))
	lists, groups := playlist.DeviceLists(items)
	if c := playlist.Choose(lists, playlist.Awake(dev.SleepStart, dev.SleepEnd), at); c >= 0 {
		for i := range groups[c] {
			if lists[c].Entries[i].Shown(at) {
				d.Playlist = append(d.Playlist, playlistRow{Name: groups[c][i].InstanceName, Duration: itemDuration(groups[c][i].Duration)})
			}
		}
	}
	stored := deviceForm{
		Name: dev.Name, Updates: dev.UpdatesEnabled != 0, Pin: dev.FwPin, Profile: dev.ProfileOverride, Palette: dev.PaletteOverride,
		Zone: dev.TimeZone, Locale: dev.Locale,
	}
	stale := form != nil && status == http.StatusConflict && msg == settingsChanged
	if stale && form.Device != dev.ID {
		form, stale, msg = nil, false, deviceReplaced
	}
	if form == nil {
		form = &stored
	}
	form.ServerZone = s.zone.String()
	form.Locales = localeOptions(serverLocale, form.Locale)
	form.Profiles = profileOptions(dev.FwModel, form.Profile)
	// A stored palette the model no longer takes is listed while the form
	// has the stored profile and palette, so saving the form leaves it as
	// it is, as it leaves a stored profile the registry lacks.
	keep := ""
	if form.Profile == dev.ProfileOverride && form.Palette == dev.PaletteOverride {
		keep = dev.PaletteOverride
	}
	form.PaletteModel, form.Palettes = paletteChoices(dev.FwModel, form.Profile, form.Palette, keep)
	if problem := device.OverrideProblem(dev, dev.FwModel); problem != "" {
		form.Problem = capitalize(problem) + "."
	}
	if form.Palettes[0].Selected {
		// The list does not have the palette, so the form would post the
		// default.
		form.Palette = ""
	}
	// A page gets the state stored now when it shows the stored settings,
	// and when a stale save was refused and the page says what is stored.
	// A form refused for another reason keeps the state it posted: that
	// state may not have been checked, and the next save must check it.
	// The device's row ID goes with the state, for the same reason.
	if stale {
		msg = settingsChangedText(form, &stored)
		form.Seen = ""
	}
	if form.Seen == "" {
		form.Seen, form.Device = device.SettingsState(dev), dev.ID
	}
	for i := range catalog {
		// A rollout at the canary stage becomes blocked with its canary.
		if e := &catalog[i]; (e.Stage == "canary" || e.Stage == "blocked") && slices.Contains(st.Canary, e.ID) {
			d.Held = append(d.Held, rolloutRef{Version: e.Version, Model: e.FwModel})
		}
	}
	form.Pins = pinOptions(catalog, dev.FwModel, form.Pin)
	d.Form = *form
	s.render(w, r, status, "device", &pageData{Nav: "devices", Heading: d.View.Name, Error: msg, Detail: d})
}

// orderChanged is what a refused move says. The page it is shown on has the
// order as it is now.
const orderChanged = "This device's playlists changed after the page loaded, so nothing was moved. The order below is current."

// moveDevicePlaylist moves one of a device's playlists to another place in
// the order the device tries them. The form sends the order the page showed,
// so a move from a page that is out of date is refused instead of moving the
// wrong way. It never makes a link.
func (s *Server) moveDevicePlaylist(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceFromPath(w, r)
	if !ok {
		return
	}
	refuse := func(status int, msg string) { s.renderDevice(w, r, status, dev.FriendlyID, nil, "", msg, "") }
	if !parseForm(w, r, refuse) {
		return
	}
	id, idErr := strconv.ParseInt(r.PostForm.Get("playlist"), 10, 64)
	order, orderOK := idList(r.PostForm, "order")
	to, toErr := strconv.ParseInt(r.PostForm.Get("to"), 10, 64)
	if idErr != nil || !orderOK || toErr != nil {
		refuse(http.StatusBadRequest, "Choose a playlist to move.")
		return
	}
	err := playlist.MoveLink(r.Context(), s.db, playlist.ByID(id), dev.FriendlyID, order, to)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+dev.FriendlyID+"#playlists", http.StatusSeeOther)
	case errors.Is(err, device.ErrNoDevice):
		s.renderNotFound(w, r, noDevice)
	case errors.Is(err, playlist.ErrNoPlaylist):
		refuse(http.StatusConflict, "That playlist no longer exists.")
	case errors.Is(err, playlist.ErrNotLinked):
		refuse(http.StatusConflict, "That playlist is no longer linked to this device.")
	case errors.Is(err, playlist.ErrOrderChanged):
		refuse(http.StatusConflict, orderChanged)
	case errors.Is(err, errkind.ErrInvalid):
		// A place past the end comes from a page that showed more
		// playlists than the device has now.
		refuse(http.StatusBadRequest, orderChanged)
	default:
		serverError(w, r, "move device playlist", err)
	}
}

// orMissing returns s, or the placeholder if it is empty.
func orMissing(s string) string {
	if s == "" {
		return missing
	}
	return s
}

// pinOptions lists the versions of model in the catalog, newest first. The
// selected version is always listed, so saving the form does not change a
// pin that is not in the catalog.
func pinOptions(catalog []firmware.Entry, model, selected string) []pinOption {
	versions := map[string]firmware.Version{}
	for i := range catalog {
		if catalog[i].FwModel != model {
			continue
		}
		if v, err := firmware.ParseVersion(catalog[i].Version); err == nil {
			versions[catalog[i].Version] = v
		}
	}
	if selected != "" {
		if _, ok := versions[selected]; !ok {
			v, _ := firmware.ParseVersion(selected)
			versions[selected] = v
		}
	}
	names := make([]string, 0, len(versions))
	for name := range versions {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		if c := versions[b].Compare(versions[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	opts := make([]pinOption, len(names))
	for i, name := range names {
		opts[i] = pinOption{Value: name, Selected: name == selected}
	}
	return opts
}

// modelLabel is how a registry model is named in a list: its label and its
// name.
func modelLabel(m *profile.Model) string {
	if m.Label == "" {
		return m.Name
	}
	return m.Label + " (" + m.Name + ")"
}

// modelLabelByName is modelLabel of the registry model with the name, or the
// name alone for one the registry does not have.
func modelLabelByName(name string) string {
	if m, ok := profile.ModelByName(name); ok {
		return modelLabel(&m)
	}
	return name
}

// profileOptions lists the choices of a device's profile: the model its
// firmware reports, then every registry model by label. The selected one is
// always listed, so a stored model the registry no longer has shows as what
// it is.
func profileOptions(fwModel, selected string) []profileOption {
	reported := "unknown model"
	if p := profile.Resolve(fwModel, nil, nil, profile.Override{}); p.Model != "" {
		reported = modelLabelByName(p.Model)
	}
	models := profile.Models()
	opts := make([]profileOption, 1, len(models)+2)
	opts[0] = profileOption{Label: "As the device reports: " + reported, Selected: selected == ""}
	found := selected == ""
	for i := range models {
		found = found || models[i].Name == selected
		opts = append(opts, profileOption{Value: models[i].Name, Label: modelLabel(&models[i]), Selected: models[i].Name == selected})
	}
	if !found {
		opts = append(opts, profileOption{Value: selected, Label: selected, Selected: true})
	}
	// The reported model stays first.
	slices.SortStableFunc(opts[1:], func(a, b profileOption) int {
		return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
	})
	return opts
}

// paletteChoices lists the palettes a device with the firmware model and
// the profile takes, after the model's default, and names that model. It is
// the model device.Update checks a palette against. A selected palette the
// model does not take is not listed, which leaves the default selected,
// unless it is keep, the stored one, which is listed as what it is.
func paletteChoices(fwModel, override, selected, keep string) (string, []paletteOption) {
	p := profile.Resolve(fwModel, nil, nil, profile.Override{Model: override})
	model := "an unknown model"
	if p.Model != "" {
		model = modelLabelByName(p.Model)
	}
	def := "Model's default"
	if p.DefaultPalette != "" {
		def += ": " + paletteLabel(p.DefaultPalette)
	}
	opts := make([]paletteOption, 0, len(p.Palettes)+1)
	kept := keep != "" && keep == selected && !slices.Contains(p.Palettes, keep)
	opts = append(opts, paletteOption{Label: def, Selected: !kept && !slices.Contains(p.Palettes, selected)})
	for _, id := range p.Palettes {
		opts = append(opts, paletteOption{ID: id, Label: paletteLabel(id), Selected: id == selected})
	}
	if kept {
		opts = append(opts, paletteOption{ID: keep, Label: paletteLabel(keep) + " (not a palette of this model)", Selected: true})
	}
	return model, opts
}

// paletteLabel is a palette's name, or its ID for one the registry does not
// have.
func paletteLabel(id string) string {
	p, _ := profile.PaletteByID(id)
	return cmp.Or(p.Name, id)
}

// settingsChangedText is settingsChanged with what is stored now, for each
// setting where that is not what the form has.
func settingsChangedText(form, stored *deviceForm) string {
	orWord := func(s, none string) string {
		if s == "" {
			return none
		}
		return strconv.Quote(s)
	}
	var now []string
	if strings.TrimSpace(form.Name) != stored.Name {
		now = append(now, "the name is "+orWord(stored.Name, "empty"))
	}
	if form.Updates != stored.Updates {
		if stored.Updates {
			now = append(now, "firmware updates are on")
		} else {
			now = append(now, "firmware updates are off")
		}
	}
	// A pin that is not a version is compared as typed.
	if pin, err := firmware.CleanPin(form.Pin); (err == nil && pin != stored.Pin) || (err != nil && form.Pin != stored.Pin) {
		now = append(now, "the firmware pin is "+orWord(stored.Pin, "not set"))
	}
	if form.Profile != stored.Profile {
		now = append(now, "the profile is "+orWord(stored.Profile, "as the device reports"))
	}
	if form.Palette != stored.Palette {
		now = append(now, "the palette is "+orWord(stored.Palette, "the model's default"))
	}
	if form.Zone != stored.Zone {
		now = append(now, "the time zone is "+orWord(stored.Zone, "the server's"))
	}
	if form.Locale != stored.Locale {
		now = append(now, "the locale is "+orWord(stored.Locale, "the server's"))
	}
	if len(now) == 0 {
		return settingsChanged + " The values below are what is stored now."
	}
	return settingsChanged + " Stored now: " + strings.Join(now, ", ") + ". Save again to use the values below."
}

// localeOptions lists the choices of a device's locale: the server's, then
// every locale. The selected one is always listed, so a stored locale the
// server no longer has shows as what it is.
func localeOptions(server, selected string) []localeOption {
	codes := locale.Codes()
	opts := make([]localeOption, 1, len(codes)+2)
	opts[0] = localeOption{Label: "The server's: " + server, Selected: selected == ""}
	found := selected == ""
	for _, code := range codes {
		found = found || code == selected
		opts = append(opts, localeOption{Value: code, Label: code, Selected: code == selected})
	}
	if !found {
		opts = append(opts, localeOption{Value: selected, Label: selected + " (not a locale the server has)", Selected: true})
	}
	return opts
}

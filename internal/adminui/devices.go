package adminui

import (
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
	"github.com/die-net/trmnl-byos-go/internal/playlist"
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

// timeView is a time as the detail page shows it.
type timeView struct {
	// ISO is the time as RFC 3339, or "" if it is not known, when Text is
	// missing.
	ISO string
	// Text is the time in the server's zone.
	Text string
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

// pinOption is a choice in the firmware pin list.
type pinOption struct {
	Value    string
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
}

// joinView is the state of the join window.
type joinView struct {
	// Open is whether new devices can register now.
	Open bool
	// Until is when an open window closes.
	Until timeView
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	s.renderDevices(w, r, http.StatusOK, "")
}

// renderDevices sends the devices page, with an error message above the
// list if msg is not "".
func (s *Server) renderDevices(w http.ResponseWriter, r *http.Request, status int, msg string) {
	rows, err := device.ListStatus(r.Context(), s.db.Read)
	if err != nil {
		slog.Error("admin ui: list devices", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	until, err := device.JoinUntil(r.Context(), s.db.Read)
	if err != nil {
		slog.Error("admin ui: read join window", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
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
		join.Until.Text = s.untilText(until, now)
	}
	s.render(w, r, status, "devices", pageData{Nav: "devices", Heading: "Devices", Error: msg, Devices: views, Join: join})
}

// joinWindowForm opens or closes the join window, as the action field of
// the form says.
func (s *Server) joinWindowForm(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r, func(status int, msg string) { s.renderDevices(w, r, status, msg) }) {
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
		s.renderDevices(w, r, http.StatusBadRequest, "Choose Open or Close.")
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
	switch device.StatusHealth(x.LastSeen, x.LastRefreshRateSent, now) {
	case device.Online:
		v.Badge, v.BadgeClass = "Online", "badge-ok"
	case device.Late:
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

// untilText says when until is, for the join window. The window usually
// closes within minutes, so a time later today is given as the time alone.
func (s *Server) untilText(until, now time.Time) string {
	u, n := until.In(s.zone), now.In(s.zone)
	if u.Year() == n.Year() && u.YearDay() == n.YearDay() {
		return u.Format(clockLayout)
	}
	return u.Format(timeLayout)
}

// timeAt returns the time given in Unix seconds, with how long ago it was
// if rel is set.
func (s *Server) timeAt(unix int64, now time.Time, rel bool) timeView {
	t := time.Unix(unix, 0)
	v := timeView{ISO: t.UTC().Format(time.RFC3339), Text: s.formatTime(t)}
	if rel {
		v.Rel = strings.ToLower(relativeTime(now.Sub(t)))
	}
	return v
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

// deviceFromPath returns the device the path names. A device that does not
// exist gets the not-found page, and any other failure a plain error, and
// both return false.
func (s *Server) deviceFromPath(w http.ResponseWriter, r *http.Request) (db.Device, bool) {
	dev, err := device.FindDevice(r.Context(), s.db.Read, r.PathValue("id"))
	switch {
	case err == nil:
		return dev, true
	case errors.Is(err, device.ErrNoDevice):
		s.render(w, r, http.StatusNotFound, "notfound", pageData{Nav: "devices", Heading: "Not found"})
	default:
		slog.Error("admin ui: find device", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
	}
	return db.Device{}, false
}

func (s *Server) deviceDetail(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceFromPath(w, r)
	if !ok {
		return
	}
	s.renderDevice(w, r, http.StatusOK, &dev, nil, "")
}

// updateDevice saves the settings form. It checks the name and the pin
// before writing either, as the JSON API does, so a bad value changes
// nothing.
func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.deviceFromPath(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r, func(status int, msg string) { s.renderDevice(w, r, status, &dev, nil, msg) }) {
		return
	}
	form := deviceForm{
		Name:    r.PostForm.Get("name"),
		Updates: r.PostForm.Get("updates_enabled") != "",
		Pin:     r.PostForm.Get("fw_pin"),
	}
	err := s.saveDevice(r, dev.FriendlyID, &form)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+dev.FriendlyID, http.StatusSeeOther)
	case errors.Is(err, errkind.ErrInvalid):
		msg := strings.TrimPrefix(err.Error(), errkind.ErrInvalid.Error()+": ")
		s.renderDevice(w, r, http.StatusBadRequest, &dev, &form, capitalize(msg)+".")
	default:
		slog.Error("admin ui: save device", "device", dev.FriendlyID, "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
	}
}

// saveDevice stores the form. It is what the JSON API's PATCH does with all
// three fields set.
func (s *Server) saveDevice(r *http.Request, id string, form *deviceForm) error {
	if _, err := device.CleanName(form.Name); err != nil {
		return err
	}
	_, err := firmware.SetPolicy(r.Context(), s.db, id, firmware.Policy{UpdatesEnabled: &form.Updates, Pin: &form.Pin})
	if err != nil {
		return err
	}
	_, err = device.SetName(r.Context(), s.db, id, form.Name)
	return err
}

// capitalize returns msg with its first letter in upper case.
func capitalize(msg string) string {
	if msg == "" {
		return msg
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// renderDevice sends the page of dev. A nil form shows the device's stored
// settings, and a form is what was submitted. msg is an error to show above
// the form, or "".
func (s *Server) renderDevice(w http.ResponseWriter, r *http.Request, status int, dev *db.Device, form *deviceForm, msg string) {
	ctx := r.Context()
	row, err := s.db.Read.GetDeviceStatus(ctx, dev.ID)
	if err != nil {
		slog.Error("admin ui: get device status", "device", dev.FriendlyID, "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	items, err := playlist.ListDeviceItems(ctx, s.db.Read, dev.ID)
	if err != nil {
		slog.Error("admin ui: list playlist", "device", dev.FriendlyID, "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	catalog, err := firmware.List(ctx, s.db.Read)
	if err != nil {
		slog.Error("admin ui: list firmware", "device", dev.FriendlyID, "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	now := s.now().In(s.zone)
	d := &deviceDetail{
		View:       s.newDeviceView((*db.ListDeviceStatusRow)(&row), now),
		ID:         dev.FriendlyID,
		MAC:        dev.Mac,
		Model:      orMissing(dev.FwModel),
		LastSeen:   timeView{Text: missing},
		NextWake:   timeView{Text: missing},
		Registered: s.timeAt(dev.CreatedAt, now, false),
		WakeTime:   missing,
		WakeCause:  missing,
		PanelSize:  missing,
		Playlist:   []playlistRow{},
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
	// The playlist the device would show at its next wake, and its items
	// that are shown then.
	at := now.Add(playlist.WakeSlack)
	lists, groups := playlist.DeviceLists(items)
	if c := playlist.Choose(lists, at); c >= 0 {
		for i := range groups[c] {
			if lists[c].Entries[i].Shown(at) {
				d.Playlist = append(d.Playlist, playlistRow{Name: groups[c][i].InstanceName, Duration: itemDuration(groups[c][i].Duration)})
			}
		}
	}
	if form == nil {
		form = &deviceForm{Name: dev.Name, Updates: dev.UpdatesEnabled != 0, Pin: dev.FwPin}
	}
	form.Pins = pinOptions(catalog, dev.FwModel, form.Pin)
	d.Form = *form
	s.render(w, r, status, "device", pageData{Nav: "devices", Heading: d.View.Name, Error: msg, Detail: d})
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

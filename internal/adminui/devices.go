package adminui

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// missing is shown in place of a value the device has not reported.
const missing = "–"

// deviceView is a device as the devices page shows it.
type deviceView struct {
	// Name is the device's name, or its friendly ID if it has none.
	Name string
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
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	rows, err := device.ListStatus(r.Context(), s.db.Read)
	if err != nil {
		slog.Error("admin ui: list devices", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	now := s.now()
	views := make([]deviceView, 0, len(rows))
	for i := range rows {
		views = append(views, newDeviceView(&rows[i], now))
	}
	s.render(w, r, http.StatusOK, "devices", pageData{Nav: "devices", Heading: "Devices", Devices: views})
}

func newDeviceView(x *db.ListDeviceStatusRow, now time.Time) deviceView {
	v := deviceView{
		Name:         x.Device.Name,
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
		t := time.Unix(*x.LastSeen, 0).UTC()
		v.LastSeenText = relativeTime(now.Sub(t))
		v.LastSeenISO = t.Format(time.RFC3339)
		v.LastSeenTitle = t.Format("2 Jan 2006 15:04 UTC")
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

package adminui

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/firmware"
)

// deviceDeleteView is the deletion of a device as its confirm page asks
// about it.
type deviceDeleteView struct {
	// Name is the device's name, or its friendly ID if it has none.
	Name string
	// ID and MAC are the device's friendly ID and MAC.
	ID, MAC string
	// Action is the path the form posts to, and Cancel the path of the
	// device's page.
	Action, Cancel string
	// Links says how many playlists the device is linked to, such as "It is
	// linked to 2 playlists.", or is "" if it is linked to none.
	Links string
	// Blocked is whether the device's MAC is blocked, which keeps it from
	// being reset while the block lasts.
	Blocked bool
	// Seen is the standing the page shows, which the form posts back.
	Seen standingView
	// Rollouts are the firmware rollouts that end with the device because it
	// is their canary.
	Rollouts []rolloutRef
}

// rolloutRef names a firmware rollout.
type rolloutRef struct {
	// Version and Model are the build's version and the firmware model it is
	// for.
	Version, Model string
}

// standingView is a device.Standing as the hidden fields of a form carry it,
// so the post can say what the page showed.
type standingView struct {
	// MAC is the MAC the standing is of.
	MAC string
	// Device is the ID of the registered device that has the MAC, or 0.
	Device int64
	// Blocked and Deleted are whether the MAC is blocked and whether it is
	// a deleted device's tombstone.
	Blocked, Deleted bool
	// Canary is the IDs of the firmware builds whose rollout has the device
	// as canary, comma-separated.
	Canary string
}

func newStandingView(st *device.Standing) standingView {
	return standingView{MAC: st.MAC, Device: st.DeviceID, Blocked: st.Blocked, Deleted: st.Deleted, Canary: joinIDs(st.Canary)}
}

// standingFromPost reads the standing a form's hidden fields say its page
// showed. It returns false if a field is missing or is not what a page
// writes.
func standingFromPost(post url.Values) (device.Standing, bool) {
	mac, ok := device.ParseMAC(post.Get("seen_mac"))
	if !ok {
		return device.Standing{}, false
	}
	id, err := strconv.ParseInt(post.Get("seen_device"), 10, 64)
	if err != nil {
		return device.Standing{}, false
	}
	blocked, err := strconv.ParseBool(post.Get("seen_blocked"))
	if err != nil {
		return device.Standing{}, false
	}
	deleted, err := strconv.ParseBool(post.Get("seen_deleted"))
	if err != nil {
		return device.Standing{}, false
	}
	canary, ok := idList(post, "seen_canary")
	if !ok {
		return device.Standing{}, false
	}
	return device.Standing{MAC: mac, DeviceID: id, Blocked: blocked, Deleted: deleted, Canary: canary}, true
}

const (
	// reloadText is what a post without the fields its page writes gets.
	reloadText = "Reload the page and try again."
	// deviceChanged is what a refused delete, block or unblock says. The
	// page it is shown on is current.
	deviceChanged = "This device changed after the page loaded, so nothing was done. What is below is current."
)

// confirmDeleteDevice asks whether to delete a device. It changes nothing.
func (s *Server) confirmDeleteDevice(w http.ResponseWriter, r *http.Request) {
	s.renderDeviceDelete(w, r, http.StatusOK, r.PathValue("id"), "")
}

// renderDeviceDelete sends the confirm page for the device that ref names,
// which gets the not-found page if it does not exist, with msg above the
// form if it is not "". What the page says goes with the device, and the
// standing its form posts back, are one snapshot.
func (s *Server) renderDeviceDelete(w http.ResponseWriter, r *http.Request, status int, ref, msg string) {
	ctx := r.Context()
	var st device.Standing
	var dev db.Device
	var linked []db.Playlist
	var catalog []firmware.Entry
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if st, err = device.ReadStanding(ctx, q, ref); err != nil {
			return err
		}
		if st.DeviceID == 0 {
			return device.ErrNoDevice
		}
		if dev, err = q.GetDevice(ctx, st.DeviceID); err != nil {
			return fmt.Errorf("get device: %w", err)
		}
		if linked, err = q.ListDevicePlaylists(ctx, dev.ID); err != nil {
			return fmt.Errorf("list device playlists: %w", err)
		}
		if len(st.Canary) > 0 {
			if catalog, err = firmware.List(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, device.ErrNoDevice) || errors.Is(err, sql.ErrNoRows):
		s.renderNotFound(w, r, noDevice)
		return
	case err != nil:
		serverError(w, r, "read device to delete", err)
		return
	}
	v := &deviceDeleteView{
		Name:    deviceLabel(dev.Name, dev.FriendlyID),
		ID:      dev.FriendlyID,
		MAC:     dev.Mac,
		Action:  "/devices/" + dev.FriendlyID + "/delete",
		Cancel:  "/devices/" + dev.FriendlyID,
		Blocked: st.Blocked,
		Seen:    newStandingView(&st),
	}
	switch len(linked) {
	case 0:
	case 1:
		v.Links = "It is linked to 1 playlist."
	default:
		v.Links = fmt.Sprintf("It is linked to %d playlists.", len(linked))
	}
	for i := range catalog {
		// A canceled rollout is deleted too, but it offers nothing now, so
		// nothing ends with it.
		if e := &catalog[i]; e.Stage != "canceled" && slices.Contains(st.Canary, e.ID) {
			v.Rollouts = append(v.Rollouts, rolloutRef{Version: e.Version, Model: e.FwModel})
		}
	}
	s.render(w, r, status, "device_delete", &pageData{Nav: "devices", Heading: "Delete " + v.Name, Error: msg, DeviceDelete: v})
}

// deleteDevice deletes the device its confirm page showed, which is reset
// at its next wake unless its MAC is blocked. The form sends the standing
// the page showed, and the delete names the device by that MAC, so a page
// for a device that has changed, or whose friendly ID a newer device now
// has, deletes nothing.
func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	refuse := func(status int, msg string) { s.renderDeviceDelete(w, r, status, ref, msg) }
	if !parseForm(w, r, refuse) {
		return
	}
	seen, ok := standingFromPost(r.PostForm)
	if !ok {
		refuse(http.StatusBadRequest, reloadText)
		return
	}
	_, err := device.Delete(r.Context(), s.db, seen.MAC, &seen, s.now())
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
	case errors.Is(err, device.ErrNoDevice):
		s.renderNotFound(w, r, noDevice)
	case errors.Is(err, device.ErrDeviceChanged):
		// The page is of the device the post was about, which the path may
		// no longer name.
		s.renderDeviceDelete(w, r, http.StatusConflict, seen.MAC, deviceChanged)
	default:
		serverError(w, r, "delete device", err)
	}
}

// blockDevice blocks or unblocks the device its page showed, as the action
// field of the form says. The form sends the standing the page showed, as
// for a delete.
func (s *Server) blockDevice(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	refuse := func(status int, msg string) { s.renderDevice(w, r, status, ref, nil, "", "", msg) }
	if !parseForm(w, r, refuse) {
		return
	}
	seen, ok := standingFromPost(r.PostForm)
	if !ok {
		refuse(http.StatusBadRequest, reloadText)
		return
	}
	var err error
	switch r.PostForm.Get("action") {
	case "block":
		_, err = device.Block(r.Context(), s.db, seen.MAC, &seen, s.now())
	case "unblock":
		_, err = device.Unblock(r.Context(), s.db, seen.MAC, &seen)
	default:
		refuse(http.StatusBadRequest, "Choose Block or Unblock.")
		return
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices/"+url.PathEscape(ref)+"#remove", http.StatusSeeOther)
	case errors.Is(err, device.ErrNotBlocked):
		s.renderDevice(w, r, http.StatusConflict, seen.MAC, nil, "", "", "This device is no longer blocked.")
	case errors.Is(err, device.ErrDeviceChanged):
		s.renderDevice(w, r, http.StatusConflict, seen.MAC, nil, "", "", deviceChanged)
	default:
		serverError(w, r, "block device", err)
	}
}

// blockMAC blocks the MAC typed into the Blocked devices panel. It takes a
// MAC and never a friendly ID. The page showed nothing of that MAC, so there
// is no standing to check, and a block of a MAC already blocked changes
// nothing.
func (s *Server) blockMAC(w http.ResponseWriter, r *http.Request) {
	var bf blockForm
	refuse := func(status int, msg string) {
		bf.Error = msg
		s.renderDevices(w, r, status, "", bf)
	}
	if !parseForm(w, r, refuse) {
		return
	}
	bf.MAC = r.PostForm.Get("mac")
	mac, ok := device.ParseMAC(bf.MAC)
	if !ok {
		bf.Invalid = true
		refuse(http.StatusBadRequest, "Enter a MAC address, such as AA:BB:CC:DD:EE:FF.")
		return
	}
	if _, err := device.Block(r.Context(), s.db, mac, nil, s.now()); err != nil {
		serverError(w, r, "block MAC", err)
		return
	}
	http.Redirect(w, r, "/devices#blocked", http.StatusSeeOther)
}

// unblockView is the unblock of a MAC as its confirm page asks about it.
type unblockView struct {
	// MAC is the blocked MAC, and Action the path the form posts to. Its
	// fragment is the Blocked devices panel, where a refusal that sends the
	// Devices page puts its message.
	MAC, Action string
	// Device is the name of the registered device that has the MAC, or its
	// friendly ID if it has no name, and Href the path of its page. Both
	// are "" when no device has it.
	Device, Href string
	// Deleted is whether the MAC is a deleted device's, which the unblock
	// lets be reset.
	Deleted bool
	// Seen is the standing the page shows, which the form posts back.
	Seen standingView
}

// noBlock is the not-found page for a MAC that is not blocked.
var noBlock = notFoundView{Nav: "devices", What: "That MAC is not blocked", BackHref: "/devices#blocked", BackName: "Devices"}

// confirmUnblock asks whether to unblock a MAC, and says what that does to
// the device that has it. It changes nothing.
func (s *Server) confirmUnblock(w http.ResponseWriter, r *http.Request) {
	s.renderUnblock(w, r, http.StatusOK, "")
}

// renderUnblock sends the confirm page for the MAC in the path, with msg
// above the form if it is not "". A path that is not a MAC, or a MAC that is
// not blocked, gets the not-found page. The standing and the device are one
// snapshot.
func (s *Server) renderUnblock(w http.ResponseWriter, r *http.Request, status int, msg string) {
	ctx := r.Context()
	mac, ok := device.ParseMAC(r.PathValue("mac"))
	if !ok {
		s.renderNotFound(w, r, noBlock)
		return
	}
	var st device.Standing
	var dev db.Device
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if st, err = device.ReadStanding(ctx, q, mac); err != nil || st.DeviceID == 0 {
			return err
		}
		if dev, err = q.GetDevice(ctx, st.DeviceID); err != nil {
			return fmt.Errorf("get device: %w", err)
		}
		return nil
	})
	switch {
	case err != nil:
		serverError(w, r, "read blocked MAC", err)
		return
	case !st.Blocked:
		s.renderNotFound(w, r, noBlock)
		return
	}
	v := &unblockView{MAC: mac, Action: "/blocklist/" + mac + "#blocked", Deleted: st.Deleted, Seen: newStandingView(&st)}
	if st.DeviceID != 0 {
		v.Device, v.Href = deviceLabel(dev.Name, dev.FriendlyID), "/devices/"+dev.FriendlyID
	}
	s.render(w, r, status, "unblock", &pageData{Nav: "devices", Heading: "Unblock " + mac, Error: msg, Unblock: v})
}

// unblockMAC unblocks the MAC in the path. The form sends the standing its
// page showed, since what an unblock does depends on it: a registered device
// is back as it was, and a deleted one is reset.
func (s *Server) unblockMAC(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, msg string) { s.renderUnblock(w, r, status, msg) }
	if !parseForm(w, r, refuse) {
		return
	}
	mac, _ := device.ParseMAC(r.PathValue("mac"))
	seen, ok := standingFromPost(r.PostForm)
	if !ok || seen.MAC != mac {
		refuse(http.StatusBadRequest, reloadText)
		return
	}
	_, err := device.Unblock(r.Context(), s.db, mac, &seen)
	switch {
	case err == nil:
		http.Redirect(w, r, "/devices#blocked", http.StatusSeeOther)
	case errors.Is(err, device.ErrNotBlocked):
		s.renderDevices(w, r, http.StatusConflict, "", blockForm{Error: "That MAC is no longer blocked."})
	case errors.Is(err, device.ErrDeviceChanged):
		refuse(http.StatusConflict, "This MAC's device changed after the page loaded, so nothing was done. What is below is current.")
	default:
		serverError(w, r, "unblock MAC", err)
	}
}

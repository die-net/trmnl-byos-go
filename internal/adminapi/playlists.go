package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// scheduleJSON is when a playlist or an item is active, as playlist.Schedule
// has it. A request that leaves out a field gets its default: every day, all
// day and no date limit.
type scheduleJSON struct {
	// Days are lowercase three-letter weekday names in week order, from
	// "sun".
	Days []string `json:"days"`
	// WindowStart and WindowEnd are the time of day as HH:MM. Equal means
	// all day, and an end before the start wraps past midnight.
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	// StartsAt and EndsAt are Unix seconds, 0 for unbounded.
	StartsAt int64 `json:"starts_at"`
	EndsAt   int64 `json:"ends_at"`
}

// newScheduleJSON describes a schedule.
func newScheduleJSON(s playlist.Schedule) scheduleJSON {
	return scheduleJSON{
		Days:        playlist.DayNames(s.Days),
		WindowStart: playlist.FormatClock(s.WindowStart),
		WindowEnd:   playlist.FormatClock(s.WindowEnd),
		StartsAt:    s.StartsAt,
		EndsAt:      s.EndsAt,
	}
}

// edit returns the edit that replaces a schedule with this one. It is
// refused as invalid input if a day or time cannot be read, and the
// schedule itself is checked where it is applied.
func (j scheduleJSON) edit() (*playlist.ScheduleEdit, error) {
	days := int64(playlist.AllDays)
	if j.Days != nil {
		var err error
		if days, err = playlist.DaysFromNames(j.Days); err != nil {
			return nil, err
		}
	}
	var start, end int64
	var err error
	if j.WindowStart != "" {
		if start, err = playlist.ParseClock(j.WindowStart); err != nil {
			return nil, err
		}
	}
	if j.WindowEnd != "" {
		if end, err = playlist.ParseClock(j.WindowEnd); err != nil {
			return nil, err
		}
	}
	e := playlist.Schedule{Days: days, WindowStart: start, WindowEnd: end, StartsAt: j.StartsAt, EndsAt: j.EndsAt}.Full()
	return &e, nil
}

// playlistJSON is a playlist, the devices that show it, and its items.
type playlistJSON struct {
	ID       int64                `json:"id"`
	Name     string               `json:"name"`
	Schedule scheduleJSON         `json:"schedule"`
	Devices  []playlistDeviceJSON `json:"devices"`
	Items    []playlistItemJSON   `json:"items"`
}

// playlistDeviceJSON is a device linked to a playlist.
type playlistDeviceJSON struct {
	Device string `json:"device"`
	// Priority is the playlist's index in the device's order, from 0, which
	// is tried first.
	Priority int64 `json:"priority"`
}

// playlistItemJSON is one item of a playlist, in position order.
type playlistItemJSON struct {
	ID int64 `json:"id"`
	// Position is the item's index in its playlist, from 0.
	Position int64 `json:"position"`
	// DurationSeconds is how long the item shows, 0 until a button press.
	DurationSeconds int64 `json:"duration_seconds"`
	// Hidden items are never shown.
	Hidden       bool         `json:"hidden"`
	Schedule     scheduleJSON `json:"schedule"`
	InstanceID   int64        `json:"instance_id"`
	InstanceName string       `json:"instance_name"`
}

func (s *Server) listPlaylists(_ http.ResponseWriter, r *http.Request) ([]playlistJSON, error) {
	return s.playlistsJSON(r.Context())
}

// playlistsJSON describes every playlist, with its devices and items.
func (s *Server) playlistsJSON(ctx context.Context) ([]playlistJSON, error) {
	lists, err := s.db.Read.ListPlaylists(ctx)
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", err)
	}
	devs, err := s.db.Read.ListPlaylistDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("list playlist devices: %w", err)
	}
	items, err := s.db.Read.ListPlaylistItems(ctx)
	if err != nil {
		return nil, fmt.Errorf("list playlist items: %w", err)
	}
	out := make([]playlistJSON, 0, len(lists))
	byName := make(map[string]*playlistJSON, len(lists))
	for _, l := range lists {
		sched := playlist.Schedule{Days: l.Days, WindowStart: l.WindowStart, WindowEnd: l.WindowEnd, StartsAt: l.StartsAt, EndsAt: l.EndsAt}
		out = append(out, playlistJSON{ID: l.ID, Name: l.Name, Schedule: newScheduleJSON(sched), Devices: []playlistDeviceJSON{}, Items: []playlistItemJSON{}})
	}
	for i := range out {
		byName[out[i].Name] = &out[i]
	}
	for _, d := range devs {
		p := byName[d.Playlist]
		p.Devices = append(p.Devices, playlistDeviceJSON{Device: d.FriendlyID, Priority: d.Priority})
	}
	for _, it := range items {
		p := byName[it.Playlist]
		sched := playlist.Schedule{Days: it.Days, WindowStart: it.WindowStart, WindowEnd: it.WindowEnd, StartsAt: it.StartsAt, EndsAt: it.EndsAt}
		p.Items = append(p.Items, playlistItemJSON{
			ID: it.ID, Position: it.Position, DurationSeconds: it.Duration, Hidden: it.Hidden != 0, Schedule: newScheduleJSON(sched),
			InstanceID: it.InstanceID, InstanceName: it.InstanceName,
		})
	}
	return out, nil
}

// addItemRequest is the body of a playlist item create request. It needs a
// playlist or a device, as playlist add does.
type addItemRequest struct {
	// Device is the friendly ID or MAC of a device to show the playlist on.
	Device string `json:"device"`
	// Playlist is the playlist to add to, created if new. Default: the
	// device's friendly ID.
	Playlist   string `json:"playlist"`
	InstanceID int64  `json:"instance_id"`
	// DurationSeconds is how long the item shows, 0 until a button press.
	// Default playlist.DefaultDuration.
	DurationSeconds *int64 `json:"duration_seconds"`
}

// addItemResponse is the item that was added.
type addItemResponse struct {
	ID       int64  `json:"id"`
	Playlist string `json:"playlist"`
	Position int64  `json:"position"`
}

func (s *Server) addPlaylistItem(w http.ResponseWriter, r *http.Request) (addItemResponse, error) {
	var req addItemRequest
	if err := decode(w, r, &req, false); err != nil {
		return addItemResponse{}, err
	}
	dur := playlist.DefaultDuration
	if req.DurationSeconds != nil {
		var err error
		if dur, err = playlist.DurationFromSeconds(*req.DurationSeconds); err != nil {
			return addItemResponse{}, err
		}
	}
	ctx := r.Context()
	var deviceID int64
	if req.Device != "" {
		dev, err := device.FindDevice(ctx, s.db.Read, req.Device)
		if err != nil {
			return addItemResponse{}, err
		}
		deviceID = dev.ID
	}
	item, name, err := playlist.AddItem(ctx, s.db, req.Playlist, deviceID, req.InstanceID, dur)
	if err != nil {
		return addItemResponse{}, err
	}
	return addItemResponse{ID: item.ID, Playlist: name, Position: item.Position}, nil
}

func (s *Server) deletePlaylistItem(r *http.Request) error {
	id, err := pathID(r, playlist.ErrNoItem)
	if err != nil {
		return err
	}
	return playlist.DeleteItem(r.Context(), s.db, id)
}

// playlistByID describes one playlist, or returns an error wrapping
// playlist.ErrNoPlaylist if there is none.
func (s *Server) playlistByID(ctx context.Context, id int64) (playlistJSON, error) {
	lists, err := s.playlistsJSON(ctx)
	if err != nil {
		return playlistJSON{}, err
	}
	for i := range lists {
		if lists[i].ID == id {
			return lists[i], nil
		}
	}
	return playlistJSON{}, fmt.Errorf("%w: %d", playlist.ErrNoPlaylist, id)
}

// createPlaylistRequest is the body of a playlist create request.
type createPlaylistRequest struct {
	// Name is the new playlist's name.
	Name string `json:"name"`
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) (playlistJSON, error) {
	var req createPlaylistRequest
	if err := decode(w, r, &req, false); err != nil {
		return playlistJSON{}, err
	}
	pl, err := playlist.CreatePlaylist(r.Context(), s.db, req.Name)
	if err != nil {
		return playlistJSON{}, err
	}
	return s.playlistByID(r.Context(), pl.ID)
}

func (s *Server) deletePlaylist(r *http.Request) error {
	id, err := pathID(r, playlist.ErrNoPlaylist)
	if err != nil {
		return err
	}
	return playlist.DeletePlaylist(r.Context(), s.db, id)
}

// patchPlaylistRequest is the body of a playlist update request. A field
// that is left out leaves that part of the playlist as it is.
type patchPlaylistRequest struct {
	// Name renames the playlist.
	Name *string `json:"name"`
	// Schedule replaces the playlist's schedule.
	Schedule *scheduleJSON `json:"schedule"`
}

func (s *Server) patchPlaylist(w http.ResponseWriter, r *http.Request) (playlistJSON, error) {
	id, err := pathID(r, playlist.ErrNoPlaylist)
	if err != nil {
		return playlistJSON{}, err
	}
	var req patchPlaylistRequest
	if err := decode(w, r, &req, false); err != nil {
		return playlistJSON{}, err
	}
	u := playlist.Update{Name: req.Name}
	if req.Schedule != nil {
		if u.Schedule, err = req.Schedule.edit(); err != nil {
			return playlistJSON{}, err
		}
	}
	if err := playlist.UpdatePlaylist(r.Context(), s.db, id, u); err != nil {
		return playlistJSON{}, err
	}
	return s.playlistByID(r.Context(), id)
}

// linkDeviceRequest is the body of a request to link a device to a playlist.
type linkDeviceRequest struct {
	// Priority is the playlist's index in the device's order. Left out, a
	// new link goes last and an existing one stays where it is.
	Priority *int64 `json:"priority"`
}

func (s *Server) linkPlaylistDevice(w http.ResponseWriter, r *http.Request) (playlistJSON, error) {
	id, err := pathID(r, playlist.ErrNoPlaylist)
	if err != nil {
		return playlistJSON{}, err
	}
	var req linkDeviceRequest
	if err := decode(w, r, &req, true); err != nil {
		return playlistJSON{}, err
	}
	ctx := r.Context()
	dev, err := device.FindDevice(ctx, s.db.Read, r.PathValue("device"))
	if err != nil {
		return playlistJSON{}, err
	}
	if err := playlist.LinkDevice(ctx, s.db, id, dev.ID, req.Priority); err != nil {
		return playlistJSON{}, err
	}
	return s.playlistByID(ctx, id)
}

func (s *Server) unlinkPlaylistDevice(r *http.Request) error {
	id, err := pathID(r, playlist.ErrNotLinked)
	if err != nil {
		return err
	}
	dev, err := device.FindDevice(r.Context(), s.db.Read, r.PathValue("device"))
	if errors.Is(err, device.ErrNoDevice) {
		return fmt.Errorf("%w: %s", playlist.ErrNotLinked, r.PathValue("device"))
	} else if err != nil {
		return err
	}
	return playlist.UnlinkDevice(r.Context(), s.db, id, dev.ID)
}

// patchItemRequest is the body of a playlist item update request. A field
// that is left out leaves that part of the item as it is.
type patchItemRequest struct {
	// Position is the item's new index in its playlist.
	Position *int64 `json:"position"`
	// DurationSeconds is how long the item shows, 0 until a button press.
	DurationSeconds *int64        `json:"duration_seconds"`
	Hidden          *bool         `json:"hidden"`
	Schedule        *scheduleJSON `json:"schedule"`
}

func (s *Server) patchPlaylistItem(w http.ResponseWriter, r *http.Request) (playlistItemJSON, error) {
	id, err := pathID(r, playlist.ErrNoItem)
	if err != nil {
		return playlistItemJSON{}, err
	}
	var req patchItemRequest
	if err := decode(w, r, &req, false); err != nil {
		return playlistItemJSON{}, err
	}
	u := playlist.ItemUpdate{Position: req.Position, Hidden: req.Hidden}
	if req.DurationSeconds != nil {
		d, err := playlist.DurationFromSeconds(*req.DurationSeconds)
		if err != nil {
			return playlistItemJSON{}, err
		}
		u.Duration = &d
	}
	if req.Schedule != nil {
		if u.Schedule, err = req.Schedule.edit(); err != nil {
			return playlistItemJSON{}, err
		}
	}
	if err := playlist.UpdateItem(r.Context(), s.db, id, u); err != nil {
		return playlistItemJSON{}, err
	}
	lists, err := s.playlistsJSON(r.Context())
	if err != nil {
		return playlistItemJSON{}, err
	}
	for i := range lists {
		for j := range lists[i].Items {
			if lists[i].Items[j].ID == id {
				return lists[i].Items[j], nil
			}
		}
	}
	return playlistItemJSON{}, fmt.Errorf("%w: %d", playlist.ErrNoItem, id)
}

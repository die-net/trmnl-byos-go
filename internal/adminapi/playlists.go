package adminapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/db"
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

// playlistsJSON describes every playlist, with its devices and items, from
// one snapshot of the database.
func (s *Server) playlistsJSON(ctx context.Context) ([]playlistJSON, error) {
	var out []playlistJSON
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		lists, err := q.ListPlaylists(ctx)
		if err != nil {
			return fmt.Errorf("list playlists: %w", err)
		}
		devs, err := q.ListPlaylistDevices(ctx)
		if err != nil {
			return fmt.Errorf("list playlist devices: %w", err)
		}
		items, err := q.ListPlaylistItems(ctx)
		if err != nil {
			return fmt.Errorf("list playlist items: %w", err)
		}
		out = buildPlaylists(lists, devs, items)
		return nil
	})
	return out, err
}

// playlistOf describes one playlist, with its devices and items, as of the
// transaction q reads in. It returns an error wrapping playlist.ErrNoPlaylist
// if there is none.
func playlistOf(ctx context.Context, q *db.Queries, id int64) (playlistJSON, error) {
	pl, err := q.GetPlaylist(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return playlistJSON{}, fmt.Errorf("%w: %d", playlist.ErrNoPlaylist, id)
	} else if err != nil {
		return playlistJSON{}, fmt.Errorf("get playlist: %w", err)
	}
	devRows, err := q.ListPlaylistDevicesOf(ctx, id)
	if err != nil {
		return playlistJSON{}, fmt.Errorf("list playlist devices: %w", err)
	}
	devs := make([]db.ListPlaylistDevicesRow, len(devRows))
	for i, d := range devRows {
		devs[i] = db.ListPlaylistDevicesRow(d)
	}
	itemRows, err := q.ListPlaylistItemsOf(ctx, id)
	if err != nil {
		return playlistJSON{}, fmt.Errorf("list playlist items: %w", err)
	}
	items := make([]db.ListPlaylistItemsRow, len(itemRows))
	for i, it := range itemRows {
		items[i] = db.ListPlaylistItemsRow(it)
	}
	return buildPlaylists([]db.Playlist{pl}, devs, items)[0], nil
}

// playlistByID describes one playlist from one snapshot, or returns an error
// wrapping playlist.ErrNoPlaylist if there is none.
func (s *Server) playlistByID(ctx context.Context, id int64) (playlistJSON, error) {
	var out playlistJSON
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		out, err = playlistOf(ctx, q, id)
		return err
	})
	return out, err
}

// buildPlaylists describes the playlists, each with the device and item rows
// that name it by ID. A row whose playlist is not in lists is skipped, which
// is what a snapshot that is not one would give.
func buildPlaylists(lists []db.Playlist, devs []db.ListPlaylistDevicesRow, items []db.ListPlaylistItemsRow) []playlistJSON {
	out := make([]playlistJSON, 0, len(lists))
	for _, l := range lists {
		sched := playlist.Schedule{Days: l.Days, WindowStart: l.WindowStart, WindowEnd: l.WindowEnd, StartsAt: l.StartsAt, EndsAt: l.EndsAt}
		out = append(out, playlistJSON{ID: l.ID, Name: l.Name, Schedule: newScheduleJSON(sched), Devices: []playlistDeviceJSON{}, Items: []playlistItemJSON{}})
	}
	byID := make(map[int64]*playlistJSON, len(out))
	for i := range out {
		byID[out[i].ID] = &out[i]
	}
	for _, d := range devs {
		if p := byID[d.PlaylistID]; p != nil {
			p.Devices = append(p.Devices, playlistDeviceJSON{Device: d.FriendlyID, Priority: d.Priority})
		}
	}
	for _, it := range items {
		p := byID[it.PlaylistID]
		if p == nil {
			continue
		}
		sched := playlist.Schedule{Days: it.Days, WindowStart: it.WindowStart, WindowEnd: it.WindowEnd, StartsAt: it.StartsAt, EndsAt: it.EndsAt}
		p.Items = append(p.Items, playlistItemJSON{
			ID: it.ID, Position: it.Position, DurationSeconds: it.Duration, Hidden: it.Hidden != 0, Schedule: newScheduleJSON(sched),
			InstanceID: it.InstanceID, InstanceName: it.InstanceName,
		})
	}
	return out
}

// addItemRequest is the body of a playlist item create request. It names the
// playlist by exactly one of PlaylistID and Playlist.
type addItemRequest struct {
	// Device is the friendly ID or MAC of a device to link to the playlist,
	// last, if it is not linked. Optional.
	Device string `json:"device"`
	// PlaylistID is the ID of the playlist to add to.
	PlaylistID int64 `json:"playlist_id"`
	// Playlist is the name of the playlist to add to.
	Playlist   string `json:"playlist"`
	InstanceID int64  `json:"instance_id"`
	// DurationSeconds is how long the item shows, 0 until a button press.
	// Default playlist.DefaultDuration.
	DurationSeconds *int64 `json:"duration_seconds"`
}

// addItemResponse is the item that was added.
type addItemResponse struct {
	ID         int64 `json:"id"`
	PlaylistID int64 `json:"playlist_id"`
	Position   int64 `json:"position"`
}

func (s *Server) addPlaylistItem(w http.ResponseWriter, r *http.Request) (addItemResponse, error) {
	var req addItemRequest
	if err := decode(w, r, &req, false); err != nil {
		return addItemResponse{}, err
	}
	if (req.PlaylistID != 0) == (req.Playlist != "") {
		return addItemResponse{}, fmt.Errorf("%w: give exactly one of playlist_id and playlist", errBadBody)
	}
	ref := playlist.ByName(req.Playlist)
	if req.PlaylistID != 0 {
		ref = playlist.ByID(req.PlaylistID)
	}
	dur := playlist.DefaultDuration
	if req.DurationSeconds != nil {
		var err error
		if dur, err = playlist.DurationFromSeconds(*req.DurationSeconds); err != nil {
			return addItemResponse{}, err
		}
	}
	item, err := playlist.AddItem(r.Context(), s.db, ref, req.Device, req.InstanceID, dur)
	if err != nil {
		return addItemResponse{}, err
	}
	return addItemResponse{ID: item.ID, PlaylistID: item.PlaylistID, Position: item.Position}, nil
}

func (s *Server) deletePlaylistItem(r *http.Request) error {
	id, err := pathID(r, playlist.ErrNoItem)
	if err != nil {
		return err
	}
	return playlist.DeleteItem(r.Context(), s.db, id)
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
	return playlist.DeletePlaylist(r.Context(), s.db, playlist.ByID(id), nil)
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
	if err := playlist.UpdatePlaylist(r.Context(), s.db, playlist.ByID(id), u); err != nil {
		return playlistJSON{}, err
	}
	return s.playlistByID(r.Context(), id)
}

// moveLinkRequest is the body of a request to move a device's link.
type moveLinkRequest struct {
	// Order is the IDs of the device's playlists in the order the caller saw
	// them, and Priority the index to move the playlist to, from 0, which is
	// tried first. Both are required.
	Order    *[]int64 `json:"order"`
	Priority *int64   `json:"priority"`
}

// linkPlaylistDevice links a device last, or leaves an existing link alone.
// It takes no body, so a stale request that means to move a link cannot make
// one.
func (s *Server) linkPlaylistDevice(w http.ResponseWriter, r *http.Request) (playlistJSON, error) {
	id, err := pathID(r, playlist.ErrNoPlaylist)
	if err != nil {
		return playlistJSON{}, err
	}
	var none struct{}
	if err := decode(w, r, &none, true); err != nil {
		return playlistJSON{}, err
	}
	ctx := r.Context()
	if err := playlist.LinkDevice(ctx, s.db, playlist.ByID(id), r.PathValue("device")); err != nil {
		return playlistJSON{}, err
	}
	return s.playlistByID(ctx, id)
}

// movePlaylistDevice moves an existing link to a place in the device's order.
func (s *Server) movePlaylistDevice(w http.ResponseWriter, r *http.Request) (playlistJSON, error) {
	id, err := pathID(r, playlist.ErrNoPlaylist)
	if err != nil {
		return playlistJSON{}, err
	}
	var req moveLinkRequest
	if err := decode(w, r, &req, false); err != nil {
		return playlistJSON{}, err
	}
	if req.Order == nil || req.Priority == nil {
		return playlistJSON{}, fmt.Errorf("%w: give both order and priority", errBadBody)
	}
	ctx := r.Context()
	if err := playlist.MoveLink(ctx, s.db, playlist.ByID(id), r.PathValue("device"), *req.Order, *req.Priority); err != nil {
		return playlistJSON{}, err
	}
	return s.playlistByID(ctx, id)
}

func (s *Server) unlinkPlaylistDevice(r *http.Request) error {
	id, err := pathID(r, playlist.ErrNoPlaylist)
	if err != nil {
		return err
	}
	return playlist.UnlinkDevice(r.Context(), s.db, playlist.ByID(id), r.PathValue("device"))
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
	var out playlistItemJSON
	err = s.db.ReadTx(r.Context(), func(q *db.Queries) error {
		item, err := q.GetPlaylistItem(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", playlist.ErrNoItem, id)
		} else if err != nil {
			return fmt.Errorf("get item: %w", err)
		}
		pl, err := playlistOf(r.Context(), q, item.PlaylistID)
		if err != nil {
			return err
		}
		for i := range pl.Items {
			if pl.Items[i].ID == id {
				out = pl.Items[i]
				return nil
			}
		}
		return fmt.Errorf("%w: %d", playlist.ErrNoItem, id)
	})
	return out, err
}

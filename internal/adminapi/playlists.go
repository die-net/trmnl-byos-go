package adminapi

import (
	"fmt"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
)

// playlistJSON is a playlist, the devices that show it, and its items.
type playlistJSON struct {
	Name    string               `json:"name"`
	Devices []playlistDeviceJSON `json:"devices"`
	Items   []playlistItemJSON   `json:"items"`
}

// playlistDeviceJSON is a device linked to a playlist. The device shows the
// playlist with the lowest priority.
type playlistDeviceJSON struct {
	Device   string `json:"device"`
	Priority int64  `json:"priority"`
}

// playlistItemJSON is one item of a playlist, in position order.
type playlistItemJSON struct {
	ID       int64 `json:"id"`
	Position int64 `json:"position"`
	// DurationSeconds is how long the item shows, 0 until a button press.
	DurationSeconds int64  `json:"duration_seconds"`
	InstanceID      int64  `json:"instance_id"`
	InstanceName    string `json:"instance_name"`
}

func (s *Server) listPlaylists(_ http.ResponseWriter, r *http.Request) ([]playlistJSON, error) {
	ctx := r.Context()
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
		out = append(out, playlistJSON{Name: l.Name, Devices: []playlistDeviceJSON{}, Items: []playlistItemJSON{}})
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
		p.Items = append(p.Items, playlistItemJSON{
			ID: it.ID, Position: it.Position, DurationSeconds: it.Duration, InstanceID: it.InstanceID, InstanceName: it.InstanceName,
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
		if dur, err = duration(*req.DurationSeconds); err != nil {
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

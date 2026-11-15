package playlist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// CleanName returns a playlist name without the space around it, or an error
// of kind errkind.ErrInvalid if it is empty, or is too long, is not UTF-8 or
// has a control character, as device.CleanName checks.
func CleanName(name string) (string, error) {
	name, err := device.CleanName(name)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("%w: name is empty", errkind.ErrInvalid)
	}
	return name, nil
}

// CreatePlaylist makes an empty playlist with the name, checked by CleanName.
// It returns an error wrapping ErrNameTaken if a playlist has the name.
func CreatePlaylist(ctx context.Context, client *db.Client, name string) (db.Playlist, error) {
	name, err := CleanName(name)
	if err != nil {
		return db.Playlist{}, err
	}
	var pl db.Playlist
	err = client.InTx(ctx, func(q *db.Queries) error {
		if err := checkNameFree(ctx, q, name, 0); err != nil {
			return err
		}
		var err error
		if pl, err = q.CreatePlaylist(ctx, name); err != nil {
			return fmt.Errorf("create playlist: %w", err)
		}
		return nil
	})
	return pl, err
}

// checkNameFree returns an error wrapping ErrNameTaken if a playlist other
// than the one with ID except has the name.
func checkNameFree(ctx context.Context, q *db.Queries, name string, except int64) error {
	other, err := q.GetPlaylistByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("get playlist: %w", err)
	}
	if other.ID != except {
		return fmt.Errorf("%w: %q", ErrNameTaken, name)
	}
	return nil
}

// Update is a change to a playlist. A nil field is left as it is.
type Update struct {
	// Name is the playlist's new name, checked by CleanName.
	Name *string
	// Schedule is applied to the playlist's schedule.
	Schedule *ScheduleEdit
}

// UpdatePlaylist applies the update to a playlist in one transaction.
// Nothing changes if any part of it is refused. It returns an error wrapping
// ErrNoPlaylist if there is no such playlist, ErrNameTaken if another
// playlist has the new name, and one wrapping errkind.ErrInvalid if a part of
// the update is not acceptable. Renaming a playlist to its own name is fine.
func UpdatePlaylist(ctx context.Context, client *db.Client, id int64, u Update) error {
	var name string
	if u.Name != nil {
		var err error
		if name, err = CleanName(*u.Name); err != nil {
			return err
		}
	}
	return client.InTx(ctx, func(q *db.Queries) error {
		pl, err := getPlaylist(ctx, q, id)
		if err != nil {
			return err
		}
		var s Schedule
		if u.Schedule != nil {
			s = u.Schedule.Apply(Schedule{Days: pl.Days, WindowStart: pl.WindowStart, WindowEnd: pl.WindowEnd, StartsAt: pl.StartsAt, EndsAt: pl.EndsAt})
			if err := s.Validate(); err != nil {
				return err
			}
		}
		if u.Name != nil {
			if err := checkNameFree(ctx, q, name, id); err != nil {
				return err
			}
			if err := q.UpdatePlaylistName(ctx, db.UpdatePlaylistNameParams{Name: name, ID: id}); err != nil {
				return fmt.Errorf("rename playlist: %w", err)
			}
		}
		if u.Schedule == nil {
			return nil
		}
		err = q.UpdatePlaylistSchedule(ctx, db.UpdatePlaylistScheduleParams{
			Days: s.Days, WindowStart: s.WindowStart, WindowEnd: s.WindowEnd, StartsAt: s.StartsAt, EndsAt: s.EndsAt, ID: id,
		})
		if err != nil {
			return fmt.Errorf("update playlist: %w", err)
		}
		return nil
	})
}

// getPlaylist returns the playlist, or an error wrapping ErrNoPlaylist.
func getPlaylist(ctx context.Context, q *db.Queries, id int64) (db.Playlist, error) {
	pl, err := q.GetPlaylist(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Playlist{}, fmt.Errorf("%w: %d", ErrNoPlaylist, id)
	} else if err != nil {
		return db.Playlist{}, fmt.Errorf("get playlist: %w", err)
	}
	return pl, nil
}

// DeletePlaylist removes a playlist with its items, slots and device links,
// and renumbers the priorities of every device that was linked to it so they
// stay 0 to n-1. It returns an error wrapping ErrNoPlaylist if there is no
// such playlist.
func DeletePlaylist(ctx context.Context, client *db.Client, id int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		if _, err := getPlaylist(ctx, q, id); err != nil {
			return err
		}
		// The links of the devices on it close up around it: one statement
		// moves their priorities out of range, and one ranks the rest.
		if err := q.NegatePlaylistDevicesPriorities(ctx, id); err != nil {
			return fmt.Errorf("renumber links: %w", err)
		}
		if err := q.RankPlaylistDevicesWithout(ctx, id); err != nil {
			return fmt.Errorf("renumber links: %w", err)
		}
		if _, err := q.DeletePlaylist(ctx, id); err != nil {
			return fmt.Errorf("delete playlist: %w", err)
		}
		return nil
	})
}

// renumberLinks gives a device's links the priorities 0 to n-1 in their
// current order, with the link to playlist moved, if not 0, placed at index to
// after it was at from. A priority is unique per device and SQLite checks that
// one row at a time, so the old priorities are first moved out of the range
// of indexes. It takes two statements however many links there are, and runs
// in the caller's transaction.
func renumberLinks(ctx context.Context, q *db.Queries, deviceID, moved, from, to int64) error {
	if err := q.NegateDevicePriorities(ctx, deviceID); err != nil {
		return fmt.Errorf("renumber links: %w", err)
	}
	err := q.RankDeviceLinks(ctx, db.RankDeviceLinksParams{DeviceID: deviceID, MovedPlaylistID: moved, MovedKey: movedKey(from, to)})
	if err != nil {
		return fmt.Errorf("renumber links: %w", err)
	}
	return nil
}

// LinkDevice links a device to a playlist, last in the device's order, if it
// is not linked, and then, if priority is not nil, moves the link to that
// index in the device's order, which shifts the links between its old and new
// place by one. A device that is linked already and a nil priority leave
// everything as it is. It returns an error wrapping ErrNoPlaylist or
// device.ErrNoDevice for an ID that matches nothing, and one wrapping
// errkind.ErrInvalid if priority is not in 0 to n-1 for the n links the
// device has once this one is added, in which case no link is made.
func LinkDevice(ctx context.Context, client *db.Client, playlistID, deviceID int64, priority *int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		if _, err := getPlaylist(ctx, q, playlistID); err != nil {
			return err
		}
		if _, err := q.GetDevice(ctx, deviceID); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", device.ErrNoDevice, deviceID)
		} else if err != nil {
			return fmt.Errorf("get device: %w", err)
		}
		ids, err := q.ListDevicePlaylistIDs(ctx, deviceID)
		if err != nil {
			return fmt.Errorf("list links: %w", err)
		}
		from := slices.Index(ids, playlistID)
		n := len(ids)
		if from < 0 {
			n++
		}
		if priority != nil && (*priority < 0 || *priority >= int64(n)) {
			return fmt.Errorf("%w: priority %d is not between 0 and %d", errkind.ErrInvalid, *priority, n-1)
		}
		if from < 0 {
			if err := q.AppendPlaylistDevice(ctx, db.AppendPlaylistDeviceParams{DeviceID: deviceID, PlaylistID: playlistID}); err != nil {
				return fmt.Errorf("link device: %w", err)
			}
			from = len(ids)
		}
		if priority == nil || *priority == int64(from) {
			return nil
		}
		return renumberLinks(ctx, q, deviceID, playlistID, int64(from), *priority)
	})
}

// UnlinkDevice removes a device's link to a playlist and renumbers the
// device's remaining links so priorities stay 0 to n-1. It returns an error
// wrapping ErrNotLinked if there is no such link, which includes an ID that
// matches nothing.
func UnlinkDevice(ctx context.Context, client *db.Client, playlistID, deviceID int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		n, err := q.DeletePlaylistDevice(ctx, db.DeletePlaylistDeviceParams{DeviceID: deviceID, PlaylistID: playlistID})
		if err != nil {
			return fmt.Errorf("unlink device: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: playlist %d, device %d", ErrNotLinked, playlistID, deviceID)
		}
		return renumberLinks(ctx, q, deviceID, 0, 0, 0)
	})
}

package playlist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// MaxNameBytes is the longest a playlist's name may be.
const MaxNameBytes = 64

// CleanName returns a playlist name without the space around it, or an error
// of kind errkind.ErrInvalid if it is empty, is over MaxNameBytes, is not
// UTF-8, or has a control character such as a newline.
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: name is empty", errkind.ErrInvalid)
	case len(name) > MaxNameBytes:
		return "", fmt.Errorf("%w: name is over %d bytes", errkind.ErrInvalid, MaxNameBytes)
	case !utf8.ValidString(name):
		return "", fmt.Errorf("%w: name is not valid UTF-8", errkind.ErrInvalid)
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("%w: name has a control character", errkind.ErrInvalid)
	}
	return name, nil
}

// Ref names a playlist by its ID, or by its name when ID is 0. It is resolved
// inside the transaction that uses it, so the playlist it means is the one
// that is changed.
type Ref struct {
	ID   int64
	Name string
}

// ByID returns a Ref to the playlist with the ID.
func ByID(id int64) Ref { return Ref{ID: id} }

// ByName returns a Ref to the playlist with the name, as it is stored.
func ByName(name string) Ref { return Ref{Name: name} }

// resolve returns the playlist r names, or an error wrapping ErrNoPlaylist. It
// runs with q inside a transaction.
func (r Ref) resolve(ctx context.Context, q *db.Queries) (db.Playlist, error) {
	var pl db.Playlist
	var err error
	label := strconv.FormatInt(r.ID, 10)
	if r.ID != 0 {
		pl, err = q.GetPlaylist(ctx, r.ID)
	} else {
		label = strconv.Quote(r.Name)
		pl, err = q.GetPlaylistByName(ctx, r.Name)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return db.Playlist{}, fmt.Errorf("%w: %s", ErrNoPlaylist, label)
	} else if err != nil {
		return db.Playlist{}, fmt.Errorf("get playlist: %w", err)
	}
	return pl, nil
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

// UpdatePlaylist applies the update to the playlist ref names in one transaction.
// Nothing changes if any part of it is refused. It returns an error wrapping
// ErrNoPlaylist if there is no such playlist, ErrNameTaken if another
// playlist has the new name, and one wrapping errkind.ErrInvalid if a part of
// the update is not acceptable. Renaming a playlist to its own name is fine.
func UpdatePlaylist(ctx context.Context, client *db.Client, ref Ref, u Update) error {
	var name string
	if u.Name != nil {
		var err error
		if name, err = CleanName(*u.Name); err != nil {
			return err
		}
	}
	return client.InTx(ctx, func(q *db.Queries) error {
		pl, err := ref.resolve(ctx, q)
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
			if err := checkNameFree(ctx, q, name, pl.ID); err != nil {
				return err
			}
			if err := q.UpdatePlaylistName(ctx, db.UpdatePlaylistNameParams{Name: name, ID: pl.ID}); err != nil {
				return fmt.Errorf("rename playlist: %w", err)
			}
		}
		if u.Schedule == nil {
			return nil
		}
		err = q.UpdatePlaylistSchedule(ctx, db.UpdatePlaylistScheduleParams{
			Days: s.Days, WindowStart: s.WindowStart, WindowEnd: s.WindowEnd, StartsAt: s.StartsAt, EndsAt: s.EndsAt, ID: pl.ID,
		})
		if err != nil {
			return fmt.Errorf("update playlist: %w", err)
		}
		return nil
	})
}

// DeletePlaylist removes a playlist with its items, slots and device links,
// and renumbers the priorities of every device that was linked to it so they
// stay 0 to n-1. It returns an error wrapping ErrNoPlaylist if there is no
// such playlist.
func DeletePlaylist(ctx context.Context, client *db.Client, ref Ref) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		pl, err := ref.resolve(ctx, q)
		if err != nil {
			return err
		}
		id := pl.ID
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

// LinkDevice links a device, named by friendly ID or MAC, to a playlist, last
// in the device's order. A device that is linked already stays where it is.
// It returns an error wrapping ErrNoPlaylist or device.ErrNoDevice for a name
// that matches nothing. MoveLink changes a link's place.
func LinkDevice(ctx context.Context, client *db.Client, ref Ref, deviceName string) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		pl, err := ref.resolve(ctx, q)
		if err != nil {
			return err
		}
		dev, err := device.FindDevice(ctx, q, deviceName)
		if err != nil {
			return err
		}
		if err := q.AppendPlaylistDevice(ctx, db.AppendPlaylistDeviceParams{DeviceID: dev.ID, PlaylistID: pl.ID}); err != nil {
			return fmt.Errorf("link device: %w", err)
		}
		return nil
	})
}

// MoveLink moves the link between a playlist and a device, named by friendly
// ID or MAC, to index in the device's playlist order, which shifts the links
// between its old and new place by one. Index 0 is tried first, and moving to
// the current index changes nothing. It never makes a link: it returns an
// error wrapping ErrNotLinked if there is none, ErrNoPlaylist or
// device.ErrNoDevice for a name that matches nothing, and one wrapping
// errkind.ErrInvalid if index is not in 0 to n-1 for the n links the device
// has.
func MoveLink(ctx context.Context, client *db.Client, ref Ref, deviceName string, index int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		pl, err := ref.resolve(ctx, q)
		if err != nil {
			return err
		}
		dev, err := device.FindDevice(ctx, q, deviceName)
		if err != nil {
			return err
		}
		ids, err := q.ListDevicePlaylistIDs(ctx, dev.ID)
		if err != nil {
			return fmt.Errorf("list links: %w", err)
		}
		from := slices.Index(ids, pl.ID)
		if from < 0 {
			return fmt.Errorf("%w: playlist %q, device %s", ErrNotLinked, pl.Name, dev.FriendlyID)
		}
		if index < 0 || index >= int64(len(ids)) {
			return fmt.Errorf("%w: %d is not a place in the device's playlist order, which runs from 0 to %d", errkind.ErrInvalid, index, len(ids)-1)
		}
		if index == int64(from) {
			return nil
		}
		return renumberLinks(ctx, q, dev.ID, pl.ID, int64(from), index)
	})
}

// UnlinkDevice removes the link between a playlist and a device, named by
// friendly ID or MAC, and renumbers the device's remaining links so
// priorities stay 0 to n-1. It returns an error wrapping ErrNoPlaylist or
// device.ErrNoDevice for a name that matches nothing, and one wrapping
// ErrNotLinked if there is no such link.
func UnlinkDevice(ctx context.Context, client *db.Client, ref Ref, deviceName string) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		pl, err := ref.resolve(ctx, q)
		if err != nil {
			return err
		}
		dev, err := device.FindDevice(ctx, q, deviceName)
		if err != nil {
			return err
		}
		playlistID, deviceID := pl.ID, dev.ID
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

// Package playlist edits playlists and moves a device's cursor through its
// playlist: which item it shows and since when.
package playlist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// DefaultDuration is how long an item shows when no time is given.
const DefaultDuration = 15 * time.Minute

// MinDuration is the shortest time an item may show. A device spends
// seconds on each wake, and the cursor treats an item as over WakeSlack
// before its end, so a shorter item could never be seen.
const MinDuration = time.Minute

// WakeSlack is how early a timer wake may come and still count as the end
// of an item. A device's clock drifts in deep sleep, and a wake a few
// seconds early would otherwise show the same item again and sleep only
// those few seconds.
const WakeSlack = 30 * time.Second

// Item is a playlist item as the cursor sees it.
type Item struct {
	ID int64
	// Duration is how long the item shows. 0 shows it until a button press.
	Duration time.Duration
	// Skip is true for an item that is not shown now, because it is hidden
	// or off its schedule. The cursor steps over it.
	Skip bool
}

// Cursor is the item a device shows and when it started.
type Cursor struct {
	ItemID  int64
	Started time.Time
}

// Advance returns the index of the item to show now and when it started,
// from the device's cursor, which is nil before the device has shown an
// item. items is the whole playlist, with the items that are not shown now
// marked Skip. A press moves to the next item that is shown. Otherwise each
// item whose duration has passed gives way to the next one that is shown,
// which starts when it ended, so timing does not drift. A cursor on an item
// that is no longer in items starts over at the first one that is shown. A
// cursor on one that is skipped moves to the next one that is shown, which
// starts now. It returns -1 when no item is shown.
func Advance(items []Item, cur *Cursor, now time.Time, pressed bool) (int, time.Time) {
	i := -1
	if cur != nil {
		for j := range items {
			if items[j].ID == cur.ItemID {
				i = j
				break
			}
		}
	}
	if i < 0 || pressed || items[i].Skip {
		// With no cursor on an item, the first shown one is the one after the last.
		from := i
		if i < 0 {
			from = len(items) - 1
		}
		if j := nextShown(items, from); j >= 0 {
			return j, now
		}
		return -1, time.Time{}
	}
	started := cur.Started
	// After a long sleep, skip whole turns through the playlist at once.
	// A turn has a fixed length only when every item shown has a duration.
	var turn time.Duration
	for _, it := range items {
		if it.Skip {
			continue
		}
		if it.Duration <= 0 {
			turn = 0
			break
		}
		turn += it.Duration
	}
	if turn > 0 {
		if behind := now.Sub(started); behind > turn {
			started = started.Add(behind / turn * turn)
		}
	}
	for range len(items) + 1 {
		d := items[i].Duration
		if d <= 0 || now.Add(WakeSlack).Before(started.Add(d)) {
			break
		}
		started = started.Add(d)
		i = nextShown(items, i)
	}
	return i, started
}

// nextShown returns the index of the first item after index i, wrapping
// around, that is not skipped, which is i itself when no other is. It returns
// -1 if every item is skipped, and for an empty list.
func nextShown(items []Item, i int) int {
	for k := 1; k <= len(items); k++ {
		if j := (i + k) % len(items); !items[j].Skip {
			return j
		}
	}
	return -1
}

// Pressed reports whether a wake's Update-Source header means a button
// press. The firmware names the ESP32 wake cause, so a press is "button" on
// some boards and "EXT0" or "EXT1" on others, such as the OG DIY kit.
func Pressed(updateSource string) bool {
	switch updateSource {
	case "button", "EXT0", "EXT1":
		return true
	default:
		return false
	}
}

// Errors that say why a request is refused, which the admin API turns into
// HTTP statuses. Each is wrapped with detail that is safe to show.
var (
	// ErrNoItem means no playlist item has the ID.
	ErrNoItem = errkind.New(errkind.ErrNotFound, "no such playlist item")
	// ErrNoPlaylist means no playlist has the ID or name.
	ErrNoPlaylist = errkind.New(errkind.ErrNotFound, "no such playlist")
)

// AddItem appends an item showing one instance on the whole screen to the
// named playlist, creating the playlist if it does not exist, and links
// the device to it when deviceID is not 0. An empty name is the device's
// friendly ID, so the device needs to be given then. duration is 0, to show
// the item until a button press, or at least MinDuration.
func AddItem(ctx context.Context, client *db.Client, name string, deviceID, instanceID int64, duration time.Duration) (db.PlaylistItem, string, error) {
	if err := validDuration(duration); err != nil {
		return db.PlaylistItem{}, "", err
	}
	if name == "" && deviceID == 0 {
		return db.PlaylistItem{}, "", fmt.Errorf("%w: give a playlist name or a device", errkind.ErrInvalid)
	}
	var item db.PlaylistItem
	err := client.InTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetInstance(ctx, instanceID); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no such instance %d", errkind.ErrNotFound, instanceID)
		} else if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		if name == "" {
			dev, err := q.GetDevice(ctx, deviceID)
			if err != nil {
				return fmt.Errorf("get device: %w", err)
			}
			name = dev.FriendlyID
		}
		pl, err := q.GetPlaylistByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			pl, err = q.CreatePlaylist(ctx, name)
		}
		if err != nil {
			return fmt.Errorf("get playlist: %w", err)
		}
		if deviceID != 0 {
			if err := q.LinkPlaylistDevice(ctx, db.LinkPlaylistDeviceParams{DeviceID: deviceID, PlaylistID: pl.ID}); err != nil {
				return fmt.Errorf("link device: %w", err)
			}
		}
		pos, err := q.NextItemPosition(ctx, pl.ID)
		if err != nil {
			return fmt.Errorf("next position: %w", err)
		}
		item, err = q.CreatePlaylistItem(ctx, db.CreatePlaylistItemParams{
			PlaylistID: pl.ID,
			Position:   pos,
			Layout:     "full",
			Duration:   int64(duration / time.Second),
		})
		if err != nil {
			return fmt.Errorf("add item: %w", err)
		}
		if err := q.CreatePlaylistSlot(ctx, db.CreatePlaylistSlotParams{ItemID: item.ID, Slot: 0, InstanceID: instanceID}); err != nil {
			return fmt.Errorf("add item: %w", err)
		}
		return nil
	})
	return item, name, err
}

// validDuration refuses an item duration that is neither 0, to show until a
// button press, nor at least MinDuration.
func validDuration(d time.Duration) error {
	if d != 0 && d < MinDuration {
		return fmt.Errorf("%w: duration %v is under %v", errkind.ErrInvalid, d, MinDuration)
	}
	return nil
}

// renumber gives the items of a playlist the positions 0 to n-1 in the order
// of ids. A position is unique within its playlist and SQLite checks that one
// row at a time, so the old positions are first moved out of the range of
// indexes. It runs in the caller's transaction.
func renumber(ctx context.Context, q *db.Queries, playlistID int64, ids []int64) error {
	if err := q.NegatePlaylistPositions(ctx, playlistID); err != nil {
		return fmt.Errorf("renumber items: %w", err)
	}
	for i, id := range ids {
		if err := q.SetItemPosition(ctx, db.SetItemPositionParams{Position: int64(i), ID: id}); err != nil {
			return fmt.Errorf("renumber items: %w", err)
		}
	}
	return nil
}

// DeleteItem removes a playlist item, with its slots, and renumbers the
// items after it so positions stay 0 to n-1. It returns an error wrapping
// ErrNoItem if there is no such item.
func DeleteItem(ctx context.Context, client *db.Client, id int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		item, err := q.GetPlaylistItem(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoItem, id)
		} else if err != nil {
			return fmt.Errorf("get item: %w", err)
		}
		if _, err := q.DeletePlaylistItem(ctx, id); err != nil {
			return fmt.Errorf("remove item: %w", err)
		}
		ids, err := q.ListPlaylistItemIDs(ctx, item.PlaylistID)
		if err != nil {
			return fmt.Errorf("list items: %w", err)
		}
		return renumber(ctx, q, item.PlaylistID, ids)
	})
}

// MoveItem moves an item to index in its playlist, which shifts the items
// between its old and new place by one. It returns an error wrapping ErrNoItem
// if there is no such item, and one wrapping errkind.ErrInvalid if index is
// not in 0 to n-1 for the n items of its playlist.
func MoveItem(ctx context.Context, client *db.Client, id, index int64) error {
	return UpdateItem(ctx, client, id, ItemUpdate{Position: &index})
}

// ItemUpdate is a change to an item. A nil field is left as it is.
type ItemUpdate struct {
	// Position is the item's new index in its playlist.
	Position *int64
	// Duration is how long the item shows, 0 until a button press, or at
	// least MinDuration.
	Duration *time.Duration
	Hidden   *bool
	Schedule *ScheduleEdit
}

// UpdateItem applies the update to an item in one transaction. Nothing
// changes if any part of it is refused. It returns an error wrapping ErrNoItem
// if there is no such item, and one wrapping errkind.ErrInvalid if a part of
// the update is not acceptable.
func UpdateItem(ctx context.Context, client *db.Client, id int64, u ItemUpdate) error {
	if u.Duration != nil {
		if err := validDuration(*u.Duration); err != nil {
			return err
		}
	}
	return client.InTx(ctx, func(q *db.Queries) error {
		item, err := q.GetPlaylistItem(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoItem, id)
		} else if err != nil {
			return fmt.Errorf("get item: %w", err)
		}
		sched := Schedule{Days: item.Days, WindowStart: item.WindowStart, WindowEnd: item.WindowEnd, StartsAt: item.StartsAt, EndsAt: item.EndsAt}
		if u.Schedule != nil {
			sched = u.Schedule.Apply(sched)
			if err := sched.Validate(); err != nil {
				return err
			}
		}
		var ids []int64
		if u.Position != nil {
			if ids, err = q.ListPlaylistItemIDs(ctx, item.PlaylistID); err != nil {
				return fmt.Errorf("list items: %w", err)
			}
			if *u.Position < 0 || *u.Position >= int64(len(ids)) {
				return fmt.Errorf("%w: position %d is not between 0 and %d", errkind.ErrInvalid, *u.Position, len(ids)-1)
			}
		}
		params := db.UpdatePlaylistItemParams{
			ID: id, Duration: item.Duration, Hidden: item.Hidden,
			Days: sched.Days, WindowStart: sched.WindowStart, WindowEnd: sched.WindowEnd, StartsAt: sched.StartsAt, EndsAt: sched.EndsAt,
		}
		if u.Duration != nil {
			params.Duration = int64(*u.Duration / time.Second)
		}
		if u.Hidden != nil {
			params.Hidden = 0
			if *u.Hidden {
				params.Hidden = 1
			}
		}
		if err := q.UpdatePlaylistItem(ctx, params); err != nil {
			return fmt.Errorf("update item: %w", err)
		}
		if u.Position == nil {
			return nil
		}
		from := slices.Index(ids, id)
		ids = slices.Delete(ids, from, from+1)
		ids = slices.Insert(ids, int(*u.Position), id)
		return renumber(ctx, q, item.PlaylistID, ids)
	})
}

// SetPlaylistSchedule applies the edit to a playlist's schedule. It returns
// an error wrapping ErrNoPlaylist if there is no such playlist, and one
// wrapping errkind.ErrInvalid if the result is not a valid schedule.
func SetPlaylistSchedule(ctx context.Context, client *db.Client, id int64, edit ScheduleEdit) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		pl, err := q.GetPlaylist(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoPlaylist, id)
		} else if err != nil {
			return fmt.Errorf("get playlist: %w", err)
		}
		s := edit.Apply(Schedule{Days: pl.Days, WindowStart: pl.WindowStart, WindowEnd: pl.WindowEnd, StartsAt: pl.StartsAt, EndsAt: pl.EndsAt})
		if err := s.Validate(); err != nil {
			return err
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

// ListDeviceItems returns the items of every playlist a device is linked
// to, with the name of each item's instance and the schedules. The order is
// the one the playlists are tried in, then position. DeviceLists groups them.
func ListDeviceItems(ctx context.Context, q *db.Queries, deviceID int64) ([]db.ListDeviceItemsRow, error) {
	items, err := q.ListDeviceItems(ctx, deviceID)
	if err != nil {
		return nil, fmt.Errorf("list device items: %w", err)
	}
	return items, nil
}

// DeviceLists groups the rows ListDeviceItems returns into lists for Choose,
// and returns each list's rows beside it, in the same order.
func DeviceLists(rows []db.ListDeviceItemsRow) ([]List, [][]db.ListDeviceItemsRow) {
	return Group(rows, func(r *db.ListDeviceItemsRow) (int64, Schedule, Entry) {
		return r.PlaylistID, Schedule{
			Days: r.PlaylistDays, WindowStart: r.PlaylistWindowStart, WindowEnd: r.PlaylistWindowEnd, StartsAt: r.PlaylistStartsAt, EndsAt: r.PlaylistEndsAt,
		}, Entry{
			ID: r.ID, Duration: time.Duration(r.Duration) * time.Second, Hidden: r.Hidden != 0,
			Schedule: Schedule{Days: r.ItemDays, WindowStart: r.ItemWindowStart, WindowEnd: r.ItemWindowEnd, StartsAt: r.ItemStartsAt, EndsAt: r.ItemEndsAt},
		}
	})
}

// Group builds lists from rows that are already in the order the playlists
// are tried and then position, so a playlist's rows are together. key gives a
// row's playlist ID, its schedule and its item. It returns each list's rows
// beside it, as subslices of rows.
func Group[R any](rows []R, key func(*R) (int64, Schedule, Entry)) ([]List, [][]R) {
	var lists []List
	var groups [][]R
	start := 0
	for i := range rows {
		id, sched, e := key(&rows[i])
		if n := len(lists); n == 0 || lists[n-1].ID != id {
			if n > 0 {
				groups = append(groups, rows[start:i])
			}
			start = i
			lists = append(lists, List{ID: id, Schedule: sched})
		}
		l := &lists[len(lists)-1]
		l.Entries = append(l.Entries, e)
	}
	if len(lists) > 0 {
		groups = append(groups, rows[start:])
	}
	return lists, groups
}

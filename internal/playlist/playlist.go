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
	"github.com/die-net/trmnl-byos-go/internal/device"
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
// marked Skip. command is the device's button command when the wake is a
// press, one of device.ButtonCommands, and empty for a timer wake. A press
// moves the cursor to an item that is shown, which starts now: for next the
// one after the current item, wrapping, for previous the one before it,
// wrapping, for first the first, and for refresh the current item itself,
// whatever its duration, so it starts over and the device picks up a newer
// render. Otherwise each item whose duration has passed gives way to the next
// one that is shown, which starts when it ended, so timing does not drift. A
// cursor on an item that is no longer in items starts over at the first one
// that is shown, whatever the command. A cursor on one that is skipped moves
// to the one that is shown after it, or before it for previous, which starts
// now. It returns -1 when no item is shown.
func Advance(items []Item, cur *Cursor, now time.Time, command string) (int, time.Time) {
	i := -1
	if cur != nil {
		for j := range items {
			if items[j].ID == cur.ItemID {
				i = j
				break
			}
		}
	}
	if i < 0 || command != "" || items[i].Skip {
		// With no cursor on an item, the first shown one is the one after the last.
		from := i
		if i < 0 {
			from = len(items) - 1
		}
		var j int
		switch {
		case i >= 0 && command == device.ButtonRefresh && !items[i].Skip:
			j = i
		case i >= 0 && command == device.ButtonPrevious:
			j = prevShown(items, from)
		case i >= 0 && command == device.ButtonFirst:
			j = nextShown(items, len(items)-1)
		default:
			j = nextShown(items, from)
		}
		if j >= 0 {
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

// prevShown returns the index of the first item before index i, wrapping
// around, that is not skipped, which is i itself when no other is. It returns
// -1 if every item is skipped, and for an empty list.
func prevShown(items []Item, i int) int {
	for k := 1; k <= len(items); k++ {
		if j := ((i-k)%len(items) + len(items)) % len(items); !items[j].Skip {
			return j
		}
	}
	return -1
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
	// ErrNameTaken means another playlist has the name.
	ErrNameTaken = errkind.New(errkind.ErrConflict, "a playlist has that name")
	// ErrNotLinked means the device is not linked to the playlist.
	ErrNotLinked = errkind.New(errkind.ErrNotFound, "device is not linked to the playlist")
	// ErrOrderChanged means a device's playlist order is not what the caller
	// saw, so its change was refused.
	ErrOrderChanged = errkind.New(errkind.ErrConflict, "the device's playlist order has changed")
	// ErrPlaylistChanged means a playlist is not what the caller saw, so
	// its change was refused.
	ErrPlaylistChanged = errkind.New(errkind.ErrConflict, "the playlist has changed")
)

// AddItem appends an item showing one instance on the whole screen to the
// playlist ref names, which must exist, and links the device, named by friendly
// ID or MAC, to it, last in the device's order, when deviceName is not empty
// and it is not linked already. The playlist and device are looked up in the
// same transaction. A missing playlist is an error wrapping ErrNoPlaylist, and
// an unknown device one wrapping device.ErrNoDevice. duration is 0, to show
// the item until a button press, or at least MinDuration.
func AddItem(ctx context.Context, client *db.Client, ref Ref, deviceName string, instanceID int64, duration time.Duration) (db.PlaylistItem, error) {
	return appendItem(ctx, client, ref.resolve, deviceName, instanceID, duration)
}

// AddItemCreating is AddItem for the playlist with the given name, which it
// creates if no playlist has it. The create is in the add's transaction, so
// a refused add, such as for an unknown instance, makes no playlist. It is
// for a first add on a new install, as from the command line. A caller that
// means a playlist it saw, such as a page that may be out of date, uses
// AddItem, so a stale or mistyped name cannot make one. The name is checked
// by CleanName.
func AddItemCreating(ctx context.Context, client *db.Client, name, deviceName string, instanceID int64, duration time.Duration) (db.PlaylistItem, error) {
	name, err := CleanName(name)
	if err != nil {
		return db.PlaylistItem{}, err
	}
	return appendItem(ctx, client, func(ctx context.Context, q *db.Queries) (db.Playlist, error) {
		pl, err := q.GetPlaylistByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			if pl, err = q.CreatePlaylist(ctx, name); err != nil {
				return db.Playlist{}, fmt.Errorf("create playlist: %w", err)
			}
			return pl, nil
		}
		if err != nil {
			return db.Playlist{}, fmt.Errorf("get playlist: %w", err)
		}
		return pl, nil
	}, deviceName, instanceID, duration)
}

// appendItem is AddItem with the playlist found by playlistOf, which runs in
// the add's transaction.
func appendItem(ctx context.Context, client *db.Client, playlistOf func(context.Context, *db.Queries) (db.Playlist, error), deviceName string, instanceID int64, duration time.Duration) (db.PlaylistItem, error) {
	if err := validDuration(duration); err != nil {
		return db.PlaylistItem{}, err
	}
	var item db.PlaylistItem
	err := client.ChangeTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetInstance(ctx, instanceID); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no such instance %d", errkind.ErrNotFound, instanceID)
		} else if err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		pl, err := playlistOf(ctx, q)
		if err != nil {
			return err
		}
		if deviceName != "" {
			dev, err := device.FindDevice(ctx, q, deviceName)
			if err != nil {
				return err
			}
			if err := q.AppendPlaylistDevice(ctx, db.AppendPlaylistDeviceParams{DeviceID: dev.ID, PlaylistID: pl.ID}); err != nil {
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
	return item, err
}

// validDuration refuses an item duration that is neither 0, to show until a
// button press, nor at least MinDuration.
func validDuration(d time.Duration) error {
	if d != 0 && d < MinDuration {
		return fmt.Errorf("%w: duration %v is under %v", errkind.ErrInvalid, d, MinDuration)
	}
	return nil
}

// movedKey is the sort key of a row that moves from index from to index to,
// among rows whose keys are twice their old index. A row moving down is placed
// just after the row now at to, and one moving up just before it, so every
// other row keeps its relative order. A row that stays has its own key.
func movedKey(from, to int64) int64 {
	switch {
	case to > from:
		return 2*to + 1
	case to < from:
		return 2*to - 1
	default:
		return 2 * to
	}
}

// renumber gives the items of a playlist the positions 0 to n-1 in their
// current order, with the item moved, if not 0, placed at index to after
// it was at from. A position is unique within its playlist and SQLite checks
// that one row at a time, so the old positions are first moved out of the
// range of indexes. It takes two statements however many items there are, and
// runs in the caller's transaction.
func renumber(ctx context.Context, q *db.Queries, playlistID, moved, from, to int64) error {
	if err := q.NegatePlaylistPositions(ctx, playlistID); err != nil {
		return fmt.Errorf("renumber items: %w", err)
	}
	err := q.RankPlaylistItems(ctx, db.RankPlaylistItemsParams{PlaylistID: playlistID, MovedID: moved, MovedKey: movedKey(from, to)})
	if err != nil {
		return fmt.Errorf("renumber items: %w", err)
	}
	return nil
}

// DeleteItem removes a playlist item, with its slots, and renumbers the
// items after it so positions stay 0 to n-1. It returns an error wrapping
// ErrNoItem if there is no such item.
func DeleteItem(ctx context.Context, client *db.Client, id int64) error {
	return client.ChangeTx(ctx, func(q *db.Queries) error {
		item, err := q.GetPlaylistItem(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrNoItem, id)
		} else if err != nil {
			return fmt.Errorf("get item: %w", err)
		}
		if _, err := q.DeletePlaylistItem(ctx, id); err != nil {
			return fmt.Errorf("remove item: %w", err)
		}
		return renumber(ctx, q, item.PlaylistID, 0, 0, 0)
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
	return client.ChangeTx(ctx, func(q *db.Queries) error {
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
				return fmt.Errorf("%w: %d is not a place in the playlist, which runs from 0 to %d", errkind.ErrInvalid, *u.Position, len(ids)-1)
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
		return renumber(ctx, q, item.PlaylistID, id, int64(slices.Index(ids, id)), *u.Position)
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

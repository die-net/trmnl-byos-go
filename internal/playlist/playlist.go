// Package playlist edits playlists and moves a device's cursor through its
// playlist: which item it shows and since when.
package playlist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

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
}

// Cursor is the item a device shows and when it started.
type Cursor struct {
	ItemID  int64
	Started time.Time
}

// Advance returns the index of the item to show now and when it started,
// from the device's cursor, which is nil before the device has shown an
// item. A press moves to the next item. Otherwise each item whose duration
// has passed gives way to the next, which starts when it ended, so timing
// does not drift. A cursor on an item no longer in items starts over. It
// returns -1 when items is empty.
func Advance(items []Item, cur *Cursor, now time.Time, pressed bool) (int, time.Time) {
	n := len(items)
	if n == 0 {
		return -1, time.Time{}
	}
	i := -1
	if cur != nil {
		for j := range items {
			if items[j].ID == cur.ItemID {
				i = j
				break
			}
		}
	}
	if i < 0 {
		return 0, now
	}
	if pressed {
		return (i + 1) % n, now
	}
	started := cur.Started
	// After a long sleep, skip whole turns through the playlist at once.
	// A turn has a fixed length only when every item has a duration.
	var turn time.Duration
	for _, it := range items {
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
	for range n + 1 {
		d := items[i].Duration
		if d <= 0 || now.Add(WakeSlack).Before(started.Add(d)) {
			break
		}
		started = started.Add(d)
		i = (i + 1) % n
	}
	return i, started
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

// AddItem appends an item showing one instance on the whole screen to the
// named playlist, creating the playlist if it does not exist, and links
// the device to it when deviceID is not 0. duration is 0, to show the item
// until a button press, or at least MinDuration.
func AddItem(ctx context.Context, client *db.Client, name string, deviceID, instanceID int64, duration time.Duration) (db.PlaylistItem, error) {
	if duration != 0 && duration < MinDuration {
		return db.PlaylistItem{}, fmt.Errorf("duration %v is under %v", duration, MinDuration)
	}
	var item db.PlaylistItem
	err := client.InTx(ctx, func(q *db.Queries) error {
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
	return item, err
}

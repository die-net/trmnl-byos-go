package playlist

import (
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/device"
)

// TestAdvanceCommands checks each button command from a cursor at the first
// item, in the middle and at the last, with items that are not shown stepped
// over, and a cursor that is gone or on a skipped item.
func TestAdvanceCommands(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_800_000_000, 0)
	now := t0.Add(2 * time.Minute)
	mins := func(m int) time.Duration { return time.Duration(m) * time.Minute }
	four := []Item{{ID: 10, Duration: mins(10)}, {ID: 20, Duration: mins(5)}, {ID: 30, Duration: mins(15)}, {ID: 40}}
	// The first and third items are not shown now.
	holes := []Item{{ID: 10, Skip: true}, {ID: 20, Duration: mins(5)}, {ID: 30, Skip: true}, {ID: 40, Duration: mins(5)}, {ID: 50, Duration: mins(5)}}
	for _, tc := range []struct {
		name    string
		items   []Item
		cursor  int64 // The item ID, or 0 for no cursor.
		command string
		want    int
	}{
		{"next from the first", four, 10, device.ButtonNext, 1},
		{"next from the middle", four, 20, device.ButtonNext, 2},
		{"next wraps from the last", four, 40, device.ButtonNext, 0},
		{"previous from the first wraps to the last", four, 10, device.ButtonPrevious, 3},
		{"previous from the middle", four, 30, device.ButtonPrevious, 1},
		{"previous from the last", four, 40, device.ButtonPrevious, 2},
		{"first from the first", four, 10, device.ButtonFirst, 0},
		{"first from the middle", four, 30, device.ButtonFirst, 0},
		{"first from the last", four, 40, device.ButtonFirst, 0},
		{"refresh at the first", four, 10, device.ButtonRefresh, 0},
		{"refresh in the middle", four, 20, device.ButtonRefresh, 1},
		{"refresh at the last", four, 40, device.ButtonRefresh, 3},
		{"next skips hidden items", holes, 20, device.ButtonNext, 3},
		{"previous skips hidden items", holes, 40, device.ButtonPrevious, 1},
		{"previous wraps over hidden items", holes, 20, device.ButtonPrevious, 4},
		{"first skips hidden items", holes, 50, device.ButtonFirst, 1},
		{"refresh stays", holes, 40, device.ButtonRefresh, 3},
		{"no cursor, previous starts at the first shown", holes, 0, device.ButtonPrevious, 1},
		{"no cursor, refresh starts at the first shown", holes, 0, device.ButtonRefresh, 1},
		{"cursor on a skipped item, refresh moves on", holes, 30, device.ButtonRefresh, 3},
		{"cursor on a skipped item, previous", holes, 30, device.ButtonPrevious, 1},
		{"cursor on a skipped item, first", holes, 30, device.ButtonFirst, 1},
		{"cursor on a skipped item, next", holes, 30, device.ButtonNext, 3},
	} {
		var cur *Cursor
		if tc.cursor != 0 {
			cur = &Cursor{ItemID: tc.cursor, Started: t0}
		}
		i, started := Advance(tc.items, cur, now, tc.command)
		if i != tc.want || !started.Equal(now) {
			t.Errorf("%s: got %d at %v, want %d at %v", tc.name, i, started.Sub(t0), tc.want, now.Sub(t0))
		}
	}
	// A refresh restarts an item whose time has run out instead of moving on.
	cur := &Cursor{ItemID: 10, Started: t0}
	late := t0.Add(mins(30))
	if i, started := Advance(four, cur, late, device.ButtonRefresh); i != 0 || !started.Equal(late) {
		t.Errorf("refresh of an item past its end: got %d at %v, want 0 at %v", i, started.Sub(t0), late.Sub(t0))
	}
	if i, _ := Advance(four, cur, late, ""); i != 3 {
		t.Errorf("timer wake past the end: got %d, want 3", i)
	}
	// Every item hidden.
	none := []Item{{ID: 10, Skip: true}, {ID: 20, Skip: true}}
	for _, command := range device.ButtonCommands {
		if i, _ := Advance(none, &Cursor{ItemID: 10, Started: t0}, now, command); i != -1 {
			t.Errorf("%s with every item hidden: got %d, want -1", command, i)
		}
	}
}

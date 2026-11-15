package playlist

import (
	"testing"
	"time"
)

func TestAdvance(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	mins := func(m int) time.Duration { return time.Duration(m) * time.Minute }
	timed := []Item{{ID: 10, Duration: mins(10)}, {ID: 20, Duration: mins(5)}, {ID: 30, Duration: mins(15)}}
	held := []Item{{ID: 10, Duration: mins(10)}, {ID: 20}, {ID: 30, Duration: mins(15)}}
	for _, tc := range []struct {
		name      string
		items     []Item
		cur       *Cursor
		now       time.Time
		pressed   bool
		wantIndex int
		wantStart time.Time
	}{
		{"empty", nil, nil, t0, false, -1, time.Time{}},
		{"first wake", timed, nil, t0, false, 0, t0},
		{"gone item starts over", timed, &Cursor{ItemID: 99, Started: t0}, t0.Add(mins(1)), false, 0, t0.Add(mins(1))},
		{"before the end", timed, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(9)), false, 0, t0},
		{"early within slack", timed, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(10) - 20*time.Second), false, 1, t0.Add(mins(10))},
		{"just past the end", timed, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(10) + time.Second), false, 1, t0.Add(mins(10))},
		{"two items passed", timed, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(16)), false, 2, t0.Add(mins(15))},
		{"wraps", timed, &Cursor{ItemID: 30, Started: t0}, t0.Add(mins(15)), false, 0, t0.Add(mins(15))},
		{"long sleep skips turns", timed, &Cursor{ItemID: 10, Started: t0}, t0.Add(10*mins(30) + mins(12)), false, 1, t0.Add(10*mins(30) + mins(10))},
		{"press moves on", timed, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(1)), true, 1, t0.Add(mins(1))},
		{"press wraps", timed, &Cursor{ItemID: 30, Started: t0}, t0.Add(mins(1)), true, 0, t0.Add(mins(1))},
		{"held item stays", held, &Cursor{ItemID: 20, Started: t0}, t0.Add(48 * time.Hour), false, 1, t0},
		{"stops at a held item", held, &Cursor{ItemID: 10, Started: t0}, t0.Add(48 * time.Hour), false, 1, t0.Add(mins(10))},
		{"press leaves a held item", held, &Cursor{ItemID: 20, Started: t0}, t0.Add(mins(1)), true, 2, t0.Add(mins(1))},
	} {
		i, started := Advance(tc.items, tc.cur, tc.now, tc.pressed)
		if i != tc.wantIndex || !started.Equal(tc.wantStart) {
			t.Errorf("%s: got %d at %v, want %d at %v", tc.name, i, started.Sub(t0), tc.wantIndex, tc.wantStart.Sub(t0))
		}
	}
}

func TestAdvanceSkip(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	mins := func(m int) time.Duration { return time.Duration(m) * time.Minute }
	// The middle item is not shown now.
	mid := []Item{{ID: 10, Duration: mins(10)}, {ID: 20, Duration: mins(5), Skip: true}, {ID: 30, Duration: mins(15)}}
	first := []Item{{ID: 10, Duration: mins(10), Skip: true}, {ID: 20, Duration: mins(5)}, {ID: 30, Duration: mins(15)}}
	last := []Item{{ID: 10, Duration: mins(10)}, {ID: 20, Duration: mins(5)}, {ID: 30, Duration: mins(15), Skip: true}}
	held := []Item{{ID: 10, Duration: mins(10)}, {ID: 20, Skip: true}, {ID: 30}}
	one := []Item{{ID: 10, Duration: mins(10), Skip: true}, {ID: 20, Duration: mins(5)}}
	none := []Item{{ID: 10, Skip: true}, {ID: 20, Skip: true}}
	for _, tc := range []struct {
		name      string
		items     []Item
		cur       *Cursor
		now       time.Time
		pressed   bool
		wantIndex int
		wantStart time.Time
	}{
		{"no cursor starts at the first shown item", first, nil, t0, false, 1, t0},
		{"gone item starts at the first shown item", first, &Cursor{ItemID: 99, Started: t0}, t0.Add(mins(3)), false, 1, t0.Add(mins(3))},
		{"timer steps over a skipped item", mid, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(11)), false, 2, t0.Add(mins(10))},
		{"timer wraps over a skipped last item", last, &Cursor{ItemID: 20, Started: t0}, t0.Add(mins(5)), false, 0, t0.Add(mins(5))},
		{"timer wraps over a skipped first item", first, &Cursor{ItemID: 30, Started: t0}, t0.Add(mins(15)), false, 1, t0.Add(mins(15))},
		{"timer stays before the end", mid, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(9)), false, 0, t0},
		{"press steps over a skipped item", mid, &Cursor{ItemID: 10, Started: t0}, t0.Add(mins(1)), true, 2, t0.Add(mins(1))},
		{"press wraps over a skipped item", mid, &Cursor{ItemID: 30, Started: t0}, t0.Add(mins(1)), true, 0, t0.Add(mins(1))},
		{"press on the only shown item", one, &Cursor{ItemID: 20, Started: t0}, t0.Add(mins(1)), true, 1, t0.Add(mins(1))},
		{"cursor on a skipped item moves on, starting now", mid, &Cursor{ItemID: 20, Started: t0}, t0.Add(mins(2)), false, 2, t0.Add(mins(2))},
		{"cursor on a skipped last item wraps", last, &Cursor{ItemID: 30, Started: t0}, t0.Add(mins(2)), false, 0, t0.Add(mins(2))},
		{"cursor on a skipped item with a press", mid, &Cursor{ItemID: 20, Started: t0}, t0.Add(mins(2)), true, 2, t0.Add(mins(2))},
		{"turns sum only the shown durations", mid, &Cursor{ItemID: 10, Started: t0}, t0.Add(10*mins(25) + mins(12)), false, 2, t0.Add(10*mins(25) + mins(10))},
		{"held item after a skipped one", held, &Cursor{ItemID: 10, Started: t0}, t0.Add(48 * time.Hour), false, 2, t0.Add(mins(10))},
		{"only shown item is the one under the cursor", one, &Cursor{ItemID: 20, Started: t0}, t0.Add(mins(12)), false, 1, t0.Add(mins(10))},
		{"every item skipped", none, &Cursor{ItemID: 10, Started: t0}, t0, false, -1, time.Time{}},
		{"every item skipped, no cursor", none, nil, t0, false, -1, time.Time{}},
		{"every item skipped, press", none, &Cursor{ItemID: 10, Started: t0}, t0, true, -1, time.Time{}},
	} {
		i, started := Advance(tc.items, tc.cur, tc.now, tc.pressed)
		if i != tc.wantIndex || !started.Equal(tc.wantStart) {
			t.Errorf("%s: got %d at %v, want %d at %v", tc.name, i, started.Sub(t0), tc.wantIndex, tc.wantStart.Sub(t0))
		}
	}
}

func TestPressed(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"button", true},
		{"EXT0", true},
		{"EXT1", true},
		{"timer", false},
		{"powercycle", false},
		{"", false},
	} {
		if got := Pressed(tc.source); got != tc.want {
			t.Errorf("Pressed(%q) = %v", tc.source, got)
		}
	}
}

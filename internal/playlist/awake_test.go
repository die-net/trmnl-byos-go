package playlist

import (
	"testing"
	"time"
)

func TestAwake(t *testing.T) {
	t.Parallel()
	if got := Awake(-1, -1); got != Always {
		t.Errorf("no window = %+v, want Always", got)
	}
	got := Awake(23*60, 6*60+30)
	if want := (Schedule{Days: AllDays, WindowStart: 6*60 + 30, WindowEnd: 23 * 60}); got != want {
		t.Errorf("Awake = %+v, want %+v", got, want)
	}
	// 00:00 is a valid start, so a window that opens at midnight is a window.
	if got := Awake(0, 6*60); got == Always {
		t.Error("a window from 00:00 was taken for none")
	}
}

func TestChooseAwake(t *testing.T) {
	t.Parallel()
	utc := time.UTC
	lists := []List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1)}}}
	night := Awake(23*60, 6*60+30) // sleeps 23:00 to 06:30, across midnight
	day := Awake(13*60, 14*60)     // sleeps 13:00 to 14:00, within a day
	for _, tc := range []struct {
		name  string
		awake Schedule
		at    string
		want  int
	}{
		{"no window", Always, "2026-11-13 03:00", 0},
		{"before the wrapped window", night, "2026-11-13 22:59", 0},
		{"window start is asleep", night, "2026-11-13 23:00", -1},
		{"after midnight is asleep", night, "2026-11-14 03:00", -1},
		{"one minute before the end", night, "2026-11-14 06:29", -1},
		{"window end is awake", night, "2026-11-14 06:30", 0},
		{"noon is awake", night, "2026-11-14 12:00", 0},
		{"a window within a day", day, "2026-11-13 13:30", -1},
		{"after a window within a day", day, "2026-11-13 14:00", 0},
	} {
		if got := Choose(lists, tc.awake, at(t, utc, tc.at)); got != tc.want {
			t.Errorf("%s: Choose = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestNextChangeAwake(t *testing.T) {
	t.Parallel()
	utc := time.UTC
	night := Awake(23*60, 6*60+30)
	// A list that is always shown, so only the sleep window can change what
	// the device shows. And no list at all, where a sleeping device and a
	// waking one still show different screens.
	shown := []List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1)}}}
	for _, tc := range []struct {
		name  string
		lists []List
		from  string
		limit string
		want  string // empty for no change
	}{
		{"awake, then sleep starts", shown, "2026-11-13 12:00", "2026-11-14 12:00", "2026-11-13 23:00"},
		{"asleep before midnight, then wakes", shown, "2026-11-13 23:30", "2026-11-14 12:00", "2026-11-14 06:30"},
		{"asleep after midnight, then wakes", shown, "2026-11-14 02:00", "2026-11-14 12:00", "2026-11-14 06:30"},
		{"no lists, sleep still starts", nil, "2026-11-13 12:00", "2026-11-14 12:00", "2026-11-13 23:00"},
		{"no lists, sleep still ends", nil, "2026-11-14 02:00", "2026-11-14 12:00", "2026-11-14 06:30"},
		{"limit before the sleep starts", shown, "2026-11-13 12:00", "2026-11-13 22:59", ""},
	} {
		want := time.Time{}
		if tc.want != "" {
			want = at(t, utc, tc.want)
		}
		got, ok := NextChange(tc.lists, night, at(t, utc, tc.from), at(t, utc, tc.limit))
		if ok != !want.IsZero() || !got.Equal(want) {
			t.Errorf("%s: NextChange = %v, %v; want %v", tc.name, got, ok, want)
		}
	}
	// No window changes nothing by itself.
	if got, ok := NextChange(shown, Always, at(t, utc, "2026-11-13 12:00"), at(t, utc, "2026-11-20 12:00")); ok {
		t.Errorf("no window: %v", got)
	}
}

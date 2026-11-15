package playlist

import (
	"errors"
	"testing"
	"time"

	// The DST cases need America/Los_Angeles on every machine.
	_ "time/tzdata"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// Nov 13, 2026 is a Friday, so Nov 14 is a Saturday and Nov 15 a Sunday.
func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func loadLA(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func mustDays(t *testing.T, s string) int64 {
	t.Helper()
	d, err := ParseDays(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestScheduleActive(t *testing.T) {
	t.Parallel()
	utc := time.UTC
	la := loadLA(t)
	weekdays := Schedule{Days: mustDays(t, "mon,tue,wed,thu,fri"), WindowStart: 9 * 60, WindowEnd: 17 * 60}
	friNight := Schedule{Days: mustDays(t, "fri"), WindowStart: 22 * 60, WindowEnd: 6 * 60}
	satNight := Schedule{Days: mustDays(t, "sat"), WindowStart: 22 * 60, WindowEnd: 2 * 60}
	monWed := Schedule{Days: mustDays(t, "mon,wed")}
	dates := Schedule{Days: AllDays, StartsAt: at(t, utc, "2026-11-14 00:00").Unix(), EndsAt: at(t, utc, "2026-11-16 00:00").Unix()}
	springGap := Schedule{Days: AllDays, WindowStart: 2*60 + 30, WindowEnd: 4 * 60}
	fallRepeat := Schedule{Days: AllDays, WindowStart: 60 + 30, WindowEnd: 2*60 + 30}
	for _, tc := range []struct {
		name string
		s    Schedule
		t    time.Time
		want bool
	}{
		{"always", Always, at(t, utc, "2026-11-14 03:00"), true},
		{"in a window", weekdays, at(t, utc, "2026-11-13 09:00"), true},
		{"last minute of a window", weekdays, at(t, utc, "2026-11-13 16:59"), true},
		{"window end is exclusive", weekdays, at(t, utc, "2026-11-13 17:00"), false},
		{"before a window", weekdays, at(t, utc, "2026-11-13 08:59"), false},
		{"day not listed", weekdays, at(t, utc, "2026-11-14 10:00"), false},
		{"wrapping window evening", friNight, at(t, utc, "2026-11-13 22:00"), true},
		{"wrapping window before its start", friNight, at(t, utc, "2026-11-13 21:59"), false},
		{"wrapping window past midnight", friNight, at(t, utc, "2026-11-14 05:59"), true},
		{"wrapping window end is exclusive", friNight, at(t, utc, "2026-11-14 06:00"), false},
		{"wrapping window belongs to its start day", friNight, at(t, utc, "2026-11-14 22:30"), false},
		{"wrapping window not on the morning before", friNight, at(t, utc, "2026-11-13 05:00"), false},
		{"saturday into sunday", satNight, at(t, utc, "2026-11-14 23:00"), true},
		{"saturday into sunday, after midnight", satNight, at(t, utc, "2026-11-15 01:59"), true},
		{"saturday into sunday, ended", satNight, at(t, utc, "2026-11-15 02:00"), false},
		{"sunday evening is not in it", satNight, at(t, utc, "2026-11-15 23:00"), false},
		{"all day, listed day", monWed, at(t, utc, "2026-11-16 23:59"), true},
		{"all day, other day", monWed, at(t, utc, "2026-11-17 12:00"), false},
		{"before the start date", dates, at(t, utc, "2026-11-13 23:59"), false},
		{"start date is inclusive", dates, at(t, utc, "2026-11-14 00:00"), true},
		{"last minute before the end", dates, at(t, utc, "2026-11-15 23:59"), true},
		{"end date is exclusive", dates, at(t, utc, "2026-11-16 00:00"), false},
		{"before a spring forward gap", springGap, at(t, la, "2026-03-08 01:59"), false},
		{"first minute after the gap", springGap, at(t, la, "2026-03-08 03:00"), true},
		{"after the gap, before the end", springGap, at(t, la, "2026-03-08 03:59"), true},
		{"fall back, first 01:30", fallRepeat, time.Date(2026, 11, 1, 8, 30, 0, 0, utc).In(la), true},
		{"fall back, second 01:30", fallRepeat, time.Date(2026, 11, 1, 9, 30, 0, 0, utc).In(la), true},
		{"fall back, window end", fallRepeat, time.Date(2026, 11, 1, 10, 30, 0, 0, utc).In(la), false},
		{"fall back, before the window", fallRepeat, time.Date(2026, 11, 1, 8, 29, 0, 0, utc).In(la), false},
	} {
		if got := tc.s.Active(tc.t); got != tc.want {
			t.Errorf("%s: Active(%v) = %v, want %v", tc.name, tc.t, got, tc.want)
		}
	}
}

func TestScheduleValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    Schedule
		ok   bool
	}{
		{"always", Always, true},
		{"no days", Schedule{}, false},
		{"bit above saturday", Schedule{Days: 128}, false},
		{"negative days", Schedule{Days: -1}, false},
		{"wrapping window", Schedule{Days: 1, WindowStart: 1439, WindowEnd: 0}, true},
		{"start out of range", Schedule{Days: 1, WindowStart: 1440}, false},
		{"end out of range", Schedule{Days: 1, WindowEnd: -1}, false},
		{"open ended", Schedule{Days: 1, StartsAt: 5}, true},
		{"negative end", Schedule{Days: 1, EndsAt: -5}, false},
		{"range", Schedule{Days: 1, StartsAt: 5, EndsAt: 6}, true},
		{"empty range", Schedule{Days: 1, StartsAt: 5, EndsAt: 5}, false},
		{"backwards range", Schedule{Days: 1, StartsAt: 6, EndsAt: 5}, false},
	} {
		err := tc.s.Validate()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, errkind.ErrInvalid)) {
			t.Errorf("%s: Validate = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"mon", 2, true},
		{"sun,sat", 65, true},
		{"Mon, Tue", 6, true},
		{"all", AllDays, true},
		{"", 0, false},
		{"mon,", 0, false},
		{"monday", 0, false},
	} {
		got, err := ParseDays(tc.in)
		if got != tc.want || (err == nil) != tc.ok || (err != nil && !errors.Is(err, errkind.ErrInvalid)) {
			t.Errorf("ParseDays(%q) = %d, %v", tc.in, got, err)
		}
	}
	for _, tc := range []struct {
		in         string
		start, end int64
		ok         bool
	}{
		{"09:00-17:00", 540, 1020, true},
		{"22:00-06:00", 1320, 360, true},
		{"00:00-23:59", 0, 1439, true},
		{"all", 0, 0, true},
		{"9:00-17:00", 0, 0, false},
		{"24:00-01:00", 0, 0, false},
		{"09:60-10:00", 0, 0, false},
		{"09:00", 0, 0, false},
		{"", 0, 0, false},
	} {
		start, end, err := ParseWindow(tc.in)
		if start != tc.start || end != tc.end || (err == nil) != tc.ok || (err != nil && !errors.Is(err, errkind.ErrInvalid)) {
			t.Errorf("ParseWindow(%q) = %d, %d, %v", tc.in, start, end, err)
		}
	}
	if got := FormatClock(1020); got != "17:00" {
		t.Errorf("FormatClock = %q", got)
	}
	if got := DayNames(65); len(got) != 2 || got[0] != "sun" || got[1] != "sat" {
		t.Errorf("DayNames = %v", got)
	}
}

func TestScheduleDescribe(t *testing.T) {
	t.Parallel()
	utc := time.UTC
	for _, tc := range []struct {
		s    Schedule
		want string
	}{
		{Always, "always"},
		{Schedule{Days: 6, WindowStart: 540, WindowEnd: 1020}, "mon,tue 09:00-17:00"},
		{Schedule{Days: AllDays, StartsAt: at(t, utc, "2026-11-14 00:00").Unix(), EndsAt: at(t, utc, "2026-11-16 00:00").Unix()}, "from 2026-11-14 00:00 until 2026-11-16 00:00"},
	} {
		if got := tc.s.Describe(utc); got != tc.want {
			t.Errorf("Describe = %q, want %q", got, tc.want)
		}
	}
}

func TestScheduleEdit(t *testing.T) {
	t.Parallel()
	two, five := int64(2), int64(5)
	base := Schedule{Days: AllDays, WindowStart: 1, WindowEnd: 3, StartsAt: 4, EndsAt: 9}
	if got := (ScheduleEdit{}).Apply(base); got != base {
		t.Errorf("empty edit changed it: %+v", got)
	}
	if got := (ScheduleEdit{Days: &two, EndsAt: &five}).Apply(base); got != (Schedule{Days: 2, WindowStart: 1, WindowEnd: 3, StartsAt: 4, EndsAt: 5}) {
		t.Errorf("partial edit: %+v", got)
	}
	if got := Always.Full().Apply(base); got != Always {
		t.Errorf("full edit: %+v", got)
	}
}

// entry is a shown item that is always scheduled, for building lists.
func entry(id int64) Entry { return Entry{ID: id, Schedule: Always} }

func TestChoose(t *testing.T) {
	t.Parallel()
	utc := time.UTC
	now := at(t, utc, "2026-11-13 12:00") // Friday noon
	evening := Schedule{Days: AllDays, WindowStart: 18 * 60, WindowEnd: 20 * 60}
	noon := Schedule{Days: AllDays, WindowStart: 11 * 60, WindowEnd: 13 * 60}
	hidden := Entry{ID: 2, Hidden: true, Schedule: Always}
	offHours := Entry{ID: 3, Schedule: evening}
	for _, tc := range []struct {
		name  string
		lists []List
		want  int
	}{
		{"none", nil, -1},
		{"default list", []List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1)}}}, 0},
		{"first scheduled list wins", []List{
			{ID: 1, Schedule: noon, Entries: []Entry{entry(1)}},
			{ID: 2, Schedule: Always, Entries: []Entry{entry(2)}},
		}, 0},
		{"list off schedule falls to the default", []List{
			{ID: 1, Schedule: evening, Entries: []Entry{entry(1)}},
			{ID: 2, Schedule: Always, Entries: []Entry{entry(2)}},
		}, 1},
		{"list whose items are all hidden falls through", []List{
			{ID: 1, Schedule: noon, Entries: []Entry{hidden}},
			{ID: 2, Schedule: Always, Entries: []Entry{entry(2)}},
		}, 1},
		{"list whose items are all off schedule falls through", []List{
			{ID: 1, Schedule: noon, Entries: []Entry{hidden, offHours}},
			{ID: 2, Schedule: Always, Entries: []Entry{entry(2)}},
		}, 1},
		{"one shown item is enough", []List{
			{ID: 1, Schedule: noon, Entries: []Entry{hidden, offHours, entry(4)}},
			{ID: 2, Schedule: Always, Entries: []Entry{entry(2)}},
		}, 0},
		{"nothing is scheduled", []List{
			{ID: 1, Schedule: evening, Entries: []Entry{entry(1)}},
			{ID: 2, Schedule: noon, Entries: []Entry{hidden}},
		}, -1},
		{"list with no items", []List{{ID: 1, Schedule: Always}}, -1},
	} {
		if got := Choose(tc.lists, now); got != tc.want {
			t.Errorf("%s: Choose = %d, want %d", tc.name, got, tc.want)
		}
	}
	l := List{Entries: []Entry{hidden, offHours, entry(4)}}
	items := l.Items(now)
	if !items[0].Skip || !items[1].Skip || items[2].Skip || items[2].ID != 4 {
		t.Errorf("Items = %+v", items)
	}
}

func TestNextChange(t *testing.T) {
	t.Parallel()
	utc := time.UTC
	la := loadLA(t)
	workHours := Schedule{Days: AllDays, WindowStart: 9 * 60, WindowEnd: 17 * 60}
	evening := Schedule{Days: AllDays, WindowStart: 18 * 60, WindowEnd: 20 * 60}
	weekend := Schedule{Days: mustDays(t, "sat,sun")}
	friNight := Schedule{Days: mustDays(t, "fri"), WindowStart: 22 * 60, WindowEnd: 6 * 60}
	for _, tc := range []struct {
		name  string
		lists []List
		from  time.Time
		limit time.Time
		want  time.Time // zero for no change
	}{
		{
			"window end",
			[]List{{ID: 1, Schedule: workHours, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 10:00"), at(t, utc, "2026-11-14 10:00"), at(t, utc, "2026-11-13 17:00"),
		},
		{
			"higher priority window start",
			[]List{{ID: 1, Schedule: evening, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 12:00"), at(t, utc, "2026-11-14 12:00"), at(t, utc, "2026-11-13 18:00"),
		},
		{
			"window that wraps ends the morning after",
			[]List{{ID: 1, Schedule: friNight, Entries: []Entry{entry(1)}}},
			at(t, utc, "2026-11-14 01:00"), at(t, utc, "2026-11-15 00:00"), at(t, utc, "2026-11-14 06:00"),
		},
		{
			"midnight between two listed days is not a change",
			[]List{{ID: 1, Schedule: weekend, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-14 12:00"), at(t, utc, "2026-11-15 23:00"),
			time.Time{},
		},
		{
			"all-day list ends at the midnight of its last day",
			[]List{{ID: 1, Schedule: weekend, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-14 12:00"), at(t, utc, "2026-11-17 12:00"), at(t, utc, "2026-11-16 00:00"),
		},
		{
			"limit before the change",
			[]List{{ID: 1, Schedule: workHours, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 10:00"), at(t, utc, "2026-11-13 16:59"),
			time.Time{},
		},
		{
			"limit is inclusive",
			[]List{{ID: 1, Schedule: workHours, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 10:00"), at(t, utc, "2026-11-13 17:00"), at(t, utc, "2026-11-13 17:00"),
		},
		{
			"a change at the start time is not after it",
			[]List{{ID: 1, Schedule: workHours, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 17:00"), at(t, utc, "2026-11-13 17:30"),
			time.Time{},
		},
		{
			"item window inside an unchanging list",
			[]List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1), {ID: 2, Schedule: evening}}}},
			at(t, utc, "2026-11-13 12:00"), at(t, utc, "2026-11-14 12:00"), at(t, utc, "2026-11-13 18:00"),
		},
		{
			"hidden item never changes it",
			[]List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1), {ID: 2, Hidden: true, Schedule: evening}}}},
			at(t, utc, "2026-11-13 12:00"), at(t, utc, "2026-11-14 12:00"),
			time.Time{},
		},
		{
			"schedule that does not change which item is shown",
			[]List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: evening, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 19:00"), at(t, utc, "2026-11-14 12:00"),
			time.Time{},
		},
		{
			"starts_at",
			[]List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1), {ID: 2, Schedule: Schedule{Days: AllDays, StartsAt: at(t, utc, "2026-11-13 15:07").Unix()}}}}},
			at(t, utc, "2026-11-13 12:00"), at(t, utc, "2026-11-14 12:00"), at(t, utc, "2026-11-13 15:07"),
		},
		{
			"ends_at",
			[]List{{ID: 1, Schedule: Schedule{Days: AllDays, EndsAt: at(t, utc, "2026-11-13 15:07").Unix()}, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 12:00"), at(t, utc, "2026-11-14 12:00"), at(t, utc, "2026-11-13 15:07"),
		},
		{
			"limit a year out",
			[]List{{ID: 1, Schedule: workHours, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, utc, "2026-11-13 18:00"), at(t, utc, "2027-11-13 18:00"), at(t, utc, "2026-11-14 09:00"),
		},
		{
			"a window start that DST skips opens when the clock jumps",
			[]List{{ID: 1, Schedule: Schedule{Days: AllDays, WindowStart: 2*60 + 30, WindowEnd: 4 * 60}, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, la, "2026-03-08 01:00"), at(t, la, "2026-03-08 12:00"), time.Date(2026, 3, 8, 10, 0, 0, 0, utc),
		},
		{
			"a window on the day DST ends",
			[]List{{ID: 1, Schedule: workHours, Entries: []Entry{entry(1)}}, {ID: 2, Schedule: Always, Entries: []Entry{entry(2)}}},
			at(t, la, "2026-11-01 10:00"), at(t, la, "2026-11-02 10:00"), at(t, la, "2026-11-01 17:00"),
		},
		{
			// 01:00 to 02:00 happens twice on 2026-11-01, first in PDT
			// (08:00 UTC) and then in PST (09:00 UTC).
			"a window end in the hour DST repeats, the second time",
			[]List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1), {ID: 2, Schedule: Schedule{Days: AllDays, WindowEnd: 90}}}}},
			time.Date(2026, 11, 1, 9, 20, 0, 0, utc).In(la), at(t, la, "2026-11-01 06:00"), time.Date(2026, 11, 1, 9, 30, 0, 0, utc),
		},
		{
			"a window end in the hour DST repeats, the first time",
			[]List{{ID: 1, Schedule: Always, Entries: []Entry{entry(1), {ID: 2, Schedule: Schedule{Days: AllDays, WindowEnd: 90}}}}},
			time.Date(2026, 11, 1, 8, 20, 0, 0, utc).In(la), at(t, la, "2026-11-01 06:00"), time.Date(2026, 11, 1, 8, 30, 0, 0, utc),
		},
	} {
		got, ok := NextChange(tc.lists, tc.from, tc.limit)
		if ok != !tc.want.IsZero() || !got.Equal(tc.want) {
			t.Errorf("%s: NextChange = %v, %v; want %v", tc.name, got, ok, tc.want)
		}
	}
	// Equal, not after, so a limit at or before the time gives nothing.
	lists := []List{{ID: 1, Schedule: workHours, Entries: []Entry{entry(1)}}}
	now := at(t, utc, "2026-11-13 10:00")
	if got, ok := NextChange(lists, now, now); ok {
		t.Errorf("limit at the time: %v", got)
	}
}

package playlist

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// AllDays is the Days of a schedule that runs every day.
const AllDays = 127

// minutesPerDay is the number of minutes in a local day, which a DST change
// does not alter: a window is in minutes on the wall clock.
const minutesPerDay = 24 * 60

// dayNames are the names of the weekdays in the API and the CLI, in the
// order of the Days bits: bit 0 is Sunday.
var dayNames = [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Schedule says when a playlist or an item is active. The zero value is not
// valid, because it has no days. Always is the schedule that never
// restricts anything, which is what a new playlist or item has.
type Schedule struct {
	// Days is a bitmask of the weekdays it is active, bit 0 for Sunday to
	// bit 6 for Saturday. AllDays is every day.
	Days int64
	// WindowStart and WindowEnd bound the time of day it is active, in
	// minutes after local midnight, 0 to 1439. They are equal for all day.
	// An end before the start wraps past midnight, and the window belongs
	// to the day it starts on: a Friday 22:00 to 06:00 window covers
	// Friday 22:00 to Saturday 06:00 when Friday is in Days.
	WindowStart, WindowEnd int64
	// StartsAt and EndsAt bound it in time, in Unix seconds, and 0 leaves
	// that side open. It is active from StartsAt up to, not including,
	// EndsAt.
	StartsAt, EndsAt int64
}

// Always is the schedule of a playlist or item with no restrictions.
var Always = Schedule{Days: AllDays}

// Validate returns an error wrapping errkind.ErrInvalid if the schedule is
// not one of the shapes the fields describe.
func (s Schedule) Validate() error {
	switch {
	case s.Days < 1 || s.Days > AllDays:
		return fmt.Errorf("%w: days must name at least one weekday", errkind.ErrInvalid)
	case s.WindowStart < 0 || s.WindowStart >= minutesPerDay || s.WindowEnd < 0 || s.WindowEnd >= minutesPerDay:
		return fmt.Errorf("%w: the window must be between 00:00 and 23:59", errkind.ErrInvalid)
	case s.StartsAt < 0 || s.EndsAt < 0:
		return fmt.Errorf("%w: starts_at and ends_at cannot be negative", errkind.ErrInvalid)
	case s.StartsAt != 0 && s.EndsAt != 0 && s.StartsAt >= s.EndsAt:
		return fmt.Errorf("%w: starts_at must be before ends_at", errkind.ErrInvalid)
	}
	return nil
}

// Active reports whether the schedule is active at t, in t's location, which
// is where its days and window are read.
func (s Schedule) Active(t time.Time) bool {
	if u := t.Unix(); u < s.StartsAt || (s.EndsAt != 0 && u >= s.EndsAt) {
		return false
	}
	day := t.Weekday()
	if s.WindowStart == s.WindowEnd {
		return s.hasDay(day)
	}
	m := int64(t.Hour()*60 + t.Minute())
	switch {
	case s.WindowStart < s.WindowEnd:
		return m >= s.WindowStart && m < s.WindowEnd && s.hasDay(day)
	case m >= s.WindowStart:
		return s.hasDay(day)
	case m < s.WindowEnd:
		// The tail of a window that started yesterday.
		return s.hasDay(day + 6)
	}
	return false
}

// hasDay reports whether Days has the weekday, which is taken modulo 7.
func (s Schedule) hasDay(d time.Weekday) bool {
	return s.Days&(1<<(d%7)) != 0
}

// ScheduleEdit changes some fields of a Schedule. A nil field is left as it is.
type ScheduleEdit struct {
	Days             *int64
	WindowStart      *int64
	WindowEnd        *int64
	StartsAt, EndsAt *int64
}

// Full returns an edit that sets every field to s's, so it replaces a
// schedule.
func (s Schedule) Full() ScheduleEdit {
	return ScheduleEdit{Days: &s.Days, WindowStart: &s.WindowStart, WindowEnd: &s.WindowEnd, StartsAt: &s.StartsAt, EndsAt: &s.EndsAt}
}

// Apply returns s with the edit's fields set. It does not validate.
func (e ScheduleEdit) Apply(s Schedule) Schedule {
	for _, f := range []struct{ to, from *int64 }{
		{&s.Days, e.Days},
		{&s.WindowStart, e.WindowStart},
		{&s.WindowEnd, e.WindowEnd},
		{&s.StartsAt, e.StartsAt},
		{&s.EndsAt, e.EndsAt},
	} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return s
}

// DaysFromNames returns the Days bitmask for lowercase three-letter weekday
// names such as "mon", or "all" for every day. It refuses an empty list and an
// unknown name.
func DaysFromNames(names []string) (int64, error) {
	var mask int64
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "all" {
			mask |= AllDays
			continue
		}
		i := slices.Index(dayNames[:], n)
		if i < 0 {
			return 0, fmt.Errorf("%w: %q is not a weekday; use sun, mon, tue, wed, thu, fri or sat", errkind.ErrInvalid, n)
		}
		mask |= 1 << i
	}
	if mask == 0 {
		return 0, fmt.Errorf("%w: days must name at least one weekday", errkind.ErrInvalid)
	}
	return mask, nil
}

// ParseDays parses a comma-separated list of weekday names, as
// DaysFromNames takes.
func ParseDays(s string) (int64, error) {
	return DaysFromNames(strings.Split(s, ","))
}

// DayNames returns the names of the weekdays in a Days bitmask, in week
// order from Sunday.
func DayNames(days int64) []string {
	names := make([]string, 0, len(dayNames))
	for i, n := range dayNames {
		if days&(1<<i) != 0 {
			names = append(names, n)
		}
	}
	return names
}

// ParseClock parses a time of day as HH:MM, 00:00 to 23:59, into minutes
// after midnight.
func ParseClock(s string) (int64, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, fmt.Errorf("%w: %q is not a time as HH:MM", errkind.ErrInvalid, s)
	}
	return int64(t.Hour()*60 + t.Minute()), nil
}

// FormatClock is the inverse of ParseClock.
func FormatClock(minutes int64) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// ParseWindow parses HH:MM-HH:MM into a window's start and end, or "all"
// for the whole day.
func ParseWindow(s string) (start, end int64, err error) {
	if s == "all" {
		return 0, 0, nil
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("%w: %q is not a window as HH:MM-HH:MM or all", errkind.ErrInvalid, s)
	}
	if start, err = ParseClock(from); err != nil {
		return 0, 0, err
	}
	if end, err = ParseClock(to); err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

// Describe describes the schedule compactly, such as "mon,tue 09:00-17:00", or
// "always". Dates are Unix seconds shown in loc.
func (s Schedule) Describe(loc *time.Location) string {
	var parts []string
	if s.Days != AllDays {
		parts = append(parts, strings.Join(DayNames(s.Days), ","))
	}
	if s.WindowStart != s.WindowEnd {
		parts = append(parts, FormatClock(s.WindowStart)+"-"+FormatClock(s.WindowEnd))
	}
	if s.StartsAt != 0 {
		parts = append(parts, "from "+time.Unix(s.StartsAt, 0).In(loc).Format("2006-01-02 15:04"))
	}
	if s.EndsAt != 0 {
		parts = append(parts, "until "+time.Unix(s.EndsAt, 0).In(loc).Format("2006-01-02 15:04"))
	}
	if len(parts) == 0 {
		return "always"
	}
	return strings.Join(parts, " ")
}

// Entry is an item as the choice sees it: what decides whether it is shown.
type Entry struct {
	ID int64
	// Duration is how long the item shows. 0 shows it until a button press.
	Duration time.Duration
	// Hidden items are never shown.
	Hidden   bool
	Schedule Schedule
}

// Shown reports whether the entry is shown at t: not hidden and scheduled.
func (e *Entry) Shown(t time.Time) bool {
	return !e.Hidden && e.Schedule.Active(t)
}

// List is a playlist as the choice sees it.
type List struct {
	ID       int64
	Schedule Schedule
	Entries  []Entry
}

// Items returns the list's items in order for Advance, with the ones that
// are not shown at t marked to skip.
func (l *List) Items(t time.Time) []Item {
	items := make([]Item, len(l.Entries))
	for i := range l.Entries {
		e := &l.Entries[i]
		items[i] = Item{ID: e.ID, Duration: e.Duration, Skip: !e.Shown(t)}
	}
	return items
}

// shows reports whether the list is scheduled at t and has an item to show.
func (l *List) shows(t time.Time) bool {
	if !l.Schedule.Active(t) {
		return false
	}
	for i := range l.Entries {
		if l.Entries[i].Shown(t) {
			return true
		}
	}
	return false
}

// Choose returns the index of the list a device shows at t, or -1 if none.
// lists are in the order they are tried. The first that is scheduled and has
// an item to show wins, so a list with no schedule at the end is the default,
// and a scheduled list whose items are all hidden or off schedule does not
// blank the device.
func Choose(lists []List, t time.Time) int {
	for i := range lists {
		if lists[i].shows(t) {
			return i
		}
	}
	return -1
}

// choice is what a device shows at a moment: the chosen list and which of its
// items are shown.
type choice struct {
	list  int
	shown []bool
}

func choiceAt(lists []List, t time.Time) choice {
	c := choice{list: Choose(lists, t)}
	if c.list >= 0 {
		for i := range lists[c.list].Entries {
			c.shown = append(c.shown, lists[c.list].Entries[i].Shown(t))
		}
	}
	return c
}

func (c choice) equal(o choice) bool {
	return c.list == o.list && slices.Equal(c.shown, o.shown)
}

// NextChange returns the first time in (t, limit] at which what a device
// shows changes: the list chosen, or which of its items are shown. It reports
// false if there is none. A schedule changes only at the start or end of a
// window on a local day, a local midnight, a starts_at or ends_at, or a
// change of UTC offset, so those are the times tried, in t's location, with
// both instants of a wall clock time that a change back repeats.
func NextChange(lists []List, t, limit time.Time) (time.Time, bool) {
	if !limit.After(t) {
		return time.Time{}, false
	}
	loc := t.Location()
	var cands []time.Time
	add := func(c time.Time) {
		if c.After(t) && !c.After(limit) {
			cands = append(cands, c)
		}
	}
	var scheds []Schedule
	for _, l := range lists {
		scheds = append(scheds, l.Schedule)
		for j := range l.Entries {
			scheds = append(scheds, l.Entries[j].Schedule)
		}
	}
	for _, s := range scheds {
		if s.StartsAt != 0 {
			add(time.Unix(s.StartsAt, 0))
		}
		if s.EndsAt != 0 {
			add(time.Unix(s.EndsAt, 0))
		}
	}
	// A wall clock time that a DST change skips has no instant of its own, so
	// the schedule changes when the offset does. One that a change back
	// repeats has two instants, and time.Date gives only the first, so each
	// change back is kept, with how far it turns the clock back. The change
	// that began t's offset counts too, since t may be in the repeated hour.
	type turnBack struct {
		at   time.Time
		back time.Duration
	}
	var backs []turnBack
	start, _ := t.ZoneBounds()
	for cur := start; !cur.IsZero() && !cur.After(limit); {
		add(cur)
		_, before := cur.Add(-time.Second).Zone()
		_, after := cur.Zone()
		if back := time.Duration(before-after) * time.Second; back > 0 {
			backs = append(backs, turnBack{cur, back})
		}
		_, cur = cur.ZoneBounds()
	}
	addWall := func(c time.Time) {
		add(c)
		for _, b := range backs {
			if !c.Before(b.at.Add(-b.back)) && c.Before(b.at) {
				add(c.Add(b.back))
			}
		}
	}
	// A window that wraps ends on the day after it starts, so start a day
	// early. time.Date handles a day that is not 24 hours long.
	y, m, d := t.Date()
	last := limit.In(loc)
	for day := d - 1; ; day++ {
		midnight := time.Date(y, m, day, 0, 0, 0, 0, loc)
		if midnight.After(last) {
			break
		}
		addWall(midnight)
		for _, s := range scheds {
			if s.WindowStart != s.WindowEnd {
				addWall(time.Date(y, m, day, 0, int(s.WindowStart), 0, 0, loc))
				addWall(time.Date(y, m, day, 0, int(s.WindowEnd), 0, 0, loc))
			}
		}
	}
	slices.SortFunc(cands, time.Time.Compare)
	now := choiceAt(lists, t)
	for _, c := range cands {
		if !choiceAt(lists, c).equal(now) {
			return c, true
		}
	}
	return time.Time{}, false
}

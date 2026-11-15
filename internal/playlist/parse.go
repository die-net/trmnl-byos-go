package playlist

import (
	"fmt"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// MaxDuration is the longest time an item may be given, which also keeps a
// count of seconds from overflowing a time.Duration.
const MaxDuration = 365 * 24 * time.Hour

// DurationFromSeconds converts a count of seconds to an item duration. It
// refuses a negative count or one over MaxDuration, with an error wrapping
// errkind.ErrInvalid. It does not check MinDuration, which UpdateItem and
// AddItem do.
func DurationFromSeconds(sec int64) (time.Duration, error) {
	if limit := int64(MaxDuration / time.Second); sec < 0 || sec > limit {
		return 0, fmt.Errorf("%w: %d seconds is not between 0 and %d", errkind.ErrInvalid, sec, limit)
	}
	return time.Duration(sec) * time.Second, nil
}

// ParseDay returns the Unix time of midnight in loc at the start of the day
// YYYY-MM-DD, which is when a schedule that starts on that day begins. An
// empty string is 0, for no bound.
func ParseDay(s string, loc *time.Location) (int64, error) {
	return parseDayPlus(s, 0, loc)
}

// ParseLastDay returns the Unix time of midnight in loc at the end of the
// day YYYY-MM-DD, which is when a schedule that includes that day as its
// last ends. An empty string is 0, for no bound.
func ParseLastDay(s string, loc *time.Location) (int64, error) {
	return parseDayPlus(s, 1, loc)
}

// parseDayPlus is ParseDay for the day plusDays after the one named.
func parseDayPlus(s string, plusDays int, loc *time.Location) (int64, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseInLocation(time.DateOnly, s, loc)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a date as YYYY-MM-DD", errkind.ErrInvalid, s)
	}
	return d.AddDate(0, 0, plusDays).Unix(), nil
}

// FormatDay is the inverse of ParseDay: the day in loc that a StartsAt falls
// on, as YYYY-MM-DD, or "" for 0.
func FormatDay(startsAt int64, loc *time.Location) string {
	if startsAt == 0 {
		return ""
	}
	return time.Unix(startsAt, 0).In(loc).Format(time.DateOnly)
}

// FormatLastDay is the inverse of ParseLastDay: the last day in loc that an
// EndsAt includes, as YYYY-MM-DD, or "" for 0.
func FormatLastDay(endsAt int64, loc *time.Location) string {
	if endsAt == 0 {
		return ""
	}
	return time.Unix(endsAt-1, 0).In(loc).Format(time.DateOnly)
}

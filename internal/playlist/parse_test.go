package playlist

import (
	"errors"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func TestDurationFromSeconds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		sec     int64
		want    time.Duration
		invalid bool
	}{
		{"zero", 0, 0, false},
		{"minute", 60, time.Minute, false},
		{"max", int64(MaxDuration / time.Second), MaxDuration, false},
		{"negative", -1, 0, true},
		{"over max", int64(MaxDuration/time.Second) + 1, 0, true},
		{"would overflow", 1 << 62, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DurationFromSeconds(tc.sec)
			if tc.invalid != errors.Is(err, errkind.ErrInvalid) || got != tc.want {
				t.Errorf("DurationFromSeconds(%d) = %v, %v; want %v, invalid %v", tc.sec, got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestParseDays(t *testing.T) {
	t.Parallel()
	// A zone that is neither UTC nor the machine's.
	loc := time.FixedZone("TST", 5*3600+1800)
	first := time.Date(2026, 11, 20, 0, 0, 0, 0, loc).Unix()
	for _, tc := range []struct {
		name    string
		in      string
		start   int64
		last    int64
		invalid bool
	}{
		{"empty is unbounded", "", 0, 0, false},
		{"a day", "2026-11-20", first, first + 24*3600, false},
		{"not a date", "20 Nov", 0, 0, true},
		{"no such day", "2026-02-30", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			start, err1 := ParseDay(tc.in, loc)
			last, err2 := ParseLastDay(tc.in, loc)
			if tc.invalid {
				if !errors.Is(err1, errkind.ErrInvalid) || !errors.Is(err2, errkind.ErrInvalid) {
					t.Errorf("errors %v, %v; want invalid input", err1, err2)
				}
				return
			}
			if err1 != nil || err2 != nil || start != tc.start || last != tc.last {
				t.Errorf("ParseDay, ParseLastDay = %d, %d, %v, %v; want %d, %d", start, last, err1, err2, tc.start, tc.last)
			}
			if got := FormatDay(start, loc); got != tc.in {
				t.Errorf("FormatDay = %q, want %q", got, tc.in)
			}
			if got := FormatLastDay(last, loc); got != tc.in {
				t.Errorf("FormatLastDay = %q, want %q", got, tc.in)
			}
		})
	}
}

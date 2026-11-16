package device

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

func TestParseSleep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in         string
		start, end int64
		bad        bool
	}{
		{"23:00-06:30", 23 * 60, 6*60 + 30, false},
		{"00:00-06:00", 0, 6 * 60, false},
		{"13:00-14:00", 13 * 60, 14 * 60, false},
		{"23:59-00:00", 23*60 + 59, 0, false},
		{"", -1, -1, false},
		{"23:00", 0, 0, true},
		{"23:00-", 0, 0, true},
		{"-06:30", 0, 0, true},
		{"23:00-06:30-07:00", 0, 0, true},
		{"6:00-7:00", 0, 0, true},
		{"24:00-06:00", 0, 0, true},
		{"23:60-06:00", 0, 0, true},
		{"23:00-25:00", 0, 0, true},
		{"ab:cd-06:00", 0, 0, true},
		{"none", 0, 0, true},
		{"06:00-06:00", 0, 0, true},
		{" 23:00-06:30", 0, 0, true},
	} {
		start, end, err := ParseSleep(tc.in)
		if tc.bad {
			if !errors.Is(err, errkind.ErrInvalid) {
				t.Errorf("ParseSleep(%q) err = %v, want ErrInvalid", tc.in, err)
			}
			continue
		}
		if err != nil || start != tc.start || end != tc.end {
			t.Errorf("ParseSleep(%q) = %d, %d, %v; want %d, %d", tc.in, start, end, err, tc.start, tc.end)
		}
		if got := FormatSleep(start, end); got != tc.in {
			t.Errorf("FormatSleep(%d, %d) = %q, want %q", start, end, got, tc.in)
		}
	}
}

// TestUpdateSleep checks that Update sets and clears a sleep window, keeps
// it when a change leaves it out, that a refused one changes nothing, even
// the change's valid fields, and that the window is part of SettingsState.
func TestUpdateSleep(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	ctx := t.Context()
	client, err := db.Open(ctx, filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	dev, err := client.Write.CreateDevice(ctx, db.CreateDeviceParams{Mac: testMAC, ApiKeyHash: []byte{1}, FriendlyID: "ABC123", Name: "Old", FwModel: "og", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	stored := func() (sleep, state string) {
		t.Helper()
		d, err := client.Read.GetDevice(ctx, dev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return FormatSleep(d.SleepStart, d.SleepEnd), SettingsState(&d)
	}
	if sleep, _ := stored(); sleep != "" {
		t.Fatalf("a new device sleeps %q", sleep)
	}
	for _, tc := range []struct {
		name   string
		change Change
		want   error
		sleep  string // The device's after the change, accepted or not.
	}{
		{"set", Change{Sleep: s("23:00-06:30")}, nil, "23:00-06:30"},
		{"a change that leaves it out keeps it", Change{Name: s("New")}, nil, "23:00-06:30"},
		{"the same again", Change{Sleep: s("23:00-06:30")}, nil, "23:00-06:30"},
		{"bad format", Change{Name: s("Newer"), Sleep: s("late")}, errkind.ErrInvalid, "23:00-06:30"},
		{"hour out of range", Change{Name: s("Newer"), Sleep: s("24:00-06:00")}, errkind.ErrInvalid, "23:00-06:30"},
		{"start equal to end", Change{Name: s("Newer"), Sleep: s("06:00-06:00")}, errkind.ErrInvalid, "23:00-06:30"},
		{"another window", Change{Sleep: s("00:00-05:00")}, nil, "00:00-05:00"},
		{"clear", Change{Sleep: s("")}, nil, ""},
		{"clear again", Change{Sleep: s("")}, nil, ""},
	} {
		beforeSleep, beforeState := stored()
		beforeDev, err := client.Read.GetDevice(ctx, dev.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Update(ctx, client, "abc123", tc.change)
		sleep, state := stored()
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			}
			if sleep != beforeSleep || state != beforeState {
				t.Errorf("%s: a refused change changed the device: %q, was %q", tc.name, sleep, beforeSleep)
			}
			if d, _ := client.Read.GetDevice(ctx, dev.ID); d.Name != beforeDev.Name {
				t.Errorf("%s: a refused change renamed the device to %q", tc.name, d.Name)
			}
		} else if err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if sleep != tc.sleep {
			t.Errorf("%s: sleep %q, want %q", tc.name, sleep, tc.sleep)
		}
		if tc.name == "another window" && state == beforeState {
			t.Error("the settings state did not change with the window")
		}
	}
}

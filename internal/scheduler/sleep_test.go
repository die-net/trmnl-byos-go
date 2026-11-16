package scheduler

import (
	"slices"
	"testing"
	"time"
	_ "time/tzdata" // So the zones the tests name load on any host.

	"go.yaml.in/yaml/v3"

	"github.com/die-net/trmnl-byos-go/internal/device"
)

// TestWakeInSleepWindow checks that a device whose next wake is inside its
// sleep window wants nothing rendered, read in the device's own zone, and
// wants its item again with the window cleared.
func TestWakeInSleepWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The device wakes next at 09:30 on the server's clock, which is 13:00
	// in Tokyo, and the harness's playlist is open then.
	h.now = time.Date(2027, 1, 15, 9, 15, 0, 0, testZone)
	h.wake(t, 1, 15*time.Minute)

	wanted := func(zone, sleep string) []int64 {
		t.Helper()
		if _, err := device.Update(t.Context(), h.db, "ABC123", device.Change{TimeZone: &zone, Sleep: &sleep}); err != nil {
			t.Fatal(err)
		}
		cands, err := h.s.candidates(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, c := range cands {
			if len(c.wanted) != 0 {
				out = append(out, c.instance)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name        string
		zone, sleep string
		want        []int64
	}{
		{"no window", "", "", []int64{1}},
		{"wake inside the window", "", "09:20-09:45", nil},
		{"wake after the window", "", "09:00-09:30", []int64{1}},
		{"wake at the window's start", "", "09:30-10:00", nil},
		{"a window that wraps midnight", "", "23:00-09:45", nil},
		{"a window that wraps and ends before the wake", "", "23:00-09:00", []int64{1}},
		{"wake inside the window on the device's clock", "Asia/Tokyo", "12:50-13:30", nil},
		{"the same window on the server's clock is not", "", "12:50-13:30", []int64{1}},
		{"cleared", "", "", []int64{1}},
	} {
		if got := wanted(tc.zone, tc.sleep); !slices.Equal(got, tc.want) {
			t.Errorf("%s: instances wanted %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBuildConfigSleep checks that a recipe that reads trmnl.device gets the
// device's sleep window as trmnlp's sleep_mode_enabled, sleep_start_time and
// sleep_end_time, and that a device with no window is not asleep, with no
// times.
func TestBuildConfigSleep(t *testing.T) {
	t.Parallel()
	type deviceConfig struct {
		Variables struct {
			Trmnl struct {
				Device *struct {
					SleepModeEnabled bool `yaml:"sleep_mode_enabled"`
					SleepStartTime   *int `yaml:"sleep_start_time"`
					SleepEndTime     *int `yaml:"sleep_end_time"`
				} `yaml:"device"`
			} `yaml:"trmnl"`
		} `yaml:"variables"`
	}
	for _, tc := range []struct {
		name       string
		sleep      string
		enabled    bool
		start, end int
	}{
		{"a window", "23:00-06:30", true, 23 * 60, 6*60 + 30},
		{"a window from midnight", "00:00-05:00", true, 0, 5 * 60},
		{"none", "", false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			perDevice := h.installRecipe(t, "<p>{{ trmnl.device.friendly_id }}</p>")
			if _, err := addItemTo(t.Context(), h.db, "", "ABC123", perDevice.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := device.Update(t.Context(), h.db, "ABC123", device.Change{Sleep: &tc.sleep}); err != nil {
				t.Fatal(err)
			}
			h.renderDue(t, 2)

			h.mu.Lock()
			defer h.mu.Unlock()
			saw := false
			for _, text := range h.configs {
				var c deviceConfig
				if err := yaml.Unmarshal([]byte(text), &c); err != nil {
					t.Fatalf("%v in:\n%s", err, text)
				}
				d := c.Variables.Trmnl.Device
				if d == nil {
					continue
				}
				saw = true
				if d.SleepModeEnabled != tc.enabled {
					t.Errorf("sleep_mode_enabled = %v, want %v in:\n%s", d.SleepModeEnabled, tc.enabled, text)
				}
				if !tc.enabled {
					if d.SleepStartTime != nil || d.SleepEndTime != nil {
						t.Errorf("times given with no window in:\n%s", text)
					}
				} else if d.SleepStartTime == nil || d.SleepEndTime == nil || *d.SleepStartTime != tc.start || *d.SleepEndTime != tc.end {
					t.Errorf("times %v, %v; want %d, %d in:\n%s", d.SleepStartTime, d.SleepEndTime, tc.start, tc.end, text)
				}
			}
			if !saw {
				t.Errorf("no build had trmnl.device: %v", h.configs)
			}
		})
	}
}

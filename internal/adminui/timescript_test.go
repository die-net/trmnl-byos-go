package adminui

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// scriptResult is what testdata/time_harness.js prints: what the script
// wrote into each time and into the line that names the zones.
type scriptResult struct {
	Times []struct {
		Text  string `json:"text"`
		Title string `json:"title"`
	} `json:"times"`
	Note   string `json:"note"`
	Hidden bool   `json:"hidden"`
}

// scriptPage is what testdata/time_harness.js reads: the page the script
// runs on.
type scriptPage struct {
	// Device is the device's zone, for the line that names the zones.
	Device string `json:"device"`
	// Times is each time on the page, as "KIND=INSTANT".
	Times []string `json:"times"`
	// NoIntl runs the script as in a browser that has no Intl.
	NoIntl bool `json:"noIntl"`
}

// runTimeScript runs static/time.js under node on the page, with the
// browser's zone set to zone. The page goes in on standard input, so the
// command line is fixed.
func runTimeScript(t *testing.T, zone string, page scriptPage) scriptResult {
	t.Helper()
	in, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", "testdata/time_harness.js", "static/time.js")
	cmd.Env = append(os.Environ(), "TZ="+zone)
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var res scriptResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("harness output %q: %v", out, err)
	}
	return res
}

// TestTimeScript runs the page's script and checks that it writes each kind
// of time as the server's layouts do, in the browser's zone, and that the
// zone line is hidden only when the browser and the device keep the same
// time. It needs node, and is skipped without it, so only a machine with
// node checks the script.
func TestTimeScript(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed, so the script is not run")
	}
	// A winter and a summer instant, so both of a zone's offsets are used.
	winter := time.Date(2026, 1, 2, 23, 30, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 4, 5, 6, 0, 0, time.UTC)
	iso := func(at time.Time) string { return at.Format(time.RFC3339) }
	for _, zone := range []string{"America/Los_Angeles", "Asia/Kolkata", "UTC"} {
		t.Run(zone, func(t *testing.T) {
			t.Parallel()
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			res := runTimeScript(t, zone, scriptPage{Device: "Europe/Berlin", Times: []string{
				timeFull + "=" + iso(winter), timeFull + "=" + iso(summer), timeDay + "=" + iso(winter), "title=" + iso(summer),
				timeClock + "=" + iso(winter), timeFull + "=not a time", "unknown=" + iso(winter),
			}})
			if len(res.Times) != 7 {
				t.Fatalf("got %d times: %+v", len(res.Times), res)
			}
			// The day and the time of day are the server's layouts. The
			// zone's name is the browser's own, so only that it is there
			// is checked.
			const dayTime = "2 Jan 2006 15:04"
			for i, at := range []time.Time{winter, summer} {
				want := at.In(loc).Format(dayTime) + " "
				if got := res.Times[i].Text; !strings.HasPrefix(got, want) || len(got) == len(want) {
					t.Errorf("full %d = %q, want %q and a zone name", i, got, want)
				}
			}
			if got, want := res.Times[2].Text, winter.In(loc).Format(dayLayout); got != want {
				t.Errorf("day = %q, want %q", got, want)
			}
			if got, want := res.Times[3], summer.In(loc).Format(dayTime)+" "; got.Text != "server text" || !strings.HasPrefix(got.Title, want) {
				t.Errorf("title kind = %+v, want the text kept and a title of %q", got, want)
			}
			// The instant is not today, so a clock gets its day back.
			if got, want := res.Times[4].Text, winter.In(loc).Format(dayTime)+" "; !strings.HasPrefix(got, want) {
				t.Errorf("clock on another day = %q, want %q", got, want)
			}
			for _, i := range []int{5, 6} {
				if res.Times[i].Text != "server text" {
					t.Errorf("time %d was written as %q, want the server's text kept", i, res.Times[i].Text)
				}
			}
			want := "The times above are in " + zone + ", this browser's time zone. Schedules are read in this device's time zone, Europe/Berlin."
			// A browser may name its zone by another of the zone's names.
			if res.Hidden || (zone != "Asia/Kolkata" && res.Note != want) || !strings.HasSuffix(res.Note, "time zone, Europe/Berlin.") {
				t.Errorf("zone line hidden %v, %q, want shown, %q", res.Hidden, res.Note, want)
			}
		})
	}
	for _, tc := range []struct {
		name, zone, device string
		hidden             bool
	}{
		{"same name", "America/Los_Angeles", "America/Los_Angeles", true},
		{"two names for one zone", "Asia/Kolkata", "Asia/Calcutta", true},
		{"UTC by another name", "UTC", "Etc/UTC", true},
		{"same offset for part of the year", "Europe/London", "UTC", false},
		{"a zone the browser does not know", "UTC", "Nowhere/Gone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if res := runTimeScript(t, tc.zone, scriptPage{Device: tc.device}); res.Hidden != tc.hidden {
				t.Errorf("hidden = %v, want %v: %q", res.Hidden, tc.hidden, res.Note)
			}
		})
	}
	t.Run("no Intl", func(t *testing.T) {
		t.Parallel()
		res := runTimeScript(t, "America/Los_Angeles", scriptPage{Device: "Europe/Berlin", Times: []string{timeFull + "=" + iso(winter)}, NoIntl: true})
		if got, want := res.Times[0].Text, "2 Jan 2026 15:30"; got != want {
			t.Errorf("full = %q, want %q with no zone name", got, want)
		}
		if want := "The times above are in this browser's time zone. Schedules are read in this device's time zone, Europe/Berlin."; res.Hidden || res.Note != want {
			t.Errorf("zone line hidden %v, %q, want shown, %q", res.Hidden, res.Note, want)
		}
	})
}

// TestSameZone checks the server's side of the same decision.
func TestSameZone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"UTC", "UTC", true},
		{"UTC", "Etc/UTC", true},
		{"Asia/Kolkata", "Asia/Calcutta", true},
		{"Europe/London", "UTC", false},
		{"Europe/Berlin", "America/Los_Angeles", false},
	} {
		a, errA := time.LoadLocation(tc.a)
		b, errB := time.LoadLocation(tc.b)
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if got := sameZone(a, b, now); got != tc.want {
			t.Errorf("sameZone(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

package scheduler

import (
	"slices"
	"testing"
	"time"
	_ "time/tzdata" // So the zones the tests name load on any host.

	"go.yaml.in/yaml/v3"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// zoneDevice adds a device that shows the harness's instance on its own
// playlist, with the given time zone and locale settings.
func (h *harness) zoneDevice(t *testing.T, id, zone, locale string) {
	t.Helper()
	_, err := h.db.Write.CreateDevice(t.Context(), db.CreateDeviceParams{
		Mac: "D8:3B:DA:00:00:" + id[len(id)-2:], ApiKeyHash: []byte(id), FriendlyID: id, FwModel: "xiao_epaper_display", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := addItemTo(t.Context(), h.db, "p"+id, id, 1); err != nil {
		t.Fatal(err)
	}
	h.setZoneLocale(t, id, zone, locale)
}

// setZoneLocale sets a device's time zone and locale, either of which may be
// empty to clear it.
func (h *harness) setZoneLocale(t *testing.T, id, zone, locale string) {
	t.Helper()
	if _, err := device.Update(t.Context(), h.db, id, device.Change{TimeZone: &zone, Locale: &locale}); err != nil {
		t.Fatal(err)
	}
}

// renderZones returns the time zone and locale of each render row, sorted,
// as "zone locale".
func (h *harness) renderZones(t *testing.T) []string {
	t.Helper()
	keys, err := h.db.Read.ListRenderKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.TimeZone + " " + k.Locale
	}
	slices.Sort(out)
	return out
}

// TestRenderPerZoneLocale checks that devices with the same effective time
// zone and locale share a render, however they came by it, and that a device
// with another zone or locale gets a render of its own. Each render is built
// once.
func TestRenderPerZoneLocale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	server := testZone.String()
	// The harness's ABC123 sets neither, so it is at the server's zone and
	// the default locale.
	h.zoneDevice(t, "ABC124", "", "")             // Shares ABC123's render.
	h.zoneDevice(t, "ABC125", "Asia/Tokyo", "")   // Tokyo, en.
	h.zoneDevice(t, "ABC126", "Asia/Tokyo", "en") // Shares ABC125's, which is en too.
	h.zoneDevice(t, "ABC127", "", "de")           // The server's zone, de.
	h.zoneDevice(t, "ABC128", "Asia/Tokyo", "de") // Tokyo, de.
	h.zoneDevice(t, "ABC129", "Asia/Tokyo", "de") // Shares ABC128's.
	h.renderDue(t, 4)
	want := []string{"Asia/Tokyo de", "Asia/Tokyo en", server + " de", server + " en"}
	if got := h.renderZones(t); !slices.Equal(got, want) {
		t.Errorf("renders %q, want %q", got, want)
	}
	h.renderDue(t, 4) // Nothing is due.
}

// TestCandidatesOrderZoneLocale checks that targets that differ only in
// their time zone or locale come out of candidates in one fixed order,
// sorted by zone and then locale, on every scan, as the order of the other
// fields does.
func TestCandidatesOrderZoneLocale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.zoneDevice(t, "ABC124", "Asia/Tokyo", "")
	h.zoneDevice(t, "ABC125", "Europe/Berlin", "de")
	h.zoneDevice(t, "ABC126", "Europe/Berlin", "")
	want := []string{"Asia/Tokyo en", "Europe/Berlin de", "Europe/Berlin en", testZone.String() + " en"}
	for range 20 {
		cands, err := h.s.candidates(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(cands))
		for _, c := range cands {
			got = append(got, c.zone+" "+c.locale)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("candidates in the order %q, want %q", got, want)
		}
	}
}

// builtConfig is the part of a build's .trmnlp.yml these tests read.
type builtConfig struct {
	TimeZone  string `yaml:"time_zone"`
	Variables struct {
		Trmnl struct {
			User struct {
				Locale string `yaml:"locale"`
			} `yaml:"user"`
			Device *struct {
				FriendlyID string `yaml:"friendly_id"`
			} `yaml:"device"`
		} `yaml:"trmnl"`
	} `yaml:"variables"`
}

// TestBuildConfigZoneLocale checks the .trmnlp.yml of a build: the device's
// time zone, or the server's, and its locale under variables.trmnl.user, and
// that a recipe that reads trmnl.device gets the device too, with the locale
// kept.
func TestBuildConfigZoneLocale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		zone, locale string
		wantZone     string
		wantLocale   string
	}{
		{"device settings", "Asia/Tokyo", "de", "Asia/Tokyo", "de"},
		{"the server's zone and locale", "", "", testZone.String(), "en"},
		{"locale with a region", "America/Los_Angeles", "pt-BR", "America/Los_Angeles", "pt-BR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			perDevice := h.installRecipe(t, "<p>{{ trmnl.device.friendly_id }}</p>")
			if _, err := addItemTo(t.Context(), h.db, "", "ABC123", perDevice.ID); err != nil {
				t.Fatal(err)
			}
			h.setZoneLocale(t, "ABC123", tc.zone, tc.locale)
			h.renderDue(t, 2)

			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.configs) != 2 {
				t.Fatalf("%d builds, want 2", len(h.configs))
			}
			sawDevice, sawShared := false, false
			for _, text := range h.configs {
				var c builtConfig
				if err := yaml.Unmarshal([]byte(text), &c); err != nil {
					t.Fatalf("%v in:\n%s", err, text)
				}
				if c.TimeZone != tc.wantZone || c.Variables.Trmnl.User.Locale != tc.wantLocale {
					t.Errorf("time_zone %q, locale %q, want %q, %q in:\n%s", c.TimeZone, c.Variables.Trmnl.User.Locale, tc.wantZone, tc.wantLocale, text)
				}
				if c.Variables.Trmnl.Device != nil {
					sawDevice = true
					if c.Variables.Trmnl.Device.FriendlyID != "ABC123" {
						t.Errorf("trmnl.device is %+v in:\n%s", c.Variables.Trmnl.Device, text)
					}
				} else {
					sawShared = true
				}
			}
			if !sawDevice || !sawShared {
				t.Errorf("want one build with trmnl.device and one without, got %v", h.configs)
			}
		})
	}
}

// TestPruneZoneLocale checks that a render at a zone or locale no device
// has is deleted and built again at the one that is in effect, as after a
// change of the device's zone, the server's locale setting or the server's
// zone, and that a render with the empty zone and locale a migration gives
// is pruned too.
func TestPruneZoneLocale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	server := testZone.String()

	// A row as step 10 leaves one: no zone, no locale.
	if _, err := h.db.Write.SaveRenderSuccess(t.Context(), db.SaveRenderSuccessParams{
		InstanceID: 1, Width: 800, Height: 480, Palette: "bw", RenderedAt: h.now.Unix(), AttemptedAt: h.now.Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 1)
	if got, want := h.renderZones(t), []string{server + " en"}; !slices.Equal(got, want) {
		t.Fatalf("renders %q, want %q", got, want)
	}

	for _, tc := range []struct {
		name   string
		apply  func()
		want   string
		builds int32
	}{
		{"the device's zone", func() { h.setZoneLocale(t, "ABC123", "Asia/Tokyo", "") }, "Asia/Tokyo en", 1},
		{"the device's locale", func() { h.setZoneLocale(t, "ABC123", "Asia/Tokyo", "de") }, "Asia/Tokyo de", 1},
		{"the server's locale setting, which the device overrides", func() {
			if err := settings.Set(t.Context(), h.db, settings.KeyLocale, "fr", h.now); err != nil {
				t.Fatal(err)
			}
		}, "Asia/Tokyo de", 0},
		{"the device's locale cleared, so the server's applies", func() { h.setZoneLocale(t, "ABC123", "Asia/Tokyo", "") }, "Asia/Tokyo fr", 1},
		{"the device's zone cleared, so the server's applies", func() { h.setZoneLocale(t, "ABC123", "", "") }, server + " fr", 1},
		{"the server's zone", func() { h.s.cfg.Zone = time.FixedZone("OTH", 3600) }, "OTH fr", 1},
	} {
		before := h.builds.Load()
		tc.apply()
		h.drain(t)
		if got := h.builds.Load() - before; got != tc.builds {
			t.Errorf("%s: %d builds, want %d", tc.name, got, tc.builds)
		}
		if got := h.renderZones(t); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s: renders %q, want only %q", tc.name, got, tc.want)
		}
	}
}

// TestWakeInDeviceZone checks that the scheduler predicts what a device will
// show at its next wake on the device's own clock: the playlist whose window
// is open in the device's zone is the one it wants rendered, not the one
// open in the server's.
func TestWakeInDeviceZone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The harness's playlist p, tried first, is open from 09:00 to 10:00.
	// The fallback q has an instance of its own and is always open.
	other := h.installRecipe(t, "other")
	if _, err := addItemTo(t.Context(), h.db, "q", "ABC123", other.ID); err != nil {
		t.Fatal(err)
	}
	start, end := int64(9*60), int64(10*60)
	if err := playlist.UpdatePlaylist(t.Context(), h.db, playlist.ByID(1), playlist.Update{Schedule: &playlist.ScheduleEdit{WindowStart: &start, WindowEnd: &end}}); err != nil {
		t.Fatal(err)
	}
	// The device wakes next at 09:30 on the server's clock, which is
	// 13:00 in Tokyo.
	h.now = time.Date(2027, 1, 15, 9, 15, 0, 0, testZone)
	h.wake(t, 1, 15*time.Minute)

	wantedBy := func(zone string) []int64 {
		t.Helper()
		h.setZoneLocale(t, "ABC123", zone, "")
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
	if got := wantedBy(""); !slices.Equal(got, []int64{1}) {
		t.Errorf("server's zone, 09:30: instances wanted %v, want [1] from p", got)
	}
	if got := wantedBy("Asia/Tokyo"); !slices.Equal(got, []int64{other.ID}) {
		t.Errorf("Tokyo, 13:00: instances wanted %v, want [%d] from q", got, other.ID)
	}
	// A zone that puts the wake inside the window again.
	if got := wantedBy("Asia/Kolkata"); !slices.Equal(got, []int64{1}) {
		t.Errorf("Kolkata, 09:30: instances wanted %v, want [1] from p", got)
	}
}

package deviceapi

import (
	"image/color"
	"slices"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// TestDisplayProfileOverride checks that /api/display looks up a device's
// render at its overridden profile and palette, and at its reported one
// again once the override is cleared. The device is the OG DIY kit, an
// og_plus whose default palette is bw, and the only render is at bw.
func TestDisplayProfileOverride(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key, first, _ := ts.playlistDevice(t)
	image := ts.saveRender(t, first, "")
	h := displayHeaders(testMAC, key)
	h["Update-Source"] = "timer"
	shown := func() string {
		t.Helper()
		m := ts.getJSON(t, "/api/display", h)
		name, _ := m["filename"].(string)
		return name
	}
	set := func(c device.Change) {
		t.Helper()
		if _, err := device.Update(t.Context(), ts.db, testMAC, c); err != nil {
			t.Fatal(err)
		}
	}
	s := func(v string) *string { return &v }

	if got := shown(); got != image {
		t.Fatalf("no override: shown %q, want the bw render %q", got, image)
	}
	for _, c := range []device.Change{
		{Palette: s("gray-4")},
		{Profile: s("og_bwry"), Palette: s("")},
	} {
		set(c)
		if got := shown(); got == image {
			t.Errorf("with %+v the device still gets the bw render", c)
		}
	}
	set(device.Change{Profile: s(""), Palette: s("")})
	if got := shown(); got != image {
		t.Errorf("override cleared: shown %q, want %q", got, image)
	}

	// A stored override that no longer applies, a model the registry lacks
	// and a palette the reported model does not take, falls back to what
	// the firmware reports, so the device still gets its render.
	dev, err := ts.db.Read.GetDeviceByMAC(t.Context(), testMAC)
	if err != nil {
		t.Fatal(err)
	}
	err = ts.db.Write.UpdateDeviceSettings(t.Context(), db.UpdateDeviceSettingsParams{ProfileOverride: "og_gone", PaletteOverride: "gray-16", ID: dev.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := shown(); got != image {
		t.Errorf("a stale override: shown %q, want the reported model's render %q", got, image)
	}
	// It is warned about once, not at every wake.
	for range 3 {
		shown()
	}
	warned := 0
	ts.srv.overrideWarned.Range(func(any, any) bool { warned++; return true })
	if warned != 1 {
		t.Errorf("%d warnings recorded for one stale override over four wakes, want 1", warned)
	}
}

// TestDisplayOverrideReportedModel checks that a stale override is warned
// about for the model a wake reports, which is the one its profile is
// resolved from, and not the stored fw_model, which is written later in the
// request and kept for an empty Model header. The device's gray-4 palette
// suits the DIY kit, an og_plus, and not og_4clr, an og_bwry.
func TestDisplayOverrideReportedModel(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	gray := "gray-4"
	if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{Palette: &gray}); err != nil {
		t.Fatal(err)
	}
	const (
		bwry    = `palette "gray-4" is not one of og_bwry's palettes`
		unknown = `palette "gray-4" is not one of an unknown model's palettes`
	)
	for _, tc := range []struct {
		name, model string
		// warned is every problem warned about so far, sorted.
		warned []string
	}{
		{"the stored model", "xiao_epaper_display", nil},
		{"reflashed as a model without the palette, warned at once", "og_4clr", []string{bwry}},
		{"back to a model with it, after og_4clr was stored", "xiao_epaper_display", []string{bwry}},
		{"no Model header, which keeps the stored model", "", []string{unknown, bwry}},
	} {
		h := displayHeaders(testMAC, key)
		h["Update-Source"] = "timer"
		h["Model"] = tc.model
		ts.getJSON(t, "/api/display", h)
		var got []string
		ts.srv.overrideWarned.Range(func(k, _ any) bool {
			_, problem, _ := strings.Cut(k.(string), " ")
			got = append(got, problem)
			return true
		})
		slices.Sort(got)
		if !slices.Equal(got, tc.warned) {
			t.Errorf("%s: warned %q, want %q", tc.name, got, tc.warned)
		}
	}
}

// TestBlockedScreenProfileOverride checks that the Blocked screens a
// registered device gets from /api/display and /api/setup are drawn for its
// profile override. The DIY kit is an og_plus, whose screens use 4 grays,
// and og_bwry has none, so its screens are black and white.
func TestBlockedScreenProfileOverride(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	depth := func(m map[string]any) uint8 {
		t.Helper()
		cfg, _ := ts.imageOf(t, m)
		p, ok := cfg.ColorModel.(color.Palette)
		if !ok {
			t.Fatalf("image has no palette: %v", cfg.ColorModel)
		}
		return bitsFor(len(p))
	}
	if _, err := device.Block(t.Context(), ts.db, testMAC, nil, ts.now); err != nil {
		t.Fatal(err)
	}
	if got := depth(ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))); got != 2 {
		t.Fatalf("blocked screen with no override: %d bits, want 2", got)
	}
	profile := "og_bwry"
	if _, err := device.Update(t.Context(), ts.db, testMAC, device.Change{Profile: &profile}); err != nil {
		t.Fatal(err)
	}
	if got := depth(ts.getJSON(t, "/api/display", displayHeaders(testMAC, key))); got != 1 {
		t.Errorf("blocked display screen with og_bwry: %d bits, want 1", got)
	}
	if got := depth(ts.getJSON(t, "/api/setup", setupHeaders(testMAC))); got != 1 {
		t.Errorf("blocked setup screen with og_bwry: %d bits, want 1", got)
	}
}

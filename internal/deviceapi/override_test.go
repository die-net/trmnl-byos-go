package deviceapi

import (
	"image/color"
	"testing"

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

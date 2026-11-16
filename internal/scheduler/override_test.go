package scheduler

import (
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/device"
)

// TestProfileOverrideTarget checks that the scheduler builds for a device's
// overridden profile and palette, and prunes the render it built before the
// override, which no device needs then.
func TestProfileOverrideTarget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.wake(t, 1, 15*time.Minute)
	palettes := func() []string {
		t.Helper()
		keys, err := h.db.Read.ListRenderKeys(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(keys))
		for i, k := range keys {
			out[i] = k.Palette
		}
		return out
	}
	h.renderDue(t, 1)
	if got := palettes(); len(got) != 1 || got[0] != "bw" {
		t.Fatalf("renders before the override: %v, want one at bw", got)
	}
	s := func(v string) *string { return &v }
	for i, tc := range []struct {
		change device.Change
		want   string
	}{
		{device.Change{Palette: s("gray-4")}, "gray-4"},
		{device.Change{Profile: s("og_bwry"), Palette: s("")}, "color-4bwry"},
	} {
		if _, err := device.Update(t.Context(), h.db, "ABC123", tc.change); err != nil {
			t.Fatal(err)
		}
		h.renderDue(t, int32(i+2))
		if got := palettes(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("renders with %+v: %v, want one at %s", tc.change, got, tc.want)
		}
	}
}

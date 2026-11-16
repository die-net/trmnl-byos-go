package scheduler

import (
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/device"
)

// TestBlockedDeviceNotRendered checks that nothing is built for a blocked
// device, that its renders, which no other device shows, are pruned, and
// that an unblock builds them again.
func TestBlockedDeviceNotRendered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	perDevice := h.installRecipe(t, "<p>{{ trmnl.device.friendly_id }}</p>")
	if _, err := addItemTo(t.Context(), h.db, "p", "ABC123", perDevice.ID); err != nil {
		t.Fatal(err)
	}
	h.wake(t, 1, 15*time.Minute)
	h.renderDue(t, 2)

	if _, err := device.Block(t.Context(), h.db, "ABC123", nil, h.now); err != nil {
		t.Fatal(err)
	}
	// Past every render's refresh interval, so each would be due again.
	h.now = h.now.Add(2 * time.Hour)
	h.wake(t, 1, 15*time.Minute)
	h.renderDue(t, 2)
	if keys, err := h.db.Read.ListRenderKeys(t.Context()); err != nil || len(keys) != 0 {
		t.Errorf("renders of a blocked device: %+v, %v", keys, err)
	}

	if _, err := device.Unblock(t.Context(), h.db, "ABC123", nil); err != nil {
		t.Fatal(err)
	}
	h.renderDue(t, 4)
	if keys, err := h.db.Read.ListRenderKeys(t.Context()); err != nil || len(keys) != 2 {
		t.Errorf("renders after unblock: %+v, %v", keys, err)
	}
}

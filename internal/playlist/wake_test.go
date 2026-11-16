package playlist

import (
	"testing"
)

// TestChangesWake checks that each playlist write that changes what devices
// show reports the change through the database client, which wakes the
// scheduler whichever front end made it, and that one that changes nothing
// a render reads, or is refused, does not.
func TestChangesWake(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client, ids, inst := newPlaylist(t, 2)
	woken := 0
	client.OnChange = func() { woken++ }
	dev := newDevice(t, client, 1)
	hide, show, bad := true, false, int64(99)
	for _, tc := range []struct {
		name string
		do   func() error
		want int
		// refused is whether the write is refused.
		refused bool
	}{
		{"create an empty playlist", func() error { _, err := CreatePlaylist(ctx, client, "e"); return err }, 0, false},
		{"add an item", func() error { _, err := AddItem(ctx, client, ByName("p"), "", inst, DefaultDuration); return err }, 1, false},
		{"hide an item", func() error { return UpdateItem(ctx, client, ids[0], ItemUpdate{Hidden: &hide}) }, 1, false},
		{"show an item", func() error { return UpdateItem(ctx, client, ids[0], ItemUpdate{Hidden: &show}) }, 1, false},
		{"move an item", func() error { return MoveItem(ctx, client, ids[0], 1) }, 1, false},
		{"link a device", func() error { return LinkDevice(ctx, client, ByName("p"), dev.FriendlyID) }, 1, false},
		{"link another", func() error { return LinkDevice(ctx, client, ByName("q"), dev.FriendlyID) }, 1, false},
		{"move a link", func() error {
			return MoveLink(ctx, client, ByName("q"), dev.FriendlyID, links(t, client, dev.ID), 0)
		}, 1, false},
		{"a refused move", func() error { return MoveItem(ctx, client, ids[0], bad) }, 0, true},
		{"remove an item", func() error { return DeleteItem(ctx, client, ids[1]) }, 1, false},
		{"unlink a device", func() error { return UnlinkDevice(ctx, client, ByName("q"), dev.FriendlyID) }, 1, false},
	} {
		woken = 0
		err := tc.do()
		if (err != nil) != tc.refused {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if woken != tc.want {
			t.Errorf("%s: %d wake-ups, want %d", tc.name, woken, tc.want)
		}
	}
}

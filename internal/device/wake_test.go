package device

import (
	"testing"
	"time"
)

// TestChangesWake checks that each device write that can change its render
// targets reports the change through the database client, which wakes the
// scheduler whichever front end made it, and that a refused one does not.
func TestChangesWake(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := removeFixture(t)
	woken := 0
	client.OnChange = func() { woken++ }
	zone, badZone := "Asia/Tokyo", "Nowhere/Else"
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name    string
		do      func() error
		want    int
		refused bool
	}{
		{"change a setting", func() error { _, err := Update(ctx, client, "ABC123", Change{TimeZone: &zone}); return err }, 1, false},
		{"a refused change", func() error { _, err := Update(ctx, client, "ABC123", Change{TimeZone: &badZone}); return err }, 0, true},
		{"block", func() error { _, err := Block(ctx, client, "ABC456", nil, now); return err }, 1, false},
		{"unblock", func() error { _, err := Unblock(ctx, client, "ABC456", nil); return err }, 1, false},
		{"delete", func() error { _, err := Delete(ctx, client, "ABC456", nil, now); return err }, 1, false},
	} {
		woken = 0
		if err := tc.do(); (err != nil) != tc.refused {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if woken != tc.want {
			t.Errorf("%s: %d wake-ups, want %d", tc.name, woken, tc.want)
		}
	}
}

package settings

import (
	"testing"
	"time"
)

// TestChangesWake checks that a stored or reset setting, which can change
// the render targets, as the locale does, reports the change through the
// database client, and that a refused one does not.
func TestChangesWake(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := openTest(t)
	woken := 0
	client.OnChange = func() { woken++ }
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name    string
		do      func() error
		want    int
		refused bool
	}{
		{"set", func() error { return Set(ctx, client, KeyLocale, "de", now) }, 1, false},
		{"a refused set", func() error { return Set(ctx, client, KeyLocale, "xx-nope", now) }, 0, true},
		{"reset", func() error { return Reset(ctx, client, KeyLocale) }, 1, false},
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

package deviceapi

import (
	"testing"
)

// TestDisplayWakes checks that a device's first wake, and a wake that
// reports another size or model, which give it render targets it did not
// have, wake the scheduler, and that a wake like the last does not.
func TestDisplayWakes(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	woken := 0
	ts.db.OnChange = func() { woken++ }
	for _, tc := range []struct {
		name   string
		change map[string]string
		want   int
	}{
		{"first wake", nil, 1},
		{"the same again", nil, 0},
		{"another width", map[string]string{"Width": "1872"}, 1},
		{"that width again", map[string]string{"Width": "1872"}, 0},
		{"another model", map[string]string{"Width": "1872", "Model": "og_png"}, 1},
	} {
		woken = 0
		h := displayHeaders(testMAC, key)
		for k, v := range tc.change {
			h[k] = v
		}
		ts.getJSON(t, "/api/display", h)
		if woken != tc.want {
			t.Errorf("%s: %d wake-ups, want %d", tc.name, woken, tc.want)
		}
	}
}

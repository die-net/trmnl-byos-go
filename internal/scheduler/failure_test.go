package scheduler

import (
	"context"
	"os"
	"testing"
	"time"
)

// renderedSettingsAt returns instance 1's render's rendered_settings_at.
func (h *harness) renderedSettingsAt(t *testing.T) int64 {
	t.Helper()
	var at int64
	if err := h.db.Reader.QueryRowContext(t.Context(), `SELECT rendered_settings_at FROM render WHERE instance_id = 1`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// TestRenderedSettingsAt checks that a good build records the settings its
// images were made from, and that a failed build of newer settings leaves
// them, so the device API can tell its images show settings the instance
// no longer has.
func TestRenderedSettingsAt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	if got := h.renderedSettingsAt(t); got != 0 {
		t.Fatalf("rendered_settings_at %d after the first build, want 0", got)
	}

	h.now = h.now.Add(time.Minute)
	h.setToken(t, "swordfish")
	h.renderDue(t, 2)
	good := h.now.Unix()
	if r := h.render(t); r.SettingsAt != good || h.renderedSettingsAt(t) != good {
		t.Fatalf("settings_at %d, rendered_settings_at %d after a good build; want both %d", r.SettingsAt, h.renderedSettingsAt(t), good)
	}

	if err := os.WriteFile(h.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	h.setToken(t, "trout-fish")
	h.renderDue(t, 3)
	r := h.render(t)
	if r.Error == "" || r.Failures != 1 || r.SettingsAt != h.now.Unix() || h.renderedSettingsAt(t) != good {
		t.Errorf("after a failed build of newer settings: %+v, rendered_settings_at %d; want the failure at %d and %d kept",
			r, h.renderedSettingsAt(t), h.now.Unix(), good)
	}
	counted, countedErr := h.failedAt(t)
	if counted != h.now.Unix() || countedErr != r.Error {
		t.Errorf("latest counted failure %d %q, want %d %q", counted, countedErr, h.now.Unix(), r.Error)
	}

	// A renderer that does not take the build of still newer settings
	// changes nothing: the row and the latest counted failure stay.
	before := h.render(t)
	h.down.Store(true)
	h.now = h.now.Add(time.Minute)
	h.setToken(t, "salmon-fish")
	h.renderDue(t, 4)
	if r := h.render(t); r != before {
		t.Errorf("after an outage: %+v, want it unchanged from %+v", r, before)
	}
	if at, msg := h.failedAt(t); at != counted || msg != countedErr {
		t.Errorf("latest counted failure %d %q after an outage, want %d %q kept", at, msg, counted, countedErr)
	}
}

// failedAt returns instance 1's render's failed_settings_at and
// failed_error.
func (h *harness) failedAt(t *testing.T) (int64, string) {
	t.Helper()
	var at int64
	var msg string
	if err := h.db.Reader.QueryRowContext(t.Context(), `SELECT failed_settings_at, failed_error FROM render WHERE instance_id = 1`).Scan(&at, &msg); err != nil {
		t.Fatal(err)
	}
	return at, msg
}

// TestRunWakes checks that a wake-up makes Run scan at once, rather than
// after checkEvery, so a settings change is built without waiting. Run
// starts with nothing due and is given time to settle into its wait before
// the change, so it is only the wake-up that can make it scan. If Run is
// slower to settle, its first scan finds the change and the test passes
// without the wake-up, which can hide a regression but never fail falsely.
func TestRunWakes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	h.s.cfg.Wake = NewWaker()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		h.s.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	time.Sleep(300 * time.Millisecond)
	if n := h.builds.Load(); n != 1 {
		t.Fatalf("%d builds before the change, want 1", n)
	}
	h.setToken(t, "swordfish")
	h.s.cfg.Wake.Wake()
	h.s.cfg.Wake.Wake() // A second wake-up before the scan does not block.
	// Run would otherwise wait checkEvery, far longer than this.
	deadline := time.Now().Add(checkEvery / 3)
	for h.builds.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d builds %v after the wake-up, want 2", h.builds.Load(), checkEvery/3)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNilWaker checks that waking a nil Waker, which a server with no
// scheduler has, does nothing and does not block.
func TestNilWaker(t *testing.T) {
	t.Parallel()
	var w Waker
	w.Wake()
}

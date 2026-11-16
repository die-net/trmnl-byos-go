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
}

// TestRunWakes checks that a wake-up makes Run scan at once, rather than
// after checkEvery, so a settings change is built without waiting.
func TestRunWakes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.s.cfg.Wake = NewWaker()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.s.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitBuilds := func(n int32, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for h.builds.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d builds after %v, want %d", h.builds.Load(), within, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitBuilds(1, 10*time.Second)
	// Run now waits checkEvery, which is far longer than the wait below.
	h.setToken(t, "swordfish")
	h.s.cfg.Wake.Wake()
	h.s.cfg.Wake.Wake() // A second wake-up before the scan does not block.
	waitBuilds(2, checkEvery/3)
}

// TestNilWaker checks that waking a nil Waker, which a server with no
// scheduler has, does nothing and does not block.
func TestNilWaker(t *testing.T) {
	t.Parallel()
	var w Waker
	w.Wake()
}

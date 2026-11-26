package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

// attempts returns the builds recorded as started and not yet ended.
func (h *harness) attempts(t *testing.T) []db.RenderAttempt {
	t.Helper()
	rows, err := h.db.Read.ListRenderAttempts(context.WithoutCancel(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// leaveAttempt records a build of instance 1's OG target at width, as one
// the server stopped during would have left it, started at startedAt.
func (h *harness) leaveAttempt(t *testing.T, width, startedAt int64) {
	t.Helper()
	r := h.render(t)
	if err := h.db.Write.StartRenderAttempt(t.Context(), db.StartRenderAttemptParams{
		InstanceID: r.InstanceID, DeviceID: r.DeviceID, Width: width, Height: r.Height,
		Palette: r.Palette, TimeZone: r.TimeZone, Locale: r.Locale,
		StartedAt: startedAt, DataAt: r.DataAt, SettingsAt: r.SettingsAt,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAttemptDuringBuild checks that a build is recorded as started before
// the renderer gets it, so a server that stops during it leaves the record,
// and that the record goes once the outcome is saved.
func TestAttemptDuringBuild(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	during := -1
	h.duringBuild = func() { during = len(h.attempts(t)) }
	h.renderDue(t, 1)
	if during != 1 {
		t.Errorf("%d builds recorded as started during the build, want 1", during)
	}
	if rows := h.attempts(t); len(rows) != 0 {
		t.Errorf("records left after the build: %+v", rows)
	}
}

// TestAttemptRecovered checks that a build left recorded, as when the
// server stopped during it, becomes a counted failure of its target at the
// next start, with why, and that the target then waits out the backoff
// rather than being built again at once, as it would if nothing were
// recorded.
func TestAttemptRecovered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	// Two refresh intervals later the target is due again, and the server
	// stops during its build.
	h.now = h.now.Add(3 * time.Hour)
	r := h.render(t)
	h.leaveAttempt(t, r.Width, h.now.Unix())

	n, err := h.s.RecoverAttempts(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("RecoverAttempts = %d, %v; want 1", n, err)
	}
	r = h.render(t)
	if r.Error != stoppedDuringBuild || r.Failures != 1 || r.AttemptedAt != h.now.Unix() {
		t.Errorf("render after recovery: error %q, failures %d, attempted %d; want %q, 1, %d", r.Error, r.Failures, r.AttemptedAt, stoppedDuringBuild, h.now.Unix())
	}
	if rows := h.attempts(t); len(rows) != 0 {
		t.Errorf("records left after recovery: %+v", rows)
	}

	// The restart: nothing is built until the backoff has passed.
	h.renderDue(t, 1)
	h.now = h.now.Add(retryDelay(1))
	h.renderDue(t, 2)
	if r := h.render(t); r.Error != "" || r.Failures != 0 {
		t.Errorf("render after the retry: error %q, failures %d; want a good build", r.Error, r.Failures)
	}
}

// TestAttemptTwoRecovered checks that every build left recorded is
// recovered, not only one: two targets that were in flight both become
// failures.
func TestAttemptTwoRecovered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	r := h.render(t)
	h.leaveAttempt(t, r.Width, h.now.Unix())
	h.leaveAttempt(t, r.Width/2, h.now.Unix())

	n, err := h.s.RecoverAttempts(t.Context())
	if err != nil || n != 2 {
		t.Fatalf("RecoverAttempts = %d, %v; want 2", n, err)
	}
	var failed int
	if err := h.db.Reader.QueryRowContext(t.Context(), `SELECT count(*) FROM render WHERE error = ? AND failures = 1`, stoppedDuringBuild).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 2 {
		t.Errorf("%d renders recorded as stopped during their build, want 2", failed)
	}
	if rows := h.attempts(t); len(rows) != 0 {
		t.Errorf("records left after recovery: %+v", rows)
	}
}

// TestAttemptDeletedTarget checks that a build left recorded whose
// instance, or device, was deleted since goes with it, so recovery does not
// fail on a target that is gone, and the server still starts.
func TestAttemptDeletedTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		delete []string
	}{
		{"instance", []string{`DELETE FROM plugin_instance WHERE id = 1`}},
		{"device", []string{`DELETE FROM device`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.renderDue(t, 1)
			r := h.render(t)
			// A target of the device's own, as for a recipe that reads
			// trmnl.device, and one every device shares.
			var device int64
			if err := h.db.Reader.QueryRowContext(t.Context(), `SELECT id FROM device`).Scan(&device); err != nil {
				t.Fatal(err)
			}
			for _, dev := range []int64{0, device} {
				if err := h.db.Write.StartRenderAttempt(t.Context(), db.StartRenderAttemptParams{
					InstanceID: r.InstanceID, DeviceID: dev, Width: r.Width, Height: r.Height,
					Palette: r.Palette, TimeZone: r.TimeZone, Locale: r.Locale, StartedAt: h.now.Unix(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, stmt := range tc.delete {
				if _, err := h.db.Writer.ExecContext(t.Context(), stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			want := 1
			if tc.name == "instance" {
				want = 0
			}
			if n, err := h.s.RecoverAttempts(t.Context()); err != nil || n != want {
				t.Errorf("RecoverAttempts = %d, %v; want %d", n, err, want)
			}
			if rows := h.attempts(t); len(rows) != 0 {
				t.Errorf("records left after recovery: %+v", rows)
			}
		})
	}
}

// TestAttemptCanceled checks that a build canceled partway, as at a
// shutdown, leaves no record, so the next start counts nothing against
// it: the shutdown was not the build's doing.
func TestAttemptCanceled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.duringBuild = cancel
	if _, err := h.s.RenderNext(ctx); err == nil {
		t.Fatal("RenderNext succeeded through a cancel")
	}
	if rows := h.attempts(t); len(rows) != 0 {
		t.Errorf("records left after a canceled build: %+v", rows)
	}
}

// TestAttemptShutdown checks that Run stopped during a build, as at a
// graceful shutdown, returns and leaves no record behind.
func TestAttemptShutdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.duringBuild = cancel
	done := make(chan struct{})
	go func() {
		h.s.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if n := h.builds.Load(); n != 1 {
		t.Errorf("%d builds, want the 1 the shutdown stopped", n)
	}
	if rows := h.attempts(t); len(rows) != 0 {
		t.Errorf("records left after a shutdown: %+v", rows)
	}
}

// TestAttemptUnavailable checks that a build the renderer did not take,
// which records nothing against the target, leaves no record either.
func TestAttemptUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.down.Store(true)
	if _, err := h.s.RenderNext(t.Context()); err == nil {
		t.Fatal("RenderNext succeeded with the renderer down")
	}
	if rows := h.attempts(t); len(rows) != 0 {
		t.Errorf("records left after an outage: %+v", rows)
	}
}

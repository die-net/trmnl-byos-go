package adminapi

import (
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

// TestPushRebuilds checks that a stored data push asks the scheduler to
// look for due renders at once, and that an instance create, which makes
// nothing due, and a refused push do not.
func TestPushRebuilds(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	var woken atomic.Int32
	ta.db.OnChange = func() { woken.Add(1) }
	_, path := ta.newInstancePath(t)
	// A new instance is in no playlist, so it makes nothing due.
	if n := woken.Swap(0); n != 0 {
		t.Fatalf("%d wake-ups after a create, want none", n)
	}
	ta.push(t, path, `{"merge_variables": {"a": 1}}`, 200)
	if n := woken.Load(); n != 1 {
		t.Fatalf("%d wake-ups after a push, want 1", n)
	}
	ta.push(t, path, `{"merge_variables": {"trmnl": 1}}`, 400)
	if n := woken.Load(); n != 1 {
		t.Errorf("%d wake-ups after a refused push, want still 1", n)
	}
}

// TestInstanceRenderStatus checks an instance's last_rendered_at and
// last_error, in the list and alone: zero and empty with no renders, the
// latest good build and no error after one, and the error of the most
// recently attempted failing target, a failed data fetch included, while
// one fails.
func TestInstanceRenderStatus(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	id, _ := ta.newInstancePath(t)
	path := "/api/v1/instances/" + strconv.FormatInt(id, 10)
	check := func(name string, renderedAt int64, lastError string) {
		t.Helper()
		var one instanceJSON
		ta.call(t, "GET", path, "", 200, &one)
		var all []instanceJSON
		ta.call(t, "GET", "/api/v1/instances", "", 200, &all)
		if one.LastRenderedAt != renderedAt || one.LastError != lastError || len(all) != 1 || all[0].LastRenderedAt != renderedAt || all[0].LastError != lastError {
			t.Errorf("%s: alone %d %q, listed %+v; want %d %q", name, one.LastRenderedAt, one.LastError, all, renderedAt, lastError)
		}
	}
	check("no renders", 0, "")

	ctx := t.Context()
	target := func(width int64) db.SaveRenderSuccessParams {
		return db.SaveRenderSuccessParams{InstanceID: id, Width: width, Height: 480, Palette: "bw", TimeZone: "UTC", Locale: "en", RenderedAt: 100, AttemptedAt: 100}
	}
	for _, w := range []int64{800, 400} {
		if _, err := ta.db.Write.SaveRenderSuccess(ctx, target(w)); err != nil {
			t.Fatal(err)
		}
	}
	check("good builds", 100, "")

	for _, f := range []struct {
		width, at int64
		err       string
	}{
		{800, 200, "trmnlp: boom"},
		{400, 300, "data fetch failed: HTTP 401 from https://api.example/x"},
	} {
		err := ta.db.Write.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
			InstanceID: id, Width: f.width, Height: 480, Palette: "bw", TimeZone: "UTC", Locale: "en", AttemptedAt: f.at, Error: f.err, Failures: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check("two failing targets", 100, "data fetch failed: HTTP 401 from https://api.example/x")

	p := target(400)
	p.RenderedAt, p.AttemptedAt = 400, 400
	if _, err := ta.db.Write.SaveRenderSuccess(ctx, p); err != nil {
		t.Fatal(err)
	}
	check("one target fixed", 400, "trmnlp: boom")
}

package scheduler

import (
	"strings"
	"testing"
	"time"
)

// TestFetchFailureKeepsImages checks that a build whose data fetch failed is
// recorded as a counted failure with trmnlp's warning, keeps the last good
// images, and records its settings as the latest counted failure's, so a
// failed fetch of newer settings shows on the device.
func TestFetchFailureKeepsImages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	before := h.screenBody(t, 1, 1)

	h.fetchFail.Store(true)
	h.now = h.now.Add(time.Minute)
	h.setToken(t, "swordfish")
	h.renderDue(t, 2)
	r := h.render(t)
	if r.Failures != 1 || r.Error != "data fetch failed: HTTP 401 from https://api.example — Unauthorized" {
		t.Errorf("after a failed fetch: %+v, want a counted failure with the warning", r)
	}
	if got := h.screenBody(t, 1, 1); got != before {
		t.Errorf("image after a failed fetch %q, want %q kept", got, before)
	}
	if at, msg := h.failedAt(t); at != h.now.Unix() || msg != r.Error {
		t.Errorf("latest counted failure %d %q, want %d %q", at, msg, h.now.Unix(), r.Error)
	}
}

// TestFetchFailureHidesSecretBase checks the stored error and log of a
// failed fetch whose URL starts with a secret field's value, a base URL, as
// a build records them. The line is cut from the raw log, so the URL's path
// and query, with a key the recipe wrote, never reach the error, and the
// log hides the secret.
func TestFetchFailureHidesSecretBase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fetchURL = "{{token}}/api/states?key=ABC123"
	h.setToken(t, "https://ha.example.com")
	h.fetchFail.Store(true)
	h.renderDue(t, 1)
	r, err := h.db.Read.GetRenderStatus(t.Context(), h.render(t).ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != "data fetch failed: HTTP 401 from [redacted] — Unauthorized" {
		t.Errorf("error %q, want the URL cut and its secret base hidden", r.Error)
	}
	if want := "warning: HTTP 401 from [redacted]/api/states?key=ABC123 — Unauthorized"; !strings.Contains(r.Log, want) || strings.Contains(r.Log, "ha.example.com") {
		t.Errorf("log %q, want it to hold %q and not the secret", r.Log, want)
	}
}

// TestUnsupportedStrategyFails checks that a recipe whose data trmnlp would
// only warn about and then draw nothing for, one that uses async polling, is
// recorded as a counted failure saying why, without a build.
func TestUnsupportedStrategyFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.renderDue(t, 1)
	h.now = h.now.Add(time.Minute)
	h.replaceHarnessRecipe(t, "name: Hourly\nrefresh_interval: 60\nstrategy: async_polling\npolling_url: https://api.example/start\ncustom_fields:\n- keyname: token\n  field_type: password\n", "x")
	h.drain(t)
	if n := h.builds.Load(); n != 1 {
		t.Errorf("%d builds, want no new one", n)
	}
	if r := h.render(t); r.Failures != 1 || !strings.Contains(r.Error, "async polling") {
		t.Errorf("after the replace: %+v, want a counted failure about async polling", r)
	}
}

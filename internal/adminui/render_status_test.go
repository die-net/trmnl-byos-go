package adminui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

// renderOutcome is one build of an instance for a device: a good one if
// err is "", and a failed one with that error otherwise, ago seconds before
// the test's now.
type renderOutcome struct {
	device int64
	ago    int64
	err    string
}

// saveOutcomes records the builds of the instance, in order.
func saveOutcomes(t *testing.T, tu *testUI, instanceID int64, outcomes []renderOutcome) {
	t.Helper()
	for _, o := range outcomes {
		at := tu.now.Unix() - o.ago
		var err error
		if o.err == "" {
			_, err = tu.db.Write.SaveRenderSuccess(t.Context(), db.SaveRenderSuccessParams{
				InstanceID: instanceID, DeviceID: o.device, Width: 800, Height: 480, Palette: "bw", RenderedAt: at, AttemptedAt: at,
			})
		} else {
			err = tu.db.Write.SaveRenderFailure(t.Context(), db.SaveRenderFailureParams{
				InstanceID: instanceID, DeviceID: o.device, Width: 800, Height: 480, Palette: "bw", AttemptedAt: at, Error: o.err, Failures: 1,
			})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestRenderStatus checks that the recipes list and an instance's page say
// when the instance was last rendered and why a build of it failed, and
// read well when there is neither: an instance never built, one built, one
// whose only build failed, one whose build failed after a good one, which
// keeps the good one's time, one whose failure was followed by a good build,
// which has no error, and one failing for one device and good for another.
// The error is shown as text.
func TestRenderStatus(t *testing.T) {
	t.Parallel()
	// The test's now is 1,000,000 seconds after the epoch.
	const fiveMinAgo = `Last rendered <time datetime="1970-01-12T13:41:40Z" data-local="full">`
	const never = "Not rendered yet"
	const failed = "A build failed: "
	for _, tc := range []struct {
		name     string
		outcomes []renderOutcome
		// rendered is what the render line starts with, and ago how it ends.
		rendered, ago string
		// err is the error as the page has it, escaped, or "" for none.
		err string
	}{
		{name: "never built", rendered: never},
		{name: "built", outcomes: []renderOutcome{{1, 300, ""}}, rendered: fiveMinAgo, ago: "</time> (5 min ago)"},
		{name: "only build failed", outcomes: []renderOutcome{{1, 60, "data fetch failed: HTTP 401"}}, rendered: never, err: "data fetch failed: HTTP 401"},
		{
			name:     "failed after a good build",
			outcomes: []renderOutcome{{1, 300, ""}, {1, 60, `trmnlp: <b>bad</b> & "worse"`}},
			rendered: fiveMinAgo, ago: "</time> (5 min ago)", err: "trmnlp: &lt;b&gt;bad&lt;/b&gt; &amp; &#34;worse&#34;",
		},
		{name: "good after a failure", outcomes: []renderOutcome{{1, 900, "old failure"}, {1, 300, ""}}, rendered: fiveMinAgo, ago: "</time> (5 min ago)"},
		{
			name:     "one device failing",
			outcomes: []renderOutcome{{1, 300, ""}, {2, 7200, "first failure"}, {2, 120, "renderer said no"}},
			rendered: fiveMinAgo, ago: "</time> (5 min ago)", err: "renderer said no",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, "name: Kitchen\n", false)
			addDevice(t, tu, "AA:BB:CC:00:00:01", "ABC123", "One", 0)
			addDevice(t, tu, "AA:BB:CC:00:00:02", "ABC124", "Two", 0)
			inst := newInstance(t, tu, rec.ID, "Home", "")
			// Another instance's builds are not this one's.
			other := newInstance(t, tu, rec.ID, "Other", "")
			saveOutcomes(t, tu, other.ID, []renderOutcome{{1, 3600, ""}, {2, 30, "the other one's error"}})
			saveOutcomes(t, tu, inst.ID, tc.outcomes)
			c := tu.signIn(t)

			list := tu.get(t, c, "/recipes").Body.String()
			// The list's row for the instance, up to the other instance's.
			row := list[strings.Index(list, `<span class="item-name">Home</span>`):strings.Index(list, `<span class="item-name">Other</span>`)]
			page := tu.get(t, c, instanceHref(inst.ID)).Body.String()
			panel := page[strings.Index(page, `<h2 id="render-title">Rendering</h2>`):strings.Index(page, `<h2 id="edit-title">`)]
			for where, got := range map[string]string{"list": row, "page": panel} {
				if !strings.Contains(got, tc.rendered) || !strings.Contains(got, tc.ago) {
					t.Errorf("%s lacks %q then %q:\n%s", where, tc.rendered, tc.ago, got)
				}
				if (tc.rendered == never) != strings.Contains(got, never) || (tc.rendered == never) == strings.Contains(got, "<time") {
					t.Errorf("%s says both that it was rendered and that it was not, or neither:\n%s", where, got)
				}
				if tc.err == "" && strings.Contains(got, failed) {
					t.Errorf("%s has an error with no failed build:\n%s", where, got)
				}
				if strings.Contains(got, "<b>") || strings.Contains(got, "the other one") || strings.Contains(got, "old failure") || strings.Contains(got, "first failure") {
					t.Errorf("%s has the error as HTML, another instance's, or one a later build replaced:\n%s", where, got)
				}
			}
			if tc.err != "" {
				if want := `<span class="item-error">` + failed + tc.err + "</span>"; !strings.Contains(row, want) {
					t.Errorf("list lacks %q:\n%s", want, row)
				}
				if want := `<p class="error">` + failed + tc.err + "</p>"; !strings.Contains(panel, want) {
					t.Errorf("page lacks %q:\n%s", want, panel)
				}
			}
			// A build's failure is the page's state, not the answer to a post.
			if strings.Contains(page, `role="alert"`) {
				t.Errorf("the page announces the build's error as an alert:\n%s", page)
			}
			wantOneScript(t, page)
		})
	}
}

// TestRenderStatusRefusedSave checks that the page a refused save shows
// still has the render status, above the refusal.
func TestRenderStatusRefusedSave(t *testing.T) {
	t.Parallel()
	tu, rec := newFormUI(t, "name: Kitchen\n", false)
	addDevice(t, tu, "AA:BB:CC:00:00:01", "ABC123", "One", 0)
	inst := newInstance(t, tu, rec.ID, "Home", "")
	saveOutcomes(t, tu, inst.ID, []renderOutcome{{1, 60, "data fetch failed: HTTP 401"}})
	w := tu.post(t, tu.signIn(t), instanceHref(inst.ID), url.Values{"name": {"Away"}, "seen": {"stale"}})
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	wantInOrder(t, w.Body.String(),
		`<h2 id="render-title">Rendering</h2>`,
		`<p class="rel render-when">Not rendered yet</p>`,
		`<p class="error">A build failed: data fetch failed: HTTP 401</p>`,
		`<p class="error" role="alert">This instance changed after the page loaded`,
		`<h2 id="edit-title">Edit instance</h2>`,
	)
}

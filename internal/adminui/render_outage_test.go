package adminui

import (
	"strings"
	"testing"
)

// TestRenderOutage checks that an instance's page says why builds cannot
// start when Config.RenderProblem gives a reason, shows it as text, and says
// nothing when it gives none or there is no such function, as when builds
// go to a renderer run apart.
func TestRenderOutage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		problem func() string
		// want is the line the page must have, or "" for none.
		want string
	}{
		{name: "no function"},
		{name: "empty", problem: func() string { return "" }},
		{
			name:    "a reason",
			problem: func() string { return "no bubblewrap; run trmnl-byos doctor on the server to see why" },
			want:    `<p class="error">Builds cannot start: no bubblewrap; run trmnl-byos doctor on the server to see why</p>`,
		},
		{
			name:    "a reason with markup",
			problem: func() string { return `<b>x</b> & "y"` },
			want:    `<p class="error">Builds cannot start: &lt;b&gt;x&lt;/b&gt; &amp; &#34;y&#34;</p>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu, rec := newFormUI(t, "name: Kitchen\n", false)
			tu.srv.renderProblem = tc.problem
			inst := newInstance(t, tu, rec.ID, "Home", "")
			page := tu.get(t, tu.signIn(t), instanceHref(inst.ID)).Body.String()
			if tc.want == "" {
				if strings.Contains(page, "Builds cannot start") {
					t.Errorf("the page says builds cannot start:\n%s", page)
				}
				return
			}
			if !strings.Contains(page, tc.want) {
				t.Errorf("the page lacks %q:\n%s", tc.want, page)
			}
			if strings.Contains(page, "<b>x</b>") {
				t.Errorf("the reason is shown as HTML:\n%s", page)
			}
		})
	}
}

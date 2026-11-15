package adminui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVanishedRowIsNotFound checks that a page that reads its row in the
// same snapshot as the rest of what it shows, and finds the row gone, as
// after a delete that lands just after the handler's own write, sends the
// not-found page rather than a server error or a page for nothing.
func TestVanishedRowIsNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		render func(tu *testUI, w http.ResponseWriter, r *http.Request)
		want   string
	}{
		{"device by friendly ID", func(tu *testUI, w http.ResponseWriter, r *http.Request) {
			tu.srv.renderDevice(w, r, http.StatusBadRequest, "ZZZ999", nil, "a message", "")
		}, "No such device"},
		{"device by MAC", func(tu *testUI, w http.ResponseWriter, r *http.Request) {
			tu.srv.renderDevice(w, r, http.StatusOK, "aa:bb:cc:dd:ee:ff", nil, "", "")
		}, "No such device"},
		{"instance to remove", func(tu *testUI, w http.ResponseWriter, r *http.Request) {
			tu.srv.renderInstanceDelete(w, r, http.StatusConflict, 999)
		}, "No such instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tu := newTestUI(t, Config{}, true)
			w := httptest.NewRecorder()
			tc.render(tu, w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
			if body := w.Body.String(); w.Code != http.StatusNotFound || !strings.Contains(body, tc.want) {
				t.Errorf("status %d, want 404 with %q: %s", w.Code, tc.want, body)
			}
		})
	}
}

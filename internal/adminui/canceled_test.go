package adminui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/testutil/testlog"
)

// TestRequestEnded checks that a request whose context ended while it read
// the database is not answered with a 500 or logged as an error, in each
// handler that turns a database error into a response. The handlers that sit
// behind requireSession are called directly, since a canceled request stops
// in requireSession first.
func TestRequestEnded(t *testing.T) {
	t.Parallel()
	tu := newTestUI(t, Config{}, true)
	cookie := tu.signIn(t)
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		id      string
		cookie  bool
		handler http.HandlerFunc
	}{
		{"check session", "GET", "/devices", "", true, tu.mux.ServeHTTP},
		{"sign in", "POST", "/login", "", false, tu.mux.ServeHTTP},
		{"sign out", "POST", "/logout", "", true, tu.mux.ServeHTTP},
		{"join window", "POST", "/join", "", false, tu.srv.joinWindowForm},
		{"find device", "GET", "/devices/ABC123", "ABC123", false, tu.srv.deviceDetail},
		{"list playlists", "GET", "/playlists", "", false, tu.srv.playlists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, logged := testlog.Capture(t.Context(), t)
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			body := url.Values{"password": {testPassword}, "action": {"open"}}.Encode()
			req := httptest.NewRequestWithContext(ctx, tc.method, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.cookie {
				req.Header.Set("Cookie", cookieHeader(cookie))
			}
			if tc.id != "" {
				req.SetPathValue("id", tc.id)
			}
			w := httptest.NewRecorder()
			tc.handler(w, req)
			if w.Code == http.StatusInternalServerError || w.Body.Len() != 0 {
				t.Errorf("status %d with body %q, want nothing written", w.Code, w.Body)
			}
			if testlog.HasLevel(logged(), slog.LevelError) || !testlog.HasLevel(logged(), slog.LevelDebug) {
				t.Errorf("logged %v, want only debug", logged())
			}
		})
	}
}

// TestServerErrorLevels checks that serverError answers a failure with a 500
// and an error log, and does so for a failure that is not the request's own
// end even when the request's context has ended.
func TestServerErrorLevels(t *testing.T) {
	t.Parallel()
	failure := errors.New("test failure")
	for _, tc := range []struct {
		name   string
		cancel bool
		err    error
		status int
		level  slog.Level
	}{
		{"failure", false, failure, http.StatusInternalServerError, slog.LevelError},
		{"ended", true, context.Canceled, http.StatusOK, slog.LevelDebug},
		{"other failure after the end", true, failure, http.StatusInternalServerError, slog.LevelError},
		{"cancel error, live context", false, context.Canceled, http.StatusInternalServerError, slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, logged := testlog.Capture(t.Context(), t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.cancel {
				cancel()
			}
			w := httptest.NewRecorder()
			serverError(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), "test", tc.err)
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}
			if !testlog.HasLevel(logged(), tc.level) || (tc.level != slog.LevelError && testlog.HasLevel(logged(), slog.LevelError)) {
				t.Errorf("logged %v, want a %v record and no error unless that is the level", logged(), tc.level)
			}
		})
	}
}

func TestMain(m *testing.M) {
	os.Exit(testlog.Run(m))
}

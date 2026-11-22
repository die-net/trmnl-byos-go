package deviceapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/testutil/testlog"
)

// TestRequestEnded checks that a device request whose context ended while
// the server read the database gets no 500 and no error log, and that the
// answer a device needs is not written to a connection that is gone.
func TestRequestEnded(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key := ts.register(t)
	mux := http.NewServeMux()
	ts.srv.Register(mux)
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		headers map[string]string
	}{
		{"setup", "GET", "/api/setup", setupHeaders(testMAC)},
		{"display", "GET", "/api/display", displayHeaders(testMAC, key)},
		{"log", "POST", "/api/log", map[string]string{"ID": testMAC, "Access-Token": key, "Content-Type": "application/json"}},
		{"firmware download", "GET", "/firmware/" + strings.Repeat("a", 64) + ".bin", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, logged := testlog.Capture(t.Context(), t)
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			req := httptest.NewRequestWithContext(ctx, tc.method, tc.path, bytes.NewReader(logBody(1)))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code == http.StatusInternalServerError || w.Body.Len() != 0 {
				t.Errorf("status %d with body %q, want nothing written", w.Code, w.Body)
			}
			if testlog.HasLevel(logged(), slog.LevelError) || !testlog.HasLevel(logged(), slog.LevelDebug) {
				t.Errorf("logged %v, want only debug", logged())
			}
		})
	}
}

// brokenWriter is a ResponseWriter whose body write always fails, as one
// does after the client hung up.
type brokenWriter struct{ http.ResponseWriter }

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestWriteJSONLevels checks that a body write that fails is logged as an
// error, unless the request's context ended, because the client hung up.
func TestWriteJSONLevels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cancel bool
		want   bool
	}{
		{"live", false, true},
		{"hung up", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, logged := testlog.Capture(t.Context(), t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.cancel {
				cancel()
			}
			w := brokenWriter{httptest.NewRecorder()}
			writeJSON(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), 1)
			if got := testlog.HasLevel(logged(), slog.LevelError); got != tc.want {
				t.Errorf("error logged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMain(m *testing.M) {
	os.Exit(testlog.Run(m))
}

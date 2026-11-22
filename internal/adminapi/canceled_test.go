package adminapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/testutil/testlog"
)

// TestRequestEnded checks that a request whose context ended while it read
// the database gets no 500 and no error log.
func TestRequestEnded(t *testing.T) {
	t.Parallel()
	ta := newTestAPI(t, false)
	ctx, logged := testlog.Capture(t.Context(), t)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	req := httptest.NewRequestWithContext(ctx, "GET", "/api/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+ta.token)
	w := httptest.NewRecorder()
	ta.mux.ServeHTTP(w, req)
	if w.Code == http.StatusInternalServerError || w.Body.Len() != 0 {
		t.Errorf("status %d with body %q, want nothing written", w.Code, w.Body)
	}
	if testlog.HasLevel(logged(), slog.LevelError) || !testlog.HasLevel(logged(), slog.LevelDebug) {
		t.Errorf("logged %v, want only debug", logged())
	}
}

// TestFailLevels checks that fail answers a failure with a 500 and an error
// log unless the error is the end of the request's own context.
func TestFailLevels(t *testing.T) {
	t.Parallel()
	failure := errors.New("disk full")
	for _, tc := range []struct {
		name   string
		cancel bool
		err    error
		status int
		level  slog.Level
	}{
		{"failure", false, failure, http.StatusInternalServerError, slog.LevelError},
		{"ended", true, fmt.Errorf("begin: %w", context.Canceled), http.StatusOK, slog.LevelDebug},
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
			fail(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), tc.err)
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}
			if !testlog.HasLevel(logged(), tc.level) || (tc.level != slog.LevelError && testlog.HasLevel(logged(), slog.LevelError)) {
				t.Errorf("logged %v, want a %v record and no error unless that is the level", logged(), tc.level)
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
			writeJSON(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), http.StatusOK, 1)
			if got := testlog.HasLevel(logged(), slog.LevelError); got != tc.want {
				t.Errorf("error logged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMain(m *testing.M) {
	os.Exit(testlog.Run(m))
}

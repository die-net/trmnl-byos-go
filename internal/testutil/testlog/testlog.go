// Package testlog lets a test read what the code under test logged.
//
// Only _test.go files may import it, which the depguard rule in
// .golangci.yml enforces.
package testlog

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
)

// key is the context key that names the test a log record belongs to.
type key struct{}

// records is the process's default logger in tests. It keeps a record only
// when the context passed to the log call came from Capture, under the name
// of that test, so a parallel test reads only its own records and no test
// replaces the default logger.
type records struct {
	mu   sync.Mutex
	byID map[string][]slog.Record
}

var all = &records{byID: map[string][]slog.Record{}}

func (*records) Enabled(context.Context, slog.Level) bool { return true }

func (h *records) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := ctx.Value(key{}).(string); ok {
		h.mu.Lock()
		h.byID[id] = append(h.byID[id], r)
		h.mu.Unlock()
	}
	return nil
}

func (h *records) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *records) WithGroup(string) slog.Handler { return h }

// Run makes the capture handler the default logger, runs m's tests and
// returns the exit code. A package's TestMain calls it, as
// os.Exit(testlog.Run(m)). Logs written with a context that did not come from
// Capture are dropped.
func Run(m *testing.M) int {
	slog.SetDefault(slog.New(all))
	return m.Run()
}

// Capture returns ctx marked as t's, so that what the code under test logs
// with it is kept, and a function that returns what was kept so far. It needs
// Run in the package's TestMain.
func Capture(ctx context.Context, t *testing.T) (context.Context, func() []slog.Record) {
	t.Helper()
	id := t.Name()
	// A test run again with -count keeps the same name.
	t.Cleanup(func() {
		all.mu.Lock()
		defer all.mu.Unlock()
		delete(all.byID, id)
	})
	return context.WithValue(ctx, key{}, id), func() []slog.Record {
		all.mu.Lock()
		defer all.mu.Unlock()
		return slices.Clone(all.byID[id])
	}
}

// HasLevel reports whether any of recs is at level.
func HasLevel(recs []slog.Record, level slog.Level) bool {
	return slices.ContainsFunc(recs, func(r slog.Record) bool { return r.Level == level })
}

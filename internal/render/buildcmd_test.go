package render

import (
	"io"
	"strings"
	"testing"
)

// failingReader fails the test if it is read.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("stdin was read")
	return 0, io.EOF
}

// TestRunBuildCommandUsage checks that the build command refuses a missing
// flag or an extra argument with status 2 and a usage error, before it
// reads its input or starts anything, so a person who runs it by hand
// starts nothing.
func TestRunBuildCommandUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no flags", nil},
		{"no -trmnlp", []string{"-xvfb", "/x/Xvfb"}},
		{"no -xvfb", []string{"-trmnlp", "/x/trmnlp"}},
		{"empty -trmnlp", []string{"-trmnlp", "", "-xvfb", "/x/Xvfb"}},
		{"extra argument", []string{"-trmnlp", "/x/trmnlp", "-xvfb", "/x/Xvfb", "more"}},
		{"unknown flag", []string{"-trmnlp", "/x/trmnlp", "-xvfb", "/x/Xvfb", "-other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			var started startedRecorder
			code, err := RunBuildCommand(t.Context(), tc.args, failingReader{t}, &out, &started)
			if code != exitUsage {
				t.Errorf("exit status %d, want %d", code, exitUsage)
			}
			if err == nil || !strings.Contains(err.Error(), "usage") {
				t.Errorf("error %v, want a usage error", err)
			}
			if out.Len() != 0 {
				t.Errorf("wrote %q to stdout", out.String())
			}
			// It never started, so the server takes its end as the
			// renderer's.
			if started.Len() != 0 {
				t.Errorf("said it started: %q", started.String())
			}
		})
	}
}

// startedRecorder records what the build command writes on its started
// descriptor.
type startedRecorder struct {
	strings.Builder
	closed bool
}

func (s *startedRecorder) Close() error {
	s.closed = true
	return nil
}

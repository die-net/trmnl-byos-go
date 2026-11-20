package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSuperviseShimFails checks that a shim that stops taking builds stops
// the server with an error, so the process exits and is restarted, and that
// a stop of the process is not an error.
func TestSuperviseShimFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// fail makes the shim fail while the process runs.
		fail    bool
		wantErr bool
	}{
		{"shim fails", true, true},
		{"process stops", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failed := make(chan struct{})
			srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
			done := make(chan error, 1)
			go func() { done <- supervise(ctx, srv, failed, time.Second) }()
			if tc.fail {
				close(failed)
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if (err != nil) != tc.wantErr {
					t.Fatalf("error %v, want error: %v", err, tc.wantErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("supervise did not return")
			}
		})
	}
}

// TestStopCode checks that a stop by SIGTERM or an interrupt exits 0, and
// a stop by any other signal exits with an error, so it is restarted.
func TestStopCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause error
		want  int
	}{
		{"no signal", nil, 0},
		{"SIGTERM", stoppedBy{syscall.SIGTERM}, 0},
		{"interrupt", stoppedBy{os.Interrupt}, 0},
		{"SIGHUP", stoppedBy{syscall.SIGHUP}, 1},
	} {
		if got := stopCode(tc.cause); got != tc.want {
			t.Errorf("%s: stopCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestServeMissingCommands checks that the renderer refuses to start
// without trmnlp or Xvfb, naming what is missing and the PATH it searched,
// rather than answering every build as an outage. It sets PATH, so it does
// not run alongside other tests.
func TestServeMissingCommands(t *testing.T) {
	dir := t.TempDir()
	trmnlp := filepath.Join(dir, "trmnlp")
	if err := os.WriteFile(trmnlp, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: the fake command must be executable.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		name, trmnlp string
		want         []string
	}{
		{"no trmnlp", filepath.Join(dir, "missing"), []string{"find trmnlp", filepath.Join(dir, "missing")}},
		{"no Xvfb", trmnlp, []string{"find Xvfb", dir}},
	} {
		err := serve(t.Context(), "127.0.0.1:0", tc.trmnlp, 1, time.Second)
		for _, w := range tc.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: serve = %v, want it to name %q", tc.name, err, w)
			}
		}
	}
}

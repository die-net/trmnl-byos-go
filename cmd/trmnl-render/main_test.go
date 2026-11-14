package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestSuperviseDisplayExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		// exitFirst makes the fake display exit while the process runs.
		exitFirst bool
		wantErr   bool
	}{
		{"display dies", true, true},
		{"process stops", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			g, gctx := errgroup.WithContext(ctx)
			wait := func() error {
				if tc.exitFirst {
					return errors.New("signal: killed")
				}
				<-gctx.Done()
				return errors.New("signal: killed")
			}
			srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
			done := make(chan error, 1)
			go func() { done <- supervise(gctx, g, srv, wait, time.Second) }()
			if !tc.exitFirst {
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

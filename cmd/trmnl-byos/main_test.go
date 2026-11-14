package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
)

// newServer returns a server for addr that answers every request 404.
func newServer(t *testing.T, addr string) *http.Server {
	t.Helper()
	return &http.Server{Addr: addr, Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
}

func TestListenAndServeStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listenAndServe(ctx, newServer(t, "127.0.0.1:0"), time.Second) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listenAndServe did not return after its context ended")
	}
}

// TestGroupStopsWhenServerCannotListen is the shape of serve: a server that
// cannot listen ends the group, and the loops beside it return before Wait
// does, so the database can be closed after it.
func TestGroupStopsWhenServerCannotListen(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	client, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	images, err := imagestore.New(filepath.Join(t.TempDir(), "images"))
	if err != nil {
		t.Fatal(err)
	}

	g, ctx := errgroup.WithContext(t.Context())
	loopDone := make(chan struct{})
	g.Go(func() error {
		defer close(loopDone)
		expireLoop(ctx, client, images)
		return nil
	})
	g.Go(func() error { return listenAndServe(ctx, newServer(t, busy.Addr().String()), time.Second) })

	waited := make(chan error, 1)
	go func() { waited <- g.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("Wait returned nil, want the listen error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the group kept running after a server failed to listen")
	}
	select {
	case <-loopDone:
	default:
		t.Fatal("Wait returned while the expiry loop was still running")
	}
}

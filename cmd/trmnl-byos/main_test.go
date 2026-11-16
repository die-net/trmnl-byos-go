package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
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

// TestListenOnRefusesEmpty checks that an empty listen address is refused,
// rather than taken as a random port on every interface.
func TestListenOnRefusesEmpty(t *testing.T) {
	t.Parallel()
	if ln, err := listenOn(t.Context(), ""); err == nil {
		_ = ln.Close()
		t.Fatalf("listenOn(\"\") listened on %s, want an error", ln.Addr())
	}
}

func TestListenAndServeStopsWithContext(t *testing.T) {
	t.Parallel()
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

// TestServeOnNamesListener checks that a shutdown that runs out of grace
// names the listener, for a server with no Addr, as startProxy's has.
func TestServeOnNamesListener(t *testing.T) {
	t.Parallel()
	ln, err := listenOn(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveOn(ctx, srv, ln, 10*time.Millisecond) }()

	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/", http.NoBody)
		if err != nil {
			return
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the server")
	}
	cancel()
	err = <-done
	if want := "shutdown " + ln.Addr().String() + ": "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("serveOn: %v, want an error starting %q", err, want)
	}
}

// TestGroupStopsWhenServerCannotListen is the shape of serve: a server that
// cannot listen ends the group, and the loops beside it return before Wait
// does, so the database can be closed after it.
func TestGroupStopsWhenServerCannotListen(t *testing.T) {
	t.Parallel()
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

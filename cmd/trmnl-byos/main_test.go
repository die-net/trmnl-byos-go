package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
)

// dataDir returns a data directory holding an empty database, as one the
// server has started in. Commands other than serve refuse a directory
// without one.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	client, err := db.Open(t.Context(), dbPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

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

// TestCommandsRefuseMissingDatabase checks that every command but serve
// fails on a data directory with no database, says what to do, and creates
// neither the directory nor a file in it. A command run from the wrong
// directory would otherwise change a new empty database the server never
// reads.
func TestCommandsRefuseMissingDatabase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// args has "{data}" where the -data directory goes.
		args []string
		run  func(ctx context.Context, args []string, out io.Writer) error
	}{
		{"join", []string{"-data", "{data}", "-for", "1m"}, join},
		{"calibrate", []string{"-data", "{data}", "ABC123"}, calibrate},
		{"device list", []string{"list", "-data", "{data}"}, devices},
		{"device delete", []string{"delete", "-data", "{data}", "ABC123"}, devices},
		{"recipe list", []string{"list", "-data", "{data}"}, recipes},
		{"instance list", []string{"list", "-data", "{data}"}, instances},
		{"playlist list", []string{"list", "-data", "{data}"}, playlists},
		{"token list", []string{"list", "-data", "{data}"}, tokens},
		{"token add", []string{"add", "-data", "{data}", "-name", "script"}, tokens},
		{"setting list", []string{"list", "-data", "{data}"}, setting},
		{"setting set", []string{"set", "-data", "{data}", "refresh_seconds", "600"}, setting},
		{"firmware list", []string{"list", "-data", "{data}"}, firmwares},
		{"password", []string{"-data", "{data}"}, func(ctx context.Context, args []string, out io.Writer) error {
			return password(ctx, args, stdin(t, "a long enough password\n"), out)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := filepath.Join(t.TempDir(), "data")
			args := slices.Clone(tc.args)
			for i, a := range args {
				if a == "{data}" {
					args[i] = data
				}
			}
			err := tc.run(t.Context(), args, io.Discard)
			if !errors.Is(err, db.ErrNoDatabase) {
				t.Fatalf("err = %v, want one wrapping db.ErrNoDatabase", err)
			}
			want := "no database at " + filepath.Join(data, "trmnl.db") + ": pass the -data directory the server uses"
			if err.Error() != want {
				t.Errorf("err = %q, want %q", err, want)
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stat %s = %v, want it not created", data, err)
			}
		})
	}
}

// TestCommandsRefuseMissingDatabaseInExistingDir checks the same with a data
// directory that exists, as the wrong working directory with a data
// directory of other files in it would: no database file appears.
func TestCommandsRefuseMissingDatabaseInExistingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := join(t.Context(), []string{"-data", dir}, io.Discard)
	if !errors.Is(err, db.ErrNoDatabase) {
		t.Fatalf("err = %v, want one wrapping db.ErrNoDatabase", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("directory holds %v, %v; want it empty", entries, err)
	}
}

// TestServeCreatesDatabase checks that serve, the one command that may,
// creates the data directory and the database in it.
func TestServeCreatesDatabase(t *testing.T) {
	t.Parallel()
	data := filepath.Join(t.TempDir(), "state", "data")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, []string{"-data", data, "-listen", "127.0.0.1:0"}) }()

	// The file appears before the schema is applied, so wait until another
	// command can use the database, which is after serve has made it.
	deadline := time.Now().Add(10 * time.Second)
	for join(t.Context(), []string{"-data", data}, io.Discard) != nil {
		select {
		case err := <-done:
			t.Fatalf("serve returned %v before creating a database", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("serve created no database")
		}
	}
	if _, err := os.Stat(dbPath(data)); err != nil {
		t.Error(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
}

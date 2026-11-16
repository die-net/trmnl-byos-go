package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/die-net/trmnl-byos-go/internal/renderproxy"
)

// proxy runs the renderer's proxy apart from the server, so the renderer's
// network holds nothing of the server's but the proxy. It serves the forward
// proxy the renderer's builds reach the internet through, and, with
// -forward-listen, forwards the server's build requests to the renderer, for
// a server that shares no network with it.
func proxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	proxyListen := fs.String("listen", ":3128", "address the renderer's builds reach the proxy at; never publish it")
	cacheDir := fs.String("cache", "cache", "directory holding the renderer's asset cache")
	forwardListen := fs.String("forward-listen", "", "address the server sends builds to, such as :8081, each forwarded to -forward-to (default: no forward)")
	forwardTo := fs.String("forward-to", "", "the renderer's address, such as renderer:8081, the one place -forward-listen forwards to")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos proxy [-listen addr] [-cache dir] [-forward-listen addr -forward-to host:port]")
	}
	if (*forwardListen == "") != (*forwardTo == "") {
		return errors.New("-forward-listen and -forward-to go together")
	}
	// A bad target would otherwise show only as each forwarded build failing.
	if *forwardTo != "" {
		if _, port, err := net.SplitHostPort(*forwardTo); err != nil || port == "" {
			return fmt.Errorf("-forward-to %q: want host:port", *forwardTo)
		}
	}

	pln, err := listenOn(ctx, *proxyListen)
	if err != nil {
		return err
	}
	var fln net.Listener
	if *forwardListen != "" {
		if fln, err = listenOn(ctx, *forwardListen); err != nil {
			_ = pln.Close()
			return err
		}
	}
	slog.Info("proxying", "listen", *proxyListen, "cache", *cacheDir, "forward_listen", *forwardListen, "forward_to", *forwardTo)
	return runProxy(ctx, pln, *cacheDir, fln, *forwardTo)
}

// runProxy serves the forward proxy on pln, with its asset cache in
// cacheDir, and, if fln is not nil, forwards each connection on fln to
// target, until ctx is done or one of them fails.
func runProxy(ctx context.Context, pln net.Listener, cacheDir string, fln net.Listener, target string) error {
	g, ctx := errgroup.WithContext(ctx)
	if err := startProxy(ctx, g, pln, cacheDir); err != nil {
		if fln != nil {
			_ = fln.Close()
		}
		return err
	}
	if fln != nil {
		g.Go(func() error { return renderproxy.Forward(ctx, fln, target, &net.Dialer{Timeout: 10 * time.Second}) })
	}
	return g.Wait()
}

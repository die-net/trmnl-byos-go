package renderproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Forward accepts connections on ln and copies each one, both ways, to a new
// connection to target, until ctx is done. It is how the server reaches the
// renderer when the two share no network: the proxy, which is on both, runs
// it. target is fixed when it starts, and nothing a client sends chooses
// where a connection goes, so a connection from the renderer's side reaches
// only the renderer.
//
// An Accept error that net/http's Server.Serve would retry, such as running
// out of file descriptors, is retried the same way, after 5 ms doubling to
// 1 s, so a renderer that opens many tunnels cannot stop the forward. On any
// return, ln and every connection it forwarded are closed before Forward
// returns: nil when ctx is done, or the Accept error that ended it.
func Forward(ctx context.Context, ln net.Listener, target string, dialer *net.Dialer) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// On a return for any reason: end every forwarded connection and close
	// ln, then wait for the connections, rather than for each to end on its
	// own. ln is closed here as well as by the AfterFunc below, which runs
	// in its own goroutine, so it is closed before Forward returns.
	defer func() {
		cancel()
		_ = ln.Close()
		wg.Wait()
	}()
	// Ends a blocked Accept when ctx is done.
	context.AfterFunc(ctx, func() { _ = ln.Close() })

	var delay time.Duration
	var warned time.Time
	retries := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !temporary(err) {
				return fmt.Errorf("forward accept: %w", err)
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			retries++
			// At most one line a second, however fast the retries come.
			if now := time.Now(); now.Sub(warned) >= time.Second {
				slog.Warn("forward accept", "err", err, "retries", retries, "retrying_in", delay)
				warned, retries = now, 0
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		wg.Go(func() { forwardOne(ctx, conn, target, dialer) })
	}
}

// temporary reports whether an Accept error is one to retry, as net/http's
// Server.Serve decides it. Serve asks the error's Temporary method, and a
// listener's Accept fails with a *net.OpError, whose Temporary is true for an
// accept's ECONNRESET and ECONNABORTED and for EINTR, EMFILE, ENFILE and
// timeouts. That method is called here on the *net.OpError itself, since
// Temporary is deprecated on the net.Error interface for errors in general,
// where it is not well defined.
func temporary(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Temporary()
}

// forwardOne copies conn to and from a new connection to target, and closes
// both when either side is done or ctx is.
func forwardOne(ctx context.Context, conn net.Conn, target string, dialer *net.Dialer) {
	defer conn.Close()
	up, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		slog.Warn("forward dial", "target", target, "err", err)
		return
	}
	defer up.Close()
	if err := copyBidirectional(ctx, conn, up); err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("forward copy", "target", target, "err", err)
	}
}

// Command trmnl-render runs inside the renderer container and builds
// TRMNL recipes with trmnlp for the trmnl-byos server. It serves POST
// /build, and gives each build a virtual display for Firefox to draw on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/render"
)

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("trmnl-render", flag.ExitOnError)
	listen := fs.String("listen", ":8081", "address to listen on")
	trmnlp := fs.String("trmnlp", "/app/bin/trmnlp", "path to the trmnlp command")
	concurrency := fs.Int("concurrency", 1, "builds to run at once; each takes about 1 GB, and above 1, builds that overlap can read each other's values")
	timeout := fs.Duration("timeout", 2*time.Minute, "longest one build may take")
	_ = fs.Parse(os.Args[1:]) // ExitOnError exits on a parse error.

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		if sig, ok := <-sigs; ok {
			cancel(stoppedBy{sig})
		}
	}()
	err := serve(ctx, *listen, *trmnlp, *concurrency, *timeout)
	if err != nil {
		slog.Error("trmnl-render", "err", err)
		return 1
	}
	return stopCode(context.Cause(ctx))
}

// stoppedBy is the cause of a stop by a signal.
type stoppedBy struct{ sig os.Signal }

func (s stoppedBy) Error() string { return "stopped by " + s.sig.String() }

// stopCode logs why the renderer stopped, and returns its exit status.
// SIGTERM and an interrupt are how it is meant to be stopped. Any other
// signal, such as a SIGHUP a build sent, is logged as a warning, and the
// renderer exits with an error, so whatever runs it starts it again.
func stopCode(cause error) int {
	var s stoppedBy
	if !errors.As(cause, &s) {
		return 0
	}
	if s.sig == syscall.SIGTERM || s.sig == os.Interrupt {
		slog.Info("trmnl-render: stopping", "signal", s.sig.String())
		return 0
	}
	slog.Warn("trmnl-render: stopped by an unexpected signal", "signal", s.sig.String())
	return 1
}

func serve(ctx context.Context, listen, trmnlp string, concurrency int, timeout time.Duration) error {
	// Without trmnlp or Xvfb, which every build starts, every build would
	// fail, and nothing would restart the renderer, so it refuses to start.
	if _, err := exec.LookPath(trmnlp); err != nil {
		return fmt.Errorf("find trmnlp at %s, from -trmnlp: %w", trmnlp, err)
	}
	xvfb, err := exec.LookPath("Xvfb")
	if err != nil {
		return fmt.Errorf("find Xvfb in PATH %q: %w", os.Getenv("PATH"), err)
	}
	// After each build the shim kills every other process of its user and
	// removes its user's files from the scratch roots, so it refuses to
	// start where that would kill anything else, or where a build could
	// leave a file the cleanup would miss. It is the one process of its user
	// a sweep leaves, so no build may reach into it.
	if err := render.ProtectSelf(); err != nil {
		return fmt.Errorf("protect the renderer: %w", err)
	}
	if err := render.CheckDedicatedUser(); err != nil {
		return err
	}
	needed := []string{trmnlp, xvfb}
	if self, err := os.Executable(); err == nil {
		needed = append(needed, self)
	}
	if err := render.CheckWritable(needed...); err != nil {
		return err
	}
	shim, err := render.NewShim(trmnlp, xvfb, buildEnv(os.Environ()), timeout, concurrency)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           shim,
		ReadHeaderTimeout: 10 * time.Second,
		// A request is refused at once or sends its body, waits for its
		// build, streams the result, and, for the last build to end, waits
		// for the sweep. The shim bounds the body itself (see
		// Shim.ResponseTimeout).
		WriteTimeout: shim.ResponseTimeout(),
	}
	slog.Info("trmnl-render: serving", "listen", listen, "concurrency", concurrency)
	return supervise(ctx, srv, shim.Failed(), timeout)
}

// supervise runs srv until ctx is done or failed is closed, and then shuts
// it down. A failure is an error, so the process exits and whatever runs it
// starts it again.
func supervise(ctx context.Context, srv *http.Server, failed <-chan struct{}, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	var err error
	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	case <-failed:
		err = errors.New("the renderer stopped taking builds")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if serr := srv.Shutdown(shutdown); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		return errors.Join(err, fmt.Errorf("shutdown: %w", serr))
	}
	return err
}

// buildEnv is the environment every build starts from: the container's,
// without a display, which the shim sets for each build, with both
// spellings of each proxy variable, since tools differ in which they read,
// and localhost exempt from the proxy, since trmnlp drives Firefox over a
// local WebDriver connection.
func buildEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+8)
	vars := map[string]string{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "DISPLAY":
			// Each build gets its own display from the shim.
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
			if value != "" {
				vars[strings.ToUpper(name)] = value
			}
		default:
			env = append(env, kv)
		}
	}
	local := "localhost,127.0.0.1,::1"
	if v := vars["NO_PROXY"]; v != "" {
		local = v + "," + local
	}
	vars["NO_PROXY"] = local
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if v := vars[name]; v != "" {
			env = append(env, name+"="+v, strings.ToLower(name)+"="+v)
		}
	}
	return env
}

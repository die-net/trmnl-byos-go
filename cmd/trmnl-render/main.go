// Command trmnl-render runs inside the renderer container and builds
// TRMNL recipes with trmnlp for the trmnl-byos server. It starts the
// virtual display Firefox draws on, then serves POST /build.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/render"
)

func main() {
	os.Exit(run())
}

// display is the X display started for Firefox, the one trmnlp's image
// expects.
const display = ":99"

func run() int {
	fs := flag.NewFlagSet("trmnl-render", flag.ExitOnError)
	listen := fs.String("listen", ":8081", "address to listen on")
	trmnlp := fs.String("trmnlp", "/app/bin/trmnlp", "path to the trmnlp command")
	concurrency := fs.Int("concurrency", 1, "builds to run at once; each takes about 1 GB")
	timeout := fs.Duration("timeout", 2*time.Minute, "longest one build may take")
	_ = fs.Parse(os.Args[1:]) // ExitOnError exits on a parse error.

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *listen, *trmnlp, *concurrency, *timeout); err != nil {
		slog.Error("trmnl-render", "err", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, listen, trmnlp string, concurrency int, timeout time.Duration) error {
	xvfb, err := startXvfb(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = xvfb.Process.Kill()
		_ = xvfb.Wait()
	}()

	shim := render.NewShim(trmnlp, buildEnv(os.Environ()), timeout, concurrency)
	srv := &http.Server{
		Addr:              listen,
		Handler:           shim,
		ReadHeaderTimeout: 10 * time.Second,
		// A request waits for a free build slot, then for the build, then
		// streams the result.
		WriteTimeout: time.Duration(concurrency+1)*timeout + time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("trmnl-render: serving", "listen", listen, "concurrency", concurrency)
	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// startXvfb starts the virtual display and waits for its socket.
func startXvfb(ctx context.Context) (*exec.Cmd, error) {
	n := strings.TrimPrefix(display, ":")
	// A restarted container keeps /tmp, and Xvfb will not start over a
	// stale lock.
	_ = os.Remove(filepath.Join(os.TempDir(), ".X"+n+"-lock"))
	socket := filepath.Join(os.TempDir(), ".X11-unix", "X"+n)
	_ = os.Remove(socket)
	cmd := exec.CommandContext(ctx, "Xvfb", display, "-nolisten", "tcp")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Xvfb: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			return cmd, nil
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, errors.New("start Xvfb: its socket did not appear")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// buildEnv is the environment every build starts from: the container's,
// with the display set, both spellings of each proxy variable, since tools
// differ in which they read, and localhost exempt from the proxy, since
// trmnlp drives Firefox over a local WebDriver connection.
func buildEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+8)
	vars := map[string]string{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "DISPLAY":
			if value != "" {
				vars[strings.ToUpper(name)] = value
			}
		default:
			env = append(env, kv)
		}
	}
	vars["DISPLAY"] = display
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
	return append(env, "DISPLAY="+display)
}

package render

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// errNoDisplay means Xvfb did not start, which says nothing about the
// recipe, so the shim answers 503.
var errNoDisplay = errors.New("start Xvfb")

// displayTimeout bounds how long Xvfb may take to start.
const displayTimeout = 10 * time.Second

// display is a virtual X display that one build's Firefox draws on.
type display struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	// name is the DISPLAY value, such as ":1".
	name string
}

// startDisplay starts Xvfb on a free display, which it picks and writes to
// the pipe it gets as -displayfd once it accepts connections. Each build has
// its own, so no build sees another's windows, and none outlives its build
// to be killed by a sweep.
func startDisplay(xvfb string, env []string) (*display, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoDisplay, err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	// The first of ExtraFiles is descriptor 3 in the child.
	args := []string{"-displayfd", "3", "-nolisten", "tcp"}
	if runtime.GOOS == "linux" {
		// Clients reach it on its abstract socket, so it leaves no socket
		// or directory in /tmp for the cleanup after the build to remove.
		args = append(args, "-nolisten", "unix")
	}
	cmd := exec.CommandContext(ctx, xvfb, args...)
	cmd.ExtraFiles = []*os.File{w}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// SIGTERM lets Xvfb remove its lock file and socket.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Start()
	_ = w.Close() // Xvfb holds its own copy; an exit then reads as EOF.
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", errNoDisplay, err)
	}
	d := &display{cmd: cmd, cancel: cancel}
	if err := r.SetReadDeadline(time.Now().Add(displayTimeout)); err != nil {
		d.stop()
		return nil, fmt.Errorf("%w: %w", errNoDisplay, err)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		d.stop()
		return nil, fmt.Errorf("%w: read its display: %w", errNoDisplay, err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(line), 10, 16)
	if err != nil {
		d.stop()
		return nil, fmt.Errorf("%w: read its display: %w", errNoDisplay, err)
	}
	d.name = ":" + strconv.FormatUint(n, 10)
	return d, nil
}

// stop ends Xvfb and waits for it.
func (d *display) stop() {
	d.cancel()
	_ = d.cmd.Wait()
}

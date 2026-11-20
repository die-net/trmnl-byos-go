package render

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Every build runs as the renderer's one user. A process a build starts can
// leave its process group with setsid and outlive the build, and as the same
// user it could read later builds' environments, secret field values
// included, and their files. So after each build, and before the next one
// starts, the shim kills every other process of its user, then removes the
// files its user owns in the places a build can write (see cleanFiles).
// Killing every process of the user is safe only for a user that runs
// nothing but the shim, which CheckDedicatedUser checks at startup.

// mount is a mounted file system.
type mount struct {
	point string
	// writable is set for a file system mounted read-write that holds
	// files, not a virtual one such as /proc.
	writable bool
}

// proc is a process a sweep would kill.
type proc struct {
	pid  int
	name string
	// zombie is set for a process that has exited and waits to be reaped.
	zombie bool
}

// Bounds on one sweep. A process that has not exited by then is stuck, as
// is a cleanup that has not finished, and the shim stops taking builds.
const (
	sweepTimeout = 10 * time.Second
	cleanTimeout = 30 * time.Second
)

// sweeper kills every process a build left behind.
type sweeper struct {
	// list returns the processes a sweep must end: every live process of
	// the shim's user but the shim itself.
	list func() ([]proc, error)
	// kill sends SIGKILL to all of them at once.
	kill func() error
	// clean removes the files and IPC objects the user owns in the places a
	// build can write, once no process of the user is left, and returns how
	// many.
	clean func() (int, error)
	// self reports whether a build changed the shim itself, such as its
	// resource limits, which every later build would inherit.
	self func() error
	// timeout bounds one sweep.
	timeout time.Duration
}

// ownProcs returns every live process of the shim's user, by real, effective
// or saved user ID, but the shim itself. A zombie is left out unless the
// shim is process 1 (see listed). A process whose main thread exited but
// whose other threads run is not a zombie.
func ownProcs() ([]proc, error) {
	return listProcs(os.Getuid())
}

// run kills the processes until none is left, and returns how many it
// listed. It kills once whatever the list says, so a process the list
// misses still gets the signal, then until the list is empty. It fails if
// some are still running after the timeout.
func (s sweeper) run() (int, error) {
	seen := map[int]bool{}
	deadline := time.Now().Add(s.timeout)
	wait := 5 * time.Millisecond
	procs, err := s.list()
	for {
		if err != nil {
			return len(seen), fmt.Errorf("list processes: %w", err)
		}
		// A zombie, listed as process 1 so it gets reaped, has already
		// exited, as Firefox's helpers do, so it is not counted as killed.
		for _, p := range procs {
			if !p.zombie {
				seen[p.pid] = true
			}
		}
		if err := s.kill(); err != nil {
			return len(seen), fmt.Errorf("kill processes: %w", err)
		}
		if procs, err = s.list(); err == nil && len(procs) == 0 {
			return len(seen), nil
		}
		if err == nil && time.Now().After(deadline) {
			return len(seen), fmt.Errorf("%s still running after %s", describe(procs), s.timeout)
		}
		// A killed process takes a moment to exit.
		time.Sleep(wait)
		wait = min(2*wait, 100*time.Millisecond)
	}
}

// describe names up to ten processes, for a message.
func describe(procs []proc) string {
	names := make([]string, 0, min(len(procs), 10))
	for _, p := range procs[:min(len(procs), 10)] {
		names = append(names, fmt.Sprintf("%d (%s)", p.pid, p.name))
	}
	s := strings.Join(names, ", ")
	if len(procs) > 10 {
		s += fmt.Sprintf(" and %d more", len(procs)-10)
	}
	return fmt.Sprintf("%d processes: %s", len(procs), s)
}

// CheckDedicatedUser refuses to run as root, or as a user that runs any
// process but the shim. After each build the shim kills every other process
// of its user, so it must have a user of its own: in a container, the
// image's render user, or on a host, an account used for nothing else. A
// same-user parent, such as a shell or a per-user service manager, counts
// too, since the first sweep would kill it. So does process 1, such as
// Docker's init run as the container's user, which the sweep cannot kill:
// in a container, the shim must be process 1 itself.
func CheckDedicatedUser() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("trmnl-render runs only on Linux, not %s: it reads /proc, and kills every other process of its user in its PID namespace after each build", runtime.GOOS)
	}
	if os.Geteuid() == 0 {
		return errors.New("trmnl-render must not run as root: it kills every other process of its user after each build, so it needs an unprivileged user of its own")
	}
	procs, err := ownProcs()
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	if len(procs) > 0 {
		return fmt.Errorf("trmnl-render kills every other process of its user after each build, so its user (uid %d) must run nothing else, but it also runs %s", os.Getuid(), describe(procs))
	}
	return nil
}

// gate admits builds up to the shim's concurrency, and runs a sweep after
// builds end, while none runs. Once a build has ended, no new one starts
// until the builds still running have ended too and the sweep is done, so a
// build never starts while a process of an earlier one could still run. A
// request that comes while the gate is full or a sweep is due is refused at
// once, not queued: a build could otherwise send one that waits out its own
// sweep and runs after it. The server sends one build at a time, and the
// last build's handler sweeps before its response ends, so with concurrency
// 1 the server never finds the gate busy.
type gate struct {
	sweeper sweeper
	limit   int
	mu      sync.Mutex
	running int
	// dirty is set when a build has ended since the last sweep.
	dirty bool
	// err is why builds stopped, after a sweep failed.
	err error
	// failed is closed when a sweep fails.
	failed chan struct{}
}

// errBusy refuses a build that comes while the gate is full or a sweep is
// due.
var errBusy = errors.New("renderer busy: a build is running, or what one left is being swept up")

// newGate returns a gate that has swept up what an earlier run of the shim
// may have left. It sweeps before it returns, so no build can come first.
func newGate(sw sweeper, limit int) *gate {
	g := &gate{sweeper: sw, limit: max(limit, 1), dirty: true, failed: make(chan struct{})}
	_ = g.sweep() // A failure closes failed, which stops the shim before it serves.
	return g
}

// enter counts a build as running, or refuses it at once: with the error a
// failed sweep stopped builds with, or errBusy.
func (g *gate) enter() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.err != nil:
		return g.err
	case g.dirty || g.running >= g.limit:
		return errBusy
	}
	g.running++
	return nil
}

// leave counts a build as ended. The last one to end sweeps, and gets the
// sweep's error if it could not finish, which then stops the shim.
func (g *gate) leave() error {
	g.mu.Lock()
	g.running--
	g.dirty = true
	last := g.running == 0
	g.mu.Unlock()
	if last {
		return g.sweep()
	}
	return nil
}

// sweep kills what the builds left, then lets builds start again, or stops
// them for good if it could not finish, and says why.
func (g *gate) sweep() error {
	killed, err := g.sweeper.run()
	if killed > 0 {
		slog.Warn("render: killed processes a build left running", "count", killed)
	}
	if err == nil {
		var removed int
		removed, err = g.sweeper.clean()
		if removed > 0 {
			slog.Warn("render: removed files and IPC objects a build left", "count", removed)
		}
		if err != nil {
			err = fmt.Errorf("remove files: %w", err)
		}
	}
	if err == nil {
		err = g.sweeper.self()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.err = fmt.Errorf("renderer stopped: a sweep of what builds left could not finish: %w", err)
		slog.Error("render: taking no more builds", "err", g.err)
		close(g.failed)
		return g.err
	}
	g.dirty = false
	return nil
}

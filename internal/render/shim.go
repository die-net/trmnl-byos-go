package render

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Shim serves build requests inside the renderer container. Each build gets
// a fresh directory, with its own temporary and cache directories, which is
// removed when the build ends, and a virtual display of its own. After
// builds end, the shim kills every process they left (see gate).
type Shim struct {
	// Trmnlp is the path to the trmnlp command.
	Trmnlp string
	// Xvfb is the path to the Xvfb command, which each build starts for
	// Firefox to draw on.
	Xvfb string
	// Env is the environment every build starts from: the container's own,
	// with the proxy settings. The shim adds the build's display.
	Env []string
	// Timeout bounds one build.
	Timeout time.Duration
	// gate admits builds, and sweeps up after them.
	gate *gate
}

// NewShim returns a shim that runs at most concurrency builds at once. It
// records the shim's own state, which each sweep checks, and first sweeps
// up what an earlier run may have left, which kills every other process of
// its user and removes its user's files and IPC objects, so call it only
// after ProtectSelf, CheckDedicatedUser and CheckWritable.
func NewShim(trmnlp, xvfb string, env []string, timeout time.Duration, concurrency int) (*Shim, error) {
	base, err := snapshotSelf()
	if err != nil {
		return nil, err
	}
	roots, uid := scratchRoots(), os.Getuid()
	sw := sweeper{
		list: ownProcs,
		kill: killAll,
		clean: func() (int, error) {
			files, err := cleanFiles(roots, uid, cleanTimeout)
			if err != nil {
				return files, err
			}
			objs, err := cleanIPC(uid)
			return files + objs, err
		},
		self: func() error {
			now, err := snapshotSelf()
			if err != nil {
				return err
			}
			return base.changed(now)
		},
		timeout: sweepTimeout,
	}
	return &Shim{Trmnlp: trmnlp, Xvfb: xvfb, Env: env, Timeout: timeout, gate: newGate(sw, concurrency)}, nil
}

// bodyTimeout bounds how long a request's body may take to arrive: a
// project of maxProjectBytes at 2 Mbit/s, 64 seconds, and 10 more.
const bodyTimeout = 10*time.Second + maxProjectBytes/(256<<10)*time.Second

// ResponseTimeout is the longest a response may take, from its request to
// its end: the body, the build, and the worst sweep after it, which the
// last build's handler runs before its response ends, with time to spare.
func (s *Shim) ResponseTimeout() time.Duration {
	return bodyTimeout + s.Timeout + sweepTimeout + cleanTimeout + time.Minute
}

// Failed is closed when the shim has stopped taking builds, because a sweep
// of what builds left could not finish, or found the shim itself changed. The process should exit,
// so whatever runs it restarts it, and a container restart ends every
// process in the container.
func (s *Shim) Failed() <-chan struct{} {
	return s.gate.failed
}

// ServeHTTP handles POST /build.
func (s *Shim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/build" {
		http.NotFound(w, r)
		return
	}
	// Until the shim answers 102 Processing, nothing of the job has run, and
	// a failure is the renderer's, answered 503, which the client counts as
	// an outage. From then on the build runs, and a failure counts against
	// it, since the build could have caused it (see Client.Build).
	//
	// A request that comes while a build runs or is being swept up after is
	// refused before its body is read (see gate). A build's directory is made
	// only once it may run, so nothing an earlier build left can read it.
	if err := s.gate.enter(); err != nil {
		slog.Warn("render: refused a build", "err", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// The build leaves the gate once its result is open, before the
	// response starts, so the last build to end sweeps first and answers for
	// a sweep it broke (see below), or, on an early return, when the handler
	// ends. Its directory is removed only after the gate is left, since a
	// process the build left still writing there could keep a removal going
	// forever. In production the sweep's cleanup has removed it already,
	// unless another build still runs.
	left := false
	leave := func() error {
		if left {
			return nil
		}
		left = true
		return s.gate.leave()
	}
	var dir string
	defer func() {
		_ = leave()
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}()
	// TMPDIR may lie inside another cleanup root, which a sweep empties, so
	// it is made again.
	if err := os.MkdirAll(os.TempDir(), 0o700); err != nil {
		http.Error(w, "create the temporary directory: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	dir, err := os.MkdirTemp("", "build-")
	if err != nil {
		http.Error(w, "create build dir: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	// The body must arrive within bodyTimeout, after which reads have no
	// deadline, so a long build is not cut off. A server-wide ReadTimeout
	// would stay on the connection, and the background read net/http starts
	// once the body is read would then cancel the request mid-build.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(bodyTimeout)); err != nil {
		http.Error(w, "set read deadline", http.StatusServiceUnavailable)
		return
	}
	job, err := readRequest(r.Body, filepath.Join(dir, "project"))
	if err != nil {
		http.Error(w, err.Error(), requestErrStatus(err))
		return
	}
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		http.Error(w, "clear read deadline", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusProcessing)

	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	start := time.Now()
	buildErr := builder{trmnlp: s.Trmnlp, xvfb: s.Xvfb, env: s.Env}.build(ctx, dir, job)
	slog.Info("render: build", "width", job.Width, "height", job.Height, "palette", job.Palette,
		"seconds", time.Since(start).Seconds(), "err", buildErr)

	// Xvfb starts before anything of the recipe runs, so its failure is
	// the renderer's.
	if errors.Is(buildErr, errNoDisplay) {
		http.Error(w, buildErr.Error(), http.StatusServiceUnavailable)
		return
	}
	// What the build left is opened before the response starts, so a file
	// it replaced, or a directory it made unreadable, fails the build, with
	// the reason after its log.
	var res *result
	root, err := os.OpenRoot(dir)
	if err != nil {
		res, err = &result{}, fmt.Errorf("open the build directory: %w", err)
	} else {
		defer root.Close()
		res, err = openResult(root, buildErr == nil)
	}
	defer res.close()
	note := ""
	if err != nil {
		note = "\n" + err.Error() + "\n"
		if buildErr == nil {
			buildErr = fmt.Errorf("%w: %w", ErrBuildFailed, err)
		}
		slog.Warn("render: build result", "err", err)
	}
	// With the result held open, the sweep kills what the build left and
	// removes its files, its directory included, before anything is sent,
	// so nothing changes the result as it streams. A sweep that cannot
	// finish fails this build, which may have caused it, and then stops the
	// shim.
	if err := leave(); err != nil {
		res.dropPNGs()
		note += "\n" + err.Error() + "\n"
		buildErr = fmt.Errorf("%w: %w", ErrBuildFailed, err)
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if buildErr != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	if err := writeResult(w, res, note, errors.Is(buildErr, ErrFetchFailed)); err != nil {
		slog.Warn("render: write result", "err", err)
	}
}

// requestErrStatus is the status for a request whose job or project could
// not be read.
func requestErrStatus(err error) int {
	if errors.Is(err, errInvalidRequest) {
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

package render

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	buildErr := s.build(ctx, dir, job)
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

// errInvalidRequest marks a request the shim refuses as malformed: a tar it
// cannot read, a bad job, a file name that leaves its directory, a member
// that is neither a directory nor a regular file, or a body over
// maxProjectBytes. It is answered 400, which counts against the build. Any
// other error reading a request, such as a body that stops arriving or a
// disk that is full, is the transport's or the renderer's, answered 503, an
// outage.
var errInvalidRequest = errors.New("invalid request")

// invalid marks err as a malformed request.
func invalid(err error) error {
	return fmt.Errorf("%w: %w", errInvalidRequest, err)
}

// requestErrStatus is the status for a request whose job or project could
// not be read.
func requestErrStatus(err error) int {
	if errors.Is(err, errInvalidRequest) {
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

// tarErr marks an error from reading the request's tar as malformed when
// the tar itself is, and leaves an error from the body as it is.
func tarErr(err error) error {
	if errors.Is(err, tar.ErrHeader) || errors.Is(err, tar.ErrFieldTooLong) || errors.Is(err, tar.ErrInsecurePath) {
		return invalid(err)
	}
	return err
}

// limitedBody reads a request body, failing as a malformed request once it
// is over n bytes.
type limitedBody struct {
	r io.Reader
	n int64
}

func (l *limitedBody) Read(p []byte) (int, error) {
	if l.n <= 0 {
		// At the limit: the body must end here.
		var b [1]byte
		n, err := l.r.Read(b[:])
		if n > 0 {
			return 0, invalid(fmt.Errorf("the request is over %d bytes", maxProjectBytes))
		}
		return 0, err
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// readRequest reads the job, the first member of the request's tar, and
// extracts the other members into dir as they stream in. An error marked
// errInvalidRequest is a malformed request.
func readRequest(r io.Reader, dir string) (Job, error) {
	tr := tar.NewReader(&limitedBody{r: r, n: maxProjectBytes})
	h, err := tr.Next()
	if err != nil && !errors.Is(err, io.EOF) {
		return Job{}, fmt.Errorf("read request: %w", tarErr(err))
	}
	if err != nil || h.Name != jobMember || h.Typeflag != tar.TypeReg {
		return Job{}, invalid(errors.New("read request: the first member must be " + jobMember))
	}
	var job Job
	if err := json.NewDecoder(io.LimitReader(tr, maxJobBytes)).Decode(&job); err != nil {
		var syntax *json.SyntaxError
		var typ *json.UnmarshalTypeError
		if errors.As(err, &syntax) || errors.As(err, &typ) || errors.Is(err, io.EOF) {
			err = invalid(err)
		}
		return Job{}, fmt.Errorf("read job: %w", tarErr(err))
	}
	if err := job.validate(); err != nil {
		return Job{}, invalid(err)
	}
	if err := extract(tr, dir); err != nil {
		return Job{}, err
	}
	return job, nil
}

// extract writes the remaining members of a project tar into dir. Only
// directories and regular files with local names are written, through
// os.Root, which keeps every name inside dir.
func extract(tr *tar.Reader, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create project dir: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open project dir: %w", err)
	}
	defer root.Close()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read project: %w", tarErr(err))
		}
		name := path.Clean(h.Name)
		if !filepath.IsLocal(name) {
			return invalid(fmt.Errorf("project file %s: the name leaves the project", h.Name))
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return fmt.Errorf("project dir %s: %w", name, pathErr(err))
			}
		case tar.TypeReg:
			if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
				return fmt.Errorf("project dir %s: %w", name, pathErr(err))
			}
			if err := writeFile(root, name, tr); err != nil {
				return err
			}
		default:
			return invalid(fmt.Errorf("project file %s: only directories and regular files", name))
		}
	}
}

// pathErr marks an error making a project's file or directory as malformed
// when the project's own names clash, such as a file where a directory goes
// or a name given twice, and leaves any other, such as a full disk, as it
// is.
func pathErr(err error) error {
	if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTDIR) {
		return invalid(err)
	}
	return err
}

func writeFile(root *os.Root, name string, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("project file %s: %w", name, pathErr(err))
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("project file %s: %w", name, tarErr(err))
	}
	return f.Close()
}

// buildDirs makes the build's own home, temporary, cache, config, data,
// state and runtime directories inside dir, and returns the environment
// that points every tool at them. Python, Node, Ruby, PHP, Firefox,
// fontconfig and ImageMagick find their per-user files through these, so
// none reads or writes the account's own home, and all of it goes when dir
// is removed. PYTHONNOUSERSITE also keeps Python from loading a user site
// directory at all.
func buildDirs(dir string) ([]string, error) {
	env := []string{"PYTHONNOUSERSITE=1"}
	for _, d := range []struct{ name, sub string }{
		{"HOME", "home"},
		{"TMPDIR", "tmp"},
		{"XDG_CACHE_HOME", "cache"},
		{"XDG_CONFIG_HOME", "config"},
		{"XDG_DATA_HOME", "data"},
		{"XDG_STATE_HOME", "state"},
		{"XDG_RUNTIME_DIR", "run"},
	} {
		sub := filepath.Join(dir, d.sub)
		if err := os.Mkdir(sub, 0o700); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", d.sub, err)
		}
		env = append(env, d.name+"="+sub)
	}
	return env, nil
}

// build runs trmnlp on the project in dir, writing its output to the build
// log.
func (s *Shim) build(ctx context.Context, dir string, job Job) error {
	env, err := buildDirs(dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open build dir: %w", err)
	}
	defer root.Close()
	logFile, err := root.Create(logMember)
	if err != nil {
		return fmt.Errorf("create build log: %w", err)
	}
	defer logFile.Close()

	disp, err := startDisplay(s.Xvfb, slices.Concat(s.Env, env))
	if err != nil {
		return err
	}
	defer disp.stop()

	project := filepath.Join(dir, "project")
	//nolint:gosec // G204: the command is the operator's -trmnlp flag, every argument is a number, a palette ID that Job.validate held to [a-z0-9-], or the build's own directory, and no shell runs.
	cmd := exec.CommandContext(ctx, s.Trmnlp, "build", "--png",
		"--width", strconv.Itoa(job.Width), "--height", strconv.Itoa(job.Height),
		"--palette", job.Palette, "-d", project)
	cmd.Dir = project
	cmd.Env = slices.Concat(s.Env, env, []string{"DISPLAY=" + disp.name})
	for name, value := range job.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	inProcessGroup(cmd)
	err = cmd.Run()
	// Firefox or geckodriver may outlive trmnlp; nothing of a build stays.
	_ = killGroup(cmd)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBuildFailed, err)
	}
	for _, layout := range Layouts {
		if _, err := os.Stat(filepath.Join(project, "_build", layout+".png")); err != nil {
			return fmt.Errorf("%w: no %s.png", ErrBuildFailed, layout)
		}
	}
	// trmnlp swallows a failed data fetch: it prints a warning, draws the
	// screens from empty or error data, and exits 0. It marks the failure
	// in its cache, which is this build's own, so the screens are not sent.
	state, err := openBuildFile(root, "cache/trmnl/state", true)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read its cache: %w", ErrBuildFailed, err)
	}
	defer state.Close()
	names, err := state.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("%w: read its cache: %w", ErrBuildFailed, err)
	}
	if slices.ContainsFunc(names, func(n string) bool { return strings.HasSuffix(n, ".fetch_failed") }) {
		return ErrFetchFailed
	}
	return nil
}

// openBuildFile opens name in a build's directory, which the build could
// have changed, for reading. It does not block on a FIFO or follow a link,
// and refuses anything but a regular file, or a directory if dir is set,
// with a plain reason.
func openBuildFile(root *os.Root, name string, dir bool) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|openBuildFlags, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && info.IsDir() != dir || err == nil && !dir && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
		if dir {
			err = fmt.Errorf("%s is not a directory", name)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// result is what a build's response carries, opened before the response
// starts, so that a file the build replaced fails the build, with a plain
// reason, rather than the response. Each file's size is taken once, from
// the open file, and exactly that much is sent, so a process the build left
// cannot grow a file between the check and the send.
type result struct {
	// log is the build log, nil if there is none.
	log *os.File
	// logSize is the log's size when it was opened.
	logSize int64
	// pngs are the layouts' PNGs, in the order of Layouts, if sent.
	pngs []*os.File
	// pngSizes are their sizes when they were opened, each at most
	// maxPNGBytes.
	pngSizes []int64
}

// dropPNGs closes the PNGs, which are then not sent.
func (r *result) dropPNGs() {
	for _, f := range r.pngs {
		_ = f.Close()
	}
	r.pngs, r.pngSizes = nil, nil
}

func (r *result) close() {
	for _, f := range append([]*os.File{r.log}, r.pngs...) {
		if f != nil {
			_ = f.Close()
		}
	}
}

// openResult opens the build log, and the PNGs if withPNGs is set. An error
// is a reason the build failed: a file that is not a regular one, or a PNG
// over maxPNGBytes. The log is kept even then.
func openResult(root *os.Root, withPNGs bool) (*result, error) {
	res := &result{}
	log, err := openBuildFile(root, logMember, false)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	if log != nil {
		info, err := log.Stat()
		if err != nil {
			_ = log.Close()
			return res, fmt.Errorf("stat %s: %w", logMember, err)
		}
		res.log, res.logSize = log, info.Size()
	}
	if !withPNGs {
		return res, nil
	}
	for _, layout := range Layouts {
		size, f, err := openPNG(root, layout)
		if err != nil {
			for _, p := range res.pngs {
				_ = p.Close()
			}
			res.pngs, res.pngSizes = nil, nil
			return res, err
		}
		res.pngs = append(res.pngs, f)
		res.pngSizes = append(res.pngSizes, size)
	}
	return res, nil
}

// openPNG opens a layout's PNG and returns its size, at most maxPNGBytes.
func openPNG(root *os.Root, layout string) (int64, *os.File, error) {
	f, err := openBuildFile(root, "project/_build/"+layout+".png", false)
	if err != nil {
		return 0, nil, err
	}
	info, err := f.Stat()
	if err == nil && info.Size() > maxPNGBytes {
		err = fmt.Errorf("%s.png is over %d bytes", layout, maxPNGBytes)
	}
	if err != nil {
		_ = f.Close()
		return 0, nil, err
	}
	return info.Size(), f, nil
}

// writeResult streams a tar of the build log, cut to leave room for note
// within maxLogBytes, which is all a client keeps, then note, the PNGs res
// holds, and a fetchFailedMember when the build's data fetch failed. Each
// file is sent at the size openResult took. One that came up short since
// fails the response, which the client counts as a failed build.
func writeResult(w io.Writer, res *result, note string, fetchFailed bool) error {
	tw := tar.NewWriter(w)
	logSize := min(res.logSize, maxLogBytes-int64(len(note)))
	if err := tw.WriteHeader(&tar.Header{Name: logMember, Mode: 0o600, Size: logSize + int64(len(note)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if res.log != nil {
		if _, err := io.CopyN(tw, res.log, logSize); err != nil {
			return fmt.Errorf("send %s: %w", logMember, err)
		}
	}
	if _, err := io.WriteString(tw, note); err != nil {
		return err
	}
	for i, f := range res.pngs {
		if err := tw.WriteHeader(&tar.Header{Name: Layouts[i] + ".png", Mode: 0o600, Size: res.pngSizes[i], Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, f, res.pngSizes[i]); err != nil {
			return fmt.Errorf("send %s.png: %w", Layouts[i], err)
		}
	}
	if fetchFailed {
		if err := tw.WriteHeader(&tar.Header{Name: fetchFailedMember, Mode: 0o600, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
	}
	return tw.Close()
}

// isLayout reports whether name is a layout's PNG.
func isLayout(name string) (string, bool) {
	layout, ok := strings.CutSuffix(name, ".png")
	return layout, ok && slices.Contains(Layouts, layout)
}

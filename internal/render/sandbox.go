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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/die-net/sandbox"
	"golang.org/x/net/netutil"
)

// SandboxConfig is how Sandbox runs builds.
type SandboxConfig struct {
	// TempDir is the directory each build's home is made in, such as tmp in
	// the data directory. NewSandbox makes it. It should lie under a path
	// in Cover, so no build sees another's home.
	TempDir string
	// Cover lists host paths no build may see, beyond what the sandbox
	// always covers: the data directory and the asset cache. Each is
	// hidden by an empty directory.
	Cover []string
	// Proxy serves each build's one way out: an HTTP proxy, which Sandbox
	// serves on a listener of the build's own.
	Proxy http.Handler
	// PathEnv is the PATH the build's tools are looked for on, the server's
	// own.
	PathEnv string
	// Memory is the most memory a build may use, in bytes.
	Memory int64
	// Cgroup is a cgroup v2 directory the server may make children of,
	// such as one systemd delegated to its unit, or "" for none. Each build
	// then runs in a child of its own, which bounds its memory, its tasks
	// and the tmpfs it writes. Without one, a watchdog that polls the
	// build's resident memory bounds it less closely.
	Cgroup string
	// Timeout bounds one build.
	Timeout time.Duration
}

// Sandbox runs each build in a sandbox of its own, on this host, with
// github.com/die-net/sandbox. It satisfies the same contract as Client:
// see Client.Build for what its errors mean. It is safe to call from
// several goroutines at once, since builds share nothing: each has its own
// home, its own listener to the proxy, and its own namespaces.
type Sandbox struct {
	cfg SandboxConfig
	// tempDir is cfg.TempDir with its links resolved, since a sandbox's
	// home may have none.
	tempDir string

	mu sync.Mutex
	// tools are the tools found, nil until every required one is.
	tools *Tools
	// cgroupReady is whether the controllers builds need are enabled in
	// cfg.Cgroup.
	cgroupReady bool

	// problem is why the last build could not start, or nil once a
	// sandbox has started.
	problem atomic.Pointer[string]
}

// NewSandbox returns a Sandbox, after it makes cfg.TempDir, and resolves
// its links and those of each of cfg.Cover. It looks for
// the tools only at the first build, and again at each build until it has
// them all, so a server whose host is missing one still starts.
func NewSandbox(cfg SandboxConfig) (*Sandbox, error) {
	if err := os.MkdirAll(cfg.TempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", cfg.TempDir, err)
	}
	dir, err := filepath.EvalSymlinks(cfg.TempDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", cfg.TempDir, err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, fmt.Errorf("resolve %s: %w", cfg.TempDir, err)
	}
	// Builds' homes lie under TempDir's real path, so each cover is named
	// by its real path too, whatever the library does with a link. One
	// that does not exist yet, which doctor may be given, is kept as it is.
	covers := make([]string, 0, len(cfg.Cover))
	for _, c := range cfg.Cover {
		rp, err := filepath.EvalSymlinks(c)
		if errors.Is(err, fs.ErrNotExist) {
			rp, err = c, nil
		}
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", c, err)
		}
		covers = append(covers, rp)
	}
	cfg.Cover = covers
	return &Sandbox{cfg: cfg, tempDir: dir}, nil
}

// Prefixes of the homes in TempDir: a build's, and a self-test's, which
// doctor runs with a TempDir of its own.
const (
	homePrefix     = "build-"
	selfTestPrefix = "selftest-"
)

// RemoveLeftovers removes the homes of builds an earlier run of the server
// left in TempDir, and their cgroups in Config.Cgroup, as a crash would
// leave them. Only the server makes them, so call it once at startup,
// before any build. It goes on past one it cannot remove, and returns the
// errors joined, for the caller to log: a home left on disk costs only
// space, and refusing to start over it would leave devices with stale
// screens.
func (s *Sandbox) RemoveLeftovers() error {
	var errs []error
	for _, dir := range []string{s.tempDir, s.cfg.Cgroup} {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), homePrefix) {
				continue
			}
			if dir == s.tempDir {
				err = removeHome(filepath.Join(dir, e.Name()))
			} else {
				err = (&buildCgroup{path: filepath.Join(dir, e.Name())}).remove()
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("remove what an earlier run left: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// Build runs job in a sandbox of its own, with the project writeProject
// writes, and calls png for each layout's PNG as it streams back. Its
// result and errors are Client.Build's. A sandbox that cannot start, such
// as on a host with no bubblewrap or where user namespaces are off, is an
// error matching ErrUnavailable, which says to run trmnl-byos doctor, and
// Problem says why until a sandbox starts.
func (s *Sandbox) Build(ctx context.Context, job Job, writeProject func(*tar.Writer) error,
	png func(layout string, r io.Reader) error,
) (string, error) {
	enc, err := prepareJob(job)
	if err != nil {
		return "", err
	}
	tools, err := s.setup(ctx)
	if err != nil {
		return "", s.outage(err)
	}
	home, err := os.MkdirTemp(s.tempDir, homePrefix)
	if err != nil {
		return "", s.outage(fmt.Errorf("create the build's home: %w", err))
	}
	defer func() {
		if err := removeHome(home); err != nil {
			slog.Warn("render: remove a build's home", "err", err)
		}
	}()
	var cg *buildCgroup
	if s.cfg.Cgroup != "" {
		if cg, err = newBuildCgroup(s.cfg.Cgroup, filepath.Base(home), s.cfg.Memory); err != nil {
			return "", s.outage(err)
		}
		defer func() {
			if err := cg.remove(); err != nil {
				slog.Warn("render: remove a build's cgroup", "err", err)
			}
		}()
	}

	// The build's context ends at the timeout, at the memory limit, or when
	// its result cannot be read, each with its own cause.
	tctx, cancelTimeout := context.WithTimeoutCause(ctx, s.cfg.Timeout, errBuildTimeout)
	defer cancelTimeout()
	bctx, stop := context.WithCancelCause(tctx)
	defer stop(nil)

	// The build command says on this pipe that it started, so an end
	// before that is told apart from one the recipe could have caused.
	startedR, startedW, err := os.Pipe()
	if err != nil {
		return "", s.outage(fmt.Errorf("make the build's started pipe: %w", err))
	}
	defer startedR.Close()
	// Closed by hand once the sandbox has started, so the build command
	// holds the only write end. This is for every return before that.
	defer startedW.Close()
	spec := s.Spec(*tools, home)
	spec.Files = []*os.File{startedW}
	cmd, err := sandbox.Command(bctx, spec)
	if err != nil {
		return "", s.outage(fmt.Errorf("set up the build's sandbox: %w", err))
	}
	cmd.Env = s.Env(*tools, cmd.ListenAddrs()[0])
	if cg != nil {
		cmd.CgroupFD = int(cg.dir.Fd())
	}
	var stderr capped
	cmd.Stderr = &stderr
	// The job goes in on a pipe and the result comes out on one, so each
	// streams, and neither is a file the build could reopen.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return "", s.outage(fmt.Errorf("make the build's stdin: %w", err))
	}
	defer stdinW.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		return "", s.outage(fmt.Errorf("make the build's stdout: %w", err))
	}
	defer stdoutR.Close()
	cmd.Stdin, cmd.Stdout = stdinR, stdoutW
	err = cmd.Start()
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = startedW.Close()
	if err != nil {
		return "", s.outage(fmt.Errorf("start the build's sandbox: %w", err))
	}

	// The proxy is the build's only way out: its network namespace has
	// nothing else.
	stopProxy := s.serveProxy(cmd.Listeners()[0])
	defer stopProxy()
	if cg == nil {
		done := make(chan struct{})
		defer close(done)
		go watchMemory(done, cmd, s.cfg.Memory, stop)
	}
	written := make(chan error, 1)
	go func() {
		err := writeRequest(bodyWriter{stdinW}, enc, writeProject)
		_ = stdinW.Close()
		written <- err
	}()

	res, readErr := readResult(stdoutR, png)
	if readErr != nil {
		// What is left of the result cannot be used, so the build is
		// stopped rather than left to run to its end.
		stop(readErr)
	}
	waitErr := cmd.Wait()
	writeErr := <-written
	// Every writer of the pipe is gone now, so this does not block.
	started, _ := startedR.Read(make([]byte, 1))

	if ctx.Err() != nil {
		return res.log, ctx.Err()
	}
	if started == 0 {
		// Nothing of the recipe ran: the sandbox ended before its command
		// did anything, such as a first stage an AppArmor profile killed.
		return res.log, s.outage(fmt.Errorf("the build's sandbox ended before its command started: %w%s", waitErr, stderrSuffix(&stderr)))
	}
	s.problem.Store(nil)
	if writeErr != nil && !errors.Is(writeErr, errBody) {
		return res.log, fmt.Errorf("write project: %w", writeErr)
	}
	switch cause := context.Cause(bctx); {
	case errors.Is(cause, errBuildTimeout):
		return res.log, fmt.Errorf("the build took over %s", s.cfg.Timeout)
	case errors.Is(cause, errOverMemory), cg != nil && cg.oomKilled():
		return res.log, fmt.Errorf("the build used over %d MiB of memory", s.cfg.Memory>>20)
	}
	// A result the build command could not write ends early, and the
	// command's own reason says more than the read's error does.
	ownReason := exitCode(waitErr) == exitBuildFailed && strings.TrimSpace(stderr.String()) != ""
	if readErr != nil && !ownReason {
		return res.log, readErr
	}
	return res.log, s.exitErr(waitErr, &stderr, res)
}

// exitCode is the exit status in err, from Wait: 0 for nil, and -1 for an
// error that holds none, such as a signal's.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// stderrSuffix is what a build command said on stderr, after a colon, or
// "".
func stderrSuffix(stderr *capped) string {
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return ": " + msg
	}
	return ""
}

// Bounds on a build's proxy, which runs in the server's process, so a
// build cannot use up the server's descriptors.
const (
	// maxProxyConns is the most connections a build may have open to its
	// proxy at once. A build fetches a few hosts, and Firefox opens at most
	// a few connections to each.
	maxProxyConns = 64
	// proxyIdleTimeout is how long an idle keep-alive connection to the
	// proxy stays open.
	proxyIdleTimeout = 30 * time.Second
)

// serveProxy serves the proxy on ln, a build's listener, and returns what
// stops it. Stopping it closes the listener and every connection, CONNECT
// tunnels included, which end with the context their requests carry.
func (s *Sandbox) serveProxy(ln net.Listener) func() {
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &http.Server{
		Handler:           s.cfg.Proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       proxyIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := proxy.Serve(netutil.LimitListener(ln, maxProxyConns)); !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("render: a build's proxy stopped", "err", err)
		}
	}()
	return func() {
		cancel()
		_ = proxy.Close()
	}
}

// SelfTest runs SelfTestCommand in a sandbox that is a build's in all but
// its command, with refused and allowed as the targets whose CONNECT
// through the build's proxy listener it tries, and returns its results.
func (s *Sandbox) SelfTest(ctx context.Context, refused, allowed []string) ([]sandbox.Result, error) {
	tools, err := s.setup(ctx)
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp(s.tempDir, selfTestPrefix)
	if err != nil {
		return nil, fmt.Errorf("create the sandbox's home: %w", err)
	}
	defer func() {
		if err := removeHome(home); err != nil {
			slog.Warn("render: remove a sandbox's home", "err", err)
		}
	}()
	spec := s.Spec(*tools, home)
	spec.Command = []string{sandbox.SelfPath, SelfTestCommand}
	for _, t := range refused {
		spec.Command = append(spec.Command, "-refuse", t)
	}
	for _, t := range allowed {
		spec.Command = append(spec.Command, "-allow", t)
	}
	cmd, err := sandbox.Command(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("set up the sandbox: %w", err)
	}
	cmd.Env = s.Env(*tools, cmd.ListenAddrs()[0])
	// One result line for each target, of which there is one for each of
	// the host's addresses.
	stdout, stderr := capped{max: maxSelfTestOutput}, capped{}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the sandbox: %w", err)
	}
	stop := s.serveProxy(cmd.Listeners()[0])
	err = cmd.Wait()
	stop()
	if err != nil {
		return nil, fmt.Errorf("the self-test in the sandbox: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var results []sandbox.Result
	dec := json.NewDecoder(strings.NewReader(stdout.String()))
	for dec.More() {
		var r sandbox.Result
		if err := dec.Decode(&r); err != nil {
			return results, fmt.Errorf("read the self-test's results: %w", err)
		}
		results = append(results, r)
	}
	return results, nil
}

// exitErr is the outcome of a build whose sandbox ended with err, from
// Wait, having written stderr and the result res.
func (s *Sandbox) exitErr(err error, stderr *capped, res received) error {
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return fmt.Errorf("the build's sandbox: %w", err)
		}
		code = exit.ExitCode()
	}
	msg := strings.TrimSpace(stderr.String())
	switch code {
	case exitBuilt:
		return res.outcome(true)
	case exitBuildFailed:
		// The log says why. Stderr is empty then, unless the command
		// could not write the result, which it says there.
		if msg != "" {
			return fmt.Errorf("%w: %s", res.outcome(false), msg)
		}
		return res.outcome(false)
	case exitRejected:
		return fmt.Errorf("%w: %s", ErrRejected, msg)
	case exitUnavailable:
		if msg == "" {
			msg = fmt.Sprintf("the build command exited with status %d and gave no reason", code)
		}
		return s.outage(errors.New(msg))
	}
	if msg != "" {
		return fmt.Errorf("the build's sandbox ended: %w: %s", err, msg)
	}
	return fmt.Errorf("the build's sandbox ended: %w", err)
}

// Errors that end a build early, as its context's cause.
var (
	errBuildTimeout = errors.New("the build timed out")
	errOverMemory   = errors.New("the build used too much memory")
)

// outage records err as why builds cannot start, and returns it as an error
// matching ErrUnavailable, which says how to find out more.
func (s *Sandbox) outage(err error) error {
	msg := err.Error() + "; run trmnl-byos doctor on the server to see why"
	s.problem.Store(&msg)
	return fmt.Errorf("%w: %s", ErrUnavailable, msg)
}

// Problem returns why the last build could not start, or "" if the last
// one did, or none has been tried.
func (s *Sandbox) Problem() string {
	if p := s.problem.Load(); p != nil {
		return *p
	}
	return ""
}

// setup finds the tools, and enables the cgroup controllers builds need,
// once, and again at each build until both have worked.
func (s *Sandbox) setup(ctx context.Context) (*Tools, error) {
	if runtime.GOOS != "linux" {
		return nil, errUnsupportedOS
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A tool removed since, as by an upgrade that took its version
	// directory, is looked for again, so the build is not failed for it.
	if s.tools != nil && !s.tools.present() {
		s.tools = nil
	}
	if s.tools == nil {
		tools := FindTools(ctx, s.cfg.PathEnv)
		if missing := tools.Missing(); len(missing) > 0 {
			return nil, fmt.Errorf("%s, which %s, is not installed or not on the server's PATH", missing[0].Name, missing[0].Use)
		}
		if tools.GemErr != nil {
			return nil, tools.GemErr
		}
		if len(tools.HiddenGems) > 0 {
			return nil, fmt.Errorf("a directory of Ruby's gem path, %s, is or holds a person's home, so builds cannot see it; install trmnlp's gems in a directory of their own, such as system-wide", tools.HiddenGems[0])
		}
		s.tools = &tools
	}
	if s.cfg.Cgroup != "" && !s.cgroupReady {
		if err := enableControllers(s.cfg.Cgroup); err != nil {
			return nil, err
		}
		s.cgroupReady = true
	}
	return s.tools, nil
}

// errUnsupportedOS is why sandboxed builds cannot run outside Linux. The
// library has a macOS backend, which this server does not use yet.
var errUnsupportedOS = fmt.Errorf("sandboxed builds run only on Linux so far; run a renderer apart and give serve its -render-url: %w", errors.ErrUnsupported)

// ProbeSandbox tries this host's sandbox for real, as sandbox.Probe does,
// starting a trial sandbox whose first stage runs this binary again, so
// the binary must call sandbox.Stage first in main. It returns the probe's
// report. An error means no build's sandbox can start, and says why and
// how to fix it, such as the AppArmor profile this binary needs on Ubuntu.
// Outside Linux it is errUnsupportedOS.
func ProbeSandbox() (sandbox.Report, error) {
	if runtime.GOOS != "linux" {
		return sandbox.Report{}, errUnsupportedOS
	}
	rep, err := sandbox.Probe()
	if err != nil {
		for _, c := range rep.Checks {
			if c.Status == sandbox.Fail && !strings.Contains(err.Error(), c.Detail) {
				return rep, fmt.Errorf("%w: %s", err, c.Detail)
			}
		}
		return rep, err
	}
	return rep, nil
}

// CheckCgroup makes and removes a build's cgroup in Config.Cgroup, as a
// build does, and returns why it could not.
func (s *Sandbox) CheckCgroup() error {
	if s.cfg.Cgroup == "" {
		return errors.New("no cgroup was given")
	}
	if err := enableControllers(s.cfg.Cgroup); err != nil {
		return err
	}
	cg, err := newBuildCgroup(s.cfg.Cgroup, selfTestPrefix+strconv.Itoa(os.Getpid()), s.cfg.Memory)
	if err != nil {
		return err
	}
	return cg.remove()
}

// Limits on each build, beyond its memory.
const (
	// scratchSize is the size of each tmpfs a build may write: /tmp, where
	// the build's directory is, /var/tmp and /dev/shm. It is the 1 GB tmpfs
	// on /tmp that Compose gives the renderer.
	scratchSize = 1 << 30
	// maxFileSize is the largest file a build may write. A PNG may be 8 MB,
	// and the framework's CSS for one version is 19 MB.
	maxFileSize = 256 << 20
	// maxOpenFiles is the most descriptors a process of a build may have
	// open. Firefox opens many.
	maxOpenFiles = 8192
	// maxCPUSeconds is the most CPU time one process of a build may use.
	// The timeout stops a build sooner. This stops a process that spins on
	// several cores sooner still.
	maxCPUSeconds = 240
	// MaxTasks is the most processes and threads a build may have, with a
	// cgroup. A MapLibre map recipe with a transform peaked at 272.
	MaxTasks = 1024
)

// Spec returns the sandbox a build gets, with home as its home and tools
// as what it runs. trmnl-byos doctor checks this Spec.
func (s *Sandbox) Spec(tools Tools, home string) sandbox.Spec {
	return sandbox.Spec{
		Command:     []string{sandbox.SelfPath, BuildCommand, "-trmnlp", tools.Find("trmnlp"), "-xvfb", tools.Find("Xvfb")},
		Home:        home,
		Read:        tools.Read,
		Cover:       slices.Concat(s.cfg.Cover, tools.Cover),
		Kernel:      sandbox.KernelStrict,
		Network:     sandbox.NetworkNone,
		Listen:      1,
		ScratchSize: scratchSize,
		Rlimits: []sandbox.Rlimit{
			{Resource: sandbox.ResourceCPU, Soft: maxCPUSeconds, Hard: maxCPUSeconds},
			{Resource: sandbox.ResourceFileSize, Soft: maxFileSize, Hard: maxFileSize},
			{Resource: sandbox.ResourceCore, Soft: 0, Hard: 0},
			{Resource: sandbox.ResourceOpenFiles, Soft: maxOpenFiles, Hard: maxOpenFiles},
		},
	}
}

// Env returns a build's environment, made from nothing but what it needs:
// its PATH and Ruby's gem path, a UTF-8 locale, and proxy, the address of
// its listener to the proxy, in both spellings of each proxy variable,
// since tools differ in which they read. localhost is exempt from the
// proxy, since trmnlp drives Firefox over a local WebDriver connection.
// The sandbox adds HOME, TMPDIR and the XDG directories, and the build
// command adds the job's field values, for trmnlp only.
func (*Sandbox) Env(tools Tools, proxy string) []string {
	url := "http://" + proxy
	const local = "localhost,127.0.0.1,::1"
	env := []string{
		"PATH=" + tools.Path,
		"LANG=C.UTF-8",
		"HTTP_PROXY=" + url, "http_proxy=" + url,
		"HTTPS_PROXY=" + url, "https_proxy=" + url,
		"NO_PROXY=" + local, "no_proxy=" + local,
	}
	if tools.GemPath != "" {
		env = append(env, "GEM_PATH="+tools.GemPath)
	}
	// Selenium finds geckodriver by this, rather than trying to download
	// one.
	if gecko := tools.Find("geckodriver"); gecko != "" {
		env = append(env, "SE_GECKODRIVER="+gecko)
	}
	return env
}

// footprinter is what watchMemory reads, which a *sandbox.Cmd is.
type footprinter interface {
	Footprint() (int64, error)
}

// memoryPoll is how often watchMemory reads a build's memory.
const memoryPoll = 250 * time.Millisecond

// watchMemory reads cmd's resident memory every memoryPoll, until done is
// closed, and calls stop with errOverMemory once it is over limit. It is a
// soft limit: a spike between reads is missed, and the tmpfs a build
// writes is not resident memory of any process. A cgroup bounds both.
func watchMemory(done <-chan struct{}, cmd footprinter, limit int64, stop context.CancelCauseFunc) {
	tick := time.NewTicker(memoryPoll)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		n, err := cmd.Footprint()
		if err != nil {
			// The sandbox is gone.
			return
		}
		if n > limit {
			stop(errOverMemory)
			return
		}
	}
}

// removeHome removes a build's home and everything in it, however the build
// left it (see removeTree).
func removeHome(home string) error {
	return removeTree(home)
}

// capped keeps the first max bytes written to it, or maxStderr if max is
// 0: the reason a build command gives for its exit status, or a self-test's
// results.
type capped struct {
	max int
	mu  sync.Mutex
	buf []byte
}

// Output bounds: how much of a build command's stderr is kept, and of a
// self-test's results.
const (
	maxStderr         = 4 << 10
	maxSelfTestOutput = 1 << 20
)

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit := c.max
	if limit == 0 {
		limit = maxStderr
	}
	if room := limit - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

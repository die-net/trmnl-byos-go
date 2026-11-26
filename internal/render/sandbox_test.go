package render

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/die-net/sandbox"
)

// newUnitSandbox returns a Sandbox whose TempDir is under t.TempDir, and
// that TempDir with its links resolved. It starts nothing.
func newUnitSandbox(t *testing.T, cfg SandboxConfig) (*Sandbox, string) {
	t.Helper()
	if cfg.TempDir == "" {
		cfg.TempDir = filepath.Join(t.TempDir(), "tmp")
	}
	sb, err := NewSandbox(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sb, sb.tempDir
}

// TestSandboxSpec checks the sandbox every build gets: its command, what it
// shows and covers, and its limits, which must not include ones that stop
// Firefox or that count the user's other processes.
func TestSandboxSpec(t *testing.T) {
	t.Parallel()
	sb, _ := newUnitSandbox(t, SandboxConfig{Cover: []string{"/data", "/cache"}})
	tools := Tools{
		List: []Tool{{Name: "trmnlp", Path: "/x/trmnlp"}, {Name: "Xvfb", Path: "/x/Xvfb"}},
		Read: []string{"/r/one", "/r/two"},
		// Linuxbrew's var, which the tools hide after the server's own.
		Cover: []string{"/r/one/var"},
	}
	spec := sb.Spec(tools, "/the/home")

	wantCommand := []string{sandbox.SelfPath, BuildCommand, "-trmnlp", "/x/trmnlp", "-xvfb", "/x/Xvfb"}
	if !slices.Equal(spec.Command, wantCommand) {
		t.Errorf("Command = %v, want %v", spec.Command, wantCommand)
	}
	if spec.Home != "/the/home" {
		t.Errorf("Home = %q", spec.Home)
	}
	if !slices.Equal(spec.Read, tools.Read) {
		t.Errorf("Read = %v, want %v", spec.Read, tools.Read)
	}
	if !slices.Equal(spec.Cover, []string{"/data", "/cache", "/r/one/var"}) {
		t.Errorf("Cover = %v", spec.Cover)
	}
	if spec.Kernel != sandbox.KernelStrict {
		t.Errorf("Kernel = %v, want strict", spec.Kernel)
	}
	if spec.Network != sandbox.NetworkNone {
		t.Errorf("Network = %v, want none", spec.Network)
	}
	if spec.Listen != 1 {
		t.Errorf("Listen = %d, want 1", spec.Listen)
	}
	if spec.ScratchSize != 1<<30 {
		t.Errorf("ScratchSize = %d, want 1 GiB", spec.ScratchSize)
	}
	wantLimits := []sandbox.Rlimit{
		{Resource: sandbox.ResourceCPU, Soft: 240, Hard: 240},
		{Resource: sandbox.ResourceFileSize, Soft: 256 << 20, Hard: 256 << 20},
		{Resource: sandbox.ResourceCore, Soft: 0, Hard: 0},
		{Resource: sandbox.ResourceOpenFiles, Soft: 8192, Hard: 8192},
	}
	if !slices.Equal(spec.Rlimits, wantLimits) {
		t.Errorf("Rlimits = %v, want %v", spec.Rlimits, wantLimits)
	}
	// RLIMIT_NPROC counts every process of the user, the server's too, and
	// an address space limit stops Firefox, which maps far more than it uses.
	for _, l := range spec.Rlimits {
		if l.Resource == sandbox.ResourceProcesses || l.Resource == sandbox.ResourceAddressSpace {
			t.Errorf("Rlimits has %v, which a build must not be given", l)
		}
	}
}

// TestSandboxEnv checks that a build's environment is made from the tools
// and the proxy alone, in full, so nothing of the server's own environment,
// such as its secrets key, reaches a build.
func TestSandboxEnv(t *testing.T) {
	t.Parallel()
	sb, _ := newUnitSandbox(t, SandboxConfig{})
	const proxy = "http://127.0.0.1:3128"
	base := []string{
		"PATH=/x:/usr/bin", "LANG=C.UTF-8",
		"HTTP_PROXY=" + proxy, "http_proxy=" + proxy, "HTTPS_PROXY=" + proxy, "https_proxy=" + proxy,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
	}
	for _, tc := range []struct {
		name  string
		tools Tools
		want  []string
	}{
		{
			name:  "no gem path or geckodriver",
			tools: Tools{Path: "/x:/usr/bin"},
			want:  base,
		},
		{
			name:  "gem path",
			tools: Tools{Path: "/x:/usr/bin", GemPath: "/g1:/g2"},
			want:  append(slices.Clone(base), "GEM_PATH=/g1:/g2"),
		},
		{
			name:  "geckodriver",
			tools: Tools{Path: "/x:/usr/bin", List: []Tool{{Name: "geckodriver", Path: "/x/geckodriver"}}},
			want:  append(slices.Clone(base), "SE_GECKODRIVER=/x/geckodriver"),
		},
		{
			name: "both",
			tools: Tools{
				Path: "/x:/usr/bin", GemPath: "/g1",
				List: []Tool{{Name: "geckodriver", Path: "/x/geckodriver"}},
			},
			want: append(slices.Clone(base), "GEM_PATH=/g1", "SE_GECKODRIVER=/x/geckodriver"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sb.Env(tc.tools, "127.0.0.1:3128"); !slices.Equal(got, tc.want) {
				t.Errorf("Env = %v, want %v", got, tc.want)
			}
		})
	}
}

// exitError runs a shell that ends as what says, and returns the error from
// waiting for it, which is an *exec.ExitError for any status but 0. what is
// an exit status, or "kill" for a shell that kills itself with SIGKILL.
func exitError(t *testing.T, what string) error {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `if [ "$WHAT" = kill ]; then kill -9 $$; fi; exit "$WHAT"`)
	cmd.Env = []string{"WHAT=" + what}
	return cmd.Run()
}

// TestExitErr checks how each way a build's sandbox can end becomes an
// error: the build command's own statuses, and any other end, such as a
// signal, which matches none of the kinds that mean the renderer, the job
// or the build is at fault, so it counts against the build.
func TestExitErr(t *testing.T) {
	t.Parallel()
	full := received{layouts: len(Layouts)}
	kinds := []error{ErrUnavailable, ErrRejected, ErrBuildFailed, ErrFetchFailed}
	for _, tc := range []struct {
		name   string
		script string
		// waitErr is used instead of the script's when set.
		waitErr error
		stderr  string
		res     received
		// ok is that the result is nil.
		ok bool
		// is are the kinds the error matches, and every other is not.
		is []error
		// contains are in the error's text.
		contains []string
		// outage is that Problem is set to the error's reason.
		outage bool
	}{
		{name: "built", ok: true, res: full},
		{name: "built with too few layouts", res: received{layouts: 3}, contains: []string{"3 of 4 layouts"}},
		{name: "build failed", script: "3", res: full, is: []error{ErrBuildFailed}},
		{name: "fetch failed", script: "3", res: received{fetchFailed: true}, is: []error{ErrFetchFailed}},
		// Stderr has only the build command's own reasons, such as a
		// result it could not write. Xvfb's output goes elsewhere.
		{
			name: "build failed with a reason", script: "3", stderr: "write result: broken pipe\n", res: full,
			is: []error{ErrBuildFailed}, contains: []string{"write result: broken pipe"},
		},
		{
			name: "fetch failed with a reason", script: "3", stderr: "note", res: received{fetchFailed: true},
			is: []error{ErrFetchFailed}, contains: []string{"note"},
		},
		{
			name: "unavailable with no reason", script: "5", res: full, outage: true,
			is: []error{ErrUnavailable}, contains: []string{"status 5", "trmnl-byos doctor"},
		},
		// Once the command has said it started, any other end may be the
		// recipe's doing, so it counts against the build. One before that
		// is an outage, which Build decides (see TestSandboxStageDies).
		{
			name: "first stage status after the command started", script: "125", stderr: "sandbox: reaper", res: full,
			contains: []string{"reaper"},
		},
		{
			name: "rejected", script: "4", stderr: "bad job: no palette", res: full,
			is: []error{ErrRejected}, contains: []string{"bad job: no palette"},
		},
		{
			name: "unavailable", script: "5", stderr: "Xvfb did not start\n", res: full, outage: true,
			is: []error{ErrUnavailable}, contains: []string{"Xvfb did not start", "trmnl-byos doctor"},
		},
		{name: "usage", script: "2", stderr: "usage: render-build", res: full, contains: []string{"usage: render-build"}},
		{name: "usage with no reason", script: "2", res: full},
		{name: "other status", script: "1", stderr: "boom", res: full, contains: []string{"boom"}},
		{name: "killed", script: "kill", stderr: "partial output", res: full, contains: []string{"partial output", "killed"}},
		{name: "not an exit error", waitErr: errors.New("wait failed"), res: full, contains: []string{"wait failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb, _ := newUnitSandbox(t, SandboxConfig{})
			waitErr := tc.waitErr
			if tc.script != "" {
				waitErr = exitError(t, tc.script)
				var exit *exec.ExitError
				if !errors.As(waitErr, &exit) {
					t.Fatalf("%q gave %v, not an exit error", tc.script, waitErr)
				}
			}
			stderr := &capped{}
			_, _ = stderr.Write([]byte(tc.stderr))
			err := sb.exitErr(waitErr, stderr, tc.res)
			if tc.ok {
				if err != nil {
					t.Fatalf("exitErr = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("exitErr = nil, want an error")
			}
			for _, kind := range kinds {
				if got, want := errors.Is(err, kind), slices.Contains(tc.is, kind); got != want {
					t.Errorf("errors.Is(%q, %q) = %v, want %v", err, kind, got, want)
				}
			}
			for _, s := range tc.contains {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
			if tc.waitErr != nil && !errors.Is(err, tc.waitErr) {
				t.Errorf("error %q does not wrap %v", err, tc.waitErr)
			}
			if got := sb.Problem(); (got != "") != tc.outage || tc.outage && !strings.Contains(err.Error(), got) {
				t.Errorf("Problem() = %q after %q, want one only for an outage and in its text", got, err)
			}
		})
	}
}

// TestOutageProblem checks that a new Sandbox has no problem, that an
// outage records its reason with how to find out more, and returns an error
// matching ErrUnavailable that says the same.
func TestOutageProblem(t *testing.T) {
	t.Parallel()
	sb, _ := newUnitSandbox(t, SandboxConfig{})
	if got := sb.Problem(); got != "" {
		t.Fatalf("Problem() = %q on a new sandbox", got)
	}
	err := sb.outage(errors.New("no bubblewrap"))
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("outage error %q does not match ErrUnavailable", err)
	}
	got := sb.Problem()
	if !strings.Contains(got, "no bubblewrap") || !strings.Contains(got, "trmnl-byos doctor") {
		t.Errorf("Problem() = %q, want the reason and the doctor hint", got)
	}
	if !strings.Contains(err.Error(), got) {
		t.Errorf("outage error %q lacks the problem %q", err, got)
	}
	// A later outage replaces the reason.
	_ = sb.outage(errors.New("no userns"))
	if got := sb.Problem(); strings.Contains(got, "no bubblewrap") || !strings.Contains(got, "no userns") {
		t.Errorf("Problem() = %q after a second outage", got)
	}
}

// TestBuildOutsideLinux checks that where no sandbox can run, a build is an
// outage that says why and where to look, so the scheduler does not count it
// against the recipe, and that ProbeSandbox says it is unsupported. A job
// that is refused is still refused first.
func TestBuildOutsideLinux(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "linux" {
		t.Skip("a Linux host can run a sandbox, so its build is not an outage")
	}
	sb, _ := newUnitSandbox(t, SandboxConfig{})
	wrote := false
	write := func(*tar.Writer) error { wrote = true; return nil }
	png := func(string, io.Reader) error { return errors.New("no PNG expected") }

	_, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, write, png)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Build error %v does not match ErrUnavailable", err)
	}
	for _, want := range []string{"Linux", "trmnl-byos doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error %q lacks %q", err, want)
		}
	}
	if sb.Problem() == "" {
		t.Error("Problem() is empty after an outage")
	}
	if wrote {
		t.Error("Build wrote a project though it could not start")
	}

	sb, _ = newUnitSandbox(t, SandboxConfig{})
	_, err = sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "Bad Palette"}, write, png)
	if err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("Build of a bad job = %v, want an error that is not an outage", err)
	}
	if got := sb.Problem(); got != "" {
		t.Errorf("Problem() = %q after a bad job, which says nothing about the host", got)
	}

	if _, err := ProbeSandbox(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ProbeSandbox error = %v, want one matching errors.ErrUnsupported", err)
	}
}

// TestRemoveLeftovers checks that startup removes the homes of earlier
// builds, even when a build left a directory of mode 0 or a link out of its
// home, keeps anything else, such as a self-test's home, and never follows
// a link to what lies outside.
func TestRemoveLeftovers(t *testing.T) {
	t.Parallel()
	sb, tmp := newUnitSandbox(t, SandboxConfig{})
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(outside, "dir")
	if err := os.Mkdir(outsideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	inDir := filepath.Join(outsideDir, "inner")
	if err := os.WriteFile(inDir, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	build := filepath.Join(tmp, "build-a")
	locked := filepath.Join(build, "nested", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(build, "nested", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(build, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	// If RemoveLeftovers fails, let the test directory's own cleanup remove
	// what is left.
	t.Cleanup(func() { _, _ = cleanFiles([]string{build}, os.Getuid(), cleanTimeout) })

	now := time.Now()
	for name, mtime := range map[string]time.Time{
		"selftest-old": now.Add(-2 * time.Hour),
		"selftest-new": now,
		"other":        now.Add(-48 * time.Hour),
	} {
		dir := filepath.Join(tmp, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	if err := sb.RemoveLeftovers(); err != nil {
		t.Fatalf("RemoveLeftovers: %v", err)
	}
	for name, wantGone := range map[string]bool{"build-a": true, "selftest-old": false, "selftest-new": false, "other": false} {
		_, err := os.Lstat(filepath.Join(tmp, name))
		if gone := errors.Is(err, os.ErrNotExist); gone != wantGone {
			t.Errorf("%s gone = %v (%v), want %v", name, gone, err, wantGone)
		}
	}
	outRoot, err := os.OpenRoot(outside)
	if err != nil {
		t.Fatal(err)
	}
	defer outRoot.Close()
	for _, p := range []string{"secret", "dir/inner"} {
		if b, err := outRoot.ReadFile(p); err != nil || string(b) != "keep" {
			t.Errorf("%s was changed through a link: %q, %v", p, b, err)
		}
	}
}

// fakeFootprint is a footprinter that answers from a function and says when
// it was read.
type fakeFootprint struct {
	read  chan struct{}
	count atomic.Int32
	fn    func() (int64, error)
}

func (f *fakeFootprint) Footprint() (int64, error) {
	f.count.Add(1)
	select {
	case f.read <- struct{}{}:
	default:
	}
	return f.fn()
}

// TestWatchMemory checks the watchdog that bounds a build's memory when
// there is no cgroup: it stops the build once the footprint is over the
// limit, never while it is under, and gives up quietly when the footprint
// cannot be read, which means the sandbox is gone.
func TestWatchMemory(t *testing.T) {
	t.Parallel()
	t.Run("over the limit", func(t *testing.T) {
		t.Parallel()
		ctx, stop := context.WithCancelCause(t.Context())
		defer stop(nil)
		fp := &fakeFootprint{read: make(chan struct{}, 1), fn: func() (int64, error) { return 101, nil }}
		watchMemory(make(chan struct{}), fp, 100, stop)
		if !errors.Is(context.Cause(ctx), errOverMemory) {
			t.Errorf("cause = %v, want errOverMemory", context.Cause(ctx))
		}
		if n := fp.count.Load(); n != 1 {
			t.Errorf("read the footprint %d times, want it to stop after the first over the limit", n)
		}
	})
	t.Run("at the limit", func(t *testing.T) {
		t.Parallel()
		ctx, stop := context.WithCancelCause(t.Context())
		defer stop(nil)
		fp := &fakeFootprint{read: make(chan struct{}, 1), fn: func() (int64, error) { return 100, nil }}
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() { watchMemory(done, fp, 100, stop); close(finished) }()
		waitFor(t, fp.read, "a read of the footprint")
		close(done)
		waitFor(t, finished, "watchMemory to return after done")
		if ctx.Err() != nil {
			t.Errorf("a build at its limit was stopped: %v", context.Cause(ctx))
		}
	})
	t.Run("under the limit", func(t *testing.T) {
		t.Parallel()
		ctx, stop := context.WithCancelCause(t.Context())
		defer stop(nil)
		fp := &fakeFootprint{read: make(chan struct{}, 1), fn: func() (int64, error) { return 1, nil }}
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() { watchMemory(done, fp, 100, stop); close(finished) }()
		waitFor(t, fp.read, "a read of the footprint")
		waitFor(t, fp.read, "a second read of the footprint")
		select {
		case <-finished:
			t.Fatal("watchMemory returned before done was closed")
		default:
		}
		close(done)
		waitFor(t, finished, "watchMemory to return after done")
		if ctx.Err() != nil {
			t.Errorf("a build under its limit was stopped: %v", context.Cause(ctx))
		}
	})
	t.Run("footprint fails", func(t *testing.T) {
		t.Parallel()
		ctx, stop := context.WithCancelCause(t.Context())
		defer stop(nil)
		fp := &fakeFootprint{read: make(chan struct{}, 1), fn: func() (int64, error) { return 1 << 40, errors.New("gone") }}
		watchMemory(make(chan struct{}), fp, 100, stop)
		if ctx.Err() != nil {
			t.Errorf("a read that failed stopped the build: %v", context.Cause(ctx))
		}
		if n := fp.count.Load(); n != 1 {
			t.Errorf("read the footprint %d times, want it to give up after the first error", n)
		}
	})
}

// waitFor waits for a receive on ch, and fails the test if none comes
// within a few seconds, which is far longer than watchMemory's poll.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestCapped checks that a build's stderr is kept only up to its bound, from
// the start, however it is written, and that every write is reported whole
// so the build never sees a short write or an error from it.
func TestCapped(t *testing.T) {
	t.Parallel()
	var c capped
	var want strings.Builder
	for i, n := range []int{0, 3000, 1000, 1, 5000, 10} {
		p := []byte(strings.Repeat(string(rune('a'+i)), n))
		got, err := c.Write(p)
		if got != len(p) || err != nil {
			t.Fatalf("Write of %d bytes = %d, %v", len(p), got, err)
		}
		want.Write(p)
	}
	if got, w := c.String(), want.String()[:maxStderr]; got != w {
		t.Errorf("kept %d bytes, which differ from the first %d written", len(got), maxStderr)
	}
	if len(c.String()) != 4096 {
		t.Errorf("kept %d bytes, want exactly 4096", len(c.String()))
	}
	if n, err := c.Write([]byte("more")); n != 4 || err != nil || len(c.String()) != 4096 {
		t.Errorf("a write when full = %d, %v, keeping %d", n, err, len(c.String()))
	}
	var small capped
	_, _ = small.Write([]byte("hello "))
	_, _ = small.Write([]byte("world"))
	if got := small.String(); got != "hello world" {
		t.Errorf("small = %q", got)
	}
	if got := (&capped{}).String(); got != "" {
		t.Errorf("empty = %q", got)
	}
}

// TestCappedMax checks that a capped with a max of its own keeps that many
// bytes, from the start, however they are written, and that a max of 0 means
// the stderr bound.
func TestCappedMax(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		max    int
		writes []string
		want   string
	}{
		{"one write over", 10, []string{"0123456789abcdef"}, "0123456789"},
		{"several writes", 10, []string{"01234", "56", "789abc", "def"}, "0123456789"},
		{"exactly full", 10, []string{"0123456789"}, "0123456789"},
		{"under", 10, []string{"abc"}, "abc"},
		{"larger than the default", maxStderr * 2, []string{strings.Repeat("x", maxStderr*3)}, strings.Repeat("x", maxStderr*2)},
		{"zero is the stderr bound", 0, []string{strings.Repeat("x", maxStderr+100)}, strings.Repeat("x", maxStderr)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := capped{max: tc.max}
			for _, w := range tc.writes {
				if n, err := c.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%d bytes) = %d, %v", len(w), n, err)
				}
			}
			if got := c.String(); got != tc.want {
				t.Errorf("kept %d bytes, want %d", len(got), len(tc.want))
			}
		})
	}
}

// TestRemoveLeftoversCgroup checks that startup removes the build-* children
// of the cgroup directory as cgroups, and keeps what else is there, such as
// the server's own cgroup. A plain directory stands in for a cgroup, which
// has no cgroup.kill, and its absence is not a failure.
func TestRemoveLeftoversCgroup(t *testing.T) {
	t.Parallel()
	cg := filepath.Join(t.TempDir(), "cgroup")
	for _, name := range []string{"build-x", "build-y", "server"} {
		if err := os.MkdirAll(filepath.Join(cg, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A file named like a build is not a cgroup, and is left.
	if err := os.WriteFile(filepath.Join(cg, "build-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, tmp := newUnitSandbox(t, SandboxConfig{Cgroup: cg})
	if err := os.Mkdir(filepath.Join(tmp, "build-home"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := sb.RemoveLeftovers(); err != nil {
		t.Fatalf("RemoveLeftovers: %v", err)
	}
	for name, wantGone := range map[string]bool{"build-x": true, "build-y": true, "build-file": false, "server": false} {
		_, err := os.Lstat(filepath.Join(cg, name))
		if gone := errors.Is(err, os.ErrNotExist); gone != wantGone {
			t.Errorf("cgroup %s gone = %v (%v), want %v", name, gone, err, wantGone)
		}
	}
	if _, err := os.Lstat(filepath.Join(tmp, "build-home")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the home in TempDir was kept: %v", err)
	}
}

// TestRemoveLeftoversMissingCgroup checks that a cgroup directory that does
// not exist is not an error, since the server may have run without it.
func TestRemoveLeftoversMissingCgroup(t *testing.T) {
	t.Parallel()
	sb, _ := newUnitSandbox(t, SandboxConfig{Cgroup: filepath.Join(t.TempDir(), "nothing")})
	if err := sb.RemoveLeftovers(); err != nil {
		t.Errorf("RemoveLeftovers: %v", err)
	}
}

// TestRemoveLeftoversGoesOn checks that a home that cannot be removed does
// not stop the cgroups from being removed, and that its error is returned.
// The failure is a home in a directory that cannot be written.
func TestRemoveLeftoversGoesOn(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a directory cannot be made unwritable here")
	}
	cg := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(filepath.Join(cg, "build-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	sb, tmp := newUnitSandbox(t, SandboxConfig{Cgroup: cg})
	if err := os.Mkdir(filepath.Join(tmp, "build-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Entries cannot be removed from a directory that is not writable, so
	// the home in it cannot go.
	chmodTemp(t, tmp, 0o500)
	t.Cleanup(func() { chmodTemp(t, tmp, 0o700) })

	err := sb.RemoveLeftovers()
	if err == nil {
		t.Fatal("RemoveLeftovers succeeded, want the error of the home it could not remove")
	}
	if !strings.Contains(err.Error(), "build-a") {
		t.Errorf("error %q does not name the home", err)
	}
	if _, err := os.Lstat(filepath.Join(cg, "build-x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cgroup was not removed after the home failed: %v", err)
	}
}

// chmodTemp sets the mode of a directory a test made, and fails the test if it
// cannot.
func chmodTemp(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestSandboxCoverResolved checks that a cover given through a link, such
// as a data directory reached by a link, is covered at its real path, where
// builds' homes are made, and that one that does not exist yet is kept as
// given.
func TestSandboxCoverResolved(t *testing.T) {
	t.Parallel()
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(target, "not-yet")
	sb, err := NewSandbox(SandboxConfig{TempDir: filepath.Join(link, "tmp"), Cover: []string{link, missing}, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(target, "tmp"); sb.tempDir != want {
		t.Errorf("tempDir = %q, want %q", sb.tempDir, want)
	}
	if got, want := sb.Spec(Tools{}, sb.tempDir).Cover, []string{target, missing}; !slices.Equal(got, want) {
		t.Errorf("Cover = %q, want %q", got, want)
	}
}

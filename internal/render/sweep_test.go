//go:build linux

package render

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the test binary as a leftover process when the fake trmnlp
// asks it to, and runs the tests otherwise. A leftover's files are in the
// directory LEFTOVER_DIR: it writes its PID to pid, and what it reads to
// out.
func TestMain(m *testing.M) {
	switch os.Getenv("TRMNL_TEST_LEFTOVER") {
	case "spawn":
		os.Exit(spawnLeftover())
	case "watch":
		watchBuilds()
		os.Exit(1)
	case "spam":
		spamFiles()
		os.Exit(1)
	case "ghost":
		os.Exit(becomeGhost())
	}
	os.Exit(m.Run())
}

// spawnLeftover starts a watcher in a session of its own, as a recipe's
// transform could, so it leaves the build's process group and outlives the
// build. It writes the watcher's PID, and waits for it to start.
func spawnLeftover() int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	dir, err := os.OpenRoot(os.Getenv("LEFTOVER_DIR"))
	if err != nil {
		return 1
	}
	defer dir.Close()
	cmd := exec.CommandContext(context.Background(), self) //nolint:gosec // G204: the test binary itself.
	cmd.Env = append(os.Environ(), "TRMNL_TEST_LEFTOVER="+cmp.Or(os.Getenv("LEFTOVER_MODE"), "watch"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 1
	}
	if err := dir.WriteFile("pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return 1
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if b, err := dir.ReadFile("out"); err == nil && bytes.Contains(b, []byte("started\n")) {
			return 0
		}
	}
	return 1
}

// watchBuilds copies, until it is killed or fails, whatever it can read of builds'
// secrets to out: the PNGs the fake trmnlp writes them into, under the temp
// directory LEFTOVER_SCAN, and on Linux, every FIELD_ variable it can read
// from /proc.
func watchBuilds() {
	dir, err := os.OpenRoot(os.Getenv("LEFTOVER_DIR"))
	if err != nil {
		return
	}
	out, err := dir.OpenFile("out", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = out.WriteString("started\n")
	for {
		pngs, _ := filepath.Glob(filepath.Join(os.Getenv("LEFTOVER_SCAN"), "build-*", "project", "_build", "*.png"))
		environs, _ := filepath.Glob("/proc/[0-9]*/environ")
		for _, name := range slices.Concat(pngs, environs) {
			b, err := readRegular(name)
			if err != nil {
				continue
			}
			for v := range bytes.SplitSeq(b, []byte{0}) {
				if bytes.Contains(v, []byte("FIELD_")) || strings.HasSuffix(name, ".png") {
					_, _ = out.Write(append(v, '\n'))
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readRegular reads the start of name, at most 4 KB, if it is a regular
// file. It never blocks on a FIFO, such as the one TestBadResult leaves
// where a PNG goes, and never copies a big file, such as TestBadResult's
// 9 MB PNG, which would swell the leftover's output on every pass until the
// fake trmnlp's search of it could not keep up. The fake PNGs it looks for
// are a few dozen bytes.
func readRegular(name string) ([]byte, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: reading other builds' files is what this leftover is for.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(io.LimitReader(f, 4<<10))
}

// spamFiles creates files in the build's own temporary directory, as fast as
// it can, until it is killed or fails, so that removing the directory cannot
// finish while it runs. It writes the build's directory to builddir.
func spamFiles() {
	dir, err := os.OpenRoot(os.Getenv("LEFTOVER_DIR"))
	if err != nil {
		return
	}
	if err := dir.WriteFile("builddir", []byte(filepath.Dir(os.Getenv("TMPDIR"))), 0o600); err != nil {
		return
	}
	if err := dir.WriteFile("out", []byte("started\n"), 0o600); err != nil {
		return
	}
	tmp, err := os.OpenRoot(os.Getenv("TMPDIR"))
	if err != nil {
		return
	}
	// Several writers, so the files come faster than a removal can unlink
	// them.
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Go(func() {
			for i := 0; ; i++ {
				if err := tmp.WriteFile("f"+strconv.Itoa(w)+"-"+strconv.Itoa(i), nil, 0o600); err != nil {
					return
				}
			}
		})
	}
	wg.Wait()
}

// scopedSweeper is a sweeper for tests: it kills only the processes of the
// user named name. The shim's own would kill every process of the user
// running the tests.
func scopedSweeper(name string) sweeper {
	list := func() ([]proc, error) {
		procs, err := ownProcs()
		return slices.DeleteFunc(procs, func(p proc) bool { return p.name != name }), err
	}
	return sweeper{
		list: list,
		kill: func() error {
			procs, err := list()
			for _, p := range procs {
				if kerr := syscall.Kill(p.pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
					err = errors.Join(err, kerr)
				}
			}
			return err
		},
		clean:   func() (int, error) { return 0, nil },
		self:    func() error { return nil },
		timeout: sweepTimeout,
	}
}

// leftoverBinary copies the test binary to a name of its own, so a scoped
// sweeper finds its processes by name and nothing else.
func leftoverBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self) //nolint:gosec // G304: the test binary itself.
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	path := filepath.Join(t.TempDir(), "lo"+rand.Text()[:8])
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // G302: the copy must be executable.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLeftoverKilled checks that a process a build starts in a session of
// its own, which the process group kill misses, is killed before the next
// build starts, so it cannot read that build's secret. Without the sweep,
// the same leftover does read it.
func TestLeftoverKilled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		swept bool
	}{
		{"swept", true},
		{"not swept", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := leftoverBinary(t)
			sw := scopedSweeper(filepath.Base(bin))
			if !tc.swept {
				sw.list = func() ([]proc, error) { return nil, nil }
				sw.kill = func() error { return nil }
			}
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = root.Close() })
			c, _ := newShimServerWith(t, sw, ampleTimeout, "LEFTOVER_BIN="+bin, "LEFTOVER_DIR="+dir, "LEFTOVER_SCAN="+os.TempDir())

			first := Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"FIELD_TOKEN": "first-" + rand.Text()}}
			if log, err := c.Build(t.Context(), first, project("leftover"), collect(map[string]string{})); err != nil {
				t.Fatalf("first build: %v; log %q", err, log)
			}
			b, err := root.ReadFile("pid")
			if err != nil {
				t.Fatalf("the first build started no leftover: %v", err)
			}
			pid, err := strconv.Atoi(string(b))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

			secret := "second-" + rand.Text()
			second := Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"FIELD_TOKEN": secret}}
			// Without the sweep, the second build waits, up to 10 seconds,
			// until the leftover has read its secret, so a slow machine
			// does not fail the control. With it, the leftover is gone, and
			// a second of lingering is its chance.
			linger := "linger"
			if !tc.swept {
				linger = "lingerseen"
			}
			if log, err := c.Build(t.Context(), second, project(linger), collect(map[string]string{})); err != nil {
				t.Fatalf("second build: %v; log %q", err, log)
			}
			seen, err := root.ReadFile("out")
			if err != nil {
				t.Fatal(err)
			}
			procs, err := ownProcs()
			if err != nil {
				t.Fatal(err)
			}
			alive := slices.ContainsFunc(procs, func(p proc) bool { return p.pid == pid })
			if read := bytes.Contains(seen, []byte(secret)); read == tc.swept || alive == tc.swept {
				t.Errorf("leftover alive %v, read the second build's secret %v; want both %v", alive, read, !tc.swept)
			}
		})
	}
}

// TestSweepFails checks that a sweep that cannot finish, whether a process
// will not die or a file will not go, or that finds the shim itself
// changed, stops the shim: builds are refused as
// an outage, with the reason, and Failed is closed. The first sweep runs
// before any build, so it fails at once.
func TestSweepFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sw   sweeper
		want string
	}{
		{"process", sweeper{
			list:    func() ([]proc, error) { return []proc{{pid: 4242, name: "stuck"}}, nil },
			kill:    func() error { return nil },
			clean:   func() (int, error) { return 0, nil },
			self:    func() error { return nil },
			timeout: 50 * time.Millisecond,
		}, "4242 (stuck)"},
		{"file", sweeper{
			list:    func() ([]proc, error) { return nil, nil },
			kill:    func() error { return nil },
			clean:   func() (int, error) { return 0, errors.New("cannot remove /tmp/x") },
			self:    func() error { return nil },
			timeout: 50 * time.Millisecond,
		}, "cannot remove /tmp/x"},
		{"shim changed", sweeper{
			list:    func() ([]proc, error) { return nil, nil },
			kill:    func() error { return nil },
			clean:   func() (int, error) { return 0, nil },
			self:    func() error { return errors.New("resource limit 7 is 20/20") },
			timeout: 50 * time.Millisecond,
		}, "resource limit 7 is 20/20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, shim := newShimServerWith(t, tc.sw, ampleTimeout)
			select {
			case <-shim.Failed():
			case <-time.After(10 * time.Second):
				t.Fatal("Failed was not closed")
			}
			_, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
			if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("build after a failed sweep: err %v", err)
			}
		})
	}
}

// plantScript, run by the fake trmnlp, plants a file in every place a build
// can write: each directory the build's environment names, and REAL_HOME
// and SCRATCH, which stand in for the account's home and /tmp. It makes one
// directory unwritable and one file unreadable, as a build that wants its
// files kept could. Where Python is installed, it also plants a
// usercustomize.py, which appends PLANT_TAG to PLANT_OUT when Python starts,
// in the user site of both homes, and shows that each would run.
const plantScript = `
for d in "$HOME" "$TMPDIR" "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$REAL_HOME" "$SCRATCH" "$SCRATCH/sub/deeper"; do
  mkdir -p "$d" && : > "$d/planted"
done
mkdir "$SCRATCH/locked" && : > "$SCRATCH/locked/planted" && chmod 500 "$SCRATCH/locked"
: > "$REAL_HOME/planted-unreadable" && chmod 000 "$REAL_HOME/planted-unreadable"
if command -v python3 >/dev/null; then
  for h in "$HOME" "$REAL_HOME"; do
    site=$(env -u PYTHONNOUSERSITE HOME="$h" python3 -m site --user-site) && mkdir -p "$site" &&
      printf '%s\n' 'import os' 'open(os.environ["PLANT_OUT"], "a").write(os.environ["PLANT_TAG"] + chr(10))' > "$site/usercustomize.py"
    env -u PYTHONNOUSERSITE HOME="$h" PLANT_TAG="control $h" python3 -c 0
  done
fi
`

// checkScript, run by the fake trmnlp, starts Python as the build would,
// and as a runtime that ignored the build's home would, and lists every
// planted file it finds, all to PLANT_OUT.
const checkScript = `
if command -v python3 >/dev/null; then
  PLANT_TAG=second python3 -c 0
  env -u PYTHONNOUSERSITE HOME="$REAL_HOME" PLANT_TAG=second-real-home python3 -c 0
fi
find "$HOME" "$TMPDIR" "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$REAL_HOME" "$SCRATCH" \
  \( -name 'planted*' -o -name usercustomize.py \) >> "$PLANT_OUT"
`

// TestPlantedFilesRemoved checks that the files a build plants everywhere
// it can write, Python startup code included, are gone before the next
// build, which neither runs nor finds them.
func TestPlantedFilesRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	realHome, scratch := filepath.Join(dir, "home"), filepath.Join(dir, "tmp")
	for _, d := range []string{realHome, scratch} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scripts := filepath.Join(dir, "scripts")
	if err := os.Mkdir(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(scripts)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for name, body := range map[string]string{"plant": plantScript, "check": checkScript} {
		if err := root.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sw := scopedSweeper("no process has this name")
	sw.clean = func() (int, error) { return cleanFiles([]string{realHome, scratch}, os.Getuid(), cleanTimeout) }
	out := filepath.Join(scripts, "out")
	c, _ := newShimServerWith(t, sw, ampleTimeout, "REAL_HOME="+realHome, "SCRATCH="+scratch, "PLANT_OUT="+out,
		"PLANT_SCRIPT="+filepath.Join(scripts, "plant"), "CHECK_SCRIPT="+filepath.Join(scripts, "check"))
	job := Job{Width: 800, Height: 480, Palette: "bw"}
	for _, marker := range []string{"plant", "check"} {
		if log, err := c.Build(t.Context(), job, project(marker), collect(map[string]string{})); err != nil {
			t.Fatalf("%s build: %v; log %q", marker, err, log)
		}
	}
	b, err := root.ReadFile("out")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	// Where Python is installed, the planted code ran in the first build, in
	// its home and in REAL_HOME, so it would have run again had it been
	// left. Nothing else may be written.
	got := strings.Fields(string(b))
	want := 0
	if _, err := exec.LookPath("python3"); err == nil {
		want = 4
	}
	if len(got) != want || want > 0 && (got[0] != "control" || filepath.Base(got[1]) != "home" || got[2] != "control" || got[3] != realHome) {
		t.Errorf("PLANT_OUT holds %q, want only the first build's two controls", b)
	}
	for _, d := range []string{realHome, scratch} {
		if entries, err := os.ReadDir(d); err != nil || len(entries) != 0 {
			t.Errorf("%s holds %d entries, %v", d, len(entries), err)
		}
	}
}

// TestGateDrains checks that once a build has ended, the gate refuses new
// builds until those still running have ended too and a sweep has run, that
// the last build to end sweeps before leave returns, and that a sweep never
// runs while a build does.
func TestGateDrains(t *testing.T) {
	t.Parallel()
	var g *gate
	var sweeps []int
	g = newGate(sweeper{
		list: func() ([]proc, error) { return nil, nil },
		// With nothing listed, a sweep kills once.
		kill: func() error {
			if g != nil { // The first sweep runs inside newGate.
				g.mu.Lock()
				sweeps = append(sweeps, g.running)
				g.mu.Unlock()
			}
			return nil
		},
		clean:   func() (int, error) { return 0, nil },
		self:    func() error { return nil },
		timeout: time.Second,
	}, 2)
	for range 2 {
		if err := g.enter(); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.enter(); !errors.Is(err, errBusy) {
		t.Errorf("enter with the gate full: %v, want errBusy", err)
	}
	if err := g.leave(); err != nil {
		t.Fatal(err)
	}
	if err := g.enter(); !errors.Is(err, errBusy) {
		t.Errorf("enter after a build ended, while another runs: %v, want errBusy", err)
	}
	if len(sweeps) != 0 {
		t.Errorf("a sweep ran while a build did: %v", sweeps)
	}
	if err := g.leave(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sweeps, []int{0}) {
		t.Errorf("sweeps saw %v builds running, want one sweep with none", sweeps)
	}
	if err := g.enter(); err != nil {
		t.Errorf("enter after the sweep: %v", err)
	}
}

// TestShimBusy checks that a build request that comes while a build runs,
// as one the build itself sent would, or while the shim sweeps up after
// one, is refused at once as an outage, rather than run after the sweep,
// and that a build's response ends only once its sweep is done, so the
// server's next build never finds the shim busy.
func TestShimBusy(t *testing.T) {
	t.Parallel()
	sweeping, release := make(chan struct{}), make(chan struct{})
	var sweeps atomic.Int32
	sw := scopedSweeper("no process has this name")
	sw.self = func() error {
		// The first sweep, inside newGate, does not wait.
		if sweeps.Add(1) == 3 {
			close(sweeping)
			<-release
		}
		return nil
	}
	c, shim := newShimServerWith(t, sw, ampleTimeout)
	// One build at a time, the default.
	shim.gate.mu.Lock()
	shim.gate.limit = 1
	shim.gate.mu.Unlock()
	job := Job{Width: 800, Height: 480, Palette: "bw"}

	// A build that lingers, and one sent while it runs.
	done := make(chan error, 1)
	go func() {
		_, err := c.Build(t.Context(), job, project("linger"), collect(map[string]string{}))
		done <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		shim.gate.mu.Lock()
		running := shim.gate.running
		shim.gate.mu.Unlock()
		if running > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lingering build never started")
		}
	}
	start := time.Now()
	_, err := c.Build(t.Context(), job, project(), collect(map[string]string{}))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "busy") || time.Since(start) > 500*time.Millisecond {
		t.Errorf("build during a build: %v after %s, want busy at once", err, time.Since(start))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the lingering build: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lingering build never returned")
	}
	if n := sweeps.Load(); n != 2 {
		t.Errorf("%d sweeps when the build returned, want its own done too", n)
	}

	// A build whose sweep blocks, and one sent while it sweeps.
	go func() {
		_, err := c.Build(t.Context(), job, project(), collect(map[string]string{}))
		done <- err
	}()
	select {
	case <-sweeping:
	case err := <-done:
		t.Fatalf("the build ended before its sweep: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the build's sweep never started")
	}
	_, err = c.Build(t.Context(), job, project(), collect(map[string]string{}))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "busy") {
		t.Errorf("build during a sweep: %v, want busy", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the build whose sweep blocked: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the build whose sweep blocked never returned")
	}
}

// TestCheckDedicatedUser checks that the process list finds a stray process
// of the user, but not a zombie, and that the check then refuses to start.
func TestCheckDedicatedUser(t *testing.T) {
	t.Parallel()
	stray := exec.CommandContext(t.Context(), "sleep", "30")
	if err := stray.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stray.Process.Kill(); _ = stray.Wait() })
	// It exits at once, and stays a zombie until Wait.
	zombie := exec.CommandContext(t.Context(), "true")
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zombie.Wait() })

	inList := func(procs []proc, pid int) bool {
		return slices.ContainsFunc(procs, func(p proc) bool { return p.pid == pid })
	}
	// A zombie is never listed, so the one here leaves the list once it has
	// exited, and the stray stays.
	var procs []proc
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var err error
		if procs, err = ownProcs(); err != nil {
			t.Fatal(err)
		}
		if !inList(procs, zombie.Process.Pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the zombie %d is still listed", zombie.Process.Pid)
		}
	}
	if !slices.Contains(procs, proc{pid: stray.Process.Pid, name: "sleep"}) {
		t.Errorf("the stray sleep %d is not listed", stray.Process.Pid)
	}
	if inList(procs, os.Getpid()) {
		t.Error("the process itself is listed")
	}
	// As root, the check refuses before it looks.
	want := fmt.Sprintf("uid %d", os.Getuid())
	if os.Geteuid() == 0 {
		want = "must not run as root"
	}
	if err := CheckDedicatedUser(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("CheckDedicatedUser = %v, want a refusal naming %q", err, want)
	}
}

// TestShimChanged checks that a sweep finds a change a build made to the
// shim itself, here a lower limit on open files, and that the shim then
// stops taking builds. It changes the test process's own limit, so it does
// not run alongside other tests.
func TestShimChanged(t *testing.T) { //nolint:paralleltest // It changes the test process's own resource limit.
	base, err := snapshotSelf()
	if err != nil {
		t.Fatal(err)
	}
	sw := scopedSweeper("no process has this name")
	sw.self = func() error {
		now, err := snapshotSelf()
		if err != nil {
			return err
		}
		return base.changed(now)
	}
	c, shim := newShimServerWith(t, sw, ampleTimeout)
	job := Job{Width: 800, Height: 480, Palette: "bw"}
	if log, err := c.Build(t.Context(), job, project(), collect(map[string]string{})); err != nil {
		t.Fatalf("first build: %v; log %q", err, log)
	}

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	lower := lim
	lower.Cur--
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lower); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim) })

	// The first build's sweep ran before its response, so the next build's
	// sweep finds the change, and that build is answered as failed.
	if log, err := c.Build(t.Context(), job, project(), collect(map[string]string{})); !errors.Is(err, ErrBuildFailed) || !strings.Contains(log, "changed the renderer itself") {
		t.Fatalf("second build: %v; log %q; want it failed, saying why", err, log)
	}
	select {
	case <-shim.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("Failed was not closed")
	}
	_, err = c.Build(t.Context(), job, project(), collect(map[string]string{}))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "changed the renderer itself") {
		t.Errorf("build after the change: err %v", err)
	}
}

// TestDisplayFails checks that an Xvfb that exits, names no display, or
// hangs is an outage, not a failed build, and is not left running.
func TestDisplayFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, script string }{
		{"exits", "#!/bin/sh\necho $$ > \"$XVFB_PID\"\nexit 1\n"},
		{"garbage", "#!/bin/sh\necho $$ > \"$XVFB_PID\"\necho x >&3\nexec sleep 600\n"},
		// Only this case waits out displayTimeout.
		{"hangs", "#!/bin/sh\necho $$ > \"$XVFB_PID\"\nexec sleep 600\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pidFile := filepath.Join(t.TempDir(), "pid")
			c, _ := newShimServerXvfb(t, scopedSweeper("no process has this name"), tc.script, ampleTimeout, "XVFB_PID="+pidFile)
			_, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
			if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBuildFailed) || !strings.Contains(err.Error(), "start Xvfb") {
				t.Errorf("err %v, want an outage naming Xvfb", err)
			}
			b, err := os.ReadFile(pidFile) //nolint:gosec // G304: the test's own file.
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			if processAlive(pid) {
				t.Errorf("Xvfb %d is still running", pid)
			}
		})
	}
}

// TestListed checks which processes a sweep waits for: the user's live
// ones, and, as process 1, which must reap them, its zombies too, since
// each holds a process ID until it is reaped.
func TestListed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		zombie, mine, reaper bool
		want                 bool
	}{
		{"live", false, true, false, true},
		{"zombie", true, true, false, false},
		{"zombie, as process 1", true, true, true, true},
		{"another user's zombie, as process 1", true, false, true, false},
		{"another user's", false, false, false, false},
	} {
		if got := listed(proc{zombie: tc.zombie}, tc.mine, tc.reaper); got != tc.want {
			t.Errorf("%s: listed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRunCounts checks that a sweep counts only live processes as killed,
// not the zombies it lists as process 1 to reap them, which a build that
// behaves leaves, and that it keeps killing until none is listed.
func TestRunCounts(t *testing.T) {
	t.Parallel()
	lists := [][]proc{
		{{pid: 10, name: "sleep"}, {pid: 11, name: "firefox", zombie: true}},
		{{pid: 11, name: "firefox", zombie: true}},
		nil,
	}
	kills := 0
	sw := sweeper{
		list: func() ([]proc, error) {
			l := lists[0]
			if len(lists) > 1 {
				lists = lists[1:]
			}
			return l, nil
		},
		kill:    func() error { kills++; return nil },
		timeout: 10 * time.Second,
	}
	killed, err := sw.run()
	if err != nil || killed != 1 || kills != 2 {
		t.Errorf("run = %d, %v after %d kills; want 1 killed after 2", killed, err, kills)
	}
}

// TestClientWaitsForSweep checks that a build the client gives up on part
// way, here because storing a PNG failed, does not leave the shim busy for
// the next build: the shim swept up before its response started.
func TestClientWaitsForSweep(t *testing.T) {
	t.Parallel()
	sw := scopedSweeper("no process has this name")
	// A slow sweep, so a client that left early would find it running.
	sw.self = func() error { time.Sleep(300 * time.Millisecond); return nil }
	c, shim := newShimServerWith(t, sw, ampleTimeout)
	shim.gate.mu.Lock()
	shim.gate.limit = 1
	shim.gate.mu.Unlock()
	job := Job{Width: 800, Height: 480, Palette: "bw"}
	failed := errors.New("disk full")
	_, err := c.Build(t.Context(), job, project(), func(string, io.Reader) error { return failed })
	if !errors.Is(err, failed) {
		t.Fatalf("first build: %v, want the PNG's error", err)
	}
	if log, err := c.Build(t.Context(), job, project(), collect(map[string]string{})); err != nil {
		t.Errorf("the next build: %v; log %q", err, log)
	}
}

// TestBuildDirRemovedAfterSweep checks that a process a build left, still
// writing into the build's directory, does not keep the shim from finishing
// the build: the directory is removed only after the sweep has killed it,
// so the removal finishes, the directory is gone, and the next build runs.
// Removed before the sweep, the directory would be left, or, with writers
// fast enough, the removal would never end.
func TestBuildDirRemovedAfterSweep(t *testing.T) {
	t.Parallel()
	bin := leftoverBinary(t)
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	tmp, err := os.OpenRoot(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	c, _ := newShimServerWith(t, scopedSweeper(filepath.Base(bin)), ampleTimeout,
		"LEFTOVER_BIN="+bin, "LEFTOVER_DIR="+dir, "LEFTOVER_MODE=spam")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	job := Job{Width: 800, Height: 480, Palette: "bw"}
	if log, err := c.Build(ctx, job, project("leftover"), collect(map[string]string{})); err != nil {
		t.Fatalf("the build that left a writer: %v; log %q", err, log)
	}
	b, err := root.ReadFile("builddir")
	if err != nil {
		t.Fatal(err)
	}
	build := filepath.Base(string(b))
	t.Cleanup(func() { _ = tmp.RemoveAll(build) })
	if _, err := tmp.Stat(build); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the build's directory %s is still there: %v", build, err)
	}
	if log, err := c.Build(ctx, job, project(), collect(map[string]string{})); err != nil {
		t.Errorf("the next build: %v; log %q", err, log)
	}
}

// TestSweepFailsBuild checks that a build whose sweep cannot finish, as when
// it nests directories past the cleanup's bound, is answered as a failed
// build with the reason, so it counts against that build, not the next,
// and that the shim then refuses builds.
func TestSweepFailsBuild(t *testing.T) {
	t.Parallel()
	var sweeps atomic.Int32
	sw := scopedSweeper("no process has this name")
	sw.clean = func() (int, error) {
		if sweeps.Add(1) > 1 { // The first sweep runs when the gate is made.
			return 0, errors.New("a build nested directories over 256 deep")
		}
		return 0, nil
	}
	c, shim := newShimServerWith(t, sw, ampleTimeout)
	job := Job{Width: 800, Height: 480, Palette: "bw"}
	got := map[string]string{}
	log, err := c.Build(t.Context(), job, project(), collect(got))
	if !errors.Is(err, ErrBuildFailed) || !strings.Contains(log, "nested directories over 256 deep") || len(got) != 0 {
		t.Errorf("the build that broke its sweep: err %v, log %q, %d PNGs; want a failed build saying why", err, log, len(got))
	}
	select {
	case <-shim.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("Failed was not closed")
	}
	if _, err := c.Build(t.Context(), job, project(), collect(map[string]string{})); !errors.Is(err, ErrUnavailable) {
		t.Errorf("the next build: %v, want ErrUnavailable", err)
	}
}

// TestNestedTempDir checks that a TMPDIR inside another cleanup root, which
// a sweep removes, is made again at the next build. It sets TMPDIR, so it
// does not run alongside other tests.
func TestNestedTempDir(t *testing.T) {
	outer := t.TempDir()
	nested := filepath.Join(outer, "a", "tmpdir")
	t.Setenv("TMPDIR", nested)
	c := newShimServer(t, ampleTimeout)
	job := Job{Width: 800, Height: 480, Palette: "bw"}
	for i := range 2 {
		// As the sweep of the outer root would.
		if err := os.RemoveAll(filepath.Join(outer, "a")); err != nil {
			t.Fatal(err)
		}
		if log, err := c.Build(t.Context(), job, project(), collect(map[string]string{})); err != nil {
			t.Fatalf("build %d with TMPDIR gone: %v; log %q", i+1, err, log)
		}
	}
}

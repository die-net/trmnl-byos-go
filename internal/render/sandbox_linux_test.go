//go:build linux

package render

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/die-net/sandbox"

	"github.com/die-net/trmnl-byos-go/internal/testutil/sandboxtest"
)

// sandboxedCommand runs the build or self-test command when a sandbox
// started the test binary as its command, and reports whether it did.
func sandboxedCommand() (int, bool) {
	if len(os.Args) < 2 {
		return 0, false
	}
	var code int
	var err error
	switch os.Args[1] {
	case BuildCommand:
		code, err = RunBuildCommand(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.NewFile(StartedFD, "started"))
	case SelfTestCommand:
		code, err = RunSelfTest(context.Background(), os.Args[2:], os.Stdout)
	default:
		return 0, false
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return code, true
}

// hogMemory takes mib MiB of memory and touches every page of it, so it is
// resident, then waits to be killed.
func hogMemory(mib int) {
	b := make([]byte, mib<<20)
	for i := 0; i < len(b); i += 4096 {
		b[i] = 1
	}
	time.Sleep(time.Minute)
	fmt.Fprintln(os.Stderr, len(b))
}

// sandboxTrmnlp stands in for trmnlp in a sandboxed build. It checks the
// project and the environment the build gets, writes each layout's "PNG" as
// a line naming the layout, size, palette and FIELD_TOKEN, and fails,
// hangs, hogs memory or looks where it must not when the project asks it
// to. Asked for a failed data fetch, it does as trmnlp does: warns, marks
// the failure in its cache, and still draws the screens and exits 0.
const sandboxTrmnlp = `#!/bin/sh
while [ $# -gt 0 ]; do
  case $1 in
    -d) dir=$2; shift ;;
    --width) w=$2; shift ;;
    --height) h=$2; shift ;;
    --palette) p=$2; shift ;;
  esac
  shift
done
test -f "$dir/src/settings.yml" || { echo "no settings.yml" >&2; exit 2; }
test "$DISPLAY" = ":7" || { echo "no display" >&2; exit 5; }
case "$HTTPS_PROXY" in http://127.0.0.1:*) ;; *) echo "no proxy: $HTTPS_PROXY" >&2; exit 6 ;; esac
test -n "$XDG_CACHE_HOME" && test -d "$TMPDIR" || { echo "no build dirs" >&2; exit 4; }
test -f "$dir/fail" && { echo "boom" >&2; exit 3; }
test -f "$dir/slow" && sleep 30
test -f "$dir/hog" && TRMNL_TEST_HOG_MIB=$(cat "$dir/hog") /dev/sandbox-stage
if test -f "$dir/peek"; then
  test -e "$(cat "$dir/peek")" && { echo "saw $(cat "$dir/peek")" >&2; exit 7; }
fi
if test -f "$dir/alone"; then
  n=$(ls -A "$(cat "$dir/alone")" | wc -l)
  test "$n" = 1 || { echo "saw $n homes" >&2; exit 8; }
fi
if test -f "$dir/fetchfail"; then
  mkdir -p "$XDG_CACHE_HOME/trmnl/state"
  : > "$XDG_CACHE_HOME/trmnl/state/project.fetch_failed"
  echo "warning: HTTP 401 from https://api.example/forecast"
fi
mkdir -p "$dir/_build"
for v in full half_horizontal half_vertical quadrant; do
  printf '%s %sx%s %s %s' "$v" "$w" "$h" "$p" "$FIELD_TOKEN" > "$dir/_build/$v.png"
done
test -f "$dir/slowdone" && sleep 1
echo built
`

// probed is ProbeSandbox's answer, asked once for every test, since it
// starts a sandbox of its own.
var probed = sync.OnceValue(func() error {
	_, err := ProbeSandbox()
	return err
})

// requireSandbox skips a test where no sandbox can start, saying why, or
// fails it where the run requires sandboxes (see sandboxtest).
func requireSandbox(t *testing.T) {
	t.Helper()
	if err := probed(); err != nil {
		sandboxtest.Unavailable(t, err)
	}
}

// fakeTools makes a directory of stand-ins for every tool a build needs,
// with trmnlp as sandboxTrmnlp, and returns it, for SandboxConfig.PathEnv.
func fakeTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{
		"trmnlp":      sandboxTrmnlp,
		"ruby":        "#!/bin/sh\n",
		"firefox":     "#!/bin/sh\n",
		"geckodriver": "#!/bin/sh\n",
		"magick":      "#!/bin/sh\n",
		"Xvfb":        fakeXvfb,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake command must be executable.
			t.Fatal(err)
		}
	}
	return dir
}

// testProxy answers every proxy request with 502, as the proxy does for a
// refused address, and counts them.
type testProxy struct {
	mu   sync.Mutex
	seen []string
}

func (p *testProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.seen = append(p.seen, r.Method+" "+r.Host)
	p.mu.Unlock()
	http.Error(w, "refused", http.StatusBadGateway)
}

// newTestSandbox returns a Sandbox with fake tools, a data directory it
// covers, and cfg's Memory and Timeout if set.
func newTestSandbox(t *testing.T, cfg SandboxConfig) (*Sandbox, string) {
	t.Helper()
	requireSandbox(t)
	data, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "trmnl.db"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.TempDir = filepath.Join(data, "tmp")
	cfg.Cover = []string{data}
	if cfg.Proxy == nil {
		cfg.Proxy = &testProxy{}
	}
	if cfg.PathEnv == "" {
		cfg.PathEnv = fakeTools(t)
	}
	if cfg.Memory == 0 {
		cfg.Memory = 1 << 30
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = ampleTimeout
	}
	sb, err := NewSandbox(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sb, data
}

// homesLeft returns the builds' homes left in the sandbox's TempDir.
func homesLeft(t *testing.T, sb *Sandbox) []string {
	t.Helper()
	entries, err := os.ReadDir(sb.tempDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// projectWith writes a minimal project, plus each of files with its
// content.
func projectWith(files map[string]string) func(*tar.Writer) error {
	return func(tw *tar.Writer) error {
		for _, name := range []string{"src/settings.yml", ".trmnlp.yml"} {
			if err := AddFile(tw, name, 1, strings.NewReader("x")); err != nil {
				return err
			}
		}
		for name, body := range files {
			if err := AddFile(tw, name, int64(len(body)), strings.NewReader(body)); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestSandboxBuild(t *testing.T) {
	t.Parallel()
	sb, _ := newTestSandbox(t, SandboxConfig{})
	got := map[string]string{}
	log, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "gray-4", Env: map[string]string{"FIELD_TOKEN": "abc"}},
		project(), collect(got))
	if err != nil {
		t.Fatalf("Build: %v; log %q", err, log)
	}
	if !strings.Contains(log, "built") {
		t.Errorf("log %q", log)
	}
	for _, layout := range Layouts {
		if want := layout + " 800x480 gray-4 abc"; got[layout] != want {
			t.Errorf("%s: %q, want %q", layout, got[layout], want)
		}
	}
	if left := homesLeft(t, sb); len(left) != 0 {
		t.Errorf("homes left after the build: %v", left)
	}
	if p := sb.Problem(); p != "" {
		t.Errorf("Problem() = %q after a build that started", p)
	}
}

// TestSandboxBuildFails checks each way a build fails and what it counts
// as: trmnlp failing, a failed data fetch, a project the build command
// refuses, the timeout, and the memory limit. None is an outage, and each
// leaves no home behind.
func TestSandboxBuildFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		files   map[string]string
		memory  int64
		timeout time.Duration
		// is is the error the build must match, or nil.
		is error
		// msg is in the error.
		msg string
		// log is in the log.
		log string
	}{
		{name: "trmnlp fails", files: map[string]string{"fail": ""}, is: ErrBuildFailed, log: "boom"},
		{name: "fetch fails", files: map[string]string{"fetchfail": ""}, is: ErrFetchFailed, log: "HTTP 401"},
		{name: "project leaves its directory", files: map[string]string{"../escape": "x"}, is: ErrRejected, msg: "leaves the project"},
		{name: "timeout", files: map[string]string{"slow": ""}, timeout: 2 * time.Second, msg: "took over 2s"},
		{name: "over memory", files: map[string]string{"hog": "600"}, memory: 300 << 20, msg: "over 300 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb, _ := newTestSandbox(t, SandboxConfig{Memory: tc.memory, Timeout: tc.timeout})
			start := time.Now()
			log, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, projectWith(tc.files), collect(map[string]string{}))
			if err == nil {
				t.Fatalf("Build succeeded; log %q", log)
			}
			if errors.Is(err, ErrUnavailable) {
				t.Errorf("Build: %v, an outage, want a failed build", err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("Build: %v, want %v", err, tc.is)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("Build: %v, want %q in it", err, tc.msg)
			}
			if !strings.Contains(log, tc.log) {
				t.Errorf("log %q, want %q in it", log, tc.log)
			}
			if tc.timeout != 0 && time.Since(start) > 20*time.Second {
				t.Errorf("Build took %s with a timeout of %s", time.Since(start), tc.timeout)
			}
			if left := homesLeft(t, sb); len(left) != 0 {
				t.Errorf("homes left after the build: %v", left)
			}
		})
	}
}

// TestSandboxCanceled checks that a build whose context ends stops at once,
// returns the context's error, which is no build failure, and leaves no
// home behind.
func TestSandboxCanceled(t *testing.T) {
	t.Parallel()
	sb, _ := newTestSandbox(t, SandboxConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(time.Second, cancel)
	start := time.Now()
	_, err := sb.Build(ctx, Job{Width: 800, Height: 480, Palette: "bw"}, projectWith(map[string]string{"slow": ""}), collect(map[string]string{}))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Build: %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("Build took %s after a cancel at 1s", d)
	}
	if left := homesLeft(t, sb); len(left) != 0 {
		t.Errorf("homes left after the build: %v", left)
	}
}

// TestSandboxCannotStart checks that a sandbox that cannot be set up is an
// outage, which says to run doctor, that Problem says why until a build
// starts, and that a build that starts clears it.
func TestSandboxCannotStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  SandboxConfig
		msg  string
	}{
		{name: "trmnlp missing", cfg: SandboxConfig{PathEnv: "/nonexistent"}, msg: "trmnlp"},
		{name: "cgroup missing", cfg: SandboxConfig{Cgroup: "/nonexistent/cgroup"}, msg: "cgroup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb, _ := newTestSandbox(t, tc.cfg)
			_, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Build: %v, want ErrUnavailable", err)
			}
			for _, want := range []string{tc.msg, "trmnl-byos doctor"} {
				if !strings.Contains(err.Error(), want) || !strings.Contains(sb.Problem(), want) {
					t.Errorf("Build: %v, Problem %q, want %q in each", err, sb.Problem(), want)
				}
			}
			if left := homesLeft(t, sb); len(left) != 0 {
				t.Errorf("homes left: %v", left)
			}
			// Once the host is fixed, the next build starts, and the problem
			// is gone.
			sb.cfg.PathEnv, sb.cfg.Cgroup = fakeTools(t), ""
			if _, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{})); err != nil {
				t.Fatalf("Build after the fix: %v", err)
			}
			if p := sb.Problem(); p != "" {
				t.Errorf("Problem() = %q after a build that started", p)
			}
		})
	}
}

// TestSandboxConcurrent runs two builds at once, each with its own value,
// and checks that each gets its own result, and that neither sees the data
// directory or the other's home.
func TestSandboxConcurrent(t *testing.T) {
	t.Parallel()
	sb, data := newTestSandbox(t, SandboxConfig{})
	var wg sync.WaitGroup
	results := make([]map[string]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		results[i] = map[string]string{}
		wg.Go(func() {
			// Each looks for the database by its host path, and sees only
			// its own home where the homes are.
			files := map[string]string{"peek": filepath.Join(data, "trmnl.db"), "alone": sb.tempDir, "slowdone": ""}
			_, errs[i] = sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"FIELD_TOKEN": fmt.Sprint("token", i)}},
				projectWith(files), collect(results[i]))
		})
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("build %d: %v", i, errs[i])
		}
		if want := fmt.Sprintf("full 800x480 bw token%d", i); results[i]["full"] != want {
			t.Errorf("build %d: %q, want %q", i, results[i]["full"], want)
		}
	}
}

// TestSandboxProxy checks that the build's proxy variables lead to the
// Proxy handler, through the listener of the build's own, and that the
// self-test reports what the proxy answered.
func TestSandboxProxy(t *testing.T) {
	t.Parallel()
	proxy := &testProxy{}
	sb, _ := newTestSandbox(t, SandboxConfig{Proxy: proxy})
	results, err := sb.SelfTest(t.Context(), []string{"127.0.0.1:8080"}, []string{"example.com:443"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]sandbox.Status{"proxy.refused:127.0.0.1:8080": sandbox.Pass, "proxy.allowed:example.com:443": sandbox.Fail}
	if len(results) != len(want) {
		t.Fatalf("results %+v, want %v", results, want)
	}
	for _, r := range results {
		if st, ok := want[r.Name]; !ok || r.Status != st {
			t.Errorf("%+v, want %v", r, want)
		}
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if len(proxy.seen) != 2 || proxy.seen[0] != "CONNECT 127.0.0.1:8080" {
		t.Errorf("the proxy saw %v", proxy.seen)
	}
}

// TestSandboxCheck runs the library's checks in the sandbox a build gets,
// and requires that it reaches nothing on the host's loopback, where the
// server listens, and reads nothing of the data directory.
func TestSandboxCheck(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sb, _ := newTestSandbox(t, SandboxConfig{})
	tools, err := sb.setup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := sb.Spec(*tools, home)
	res, err := sandbox.Check(t.Context(), &spec, sandbox.Expect{Unreachable: []string{"tcp:" + ln.Addr().String()}, Unreadable: sb.cfg.Cover})
	if err != nil {
		t.Fatal(err)
	}
	expects := 0
	for _, r := range res {
		if strings.HasPrefix(r.Name, "expect.") {
			expects++
			if r.Status != sandbox.Pass {
				t.Errorf("%+v", r)
			}
		}
	}
	if expects != 2 {
		t.Errorf("%d expect results, want 2: %+v", expects, res)
	}
}

// TestSandboxGemPathFails checks that a ruby that cannot give its gem path is
// an outage whose Problem names the gem path and says to run doctor, with no
// sandbox started, and that the next build looks again and gets past it once
// ruby works. This needs no working sandbox, since it fails before one starts.
func TestSandboxGemPathFails(t *testing.T) {
	t.Parallel()
	dir := fakeTools(t)
	//nolint:gosec // G306: the fake command must be executable.
	if err := os.WriteFile(filepath.Join(dir, "ruby"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb, _ := newUnitSandbox(t, SandboxConfig{PathEnv: dir, Proxy: &testProxy{}, Memory: 1 << 30, Timeout: ampleTimeout})

	if _, err := sb.setup(t.Context()); err == nil || !strings.Contains(err.Error(), "gem path") {
		t.Fatalf("setup: %v, want an error mentioning the gem path", err)
	}
	_, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Build: %v, want ErrUnavailable", err)
	}
	for _, want := range []string{"gem path", "trmnl-byos doctor"} {
		if !strings.Contains(err.Error(), want) || !strings.Contains(sb.Problem(), want) {
			t.Errorf("Build: %v, Problem %q, want %q in each", err, sb.Problem(), want)
		}
	}
	if left := homesLeft(t, sb); len(left) != 0 {
		t.Errorf("homes left: %v", left)
	}

	//nolint:gosec // G306: the fake command must be executable.
	if err := os.WriteFile(filepath.Join(dir, "ruby"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.setup(t.Context()); err != nil {
		t.Errorf("setup after ruby was fixed: %v", err)
	}
}

// TestSandboxToolRemoved checks that a sandbox that has found its tools looks
// again when one is removed, as an upgrade can: the next build is an outage
// naming it rather than a failed start, and once the tool is back the build
// after that succeeds.
func TestSandboxToolRemoved(t *testing.T) {
	t.Parallel()
	dir := fakeTools(t)
	sb, _ := newTestSandbox(t, SandboxConfig{PathEnv: dir})
	build := func() error {
		_, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
		return err
	}
	if err := build(); err != nil {
		t.Fatalf("first Build: %v", err)
	}

	trmnlp := filepath.Join(dir, "trmnlp")
	if err := os.Remove(trmnlp); err != nil {
		t.Fatal(err)
	}
	err := build()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Build without trmnlp: %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "trmnlp") || !strings.Contains(sb.Problem(), "trmnlp") {
		t.Errorf("Build: %v, Problem %q, want trmnlp named in each", err, sb.Problem())
	}

	//nolint:gosec // G306: the fake command must be executable.
	if err := os.WriteFile(trmnlp, []byte(sandboxTrmnlp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := build(); err != nil {
		t.Fatalf("Build with trmnlp restored: %v", err)
	}
	if p := sb.Problem(); p != "" {
		t.Errorf("Problem() = %q after a build that started", p)
	}
}

// TestSandboxStageDies checks that a sandbox that ends before its build
// command says it started is an outage, which Problem names, not a failed
// build: nothing of the recipe ran. It makes the command refuse its
// arguments, as a first stage that dies does, by dropping trmnlp from the
// tools after they were found. A later build that starts clears Problem.
func TestSandboxStageDies(t *testing.T) {
	t.Parallel()
	sb, _ := newTestSandbox(t, SandboxConfig{})
	tools, err := sb.setup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	broken := *tools
	broken.List = slices.Clone(tools.List)
	for i := range broken.List {
		if broken.List[i].Name == "trmnlp" {
			broken.List[i].Path = ""
		}
	}
	sb.mu.Lock()
	sb.tools = &broken
	sb.mu.Unlock()
	_, err = sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Build: %v, want ErrUnavailable", err)
	}
	for _, want := range []string{"before its command started", "usage"} {
		if !strings.Contains(err.Error(), want) || !strings.Contains(sb.Problem(), want) {
			t.Errorf("Build: %v, Problem %q, want %q in each", err, sb.Problem(), want)
		}
	}
	sb.mu.Lock()
	sb.tools = tools
	sb.mu.Unlock()
	if _, err := sb.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{})); err != nil {
		t.Fatalf("Build once fixed: %v", err)
	}
	if p := sb.Problem(); p != "" {
		t.Errorf("Problem() = %q after a build that started", p)
	}
}

// TestWriteProbeProfile writes the AppArmor profile sandbox.Probe prints
// for this test binary into TRMNL_TEST_APPARMOR_DIR, for CI's sandbox job to
// load before it runs the binary, so CI uses the profile an operator is told
// to load. It does nothing without that variable.
func TestWriteProbeProfile(t *testing.T) {
	t.Parallel()
	sandboxtest.WriteProbeProfile(t)
}

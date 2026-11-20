//go:build linux

package render

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeTrmnlp stands in for trmnlp: it checks the project and its display,
// writes each layout's "PNG" as a line naming the layout, size, palette and
// secret, and fails or hangs when the project asks it to. Asked for a failed
// data fetch, it does as trmnlp does: warns, marks the failure in its cache,
// and still draws the screens and exits 0. Asked to, it starts a process
// that outlives it (see TestLeftoverKilled), lingers after drawing, plants
// files and looks for them (see TestPlantedFilesRemoved), or leaves a FIFO
// or an oversized PNG where the shim reads its result (see TestBadResult).
const fakeTrmnlp = `#!/bin/sh
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
test -f "$dir/leftover" && TRMNL_TEST_LEFTOVER=spawn "$LEFTOVER_BIN"
test -f "$dir/fail" && { echo "boom" >&2; exit 3; }
test -f "$dir/slow" && { sleep 30 & echo $! > "$CHILD_PID_FILE"; wait; }
test -n "$XDG_CACHE_HOME" && test -d "$TMPDIR" || { echo "no build dirs" >&2; exit 4; }
if test -f "$dir/fetchfail"; then
  mkdir -p "$XDG_CACHE_HOME/trmnl/state"
  : > "$XDG_CACHE_HOME/trmnl/state/project.fetch_failed"
  echo "warning: HTTP 401 from https://api.example/forecast"
fi
mkdir -p "$dir/_build"
for v in full half_horizontal half_vertical quadrant; do
  printf '%s %sx%s %s %s' "$v" "$w" "$h" "$p" "$FIELD_TOKEN" > "$dir/_build/$v.png"
done
test -f "$dir/linger" && sleep 1
if test -f "$dir/lingerseen"; then
  for i in $(seq 200); do grep -q "$FIELD_TOKEN" "$LEFTOVER_DIR/out" 2>/dev/null && break; sleep 0.05; done
fi
test -f "$dir/fifolog" && rm -f "$dir/../build.log" && mkfifo "$dir/../build.log"
test -f "$dir/fifopng" && rm -f "$dir/_build/full.png" && mkfifo "$dir/_build/full.png"
test -f "$dir/bigpng" && dd if=/dev/zero of="$dir/_build/full.png" bs=1048576 count=9 2>/dev/null
test -f "$dir/biglog" && head -c 70000 /dev/zero | tr '\0' x
test -f "$dir/chmoddir" && (cd "$dir/.." && pwd) > "$CHMOD_RECORD" && chmod 000 "$dir/.."
test -f "$dir/plant" && . "$PLANT_SCRIPT"
test -f "$dir/check" && . "$CHECK_SCRIPT"
echo built
`

// fakeXvfb stands in for Xvfb: it names display 7 on the descriptor it is
// given, then waits to be stopped.
const fakeXvfb = `#!/bin/sh
test "$1" = -displayfd && test "$2" = 3 || exit 2
echo 7 >&3
exec sleep 600
`

// ampleTimeout is the build timeout of a shim whose builds are meant to
// finish. It is the shim's own default, so a fork and exec of the fake
// trmnlp on a starved machine does not time out and fail a test about
// something else. Only the "slow" case of TestBuildFails, which tests the
// timeout, uses a short one.
const ampleTimeout = 2 * time.Minute

// newShimServer starts a shim with fake trmnlp and Xvfb commands, whose
// sweep can kill nothing.
func newShimServer(t *testing.T, timeout time.Duration, env ...string) *Client {
	t.Helper()
	c, _ := newShimServerWith(t, scopedSweeper("no process has this name"), timeout, env...)
	return c
}

// newShimServerWith starts a shim whose sweeps use sw. A test must never
// run the shim's own sweeper, which would kill every process of the user
// running the tests and remove its files.
func newShimServerWith(t *testing.T, sw sweeper, timeout time.Duration, env ...string) (*Client, *Shim) {
	t.Helper()
	return newShimServerXvfb(t, sw, fakeXvfb, timeout, env...)
}

// newShimServerXvfb starts a shim whose sweeps use sw, with the Xvfb script
// xvfbScript.
func newShimServerXvfb(t *testing.T, sw sweeper, xvfbScript string, timeout time.Duration, env ...string) (*Client, *Shim) {
	t.Helper()
	dir := t.TempDir()
	trmnlp, xvfb := filepath.Join(dir, "trmnlp"), filepath.Join(dir, "Xvfb")
	for path, script := range map[string]string{trmnlp: fakeTrmnlp, xvfb: xvfbScript} {
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake command must be executable.
			t.Fatal(err)
		}
	}
	// Never NewShim, whose sweep would start at once.
	shim := &Shim{Trmnlp: trmnlp, Xvfb: xvfb, Env: append([]string{"PATH=" + os.Getenv("PATH")}, env...), Timeout: timeout, gate: newGate(sw, 2)}
	srv := httptest.NewServer(shim)
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, HTTP: srv.Client()}, shim
}

func TestBuild(t *testing.T) {
	t.Parallel()
	c := newShimServer(t, ampleTimeout)
	got := map[string]string{}
	log, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "gray-4", Env: map[string]string{"FIELD_TOKEN": "abc"}},
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
}

// TestBuildFails checks that a failed or timed-out trmnlp run returns
// ErrBuildFailed with the build log, and no PNGs, and that a timed-out
// build's child processes are killed with it.
func TestBuildFails(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	for _, tc := range []struct {
		name, marker, inLog string
		c                   *Client
	}{
		{"exit status", "fail", "boom", newShimServer(t, ampleTimeout)},
		// Only this case may time out: its script sleeps for 30 s.
		{"timeout", "slow", "", newShimServer(t, 500*time.Millisecond, "CHILD_PID_FILE="+pidFile)},
	} {
		got := map[string]string{}
		log, err := tc.c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(tc.marker), collect(got))
		if !errors.Is(err, ErrBuildFailed) || !strings.Contains(log, tc.inLog) || len(got) != 0 {
			t.Errorf("%s: err %v, log %q, %d PNGs", tc.name, err, log, len(got))
		}
	}
	pid, err := fs.ReadFile(os.DirFS(filepath.Dir(pidFile)), filepath.Base(pidFile))
	if err != nil {
		t.Fatalf("the slow build never started its child: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(pid)))
	if err != nil {
		t.Fatal(err)
	}
	// The kill is a signal to the group, which the build does not wait on:
	// the child is not the shim's own, so only its new parent can reap it,
	// and until then its PID still answers. So give it a moment to go.
	for deadline := time.Now().Add(5 * time.Second); processAlive(n); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("child %d of a timed-out build is still running", n)
			break
		}
	}
}

// TestBuildFetchFails checks that a build whose data fetch failed, which
// trmnlp finishes with screens drawn from no data and exit status 0,
// returns ErrFetchFailed with the build log and no PNGs, so the last good
// images stay.
func TestBuildFetchFails(t *testing.T) {
	t.Parallel()
	c := newShimServer(t, ampleTimeout)
	got := map[string]string{}
	log, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project("fetchfail"), collect(got))
	if !errors.Is(err, ErrFetchFailed) || errors.Is(err, ErrBuildFailed) || !strings.Contains(log, "warning: HTTP 401") || len(got) != 0 {
		t.Errorf("err %v, log %q, %d PNGs; want ErrFetchFailed, the warning and none", err, log, len(got))
	}
}

// TestShimRejects checks that a bad job or a project that would write
// outside its directory, or a request without the job first, is refused before trmnlp runs.
func TestShimRejects(t *testing.T) {
	t.Parallel()
	c := newShimServer(t, ampleTimeout)
	for _, tc := range []struct {
		name    string
		first   string
		job     string
		entries []tar.Header
	}{
		{"no job", "", "", nil},
		{"job not first", "src/job.json", `{"width":800,"height":480,"palette":"bw"}`, nil},
		{"bad palette", jobMember, `{"width":800,"height":480,"palette":"../x"}`, nil},
		{"bad size", jobMember, `{"width":0,"height":480,"palette":"bw"}`, nil},
		{"env not FIELD_", jobMember, `{"width":800,"height":480,"palette":"bw","env":{"LD_PRELOAD":"/x"}}`, nil},
		{"path escape", jobMember, `{"width":800,"height":480,"palette":"bw"}`, []tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600}}},
		{"absolute path", jobMember, `{"width":800,"height":480,"palette":"bw"}`, []tar.Header{{Name: "/tmp/escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600}}},
		{"symlink", jobMember, `{"width":800,"height":480,"palette":"bw"}`, []tar.Header{{Name: "src/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}},
	} {
		var body bytes.Buffer
		tw := tar.NewWriter(&body)
		if tc.first != "" {
			_ = tw.WriteHeader(&tar.Header{Name: tc.first, Typeflag: tar.TypeReg, Size: int64(len(tc.job)), Mode: 0o600})
			_, _ = io.WriteString(tw, tc.job)
		}
		for _, h := range tc.entries {
			_ = tw.WriteHeader(&h)
			if h.Size > 0 {
				_, _ = tw.Write([]byte("x"))
			}
		}
		_ = tw.Close()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.URL+"/build", &body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-tar")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", tc.name, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "escape")); err == nil {
		t.Error("a project file escaped to the temp dir")
	}
}

// TestUnavailable checks which failures count as the renderer's, an
// outage the scheduler does not count against the build: a closed port, a
// 503, and a connection lost or an error status before the shim answers 102
// Processing. A connection lost or an error status after it may be the
// build's doing, as when a recipe kills the shim, so it is neither an
// outage nor a failed build by trmnlp, and counts. The shim's 422 is a
// failed build.
func TestUnavailable(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	server := func(t *testing.T, h http.HandlerFunc) *Client {
		t.Helper()
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return &Client{URL: srv.URL, HTTP: srv.Client()}
	}
	drop := func(w http.ResponseWriter) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	shim := newShimServer(t, ampleTimeout)

	for _, tc := range []struct {
		name        string
		c           *Client
		marker      string
		unavailable bool
		buildFailed bool
	}{
		{"closed port", &Client{URL: closedURL, HTTP: http.DefaultClient}, "", true, false},
		{"503", server(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}), "", true, false},
		{"503 after 102", server(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusProcessing)
			http.Error(w, "start Xvfb", http.StatusServiceUnavailable)
		}), "", true, false},
		{"lost before 102", server(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			drop(w)
		}), "", true, false},
		{"500 before 102", server(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "oops", http.StatusInternalServerError)
		}), "", true, false},
		{"lost after 102", server(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusProcessing)
			drop(w)
		}), "", false, false},
		{"500 after 102", server(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusProcessing)
			http.Error(w, "oops", http.StatusInternalServerError)
		}), "", false, false},
		{"422", shim, "fail", false, true},
	} {
		_, err := tc.c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(tc.marker), collect(map[string]string{}))
		if err == nil || errors.Is(err, ErrUnavailable) != tc.unavailable || errors.Is(err, ErrBuildFailed) != tc.buildFailed {
			t.Errorf("%s: err %v", tc.name, err)
		}
	}
}

// TestProjectUnwritable checks that a project that cannot be written, as
// when a recipe's files are gone, returns the project's error, which is not
// ErrUnavailable, so the build counts toward its backoff like any failed
// build and its error does not blame the renderer. A renderer that stops
// reading the body is still ErrUnavailable.
func TestProjectUnwritable(t *testing.T) {
	t.Parallel()
	shim := newShimServer(t, ampleTimeout)
	missing := filepath.Join(t.TempDir(), "gone")
	_, err := shim.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"},
		func(tw *tar.Writer) error { return AddDir(tw, missing, "src/") },
		collect(map[string]string{}))
	if err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRejected) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing project: err %v", err)
	}

	hangUp := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(hangUp.Close)
	c := &Client{URL: hangUp.URL, HTTP: hangUp.Client()}
	big := func(tw *tar.Writer) error {
		return AddFile(tw, "src/big", 8<<20, bytes.NewReader(make([]byte, 8<<20)))
	}
	if _, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, big, collect(map[string]string{})); !errors.Is(err, ErrUnavailable) {
		t.Errorf("renderer hung up: err %v", err)
	}
}

// TestJobSize checks that a job is encoded without HTML escaping, which the
// shim reads the same, that EnvSize is the size of what is sent, and that a
// job over the limit is refused by the client before anything is sent, as
// ErrRejected and not as an outage.
func TestJobSize(t *testing.T) {
	t.Parallel()
	html := strings.Repeat("<b>&</b>", 1000)
	size, err := EnvSize(map[string]string{"FIELD_TOKEN": html})
	if err != nil || size != len(`{"FIELD_TOKEN":""}`)+len(html) {
		t.Errorf("EnvSize = %d, %v; want the unescaped size", size, err)
	}

	var requests int
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(counting.Close)
	big := Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"FIELD_TOKEN": strings.Repeat("x", maxJobBytes)}}
	_, err = (&Client{URL: counting.URL, HTTP: counting.Client()}).Build(t.Context(), big, project(), collect(map[string]string{}))
	if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUnavailable) || requests != 0 {
		t.Errorf("oversize job: err %v after %d requests", err, requests)
	}

	// The largest Env fits, HTML in it included, and reaches the build as
	// it was written.
	shim := newShimServer(t, ampleTimeout)
	fits := Job{Width: 800, Height: 480, Palette: "bw", Env: map[string]string{"FIELD_TOKEN": html + strings.Repeat("y", MaxEnvBytes-len(`{"FIELD_TOKEN":""}`)-len(html))}}
	if n, err := EnvSize(fits.Env); err != nil || n != MaxEnvBytes {
		t.Fatalf("EnvSize = %d, %v, want %d", n, err, MaxEnvBytes)
	}
	got := map[string]string{}
	if _, err := shim.Build(t.Context(), fits, project(), collect(got)); err != nil {
		t.Fatalf("job at the limit: %v", err)
	}
	if !strings.HasSuffix(got["full"], " "+fits.Env["FIELD_TOKEN"]) {
		t.Error("the value did not reach the build as written")
	}
}

// TestBadResult checks that a build that leaves a FIFO where the shim reads
// its log or a PNG, which would block an open forever, or a PNG over the
// bound, or that makes its own directory unreadable, fails as a build, not
// an outage, with a plain reason, at once, and that the shim still takes
// the next build. The reason survives a log longer than a client keeps.
func TestBadResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		markers []string
		reason  string
	}{
		{"fifo log", []string{"fifolog"}, "build.log is not a regular file"},
		{"fifo png", []string{"fifopng"}, "full.png is not a regular file"},
		{"big png", []string{"bigpng"}, "full.png is over"},
		{"big png, long log", []string{"bigpng", "biglog"}, "full.png is over"},
		{"unreadable directory", []string{"chmoddir"}, "open the build directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := filepath.Join(t.TempDir(), "chmodded")
			// The build directory a build made unreadable is left for the
			// sweep, which this test's does not do.
			t.Cleanup(func() {
				b, err := os.ReadFile(record) //nolint:gosec // G304: the test's own file.
				if err != nil {
					return
				}
				tmp, err := os.OpenRoot(os.TempDir())
				if err != nil {
					return
				}
				defer tmp.Close()
				name := filepath.Base(strings.TrimSpace(string(b)))
				_ = tmp.Chmod(name, 0o700)
				_ = tmp.RemoveAll(name)
			})
			c := newShimServer(t, ampleTimeout, "CHMOD_RECORD="+record)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			job := Job{Width: 800, Height: 480, Palette: "bw"}
			got := map[string]string{}
			log, err := c.Build(ctx, job, project(tc.markers...), collect(got))
			if !errors.Is(err, ErrBuildFailed) || errors.Is(err, ErrUnavailable) || !strings.Contains(log, tc.reason) || len(got) != 0 {
				t.Errorf("err %v, log %q, %d PNGs; want a failed build saying %q", err, log, len(got), tc.reason)
			}
			if log, err := c.Build(ctx, job, project(), collect(map[string]string{})); err != nil {
				t.Errorf("the next build: %v; log %q", err, log)
			}
		})
	}
}

// TestResultSizes checks that the shim sends each file at the size it had
// when it was opened and checked: a PNG a process the build left grows
// afterwards is sent at its old size, and one that shrinks fails the
// response rather than sending less than its header said.
func TestResultSizes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(path string) error
		ok     bool
	}{
		{"grows", func(path string) error {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: the test's own file.
			if err != nil {
				return err
			}
			_, err = f.Write(make([]byte, 1<<20))
			return errors.Join(err, f.Close())
		}, true},
		{"shrinks", func(path string) error { return os.Truncate(path, 1) }, false},
	} {
		dir := t.TempDir()
		build := filepath.Join(dir, "project", "_build")
		if err := os.MkdirAll(build, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, layout := range Layouts {
			if err := os.WriteFile(filepath.Join(build, layout+".png"), []byte(layout+" image"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		res, err := openResult(root, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := tc.change(filepath.Join(build, "full.png")); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		werr := writeResult(&out, res, "", false)
		res.close()
		_ = root.Close()
		if (werr == nil) != tc.ok {
			t.Errorf("%s: writeResult = %v, want ok %v", tc.name, werr, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		tr := tar.NewReader(&out)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if h.Name == "full.png" && h.Size != int64(len("full image")) {
				t.Errorf("%s: full.png sent at %d bytes, want its size when checked", tc.name, h.Size)
			}
		}
	}
}

package render

import (
	"archive/tar"
	"bytes"
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

// fakeTrmnlp stands in for trmnlp: it checks the project, writes each
// layout's "PNG" as a line naming the layout, size, palette and secret, and
// fails or hangs when the project asks it to.
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
test -f "$dir/fail" && { echo "boom" >&2; exit 3; }
test -f "$dir/slow" && { sleep 30 & echo $! > "$CHILD_PID_FILE"; wait; }
test -n "$XDG_CACHE_HOME" && test -d "$TMPDIR" || { echo "no build dirs" >&2; exit 4; }
mkdir -p "$dir/_build"
for v in full half_horizontal half_vertical quadrant; do
  printf '%s %sx%s %s %s' "$v" "$w" "$h" "$p" "$FIELD_TOKEN" > "$dir/_build/$v.png"
done
echo built
`

func newShimServer(t *testing.T, timeout time.Duration, env ...string) *Client {
	t.Helper()
	script := filepath.Join(t.TempDir(), "trmnlp")
	if err := os.WriteFile(script, []byte(fakeTrmnlp), 0o700); err != nil { //nolint:gosec // G306: the fake command must be executable.
		t.Fatal(err)
	}
	shim := NewShim(script, append([]string{"PATH=" + os.Getenv("PATH")}, env...), timeout, 2)
	srv := httptest.NewServer(shim)
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, HTTP: srv.Client()}
}

// project writes a minimal project, plus any extra files.
func project(extra ...string) func(*tar.Writer) error {
	return func(tw *tar.Writer) error {
		files := append([]string{"src/settings.yml", ".trmnlp.yml"}, extra...)
		for _, name := range files {
			if err := AddFile(tw, name, 1, strings.NewReader("x")); err != nil {
				return err
			}
		}
		return nil
	}
}

// collect gathers the PNGs a build returns, which a test may hold in memory.
func collect(got map[string]string) func(string, io.Reader) error {
	return func(layout string, r io.Reader) error {
		b, err := io.ReadAll(r)
		got[layout] = string(b)
		return err
	}
}

func TestBuild(t *testing.T) {
	t.Parallel()
	c := newShimServer(t, 10*time.Second)
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
	c := newShimServer(t, 500*time.Millisecond, "CHILD_PID_FILE="+pidFile)
	for _, tc := range []struct {
		name, marker, inLog string
	}{
		{"exit status", "fail", "boom"},
		{"timeout", "slow", ""},
	} {
		got := map[string]string{}
		log, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(tc.marker), collect(got))
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

// TestShimRejects checks that a bad job or a project that would write
// outside its directory, or a request without the job first, is refused before trmnlp runs.
func TestShimRejects(t *testing.T) {
	t.Parallel()
	c := newShimServer(t, 10*time.Second)
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

// TestAddDir checks that a recipe's source directory is sent whole, with
// its files under the prefix.
func TestAddDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, body := range map[string]string{"settings.yml": "s", "full.liquid": "f", "sub/x.txt": "x"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := AddDir(tw, dir, "src"); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if len(got) != 3 || got["src/settings.yml"] != "s" || got["src/sub/x.txt"] != "x" {
		t.Fatalf("tar holds %v", got)
	}
}

// TestUnavailable checks that a renderer that is not listening or that
// answers 503 returns ErrUnavailable and not ErrBuildFailed, and that the
// shim's 422 is ErrBuildFailed and not ErrUnavailable.
func TestUnavailable(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(busy.Close)
	shim := newShimServer(t, time.Second)

	for _, tc := range []struct {
		name        string
		c           *Client
		marker      string
		unavailable bool
		buildFailed bool
	}{
		{"closed port", &Client{URL: closedURL, HTTP: http.DefaultClient}, "", true, false},
		{"503", &Client{URL: busy.URL, HTTP: busy.Client()}, "", true, false},
		{"422", shim, "fail", false, true},
	} {
		_, err := tc.c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(tc.marker), collect(map[string]string{}))
		if errors.Is(err, ErrUnavailable) != tc.unavailable || errors.Is(err, ErrBuildFailed) != tc.buildFailed {
			t.Errorf("%s: err %v", tc.name, err)
		}
	}
}

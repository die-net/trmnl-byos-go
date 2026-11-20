package render

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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

// TestRejected checks that a shim's 400 is ErrRejected, with its message,
// and not ErrUnavailable.
func TestRejected(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "read job: unexpected EOF", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	_, err := (&Client{URL: srv.URL, HTTP: srv.Client()}).Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
	if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "read job: unexpected EOF") {
		t.Errorf("err = %v", err)
	}
}

// TestClientRefusesBigPNG checks that the client refuses a PNG over
// maxPNGBytes before it hands it on to be stored, whatever the renderer
// sends.
func TestClientRefusesBigPNG(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusProcessing)
		w.Header().Set("Content-Type", "application/x-tar")
		tw := tar.NewWriter(w)
		_ = tw.WriteHeader(&tar.Header{Name: logMember, Mode: 0o600, Typeflag: tar.TypeReg})
		_ = tw.WriteHeader(&tar.Header{Name: "full.png", Mode: 0o600, Size: maxPNGBytes + 1, Typeflag: tar.TypeReg})
		_, _ = io.CopyN(tw, zeros{}, maxPNGBytes+1)
		_ = tw.Close()
	}))
	t.Cleanup(srv.Close)
	stored := false
	_, err := (&Client{URL: srv.URL, HTTP: srv.Client()}).Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(),
		func(string, io.Reader) error { stored = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "over the limit") || stored {
		t.Errorf("Build = %v, stored %v; want a refusal before storing", err, stored)
	}
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestResultWithout102 checks that a result that came with no 102 Processing
// before it, as through an HTTP proxy that drops 1xx responses, is an error
// that says so, and counts, rather than passing as a good build or an
// outage.
func TestResultWithout102(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-tar")
		tw := tar.NewWriter(w)
		_ = tw.WriteHeader(&tar.Header{Name: logMember, Mode: 0o600, Typeflag: tar.TypeReg})
		_ = tw.Close()
	}))
	t.Cleanup(srv.Close)
	_, err := (&Client{URL: srv.URL, HTTP: srv.Client()}).Build(t.Context(), Job{Width: 800, Height: 480, Palette: "bw"}, project(), collect(map[string]string{}))
	if err == nil || errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "drops 1xx responses") {
		t.Errorf("Build = %v, want a counted error naming the dropped 102", err)
	}
}

// stalled reads b, then fails with err, as a connection whose read deadline
// passed, or that was reset.
type stalled struct {
	b   []byte
	err error
}

func (s *stalled) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, s.err
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

// requestTar returns a request tar: a job, then members written by more.
func requestTar(t *testing.T, job string, more func(*tar.Writer)) []byte {
	t.Helper()
	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	if err := tw.WriteHeader(&tar.Header{Name: jobMember, Typeflag: tar.TypeReg, Size: int64(len(job)), Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, job); err != nil {
		t.Fatal(err)
	}
	if more != nil {
		more(tw)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

// TestRequestErrStatus checks that only a malformed request is refused with
// 400, which counts against the recipe, and that a body that stops
// arriving, wherever it stops, or a connection lost mid-body, is answered
// 503, an outage.
func TestRequestErrStatus(t *testing.T) {
	t.Parallel()
	const good = `{"width":800,"height":480,"palette":"bw"}`
	file := func(name string, size int) func(*tar.Writer) {
		return func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(size), Mode: 0o600})
			_, _ = tw.Write(make([]byte, size))
		}
	}
	full := requestTar(t, good, file("src/settings.yml", 4096))
	timeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	// A job, then the header of a file as large as the whole limit, whose
	// content is endless zeros.
	var bigHead bytes.Buffer
	tw := tar.NewWriter(&bigHead)
	_ = tw.WriteHeader(&tar.Header{Name: jobMember, Typeflag: tar.TypeReg, Size: int64(len(good)), Mode: 0o600})
	_, _ = io.WriteString(tw, good)
	_ = tw.WriteHeader(&tar.Header{Name: "src/big", Typeflag: tar.TypeReg, Size: maxProjectBytes, Mode: 0o600})
	big := bigHead.Bytes()
	for _, tc := range []struct {
		name string
		r    io.Reader
		want int
	}{
		{"stalls in the first header", &stalled{b: full[:100], err: timeout}, http.StatusServiceUnavailable},
		{"stalls in the job", &stalled{b: full[:512+10], err: timeout}, http.StatusServiceUnavailable},
		{"stalls in a project file", &stalled{b: full[:3*512+100], err: timeout}, http.StatusServiceUnavailable},
		{"reset in a project file", &stalled{b: full[:3*512+100], err: reset}, http.StatusServiceUnavailable},
		{"cut off in a project file", bytes.NewReader(full[:3*512+100]), http.StatusServiceUnavailable},
		{"no job", strings.NewReader(""), http.StatusBadRequest},
		{"not a tar", strings.NewReader(strings.Repeat("x", 1024)), http.StatusBadRequest},
		{"job not JSON", bytes.NewReader(requestTar(t, "{nope", nil)), http.StatusBadRequest},
		{"bad job", bytes.NewReader(requestTar(t, `{"width":0,"height":480,"palette":"bw"}`, nil)), http.StatusBadRequest},
		{"too large", io.MultiReader(bytes.NewReader(big), zeros{}), http.StatusBadRequest},
		{"name leaves the project", bytes.NewReader(requestTar(t, good, file("../escape", 1))), http.StatusBadRequest},
		{"absolute name", bytes.NewReader(requestTar(t, good, file("/tmp/escape", 1))), http.StatusBadRequest},
		{"name given twice", bytes.NewReader(requestTar(t, good, func(tw *tar.Writer) { file("src/a", 1)(tw); file("src/a", 1)(tw) })), http.StatusBadRequest},
		{"file where a directory goes", bytes.NewReader(requestTar(t, good, func(tw *tar.Writer) { file("src", 1)(tw); file("src/a", 1)(tw) })), http.StatusBadRequest},
		{"symlink", bytes.NewReader(requestTar(t, good, func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: "src/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
		})), http.StatusBadRequest},
	} {
		_, err := readRequest(tc.r, filepath.Join(t.TempDir(), "project"))
		if err == nil || requestErrStatus(err) != tc.want {
			t.Errorf("%s: readRequest = %v, status %d; want %d", tc.name, err, requestErrStatus(err), tc.want)
		}
	}
}

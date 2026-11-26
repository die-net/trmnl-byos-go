package main

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/die-net/sandbox"

	"github.com/die-net/trmnl-byos-go/internal/render"
)

// TestLoopbackAddr checks that a listen address with no host, or the
// wildcard, becomes one on 127.0.0.1, which is where a build would try to
// reach the server, and that anything else is kept or refused.
func TestLoopbackAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ listen, want string }{
		{":8080", "127.0.0.1:8080"},
		{"0.0.0.0:80", "127.0.0.1:80"},
		{"[::]:80", "127.0.0.1:80"},
		{"192.168.1.10:8080", "192.168.1.10:8080"},
		{"127.0.0.1:9", "127.0.0.1:9"},
		{"[fe80::1]:80", "[fe80::1]:80"},
		{"nonsense", ""},
		{"", ""},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			t.Parallel()
			if got := loopbackAddr(tc.listen); got != tc.want {
				t.Errorf("loopbackAddr(%q) = %q, want %q", tc.listen, got, tc.want)
			}
		})
	}
}

// TestRefusedTargets checks what the build's proxy must refuse: the cloud
// metadata address, the server on both loopbacks, and the server on the
// host's other addresses, which are neither loopback nor link-local, so a
// build cannot reach the server through any of them.
func TestRefusedTargets(t *testing.T) {
	t.Parallel()
	got := refusedTargets(":8080")
	if len(got) < 3 || got[0] != "169.254.169.254:80" || got[1] != "127.0.0.1:8080" || got[2] != "[::1]:8080" {
		t.Fatalf("refusedTargets(:8080) = %v, want the metadata address, 127.0.0.1:8080 and [::1]:8080 first", got)
	}
	for _, target := range got[3:] {
		host, port, err := net.SplitHostPort(target)
		if err != nil || port != "8080" {
			t.Errorf("target %q is not an address on port 8080: %v", target, err)
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			t.Errorf("target %q is not a host address that is neither loopback nor link-local", target)
		}
	}
	if got := refusedTargets("nonsense"); !slices.Equal(got, []string{"169.254.169.254:80"}) {
		t.Errorf("refusedTargets of a bad address = %v, want only the metadata address", got)
	}
}

// TestExpect checks that what doctor requires of the sandbox names the data
// directory and the cache by absolute path, since the sandbox is given
// those, and the server's listener on loopback.
func TestExpect(t *testing.T) {
	t.Parallel()
	want, err := expect(&serveFlags{data: "data", cacheDir: "/abs/cache", listen: ":8080"})
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Unreadable) != 2 || !filepath.IsAbs(want.Unreadable[0]) || filepath.Base(want.Unreadable[0]) != "data" || want.Unreadable[1] != "/abs/cache" {
		t.Errorf("Unreadable = %v, want the absolute data directory and /abs/cache", want.Unreadable)
	}
	if !slices.Equal(want.Unreachable, []string{"tcp:127.0.0.1:8080"}) {
		t.Errorf("Unreachable = %v", want.Unreachable)
	}
	want, err = expect(&serveFlags{data: "d", cacheDir: "c", listen: "nonsense"})
	if err != nil || len(want.Unreachable) != 0 {
		t.Errorf("a listen address that is not host:port: Unreachable = %v, %v", want.Unreachable, err)
	}
}

// TestReport checks the report's lines and counts: each line is the status
// padded to four columns, the name and the detail, a fail counts only when
// it is one doctor's exit status depends on, and the library's own checks
// that fail are counted apart.
func TestReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  sandbox.Status
		counted bool
		want    string
		// failures and uncounted are the counts after the one result.
		failures, uncounted int
	}{
		{"pass", sandbox.Pass, true, "pass  x.y: why\n", 0, 0},
		{"counted fail", sandbox.Fail, true, "fail  x.y: why\n", 1, 0},
		{"uncounted fail", sandbox.Fail, false, "fail  x.y: why\n", 0, 1},
		{"not enforceable", sandbox.NotEnforceable, true, "n/a   x.y: why\n", 0, 0},
		{"skipped", sandbox.Skipped, false, "skip  x.y: why\n", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			r := &report{out: &out}
			r.result(sandbox.Result{Name: "x.y", Status: tc.status, Detail: "why"}, tc.counted)
			if out.String() != tc.want {
				t.Errorf("wrote %q, want %q", out.String(), tc.want)
			}
			if r.failures != tc.failures || r.uncounted != tc.uncounted {
				t.Errorf("failures %d, uncounted %d; want %d, %d", r.failures, r.uncounted, tc.failures, tc.uncounted)
			}
			if r.err != nil {
				t.Errorf("err = %v", r.err)
			}
		})
	}

	var out bytes.Buffer
	r := &report{out: &out}
	r.line("pass", "a", "one")
	r.line("fail", "b", "two")
	r.line("warn", "c", "three")
	if want := "pass  a: one\nfail  b: two\nwarn  c: three\n"; out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
	if r.failures != 1 {
		t.Errorf("failures = %d, want 1: only a fail counts", r.failures)
	}
}

// failingWriter fails every write.
type failingWriter struct{}

var errWrite = errors.New("write failed")

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

// TestReportWriteError checks that the first write error is kept, and that
// doctor returns it rather than reporting success it could not show.
func TestReportWriteError(t *testing.T) {
	t.Parallel()
	r := &report{out: failingWriter{}}
	r.line("pass", "a", "b")
	r.line("pass", "c", "d")
	if !errors.Is(r.err, errWrite) {
		t.Errorf("err = %v, want the write error", r.err)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	if err := doctor(t.Context(), []string{"-render-url", srv.URL}, failingWriter{}); !errors.Is(err, errWrite) {
		t.Errorf("doctor = %v, want the write error", err)
	}
}

// TestDoctorRenderer checks doctor against a renderer given by -render-url:
// any answer means it is there, and nothing listening is a failed check that
// makes doctor exit non-zero.
func TestDoctorRenderer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	if err := doctor(t.Context(), []string{"-render-url", srv.URL}, &out); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}
	if want := "pass  renderer.answers: GET / answered 404 Not Found\n"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
	if strings.Contains(out.String(), "fail") {
		t.Errorf("output has a failure:\n%s", out.String())
	}

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := doctor(t.Context(), []string{"-render-url", dead}, &out); !errors.Is(err, errDoctorFailed) {
		t.Errorf("doctor of a dead renderer = %v, want errDoctorFailed", err)
	}
	if !strings.Contains(out.String(), "fail  renderer.answers: ") {
		t.Errorf("output lacks the failure:\n%s", out.String())
	}
}

// TestDoctorBadArguments checks that doctor refuses flags that cannot work
// and extra arguments before it checks anything.
func TestDoctorBadArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"memory too small", []string{"-render-memory", "10"}, "-render-memory"},
		{"relative cgroup", []string{"-render-cgroup", "rel/cgroup"}, "-render-cgroup"},
		{"extra argument", []string{"extra"}, "usage: trmnl-byos doctor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := doctor(t.Context(), tc.args, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("doctor = %v, want an error with %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("wrote %q before refusing", out.String())
			}
		})
	}
}

// TestServeFlagsCheck checks the flag combinations serve and doctor refuse.
func TestServeFlagsCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		// err is a string the error must contain, or "" for none.
		err string
	}{
		{"defaults", nil, ""},
		{"memory below the least", []string{"-render-memory", "255"}, "-render-memory 255"},
		{"memory at the least", []string{"-render-memory", "256"}, ""},
		{"memory negative", []string{"-render-memory", "-1"}, "-render-memory"},
		{"relative cgroup", []string{"-render-cgroup", "sys/fs/cgroup/trmnl"}, "-render-cgroup"},
		{"absolute cgroup", []string{"-render-cgroup", "/sys/fs/cgroup/trmnl"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs, f := newServeFlags("test")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := f.check()
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Errorf("check() = %v, want an error with %q", err, tc.err)
			}
		})
	}
}

// TestSandboxedCommandRefusesOutsideSandbox checks that the build and
// self-test commands run nothing when this process was not started by a
// sandbox, as when a person types one, since a build run so would have all
// of the user's access.
func TestSandboxedCommandRefusesOutsideSandbox(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{render.BuildCommand, render.SelfTestCommand} {
		if code := sandboxedCommand(t.Context(), cmd, nil); code != 2 {
			t.Errorf("sandboxedCommand(%q) = %d, want 2", cmd, code)
		}
	}
}

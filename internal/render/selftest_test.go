package render

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/die-net/sandbox"
)

// connectProxy starts an HTTP server that answers CONNECT for the target
// allowedTarget with 200 and every other with 502, and returns its
// host:port.
func connectProxy(t *testing.T, allowedTarget string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		if r.Host == allowedTarget {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "refused", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// TestConnectResult checks that the self-test's verdict on one CONNECT is a
// pass only for the answer the target should get, 200 for an allowed one
// and a refusal for the rest, and that the name says which it was.
func TestConnectResult(t *testing.T) {
	t.Parallel()
	const good, bad = "good.example:443", "bad.example:443"
	addr := connectProxy(t, good)
	for _, tc := range []struct {
		name    string
		target  string
		allowed bool
		want    sandbox.Status
		wantIn  string
	}{
		{"allowed and answered 200", good, true, sandbox.Pass, "proxy.allowed:" + good},
		{"allowed and refused", bad, true, sandbox.Fail, "proxy.allowed:" + bad},
		{"refused and answered 200", good, false, sandbox.Fail, "proxy.refused:" + good},
		{"refused and refused", bad, false, sandbox.Pass, "proxy.refused:" + bad},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := connectResult(t.Context(), addr, tc.target, tc.allowed)
			if res.Status != tc.want || res.Name != tc.wantIn {
				t.Errorf("result = %+v, want status %v, name %q", res, tc.want, tc.wantIn)
			}
			if !strings.Contains(res.Detail, tc.target) || !strings.Contains(res.Detail, "status ") {
				t.Errorf("Detail = %q, want the target and the status", res.Detail)
			}
		})
	}
}

// TestConnectResultNoProxy checks that a proxy that cannot be reached is a
// failure for an allowed and a refused target alike, with the error in the
// detail, since nothing was shown to be refused.
func TestConnectResultNoProxy(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []bool{true, false} {
		res := connectResult(t.Context(), addr, "x.example:443", allowed)
		if res.Status != sandbox.Fail {
			t.Errorf("allowed=%v: status %v, want Fail", allowed, res.Status)
		}
		if !strings.Contains(res.Detail, "connect") && !strings.Contains(res.Detail, "refused") {
			t.Errorf("allowed=%v: Detail %q lacks the dial error", allowed, res.Detail)
		}
	}
}

// TestConnectResultCanceled checks that a canceled context is a failed
// check, not a hang.
func TestConnectResultCanceled(t *testing.T) {
	t.Parallel()
	addr := connectProxy(t, "x.example:443")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res := connectResult(ctx, addr, "x.example:443", true); res.Status != sandbox.Fail {
		t.Errorf("status %v after the context ended, want Fail", res.Status)
	}
}

// TestRunSelfTest checks the command that runs in the sandbox: it tries
// every -refuse and -allow target through HTTPS_PROXY, writes one JSON
// result for each, and refuses bad flags and a missing proxy without
// writing any. It sets an environment variable, so it is not parallel.
func TestRunSelfTest(t *testing.T) {
	const good, bad = "good.example:443", "bad.example:443"
	addr := connectProxy(t, good)

	t.Setenv("HTTPS_PROXY", "http://"+addr)
	var out bytes.Buffer
	code, err := RunSelfTest(t.Context(), []string{"-refuse", bad, "-refuse", "other.example:80", "-allow", good}, &out)
	if code != 0 || err != nil {
		t.Fatalf("RunSelfTest = %d, %v", code, err)
	}
	var got []sandbox.Result
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r sandbox.Result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		got = append(got, r)
	}
	want := []string{"proxy.refused:" + bad, "proxy.refused:other.example:80", "proxy.allowed:" + good}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i, r := range got {
		if r.Name != want[i] || r.Status != sandbox.Pass {
			t.Errorf("result %d = %+v, want a pass named %q", i, r, want[i])
		}
	}

	out.Reset()
	if code, err := RunSelfTest(t.Context(), []string{"-bogus"}, &out); code != exitUsage || err == nil {
		t.Errorf("bad flag: %d, %v, want %d and an error", code, err, exitUsage)
	}
	if code, err := RunSelfTest(t.Context(), []string{"extra"}, &out); code != exitUsage || err == nil {
		t.Errorf("extra argument: %d, %v, want %d and an error", code, err, exitUsage)
	}

	t.Setenv("HTTPS_PROXY", "")
	if code, err := RunSelfTest(t.Context(), []string{"-refuse", bad}, &out); code == 0 || err == nil {
		t.Errorf("no HTTPS_PROXY: %d, %v, want a non-zero code and an error", code, err)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q though nothing could be tried", out.String())
	}
}

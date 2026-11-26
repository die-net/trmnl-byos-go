//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/testutil/sandboxtest"
)

// TestDoctorSandbox runs doctor on the sandbox path, with stand-ins for the
// tools on PATH, and checks that it ran each part against a real sandbox:
// the library's check found the data directory and the cache unreadable
// and the server's port unreachable, the self-test found the server's port
// refused through the build's proxy, and the build's limits were reported.
// It does not check the exit status, since the self-test also needs
// trmnl.com, which a test host may not reach.
func TestDoctorSandbox(t *testing.T) {
	if _, err := render.ProbeSandbox(); err != nil {
		sandboxtest.Unavailable(t, err)
	}
	bin := t.TempDir()
	for _, name := range []string{"trmnlp", "ruby", "firefox", "geckodriver", "magick", "Xvfb"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: the fake command must be executable.
			t.Fatal(err)
		}
	}
	// doctor finds the tools on the PATH, as serve does, and the library
	// finds bubblewrap there too.
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, cacheDir := filepath.Join(dir, "data"), filepath.Join(dir, "cache")
	for _, d := range []string{data, cacheDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	err = doctor(t.Context(), []string{"-data", data, "-cache", cacheDir, "-listen", "127.0.0.1:18080"}, &out)
	t.Logf("doctor: %v\n%s", err, out.String())
	for _, want := range []string{
		"pass  render.path:",
		"pass  sandbox.backend:",
		"pass  tool.trmnlp: " + filepath.Join(bin, "trmnlp"),
		"pass  expect.unreadable:" + data + ":",
		"pass  expect.unreadable:" + cacheDir + ":",
		"pass  expect.unreachable:tcp:127.0.0.1:18080:",
		"pass  build.memory:",
		"warn  build.tasks:",
		"pass  proxy.refused:127.0.0.1:18080:",
		"pass  proxy.refused:169.254.169.254:80:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor's output has no %q", want)
		}
	}
	if entries, err := os.ReadDir(data); err != nil || len(entries) != 0 {
		t.Errorf("doctor left %v in the data directory (%v)", entries, err)
	}
}

// TestWriteProbeProfile writes the AppArmor profile sandbox.Probe prints
// for this test binary, as render's test of the same name does for its own.
func TestWriteProbeProfile(t *testing.T) {
	t.Parallel()
	sandboxtest.WriteProbeProfile(t)
}

// Package sandboxtest decides what a test does on a host where no sandbox
// can start: it skips, saying why, unless the run requires sandboxes, as
// CI's sandbox job does, where it fails, so a green run means the
// sandboxed tests ran.
//
// Only _test.go files may import it, which the depguard rule in
// .golangci.yml enforces.
package sandboxtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/die-net/sandbox"
)

// RequireEnv is the variable that, set to 1, makes a test that cannot start
// a sandbox fail rather than skip.
const RequireEnv = "TRMNL_TEST_REQUIRE_SANDBOX"

// Unavailable ends the test, which needs a sandbox, because err says none
// can start here: with t.Fatal when RequireEnv is 1, and t.Skip otherwise.
func Unavailable(t testing.TB, err error) {
	t.Helper()
	if os.Getenv(RequireEnv) == "1" {
		t.Fatalf("no sandbox can start on this host, and %s=1 requires one: %v", RequireEnv, err)
	}
	t.Skipf("no sandbox can start on this host, so this test cannot run here (set %s=1 to fail instead): %v", RequireEnv, err)
}

// ProfileDirEnv names a directory WriteProbeProfile writes into.
const ProfileDirEnv = "TRMNL_TEST_APPARMOR_DIR"

// WriteProbeProfile writes the AppArmor profile for this test binary, from
// sandbox.AppArmorProfile, which is the one sandbox.Probe prints and doctor
// shows an operator, under the name it gives, into the directory
// ProfileDirEnv names. Without ProfileDirEnv it skips. The profile is for the
// binary's path with its links resolved, which is the file the kernel
// execs, and AppArmor attaches profiles by.
func WriteProbeProfile(t *testing.T) {
	t.Helper()
	dir := os.Getenv(ProfileDirEnv)
	if dir == "" {
		t.Skipf("%s is not set; CI's sandbox job sets it to load the profile", ProfileDirEnv)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	profile, name, err := sandbox.AppArmorProfile(exe)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = root.WriteFile(name, []byte(profile), 0o600)
	_ = root.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s for %s", name, exe)
}

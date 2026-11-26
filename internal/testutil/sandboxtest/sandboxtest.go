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
	"regexp"
	"strings"
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

// profileName finds the name the profile is to be saved under in the fix a
// probe's finding gives.
var profileName = regexp.MustCompile(`save this as /etc/apparmor\.d/([A-Za-z0-9-]+),`)

// WriteProbeProfile runs sandbox.Probe in this test binary, and writes the
// AppArmor profile its failed finding prints, under the name it gives, into
// the directory ProfileDirEnv names. That is the profile an operator is told
// to load, for this binary's own path. Without ProfileDirEnv it skips. It
// fails when Probe passes or prints no profile, since CI's sandbox job runs
// it on a host that needs one. The profile starts at the finding's blank
// line before "abi", after the fix and what the profile grants.
func WriteProbeProfile(t *testing.T) {
	t.Helper()
	dir := os.Getenv(ProfileDirEnv)
	if dir == "" {
		t.Skipf("%s is not set; CI's sandbox job sets it to load the profile", ProfileDirEnv)
	}
	rep, err := sandbox.Probe()
	if err == nil {
		t.Fatal("sandbox.Probe passed, so it printed no profile to load")
	}
	for _, c := range rep.Checks {
		m := profileName.FindStringSubmatch(c.Detail)
		if c.Status != sandbox.Fail || m == nil {
			continue
		}
		i := strings.Index(c.Detail, "\n\nabi ")
		if i < 0 {
			t.Fatalf("%s gives no profile after its fix: %s", c.Name, c.Detail)
		}
		profile := strings.TrimSpace(c.Detail[i+2:]) + "\n"
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		err = root.WriteFile(m[1], []byte(profile), 0o600)
		_ = root.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: wrote %s", c.Name, m[1])
		return
	}
	t.Fatalf("sandbox.Probe failed with no profile to load: %v", err)
}

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
	"testing"
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

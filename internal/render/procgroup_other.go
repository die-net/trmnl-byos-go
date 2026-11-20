//go:build !unix

package render

import (
	"errors"
	"os/exec"
)

// inProcessGroup does nothing where process groups are not available. The
// shim runs in a Linux container.
func inProcessGroup(*exec.Cmd) {}

func killGroup(*exec.Cmd) error { return nil }

// killAll is not built here. listProcs is not either, so the shim refuses to
// start before it would be called.
func killAll() error {
	return errors.New("killing leftover processes is built only for Unix")
}

// openBuildFlags adds nothing where the shim does not run.
const openBuildFlags = 0

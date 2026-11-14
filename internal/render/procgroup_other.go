//go:build !unix

package render

import "os/exec"

// inProcessGroup does nothing where process groups are not available. The
// shim runs in a Linux container.
func inProcessGroup(*exec.Cmd) {}

func killGroup(*exec.Cmd) error { return nil }

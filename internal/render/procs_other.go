//go:build !linux

package render

import "errors"

// errNotLinux is why the shim does not run here: it lists processes and
// mounts through /proc, and relies on kill(-1) in a PID namespace of its
// own.
var errNotLinux = errors.New("trmnl-render runs only on Linux")

func listProcs(int) ([]proc, error) { return nil, errNotLinux }

func listMounts() ([]mount, error) { return nil, errNotLinux }

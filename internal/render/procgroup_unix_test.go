//go:build linux

package render

import "syscall"

// processAlive reports whether a process with pid exists.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

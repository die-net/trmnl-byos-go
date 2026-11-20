//go:build !linux

package render

import "errors"

// cleanIPC is not built here, so the shim's first sweep fails and it does
// not start.
func cleanIPC(int) (int, error) {
	return 0, errors.New("removing System V IPC objects is built only for Linux")
}

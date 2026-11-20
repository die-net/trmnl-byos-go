package render

import "golang.org/x/sys/unix"

// ProtectSelf makes the shim not dumpable, so its /proc files belong to
// root and no process of its user may ptrace it, read its memory or
// environment, or take its file descriptors. The shim is the one process of
// its user a sweep leaves running.
func ProtectSelf() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

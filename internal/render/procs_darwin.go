package render

import "golang.org/x/sys/unix"

// readOnlyFS reports whether the open directory fd is on a read-only mount.
// The shim refuses to start outside Linux, but the cleanup it shares with
// tests is built here too.
func readOnlyFS(fd int) (bool, error) {
	var s unix.Statfs_t
	if err := unix.Fstatfs(fd, &s); err != nil {
		return false, err
	}
	return s.Flags&unix.MNT_RDONLY != 0, nil
}

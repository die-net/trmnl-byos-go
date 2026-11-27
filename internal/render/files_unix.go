//go:build unix

package render

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Besides processes, a build could leave files that change what a later
// build runs, such as a Python usercustomize.py in the account's home. Each
// build gets its own home and temporary directories inside its build
// directory (see buildDirs), and after the process sweep the shim removes
// whatever its user owns in the places a build can write: the scratch roots
// and the account's home. CheckWritable refuses to start where the user can
// write anywhere else.

// scratchRoots returns the places the cleanup covers: /tmp, /var/tmp,
// /dev/shm, /dev/mqueue, which holds POSIX message queues, the temporary
// directory, and the account's home, by $HOME and by the account database.
// Each is resolved through symlinks. One that does not exist is left out. A
// home of /, which an account without one gets, is no home, and is left
// out too: checkRoots would refuse it. One may lie inside another, such as
// a TMPDIR under /tmp, which cleanFiles removes as part of the outer one,
// and the shim makes again at the next build.
func scratchRoots() []string {
	roots := []string{"/tmp", "/var/tmp", "/dev/shm", "/dev/mqueue", os.TempDir()}
	var homes []string
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, home)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		homes = append(homes, u.HomeDir)
	}
	for _, h := range homes {
		if p, err := filepath.EvalSymlinks(h); err == nil && p != "/" {
			roots = append(roots, p)
		}
	}
	var resolved []string
	for _, r := range roots {
		if p, err := filepath.EvalSymlinks(r); err == nil && filepath.IsAbs(p) {
			resolved = append(resolved, filepath.Clean(p))
		}
	}
	slices.Sort(resolved)
	return slices.Compact(resolved)
}

// checkRoots refuses a cleanup root that is /, that holds another file
// system's mount point, which the cleanup would walk into, or that holds a
// path the shim needs, such as its own binary, which the cleanup could
// remove.
func checkRoots(roots []string, mounts []mount, needed []string) error {
	for _, r := range roots {
		if r == "/" {
			return errors.New("the cleanup after each build would cover all of /; give trmnl-render a temporary directory and a home other than /")
		}
		for _, m := range mounts {
			if m.point != r && within(m.point, r) {
				return fmt.Errorf("the cleanup after each build covers %s, which holds the mount point %s", r, m.point)
			}
		}
		for _, n := range needed {
			if within(n, r) {
				return fmt.Errorf("the cleanup after each build covers %s, which holds %s, which trmnl-render needs", r, n)
			}
		}
	}
	return nil
}

// Bounds on the cleanup after each build. A build that leaves more stops the
// shim, which exits, so whatever runs it starts it again, on a fresh /tmp
// in Compose.
const (
	maxCleanEntries = 1_000_000
	maxCleanDepth   = 256
)

// cleaner removes what a user owns under the scratch roots. It works
// relative to open directories, so no path grows past PATH_MAX however
// deep a build nested its directories.
type cleaner struct {
	uid      int
	timeout  time.Duration
	deadline time.Time
	entries  int
	removed  int
	// dry makes the walk look without changing anything, for the startup
	// check.
	dry bool
	// foreign names up to maxForeign of the other users' entries found on
	// writable mounts, and foreignN counts them all.
	foreign  []string
	foreignN int
}

// maxForeign is how many of other users' entries an error names.
const maxForeign = 5

// openEntry opens the directory name, in the open directory fd, for the
// walk, without following a link. With chmod, it first makes the directory
// writable and searchable by its owner, the user. It reports gone, with no
// error, when the entry vanished after the walk listed it, which only
// another user can cause, so the walk skips it and goes on to its siblings.
// Every other error is returned, so it fails the cleanup.
func openEntry(fd int, name string, chmod bool) (sub int, gone bool, err error) {
	if chmod {
		if err := unix.Fchmodat(fd, name, 0o700, 0); errors.Is(err, unix.ENOENT) {
			return -1, true, nil
		} else if err != nil {
			return -1, false, fmt.Errorf("make it removable: %w", err)
		}
	}
	sub, err = unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return -1, true, nil
	}
	return sub, false, err
}

// cleanFiles removes every file and directory that uid owns under roots,
// and returns how many it removed. The roots must be uid's own: an entry
// another user owns, on a writable mount, fails the cleanup (see
// errNotPrivate), since the cleanup could not see what a build left inside
// it, such as in a directory of mode 0711 that holds one of mode 1777. It
// also fails if it cannot remove something uid owns on a writable file
// system, finds more than it may look at, or runs past timeout. A root that
// does not exist is skipped. A root that lies inside another is cleaned as
// part of it, so a TMPDIR inside /tmp is removed, and ServeHTTP makes it
// again.
func cleanFiles(roots []string, uid int, timeout time.Duration) (int, error) {
	return walkRoots(roots, uid, timeout, false)
}

// checkPrivate refuses to start when a cleanup root holds an entry another
// user owns on a writable mount, as cleanFiles would fail. It changes
// nothing: what uid owns there goes at the first sweep.
func checkPrivate(roots []string, uid int) error {
	_, err := walkRoots(roots, uid, cleanTimeout, true)
	return err
}

// errNotPrivate names other users' entries found in the cleanup roots.
func errNotPrivate(c *cleaner) error {
	names := strings.Join(c.foreign, ", ")
	if c.foreignN > len(c.foreign) {
		names += fmt.Sprintf(" and %d more", c.foreignN-len(c.foreign))
	}
	return fmt.Errorf("trmnl-render's scratch directories must be its own, but other users own %s; give it a /tmp, /var/tmp, /dev/shm and /dev/mqueue of its own: a tmpfs in a container, or PrivateTmp=yes, PrivateIPC=yes and TemporaryFileSystem=/dev/shm under systemd", names)
}

func walkRoots(roots []string, uid int, timeout time.Duration, dry bool) (int, error) {
	c := &cleaner{uid: uid, timeout: timeout, deadline: time.Now().Add(timeout), dry: dry}
	for _, root := range roots {
		// One inside another is walked as part of the other.
		if slices.ContainsFunc(roots, func(q string) bool { return q != root && within(root, q) }) {
			continue
		}
		if err := c.root(root); err != nil {
			return c.removed, err
		}
	}
	if c.foreignN > 0 {
		return c.removed, errNotPrivate(c)
	}
	return c.removed, nil
}

// root cleans one root, if it exists. A root the user owns, such as its
// home, is made writable and searchable first, so a build cannot keep its
// files by taking those permissions away. Only the root's own absence is
// skipped: an error further in fails the cleanup, whatever it wraps.
func (c *cleaner) root(root string) error {
	var st unix.Stat_t
	if err := unix.Lstat(root, &st); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return &fs.PathError{Op: "stat", Path: root, Err: err}
	}
	// On a read-only mount the chmod fails with EROFS, and no user can
	// change anything there either.
	if int(st.Uid) == c.uid && st.Mode&0o700 != 0o700 && !c.dry {
		if err := os.Chmod(root, fs.FileMode(st.Mode&0o777|0o700)); err != nil && !errors.Is(err, unix.EROFS) {
			return fmt.Errorf("make %s writable: %w", root, err)
		}
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &fs.PathError{Op: "open", Path: root, Err: err}
	}
	return c.dir(fd, root, unix.Access(root, unix.W_OK) == nil, 0)
}

// count counts one more entry looked at, and fails past the bounds.
func (c *cleaner) count(depth int) error {
	c.entries++
	switch {
	case c.entries > maxCleanEntries:
		return fmt.Errorf("a build left over %d files", maxCleanEntries)
	case depth > maxCleanDepth:
		return fmt.Errorf("a build nested directories over %d deep", maxCleanDepth)
	case time.Now().After(c.deadline):
		return fmt.Errorf("removing files took over %s", c.timeout)
	}
	return nil
}

// dir cleans the open directory fd, at path, and closes it. writable says
// whether the user may remove its entries.
func (c *cleaner) dir(fd int, path string, writable bool, depth int) error {
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list %s: %w", path, err)
	}
	// Whether fd is on a read-only mount, where no user can change
	// anything, read once when needed.
	var ro, roKnown bool
	readOnly := func() (bool, error) {
		if !roKnown {
			r, err := readOnlyFS(fd)
			if err != nil {
				return false, fmt.Errorf("read the mount of %s: %w", path, err)
			}
			ro, roKnown = r, true
		}
		return ro, nil
	}
	for _, name := range names {
		if err := c.count(depth); err != nil {
			return err
		}
		full := path + "/" + name
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("stat %s: %w", full, err)
		}
		switch {
		case int(st.Uid) != c.uid:
			// Another user's entry. The walk does not look inside, since it
			// may not see all of what a build could leave there, so it
			// fails, unless nobody can change it.
			if r, err := readOnly(); err != nil {
				return err
			} else if !r {
				c.foreignN++
				if len(c.foreign) < maxForeign {
					c.foreign = append(c.foreign, fmt.Sprintf("%s (uid %d)", full, st.Uid))
				}
			}
		case writable:
			if c.dry {
				continue
			}
			if err := c.remove(fd, name, full, &st, depth+1); err != nil {
				return err
			}
			c.removed++
		default:
			// The user's own, in a directory it cannot write. On a
			// read-only mount, it cannot change it either, whatever its
			// type or mode, so it is no channel. Anywhere else it could
			// change it, as its owner.
			if r, err := readOnly(); err != nil {
				return err
			} else if !r {
				return fmt.Errorf("cannot remove %s, which the user owns, from a directory it cannot write", full)
			}
		}
	}
	return nil
}

// remove removes name, in the open directory fd, which st says the user
// owns. A directory of its own is made writable and searchable first, so a
// build cannot keep its files by taking away its own permission to remove
// them.
func (c *cleaner) remove(fd int, name, full string, st *unix.Stat_t, depth int) error {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("remove %s: %w", full, err)
		}
		return nil
	}
	sub, gone, err := openEntry(fd, name, int(st.Uid) == c.uid && st.Mode&0o700 != 0o700)
	if gone {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", full, err)
	}
	f := os.NewFile(uintptr(sub), full)
	defer f.Close()
	// At every step, ENOENT means what was being removed is already gone,
	// as when, with more than one build at once, a build's handler removes
	// its own directory while the last build's sweep walks it.
	names, err := f.Readdirnames(-1)
	if errors.Is(err, unix.ENOENT) {
		return nil // Linux lists a directory removed since it was opened so.
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", full, err)
	}
	for _, n := range names {
		if err := c.count(depth); err != nil {
			return err
		}
		var cst unix.Stat_t
		if err := unix.Fstatat(sub, n, &cst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("stat %s/%s: %w", full, n, err)
		}
		if err := c.remove(sub, n, full+"/"+n, &cst, depth+1); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove %s: %w", full, err)
	}
	return nil
}

// maxScanEntries bounds the startup scan for writable places. A read-only
// root file system leaves only small file systems to scan.
const maxScanEntries = 1_000_000

// errScanTooLarge means the startup scan gave up before it was done.
var errScanTooLarge = fmt.Errorf("found over %d files on writable file systems", maxScanEntries)

// CheckWritable refuses to start where the user can write somewhere the
// cleanup after each build does not cover, since a file a build left there
// could change what a later build runs, or where the cleanup would cover
// too much: all of /, another file system's mount point, or one of needed,
// the paths the shim itself needs. In Compose, the renderer's root file
// system is read-only and /tmp is a tmpfs, so nothing but the scratch roots
// is writable. Under systemd, ProtectSystem=strict and PrivateTmp do the
// same.
func CheckWritable(needed ...string) error {
	mounts, err := listMounts()
	if err != nil {
		return fmt.Errorf("list mounts: %w", err)
	}
	roots := scratchRoots()
	var resolved []string
	for _, n := range needed {
		if p, err := filepath.EvalSymlinks(n); err == nil {
			resolved = append(resolved, p)
		}
	}
	// Every build makes its directory there.
	if _, err := filepath.EvalSymlinks(os.TempDir()); err != nil {
		return fmt.Errorf("the temporary directory: %w", err)
	}
	if err := checkRoots(roots, mounts, resolved); err != nil {
		return err
	}
	if err := checkPrivate(roots, os.Getuid()); err != nil {
		return err
	}
	var writable []string
	for _, m := range mounts {
		if m.writable {
			writable = append(writable, m.point)
		}
	}
	found, err := writableOutside(writable, roots, os.Getuid(), maxScanEntries)
	if errors.Is(err, errScanTooLarge) {
		return fmt.Errorf("could not check where trmnl-render's user can write: %w; give it a read-only root file system", err)
	}
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf("trmnl-render's user can write to %s, which nothing cleans after each build, so a build could leave files there that change later builds; give it a read-only root file system with only /tmp writable", strings.Join(found, ", "))
	}
	return nil
}

// unlistable decides what the startup scan, or the cleanup inside a
// scratch root, does with a directory it cannot list, whose listing failed
// with err. It may skip one the user cannot search either. One the user can
// search may hold a directory it can write and reach by name, so neither
// can vouch for it, and both fail.
func unlistable(path string, err error) error {
	if unix.Access(path, unix.X_OK) != nil {
		return nil
	}
	return fmt.Errorf("cannot list %s, which trmnl-render's user can search, so cannot tell whether it can write inside; give it a read-only root file system and a /tmp of its own, as Compose does, or ProtectSystem=strict and PrivateTmp=yes under systemd: %w", path, err)
}

// writableOutside walks each mount point in mounts, staying on its file
// system, and returns up to ten directories uid can write and files it can
// write or owns, outside roots. It reads at most limit entries.
func writableOutside(mounts, roots []string, uid, limit int) ([]string, error) {
	s := &scanner{uid: uid, roots: roots, limit: limit}
	for _, m := range mounts {
		var st syscall.Stat_t
		if err := syscall.Lstat(m, &st); err != nil {
			continue
		}
		if err := s.walk(m, &st); err != nil {
			return s.found, err
		}
		if len(s.found) >= 10 {
			break
		}
	}
	return s.found, nil
}

type scanner struct {
	uid   int
	roots []string
	limit int
	n     int
	found []string
}

// walk scans path, whose mount's root is mnt.
func (s *scanner) walk(path string, mnt *syscall.Stat_t) error {
	if slices.ContainsFunc(s.roots, func(r string) bool { return within(path, r) }) || len(s.found) >= 10 {
		return nil
	}
	s.n++
	if s.n > s.limit {
		return errScanTooLarge
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); errors.Is(err, syscall.ENOENT) || err == nil && st.Dev != mnt.Dev {
		return nil // Gone, or another mount, scanned on its own.
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		if int(st.Uid) == s.uid || unix.Access(path, unix.W_OK) == nil {
			s.found = append(s.found, path)
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return unlistable(path, err)
		}
		for _, e := range entries {
			if err := s.walk(filepath.Join(path, e.Name()), mnt); err != nil {
				return err
			}
		}
	case syscall.S_IFREG:
		if int(st.Uid) == s.uid || unix.Access(path, unix.W_OK) == nil {
			s.found = append(s.found, path)
		}
	}
	return nil
}

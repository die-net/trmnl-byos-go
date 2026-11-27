//go:build unix

package render

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// removeTree removes dir and everything in it. It is for a tree that
// nothing writes any more, such as the home of a build whose sandbox has
// ended, which the build could have made as deep, as wide and as locked as
// it liked. So it has no bound on depth or entries, and holds two
// descriptors however deep the tree is: rather than descend, it moves the
// entries of each directory below dir up into dir, and removes the
// directory once it is empty. It never follows a link: a link is an entry
// like any other, removed where it lies. A directory is made writable and
// searchable before it is opened, so a mode of 0 cannot stop it.
func removeTree(dir string) error {
	// A directory needs its search bit, as each one below gets in remove.
	if err := unix.Chmod(dir, 0o700); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &fs.PathError{Op: "chmod", Path: dir, Err: err}
	}
	root, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	t := &flattener{root: root, dir: dir}
	err = t.run()
	_ = unix.Close(root)
	if err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	if err := unix.Rmdir(dir); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "remove", Path: dir, Err: err}
	}
	return nil
}

// flattener empties the directory root, at dir.
type flattener struct {
	root int
	dir  string
	// next numbers the names entries are moved up to.
	next int
}

// run removes every entry of the root until it is empty. Each pass sees
// the entries the last one moved up.
func (t *flattener) run() error {
	for {
		n, err := t.each(t.root, t.dir, t.remove)
		if err != nil || n == 0 {
			return err
		}
	}
}

// readBatch is how many names each read of a directory returns, so a
// directory of millions of entries, which a build can make, is never held
// in memory at once.
const readBatch = 1024

// each calls fn for each entry of the open directory fd, at path, from its
// start, reading readBatch names at a time, and returns how many it saw.
// An entry made or removed while it reads may be seen or not, so a caller
// that must see every one reads again until it sees none.
func (*flattener) each(fd int, path string, fn func(name string) error) (int, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(dup), path)
	defer f.Close()
	if _, err := f.Seek(0, 0); err != nil {
		return 0, err
	}
	n := 0
	for {
		names, err := f.Readdirnames(readBatch)
		for _, name := range names {
			if err := fn(name); err != nil {
				return n, err
			}
		}
		n += len(names)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("list %s: %w", path, err)
		}
	}
}

// remove removes the root's entry name: at once if it is not a directory
// or is an empty one, and otherwise after moving its entries up into the
// root, where a later pass of run removes them.
func (t *flattener) remove(name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(t.root, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(t.root, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("remove %s: %w", name, err)
		}
		return nil
	}
	if err := unix.Unlinkat(t.root, name, unix.AT_REMOVEDIR); err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	// The entry is a directory, not a link, and nothing changes the tree,
	// so the chmod reaches that directory.
	if err := unix.Fchmodat(t.root, name, 0o700, 0); err != nil {
		return fmt.Errorf("make %s writable: %w", name, err)
	}
	sub, err := unix.Openat(t.root, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer unix.Close(sub)
	for {
		n, err := t.each(sub, name, func(entry string) error {
			if err := t.moveUp(sub, entry); err != nil {
				return fmt.Errorf("move %s/%s: %w", name, entry, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		// A read that ran while entries moved out may have skipped some,
		// so the directory is read again until it can go.
		err = unix.Unlinkat(t.root, name, unix.AT_REMOVEDIR)
		if err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if n == 0 {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
}

// moveUp renames the entry name of the directory sub into the root, under
// a name of its own. A name the root has already is skipped: a rename
// onto it could replace a file there, which would be removed anyway, but
// fails onto a directory with entries.
func (t *flattener) moveUp(sub int, name string) error {
	// Moving a directory rewrites its "..", which needs write access to
	// it. It is a directory, not a link, so the chmod reaches it.
	var st unix.Stat_t
	if err := unix.Fstatat(sub, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if err := unix.Fchmodat(sub, name, 0o700, 0); err != nil {
			return err
		}
	}
	for {
		t.next++
		to := ".rm-" + strconv.Itoa(t.next)
		var st unix.Stat_t
		if err := unix.Fstatat(t.root, to, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			continue
		}
		return unix.Renameat(sub, name, t.root, to)
	}
}

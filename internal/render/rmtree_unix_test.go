//go:build unix

package render

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// mustWrite writes a file of mode perm and fails the test if it cannot.
func mustWrite(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	// The umask and a mode of 0 must not decide the mode.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// mustMkdir makes a directory of mode perm and fails the test if it cannot.
func mustMkdir(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// readTest reads the file name under dir, which must stay within it.
func readTest(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(name)
}

// requireGone fails the test unless nothing is at path.
func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists: %v", path, err)
	}
}

// newTreeDir returns a path for a tree in a fresh directory, and makes sure
// the test's cleanup can remove what a failure leaves, whatever its modes.
func newTreeDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "tree")
	t.Cleanup(func() { _ = removeTree(dir) })
	return dir
}

// TestRemoveTreeShapes checks that removeTree removes trees of several
// shapes, including empty, wide, and ones with directories and files whose
// modes would stop an ordinary removal, and that the directory is gone each
// time.
func TestRemoveTreeShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, dir string)
	}{
		{"empty", func(t *testing.T, dir string) {
			mustMkdir(t, dir, 0o700)
		}},
		{"flat with many files", func(t *testing.T, dir string) {
			mustMkdir(t, dir, 0o700)
			for i := range 5000 {
				mustWrite(t, filepath.Join(dir, "f"+strconv.Itoa(i)), "x", 0o600)
			}
		}},
		// Several of readBatch below the root, each moved up in batches,
		// and one of mode 0500, which a mover must open up first.
		{"subdirectories with many entries each", func(t *testing.T, dir string) {
			mustMkdir(t, dir, 0o700)
			for _, sub := range []string{"wide", "locked"} {
				mustMkdir(t, filepath.Join(dir, sub), 0o700)
				for i := range 3*readBatch + 7 {
					mustWrite(t, filepath.Join(dir, sub, "f"+strconv.Itoa(i)), "x", 0o600)
				}
			}
			mustMkdir(t, filepath.Join(dir, "wide", "inner"), 0o700)
			mustWrite(t, filepath.Join(dir, "wide", "inner", "f"), "x", 0o600)
			mustMkdir(t, filepath.Join(dir, "locked"), 0o500)
		}},
		{"directories of mode 0 and 0500 holding files", func(t *testing.T, dir string) {
			mustMkdir(t, dir, 0o700)
			paths := []string{"a", "a/b", "a/b/c", "a/b/c/d", "e", "e/f"}
			for _, p := range paths {
				mustMkdir(t, filepath.Join(dir, p), 0o700)
				mustWrite(t, filepath.Join(dir, p, "file"), "x", 0o600)
			}
			mustWrite(t, filepath.Join(dir, "a", "b", "c", "d", "zero"), "x", 0)
			// Lock them from the deepest up, so each can still be reached.
			for i, p := range []string{"a/b/c/d", "a/b/c", "a/b", "a", "e/f", "e"} {
				mode := os.FileMode(0)
				if i%2 == 1 {
					mode = 0o500
				}
				if err := os.Chmod(filepath.Join(dir, p), mode); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"a file of mode 0", func(t *testing.T, dir string) {
			mustMkdir(t, dir, 0o700)
			mustWrite(t, filepath.Join(dir, "zero"), "x", 0)
		}},
		{"a directory of mode 0", func(t *testing.T, dir string) {
			mustMkdir(t, dir, 0o700)
			mustWrite(t, filepath.Join(dir, "file"), "x", 0o600)
			mustMkdir(t, filepath.Join(dir, "sub"), 0o700)
			mustWrite(t, filepath.Join(dir, "sub", "file"), "x", 0o600)
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"a mode 0 directory in the way of an empty one", func(t *testing.T, dir string) {
			mustMkdir(t, filepath.Join(dir, "empty"), 0)
			mustMkdir(t, filepath.Join(dir, "x", "empty"), 0)
			if err := os.Chmod(filepath.Join(dir, "x"), 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newTreeDir(t)
			tc.build(t, dir)
			if err := removeTree(dir); err != nil {
				t.Fatalf("removeTree: %v", err)
			}
			requireGone(t, dir)
		})
	}
}

// TestRemoveTreeMissing checks that a directory that does not exist is not
// an error, since a home may already be gone.
func TestRemoveTreeMissing(t *testing.T) {
	t.Parallel()
	if err := removeTree(filepath.Join(t.TempDir(), "nothing")); err != nil {
		t.Errorf("removeTree of a missing directory: %v", err)
	}
}

// TestRemoveTreeNotDirectory checks that a path that is a file or a link to
// a directory is an error and leaves the target alone, so removeTree never
// follows a link given as the root.
func TestRemoveTreeNotDirectory(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	mustMkdir(t, target, 0o700)
	mustWrite(t, filepath.Join(target, "keep"), "keep", 0o600)
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := removeTree(link); err == nil {
		t.Error("removeTree of a link to a directory succeeded")
	}
	if b, err := readTest(target, "keep"); err != nil || string(b) != "keep" {
		t.Errorf("the link's target was changed: %q, %v", b, err)
	}
}

// deepChain makes a chain of depth directories below the directory fd, each
// holding a file, with the mode modeAt gives for its level. The chain's top
// is the entry "top" of fd. Making one by descending would be slow, since the kernel
// resolves ever longer paths, so it wraps the chain made so far in a new
// directory next to fd's entries, which keeps every path to a name or two.
func deepChain(t *testing.T, fd, depth int, modeAt func(level int) uint32) {
	t.Helper()
	const flags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC
	mkfile := func(dirfd int) {
		f, err := unix.Openat(dirfd, "f", flags, 0o600)
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		if err := unix.Close(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkdirat(fd, "top", 0o700); err != nil {
		t.Fatal(err)
	}
	// The deepest directory is level depth, and the top, wrapped last, is 0.
	top, err := unix.Openat(fd, "top", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	mkfile(top)
	_ = unix.Close(top)
	if err := unix.Fchmodat(fd, "top", modeAt(depth), 0); err != nil {
		t.Fatal(err)
	}
	for level := depth - 1; level >= 0; level-- {
		if err := unix.Mkdirat(fd, "wrap", 0o700); err != nil {
			t.Fatalf("level %d: mkdir: %v", level, err)
		}
		wrap, err := unix.Openat(fd, "wrap", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		mkfile(wrap)
		// Moving a directory rewrites its "..", so it must be writable
		// until it is moved, and is locked after.
		if err := unix.Fchmodat(fd, "top", 0o700, 0); err != nil {
			t.Fatal(err)
		}
		if err := unix.Renameat(fd, "top", wrap, "d"); err != nil {
			t.Fatalf("level %d: wrap: %v", level, err)
		}
		if err := unix.Fchmodat(wrap, "d", modeAt(level+1), 0); err != nil {
			t.Fatalf("level %d: chmod: %v", level, err)
		}
		_ = unix.Close(wrap)
		if err := unix.Renameat(fd, "wrap", fd, "top"); err != nil {
			t.Fatalf("level %d: rename: %v", level, err)
		}
	}
}

// TestRemoveTreeDeep checks that a chain of 2000 directories, whose full
// path is far past PATH_MAX, with locked modes at several depths, is
// removed. A build could make one, and removal must not need a path, or a
// descriptor for each level.
func TestRemoveTreeDeep(t *testing.T) {
	t.Parallel()
	const depth = 2000
	dir := newTreeDir(t)
	mustMkdir(t, dir, 0o700)
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	deepChain(t, fd, depth, func(level int) uint32 {
		switch level % 4 {
		case 1:
			return 0
		case 2:
			return 0o500
		}
		return 0o700
	})
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	if err := removeTree(dir); err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	requireGone(t, dir)
}

// TestRemoveTreeLinks checks that links in the tree are removed as links:
// what a link to a file or a directory outside leads to, contents and mode
// alike, is untouched, and a dangling link goes too.
func TestRemoveTreeLinks(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	mustWrite(t, secret, "keep", 0o640)
	outDir := filepath.Join(outside, "dir")
	mustMkdir(t, outDir, 0o750)
	mustWrite(t, filepath.Join(outDir, "inner"), "keep", 0o640)

	dir := newTreeDir(t)
	mustMkdir(t, filepath.Join(dir, "sub", "deeper"), 0o700)
	for link, target := range map[string]string{
		"filelink":               secret,
		"dirlink":                outDir,
		"dangling":               filepath.Join(outside, "nothing"),
		"sub/filelink":           secret,
		"sub/dirlink":            outDir,
		"sub/deeper/dirlink":     outDir,
		"sub/deeper/relative":    "../../../../../../../../nonexistent",
		"sub/deeper/dangling-up": "..",
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "sub", "deeper"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "sub"), 0); err != nil {
		t.Fatal(err)
	}

	if err := removeTree(dir); err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	requireGone(t, dir)
	for _, p := range []string{"secret", "dir/inner"} {
		if b, err := readTest(outside, p); err != nil || string(b) != "keep" {
			t.Errorf("%s was changed through a link: %q, %v", p, b, err)
		}
	}
	if info, err := os.Stat(secret); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("secret's mode changed: %v, %v", info, err)
	}
	if info, err := os.Stat(outDir); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("the outside directory's mode changed: %v, %v", info, err)
	}
}

// TestRemoveTreeNameTaken checks that a tree already holding entries named
// like the ones removeTree moves things to, in the root and below it, still
// comes out empty: a taken name is skipped, not replaced or a failure.
func TestRemoveTreeNameTaken(t *testing.T) {
	t.Parallel()
	dir := newTreeDir(t)
	mustMkdir(t, dir, 0o700)
	// A file and a directory with entries hold names the move would use.
	mustWrite(t, filepath.Join(dir, ".rm-1"), "x", 0o600)
	mustMkdir(t, filepath.Join(dir, ".rm-2", "inner"), 0o700)
	mustWrite(t, filepath.Join(dir, ".rm-2", "inner", "f"), "x", 0o600)
	// A subdirectory whose entries carry such names too, and a directory of
	// them to move up.
	sub := filepath.Join(dir, "sub")
	mustMkdir(t, sub, 0o700)
	for _, name := range []string{".rm-1", ".rm-2", ".rm-3", "plain"} {
		mustWrite(t, filepath.Join(sub, name), "x", 0o600)
	}
	for _, name := range []string{"dir-a", ".rm-4"} {
		mustMkdir(t, filepath.Join(sub, name, "x"), 0o700)
		mustWrite(t, filepath.Join(sub, name, "x", "f"), "x", 0o600)
		if err := os.Chmod(filepath.Join(sub, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeTree(dir); err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	requireGone(t, dir)
}

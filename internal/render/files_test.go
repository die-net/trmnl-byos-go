//go:build unix

package render

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestWritableOutside checks that the startup scan finds what the user owns
// outside the scratch roots, even where its mode says it is not writable,
// since the user may chmod it, but nothing inside the roots, and nothing in
// a system directory it neither owns nor can write. It gives up past its
// limit.
func TestWritableOutside(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	for _, d := range []string{"ro/sub", "scratch/x"} {
		if err := os.MkdirAll(filepath.Join(top, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(top, "ro", "sub", "f"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	// Directories need their search bits.
	for _, d := range []string{"ro/sub", "ro"} {
		if err := os.Chmod(filepath.Join(top, d), 0o500); err != nil { //nolint:gosec // G302: a directory, made read-only.
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(top, d), 0o700) }) //nolint:gosec // G302: a directory, made removable again.
	}
	uid := os.Getuid()
	for _, tc := range []struct {
		name          string
		mounts, roots []string
		want          []string
	}{
		{"owned, not writable", []string{filepath.Join(top, "ro")}, nil, []string{filepath.Join(top, "ro")}},
		{"owned file", []string{filepath.Join(top, "ro", "sub", "f")}, nil, []string{filepath.Join(top, "ro", "sub", "f")}},
		{"inside a root", []string{top}, []string{top}, nil},
		{"a root inside", []string{filepath.Join(top, "scratch")}, []string{filepath.Join(top, "scratch")}, nil},
		{"system", []string{"/usr/bin"}, nil, nil},
	} {
		found, err := writableOutside(tc.mounts, tc.roots, uid, 100_000)
		if err != nil || !slices.Equal(found, tc.want) {
			t.Errorf("%s: found %q, %v; want %q", tc.name, found, err, tc.want)
		}
	}
	if _, err := writableOutside([]string{"/usr/bin"}, nil, uid, 2); !errors.Is(err, errScanTooLarge) {
		t.Errorf("past the limit: %v, want errScanTooLarge", err)
	}
}

// TestCheckRoots checks that a cleanup root of /, one holding another mount
// point, and one holding a path the shim needs are refused.
func TestCheckRoots(t *testing.T) {
	t.Parallel()
	mounts := []mount{{point: "/"}, {point: "/tmp", writable: true}, {point: "/home/render/cache", writable: true}}
	for _, tc := range []struct {
		name   string
		roots  []string
		needed []string
		ok     bool
	}{
		{"usual", []string{"/tmp", "/var/tmp", "/dev/shm"}, []string{"/usr/local/bin/trmnl-render"}, true},
		{"nested", []string{"/tmp", "/tmp/trmnl-render", "/var/tmp"}, nil, true},
		{"root", []string{"/tmp", "/"}, nil, false},
		{"mount inside", []string{"/tmp", "/home/render"}, nil, false},
		{"binary inside", []string{"/tmp", "/opt/render"}, []string{"/opt/render/bin/trmnl-render"}, false},
	} {
		if err := checkRoots(tc.roots, mounts, tc.needed); (err == nil) != tc.ok {
			t.Errorf("%s: checkRoots = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

// TestUnlistable checks that the startup scan may skip a directory it can
// neither list nor search, but refuses one it can search without listing,
// as with mode 0711, since a directory the user can write may sit inside,
// reachable by name.
func TestUnlistable(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	for _, tc := range []struct {
		name string
		mode os.FileMode
		// skip is whether the scan may skip it. Root may search any
		// directory, so it never skips one.
		skip bool
	}{
		{"searchable", 0o311, false},
		{"not searchable", 0o000, os.Geteuid() != 0},
	} {
		dir := filepath.Join(top, strings.ReplaceAll(tc.name, " ", "-"))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, tc.mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a directory, made removable again.
		_, listErr := os.ReadDir(dir)
		if os.Geteuid() != 0 && listErr == nil {
			t.Fatalf("%s: listed a directory of mode %o", tc.name, tc.mode)
		}
		if err := unlistable(dir, listErr); (err == nil) != tc.skip {
			t.Errorf("%s: unlistable = %v, want skip %v", tc.name, err, tc.skip)
		}
	}
}

// TestScratchRootsHomeSlash checks that a home of /, which an account
// without one gets, is not a cleanup root.
func TestScratchRootsHomeSlash(t *testing.T) {
	t.Setenv("HOME", "/")
	if roots := scratchRoots(); slices.Contains(roots, "/") {
		t.Errorf("scratchRoots = %q, with /", roots)
	}
}

// TestCleanFiles checks what the cleanup leaves and removes: a link the
// user owns goes, but not what it points to; a tree nested deeper than
// PATH_MAX goes; a root the user owns but made unwritable is made writable
// and emptied; and a tree nested past the bound stops the cleanup.
func TestCleanFiles(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	root, outside := filepath.Join(top, "root"), filepath.Join(top, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "kept"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// 80 names of 64 bytes is past PATH_MAX, 4096 on Linux, but short of
	// the 8 KB of path AppArmor allows, which Docker's profile applies.
	nest(t, root, "deep", 80, 60)
	if err := os.Chmod(root, 0o500); err != nil { //nolint:gosec // G302: a directory, made unwritable.
		t.Fatal(err)
	}
	// A root that does not exist is skipped, and the next is still cleaned.
	removed, err := cleanFiles([]string{filepath.Join(top, "missing"), root}, os.Getuid(), cleanTimeout)
	if err != nil || removed != 2 {
		t.Errorf("cleanFiles = %d, %v; want 2 removed", removed, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("root holds %d entries, %v", len(entries), err)
	}
	if _, err := os.Stat(filepath.Join(outside, "kept")); err != nil {
		t.Errorf("the link's target went: %v", err)
	}

	nest(t, root, "d", maxCleanDepth+10, 0)
	if _, err := cleanFiles([]string{root}, os.Getuid(), cleanTimeout); err == nil {
		t.Error("a tree past the bound was cleaned")
	}
	t.Cleanup(func() { _, _ = cleanFiles([]string{root}, os.Getuid(), cleanTimeout) })
}

// nest makes depth directories, each inside the last, under dir, named name
// and pad more bytes. It works relative to each directory, as a build could,
// so the deepest path may be longer than PATH_MAX.
func nest(t *testing.T, dir, name string, depth, pad int) {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	long := name + strings.Repeat("x", pad)
	for range depth {
		if err := unix.Mkdirat(fd, long, 0o700); err != nil {
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, long, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	_ = unix.Close(fd)
}

// TestOpenEntry checks how the walk opens a directory it listed, which both
// the walk into a nested root and the removal of the user's own directories
// go through: an entry that has vanished since the listing, with or without
// the chmod first, is reported gone with no error, so the walk skips it and
// goes on to its siblings, but any other failure is an error, which stops
// the cleanup.
func TestOpenEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	for _, tc := range []struct {
		name, entry string
		chmod       bool
		opened      bool
		gone        bool
	}{
		{"vanished", "missing", false, false, true},
		{"vanished before the chmod", "missing", true, false, true},
		{"directory", "sub", true, true, false},
		{"file", "file", false, false, false},
		{"link", "link", false, false, false},
	} {
		sub, gone, err := openEntry(fd, tc.entry, tc.chmod)
		if sub >= 0 {
			_ = unix.Close(sub)
		}
		if gone != tc.gone || (sub >= 0) != tc.opened || (err == nil) != (tc.opened || tc.gone) {
			t.Errorf("%s: openEntry = %d, gone %v, %v", tc.name, sub, gone, err)
		}
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(dir, "sub"), &st); err != nil || st.Mode&0o700 != 0o700 {
		t.Errorf("the chmod did not make the directory writable: %o, %v", st.Mode, err)
	}
}

// TestCleanForeign checks that an entry another user owns in a cleanup
// root refuses startup, naming it, and fails a sweep, without changing
// anything inside it, and that a root that is the user's own passes. The
// walk runs for another user, so the test's own entries count as someone
// else's.
func TestCleanForeign(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for i := range 7 {
		if err := os.WriteFile(filepath.Join(root, "theirs"+strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := os.Getuid() + 1
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"startup", func() error { return checkPrivate([]string{root}, other) }},
		{"sweep", func() error { _, err := cleanFiles([]string{root}, other, cleanTimeout); return err }},
	} {
		err := tc.run()
		if err == nil || !strings.Contains(err.Error(), "theirs") || !strings.Contains(err.Error(), "and 2 more") || !strings.Contains(err.Error(), "PrivateTmp") {
			t.Errorf("%s: %v, want a refusal naming five entries and two more", tc.name, err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 7 {
		t.Errorf("root holds %d entries, %v; want all 7 left", len(entries), err)
	}
	// The user's own entries pass the startup check, which removes nothing.
	if err := checkPrivate([]string{root}, os.Getuid()); err != nil {
		t.Errorf("checkPrivate of the user's own root: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 7 {
		t.Errorf("the startup check removed entries: %d left, %v", len(entries), err)
	}
}

// TestCleanRacesRemoval checks that the cleanup does not fail when what it
// is removing goes from under it, as when, with more than one build at
// once, a build's handler removes its own directory while the last build's
// sweep walks it. Each round races a removal of a tree against the cleanup
// of its root.
func TestCleanRacesRemoval(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for round := range 200 {
		tree := filepath.Join(root, "build-"+strconv.Itoa(round))
		for _, d := range []string{"cache/a", "cache/b", "home/c", "tmp/d"} {
			if err := os.MkdirAll(filepath.Join(tree, d), 0o700); err != nil {
				t.Fatal(err)
			}
			for i := range 20 {
				if err := os.WriteFile(filepath.Join(tree, d, strconv.Itoa(i)), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		done := make(chan error, 1)
		go func() { done <- os.RemoveAll(tree) }()
		if _, err := cleanFiles([]string{root}, os.Getuid(), cleanTimeout); err != nil {
			t.Fatalf("round %d: cleanFiles = %v", round, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("round %d: RemoveAll = %v", round, err)
		}
	}
}

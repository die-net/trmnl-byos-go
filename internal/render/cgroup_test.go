package render

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOOMKilled checks that a build counts as killed for its memory when its
// cgroup's memory.events says the kernel killed a process of it, singly or
// as a group, and not for other events or when the file cannot be read.
func TestOOMKilled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// events is the file's content, or "" for no file.
		events string
		want   bool
	}{
		{"no kills", "low 0\nhigh 0\nmax 3\noom 1\noom_kill 0\n", false},
		{"a kill", "oom_kill 2", true},
		{"a group kill", "oom_group_kill 1", true},
		{"a kill among others", "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\noom_group_kill 0\n", true},
		{"max and oom only", "max 3\noom 1\n", false},
		{"not a number", "oom_kill many\n", false},
		{"empty", "\n", false},
		{"no file", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.events != "" {
				if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(tc.events), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			cg := &buildCgroup{path: dir, root: root}
			if got := cg.oomKilled(); got != tc.want {
				t.Errorf("oomKilled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWriteControl checks that a control file is written in its cgroup's
// directory only: a name that leaves it is refused, and one that does not
// exist is reported as such, which newBuildCgroup relies on for a kernel
// with no swap accounting.
func TestWriteControl(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeControl(root, "memory.max", "1024"); err != nil {
		t.Fatalf("writeControl: %v", err)
	}
	if b, err := root.ReadFile("memory.max"); err != nil || string(b) != "1024" {
		t.Errorf("memory.max = %q, %v", b, err)
	}
	if err := writeControl(root, "memory.swap.max", "0"); !os.IsNotExist(err) {
		t.Errorf("a missing control file gave %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.swap.max")); err == nil {
		t.Error("writeControl made a control file, which only the kernel does")
	}
	if err := writeControl(root, "../escape", "1"); err == nil {
		t.Error("a name outside the cgroup was written")
	}
}

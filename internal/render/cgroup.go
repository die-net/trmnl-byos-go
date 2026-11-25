package render

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A delegated cgroup v2 directory bounds what a build uses more closely
// than resource limits and a watchdog can: memory with the tmpfs and shared
// memory a build writes counted, and its tasks. Each build runs in a child
// of the directory, made for it and removed after it.

// enableControllers enables the memory and pids controllers for the
// children of the cgroup dir. The kernel refuses that while a process
// runs in dir itself, so the server must run in a child of its own, as
// systemd's DelegateSubgroup= puts it.
func enableControllers(dir string) error {
	if err := writeCgroup(dir, "cgroup.subtree_control", "+memory +pids"); err != nil {
		return fmt.Errorf("enable the memory and pids controllers in the build cgroup %s: %w; the server must be able to write it, and must not run in it itself, which DelegateSubgroup= in its systemd unit gives", dir, err)
	}
	return nil
}

// buildCgroup is the cgroup one build runs in.
type buildCgroup struct {
	path string
	// root is the directory, which control files are opened in.
	root *os.Root
	// dir is the directory open, for Cmd.CgroupFD.
	dir *os.File
}

// newBuildCgroup makes the cgroup name in parent for one build, with
// memory as its memory limit, no swap, and MaxTasks tasks. When the kernel
// kills a process of it for its memory, it kills them all, so no build
// goes on without the process it lost.
func newBuildCgroup(parent, name string, memory int64) (*buildCgroup, error) {
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, fmt.Errorf("create the build's cgroup: %w", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("open the build's cgroup: %w", err)
	}
	cg := &buildCgroup{path: path, root: root}
	for _, f := range []struct{ name, value string }{
		{"memory.max", strconv.FormatInt(memory, 10)},
		{"memory.swap.max", "0"},
		{"pids.max", strconv.Itoa(MaxTasks)},
		{"memory.oom.group", "1"},
	} {
		err := writeControl(root, f.name, f.value)
		// A kernel without swap accounting has no memory.swap.max, and
		// then no swap to limit.
		if errors.Is(err, fs.ErrNotExist) && f.name == "memory.swap.max" {
			continue
		}
		if err != nil {
			_ = cg.remove()
			return nil, fmt.Errorf("set up the build's cgroup: %w", err)
		}
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = cg.remove()
		return nil, fmt.Errorf("open the build's cgroup: %w", err)
	}
	cg.dir = dir
	return cg, nil
}

// writeCgroup writes value to the control file name in the cgroup dir.
func writeCgroup(dir, name, value string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeControl(root, name, value)
}

// writeControl writes value to the control file name in the cgroup
// directory root.
func writeControl(root *os.Root, name, value string) error {
	f, err := root.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s to %s: %w", value, name, err)
	}
	return f.Close()
}

// oomKilled reports whether the kernel killed a process of the cgroup
// for its memory.
func (c *buildCgroup) oomKilled() bool {
	b, err := c.root.ReadFile("memory.events")
	if err != nil {
		return false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, n, _ := strings.Cut(sc.Text(), " ")
		if name == "oom_kill" || name == "oom_group_kill" {
			if v, err := strconv.Atoi(n); err == nil && v > 0 {
				return true
			}
		}
	}
	return false
}

// cgroupRemoveWait bounds how long remove waits for the kernel to end the
// cgroup's processes.
const cgroupRemoveWait = 5 * time.Second

// remove kills whatever still runs in the cgroup, and removes it. The
// sandbox has ended every process of the build by then, so the kill is
// only in case one did not.
func (c *buildCgroup) remove() error {
	if c.dir != nil {
		_ = c.dir.Close()
	}
	var err error
	if c.root != nil {
		err = writeControl(c.root, "cgroup.kill", "1")
		_ = c.root.Close()
	} else {
		err = writeCgroup(c.path, "cgroup.kill", "1")
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("kill the build's cgroup: %w", err)
	}
	// The kill is not waited for, and the directory cannot go while a
	// process is in it.
	deadline := time.Now().Add(cgroupRemoveWait)
	for {
		err := os.Remove(c.path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("remove the build's cgroup: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

package render

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// listProcs reads /proc for the live processes of uid but this one. A
// process that exits while it is read is skipped. As process 1 the shim
// lists zombies too (see listed).
func listProcs(uid int) ([]proc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	want := strconv.Itoa(uid)
	var procs []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		p, mine, err := readStatus(pid, want)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if listed(p, mine, self == 1) {
			procs = append(procs, p)
		}
	}
	return procs, nil
}

// listed reports whether a sweep must wait for p, a process of the user
// when mine is set. A zombie has exited and holds no environment, so the
// sweep need not wait for it, unless the shim is reaper, process 1 of a
// container: every orphan is then its child, and a zombie holds a process
// ID, counted against the container's limit, until the shim reaps it. So
// the sweep keeps killing, which reaps, until none is left.
func listed(p proc, mine, reaper bool) bool {
	return mine && (!p.zombie || reaper)
}

// readStatus reads a process's status file, and reports whether it is a
// process of the user ID uid, in decimal.
func readStatus(pid int, uid string) (proc, bool, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return proc{}, false, err
	}
	defer f.Close()
	p, ok, err := parseStatus(f, uid)
	p.pid = pid
	if err != nil {
		return proc{}, false, fmt.Errorf("read status of %d: %w", pid, err)
	}
	return p, ok, nil
}

// parseStatus reads a process's name, state, thread count and user IDs
// from the format of /proc/<pid>/status, and reports whether it is a
// process of the user ID uid, in decimal. A process whose main thread has
// exited shows the state Z while its other threads run, so it counts as a
// zombie only with one thread left.
func parseStatus(r io.Reader, uid string) (proc, bool, error) {
	var p proc
	var dead, mine bool
	threads := 1
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, value, _ := strings.Cut(sc.Text(), ":")
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			p.name = value
		case "State":
			// Z is a zombie, X a process being reaped.
			dead = strings.HasPrefix(value, "Z") || strings.HasPrefix(value, "X")
		case "Threads":
			if n, err := strconv.Atoi(value); err == nil {
				threads = n
			}
		case "Uid":
			// Real, effective, saved and filesystem IDs. The first three
			// decide who may signal the process.
			ids := strings.Fields(value)
			for _, id := range ids[:min(len(ids), 3)] {
				if id == uid {
					mine = true
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return proc{}, false, err
	}
	p.zombie = dead && threads <= 1
	return p, mine, nil
}

// virtualFS lists file systems that hold no files a build could leave.
// POSIX message queues do, in /dev/mqueue, which is a scratch root.
var virtualFS = []string{
	"proc", "sysfs", "cgroup", "cgroup2", "devpts", "debugfs", "tracefs",
	"securityfs", "pstore", "bpf", "configfs", "fusectl", "binfmt_misc", "nsfs", "autofs",
}

// listMounts returns this process's mounts, from /proc/self/mountinfo.
func listMounts() ([]mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountinfo(f)
}

// parseMountinfo reads the format of /proc/self/mountinfo. A mount is
// writable when it is mounted rw and holds files: not a virtual file
// system, and not under /proc or /sys.
func parseMountinfo(r io.Reader) ([]mount, error) {
	var mounts []mount
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := slices.Index(fields, "-")
		if sep < 6 || sep+1 >= len(fields) {
			return nil, fmt.Errorf("mountinfo line %q", sc.Text())
		}
		point, opts, fstype := unescapeMount(fields[4]), strings.Split(fields[5], ","), fields[sep+1]
		var super []string
		if sep+3 < len(fields) {
			super = strings.Split(fields[sep+3], ",")
		}
		readOnly := slices.Contains(opts, "ro") || slices.Contains(super, "ro")
		writable := !readOnly && !slices.Contains(virtualFS, fstype) &&
			!within(point, "/proc") && !within(point, "/sys")
		mounts = append(mounts, mount{point: point, writable: writable})
	}
	return mounts, sc.Err()
}

// readOnlyFS reports whether the open directory fd is on a read-only mount,
// as a whole or as a read-only bind, such as systemd's ProtectHome=read-only
// makes, where no user can change anything.
func readOnlyFS(fd int) (bool, error) {
	var s unix.Statfs_t
	if err := unix.Fstatfs(fd, &s); err != nil {
		return false, err
	}
	return s.Flags&unix.ST_RDONLY != 0, nil
}

// unescapeMount undoes mountinfo's octal escapes, such as \040 for a space.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

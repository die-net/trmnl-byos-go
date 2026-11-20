package render

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestParseMountinfo checks that the startup scan gets the writable file
// systems of a container with a read-only root, without virtual ones or
// anything under /proc or /sys, and with escaped names undone.
func TestParseMountinfo(t *testing.T) {
	t.Parallel()
	const mountinfo = `447 385 0:48 / / ro,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w
449 447 0:57 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
450 447 0:58 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755,inode64
451 450 0:59 / /dev/pts rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=666
452 447 0:60 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
454 450 0:55 / /dev/mqueue rw,nosuid,nodev,noexec,relatime - mqueue mqueue rw
455 450 0:63 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k,inode64
456 447 253:17 /docker/containers/abc/hosts /etc/hosts rw,relatime - ext4 /dev/vdb1 rw
460 447 0:70 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw,size=1048576k,mode=1777
461 447 0:71 / /srv/with\040space rw,relatime shared:5 - tmpfs tmpfs rw
392 449 0:58 /null /proc/interrupts rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755,inode64
`
	mounts, err := parseMountinfo(strings.NewReader(mountinfo))
	var got []string
	for _, m := range mounts {
		if m.writable {
			got = append(got, m.point)
		}
	}
	want := []string{"/dev", "/dev/mqueue", "/dev/shm", "/etc/hosts", "/tmp", "/srv/with space"}
	if err != nil || len(mounts) != 11 || !slices.Equal(got, want) {
		t.Errorf("parseMountinfo = %q, %v; want %q", got, err, want)
	}
	if _, err := parseMountinfo(strings.NewReader("1 2 3\n")); err == nil {
		t.Error("a short line was accepted")
	}
}

// TestParseStatus checks which processes count as zombies: one with one
// thread does, but one whose main thread exited while another runs does
// not, though both show the state Z.
func TestParseStatus(t *testing.T) {
	t.Parallel()
	status := func(state string, threads int) string {
		return "Name:\tghost\nState:\t" + state + "\nTgid:\t411\nPid:\t411\nUid:\t10001\t10001\t10001\t10001\nThreads:\t" + strconv.Itoa(threads) + "\n"
	}
	for _, tc := range []struct {
		name, status, uid string
		mine, zombie      bool
	}{
		{"running", status("S (sleeping)", 1), "10001", true, false},
		{"zombie", status("Z (zombie)", 1), "10001", true, true},
		{"main thread exited", status("Z (zombie)", 2), "10001", true, false},
		{"being reaped", status("X (dead)", 1), "10001", true, true},
		{"another user", status("S (sleeping)", 1), "10002", false, false},
	} {
		p, mine, err := parseStatus(strings.NewReader(tc.status), tc.uid)
		if err != nil || mine != tc.mine || p.zombie != tc.zombie || p.name != "ghost" {
			t.Errorf("%s: %+v, mine %v, %v; want mine %v, zombie %v", tc.name, p, mine, err, tc.mine, tc.zombie)
		}
	}
}

// init keeps the main goroutine on the main thread when the test binary is
// to become a ghost. Only an init function is sure to run there, and
// becomeGhost must exit the main thread itself.
//
//nolint:gochecknoinits // Only an init function runs on the main thread for certain.
func init() {
	if os.Getenv("TRMNL_TEST_LEFTOVER") == "ghost" {
		runtime.LockOSThread()
	}
}

// becomeGhost exits the main thread of the test binary while another
// thread runs, as a leftover can to look like a zombie. The process then
// shows the state Z with two threads, until it is killed. It returns only
// if it cannot.
func becomeGhost() int {
	if unix.Gettid() != os.Getpid() {
		return 3 // Not the main thread.
	}
	go func() {
		for {
			time.Sleep(time.Second)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	if _, _, errno := unix.RawSyscall(unix.SYS_EXIT, 0, 0, 0); errno != 0 {
		return 4
	}
	return 0
}

// TestGhostListed checks that a process whose main thread exited is listed
// as live, so a sweep kills it and waits for it.
func TestGhostListed(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ghost := exec.CommandContext(t.Context(), self) //nolint:gosec // G204: the test binary itself.
	ghost.Env = append(os.Environ(), "TRMNL_TEST_LEFTOVER=ghost")
	if err := ghost.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ghost.Process.Kill(); _ = ghost.Wait() })
	pid := ghost.Process.Pid
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "State:\tZ") && !strings.Contains(string(b), "Threads:\t1\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ghost never lost its main thread:\n%s", b)
		}
	}
	procs, err := ownProcs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(procs, func(p proc) bool { return p.pid == pid }) {
		t.Errorf("the ghost %d is not listed", pid)
	}
}

// TestSysvipc checks that the System V IPC objects a process makes are
// listed by their owner and removed. It removes only its own, since the
// user running the tests may have others.
func TestSysvipc(t *testing.T) {
	t.Parallel()
	const fixture = `       key      shmid perms                  size  cpid  lpid nattch   uid   gid  cuid  cgid      atime      dtime      ctime                   rss                  swap
  1592524013          7  1600                  4096   411     0      0 10001 10001 10001 10001          0          0 1795190668                     0                     0
           0          8  1600                  4096   412     0      0     0     0 10001     0          0          0 1795190668                     0                     0
           0          9  1600                  4096   413     0      0     0     0     0     0          0          0 1795190668                     0                     0
`
	got, err := parseSysvipc(strings.NewReader(fixture), "shm", 10001)
	if want := []ipcObject{{"shm", 7}, {"shm", 8}}; err != nil || !slices.Equal(got, want) {
		t.Errorf("parseSysvipc = %v, %v; want %v", got, err, want)
	}

	// Each is removed at the end even if the test fails before removeIPC.
	shm, err := unix.SysvShmGet(unix.IPC_PRIVATE, 4096, unix.IPC_CREAT|0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = removeIPC([]ipcObject{{"shm", shm}}) })
	msg, _, errno := unix.Syscall(unix.SYS_MSGGET, unix.IPC_PRIVATE, unix.IPC_CREAT|0o600, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	t.Cleanup(func() { _, _ = removeIPC([]ipcObject{{"msg", int(msg)}}) })
	sem, _, errno := unix.Syscall(unix.SYS_SEMGET, unix.IPC_PRIVATE, 1, unix.IPC_CREAT|0o600)
	if errno != 0 {
		t.Fatal(errno)
	}
	t.Cleanup(func() { _, _ = removeIPC([]ipcObject{{"sem", int(sem)}}) })
	made := []ipcObject{{"shm", shm}, {"msg", int(msg)}, {"sem", int(sem)}}
	mine := func(o ipcObject) bool { return slices.Contains(made, o) }
	objs, err := listIPC(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(slices.DeleteFunc(slices.Clone(objs), func(o ipcObject) bool { return !mine(o) })); n != len(made) {
		t.Fatalf("listIPC found %d of the %d made: %v", n, len(made), objs)
	}
	if removed, err := removeIPC(made); err != nil || removed != len(made) {
		t.Errorf("removeIPC = %d, %v", removed, err)
	}
	if objs, err = listIPC(os.Getuid()); err != nil || slices.ContainsFunc(objs, mine) {
		t.Errorf("after removal: %v, %v", objs, err)
	}
}

// TestIoprioNormalized checks that a thread whose I/O priority the kernel
// derived from its nice value, as it does once the thread has done I/O
// through BFQ, matches one that reports none, but that a priority set
// explicitly does not.
func TestIoprioNormalized(t *testing.T) {
	t.Parallel()
	sched := unix.SchedAttr{Policy: unix.SCHED_NORMAL}
	be4 := uintptr(ioprioClassBE<<ioprioClassShift | 4)
	base := selfState{threads: []threadState{{tid: 1, sched: sched, ioprio: effectiveIoprio(0, &sched)}}}
	for _, tc := range []struct {
		name    string
		ioprio  uintptr
		changed bool
	}{
		{"none", 0, false},
		{"derived by BFQ", be4, false},
		{"set lower", ioprioClassBE<<ioprioClassShift | 7, true},
		{"set idle", ioprioClassIdle << ioprioClassShift, true},
	} {
		now := selfState{threads: []threadState{{tid: 2, sched: sched, ioprio: effectiveIoprio(tc.ioprio, &sched)}}}
		if err := base.changed(now); (err != nil) != tc.changed {
			t.Errorf("%s: changed = %v, want a change %v", tc.name, err, tc.changed)
		}
	}
	idle := unix.SchedAttr{Policy: unix.SCHED_IDLE}
	if got := effectiveIoprio(0, &idle); got != ioprioClassIdle<<ioprioClassShift|4 {
		t.Errorf("an idle thread's priority is %#x, want the idle class", got)
	}
}

// TestReadOnlyFS checks that the cleanup tells a read-only mount, where it
// leaves what the user owns since nobody can change it, from a writable
// one, against every mount the mount table lists, read-only ones such as a
// container's /sys or macOS's system volume included. A later mount at the
// same point hides an earlier one.
func TestReadOnlyFS(t *testing.T) {
	t.Parallel()
	// Whether each mount point is read-only, from the mount table, the
	// mount's own options or its file system's.
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for line := range strings.Lines(string(b)) {
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if sep < 6 || sep+3 >= len(fields) {
			continue
		}
		ro := slices.Contains(strings.Split(fields[5], ","), "ro") || slices.Contains(strings.Split(fields[sep+3], ","), "ro")
		dirs[unescapeMount(fields[4])] = ro
	}
	dirs[t.TempDir()] = false
	for dir, want := range dirs {
		fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			continue // Not a directory, or not one the user may open.
		}
		got, err := readOnlyFS(fd)
		_ = unix.Close(fd)
		if err != nil || got != want {
			t.Errorf("%s: readOnlyFS = %v, %v; want %v", dir, got, err, want)
		}
	}
}

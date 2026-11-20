package render

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// rlimCount is the number of resource limits, RLIM_NLIMITS.
const rlimCount = 16

// ioprioWhoProcess makes ioprio_get read one thread, IOPRIO_WHO_PROCESS.
const ioprioWhoProcess = 1

// I/O priority classes, from <linux/ioprio.h>. A priority holds its class
// above ioprioClassShift and its level below.
const (
	ioprioClassShift = 13
	ioprioClassNone  = 0
	ioprioClassRT    = 1
	ioprioClassBE    = 2
	ioprioClassIdle  = 3
)

// selfState is what another process of the shim's user may change about
// the shim, which every later build would inherit: its resource limits,
// which prlimit sets, and, for each thread, its scheduling policy and nice
// value, CPU affinity and I/O priority. Its OOM score adjustment is checked
// too, though the shim is not dumpable, so its /proc files are not the
// user's to write.
type selfState struct {
	limits      [rlimCount]unix.Rlimit
	threads     []threadState
	oomScoreAdj string
}

// threadState is what may be changed about one thread.
type threadState struct {
	tid    int
	sched  unix.SchedAttr
	cpus   unix.CPUSet
	ioprio uintptr
}

// snapshotSelf reads the shim's state.
func snapshotSelf() (selfState, error) {
	var s selfState
	for i := range s.limits {
		if err := unix.Prlimit(0, i, nil, &s.limits[i]); err != nil {
			return s, fmt.Errorf("read resource limit %d: %w", i, err)
		}
	}
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return s, fmt.Errorf("list threads: %w", err)
	}
	for _, task := range tasks {
		tid, err := strconv.Atoi(task.Name())
		if err != nil {
			continue
		}
		t, err := readThread(tid)
		if errors.Is(err, unix.ESRCH) {
			continue // The thread exited.
		}
		if err != nil {
			return s, err
		}
		s.threads = append(s.threads, t)
	}
	b, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		return s, fmt.Errorf("read oom score adjustment: %w", err)
	}
	s.oomScoreAdj = strings.TrimSpace(string(b))
	return s, nil
}

func readThread(tid int) (threadState, error) {
	t := threadState{tid: tid}
	sched, err := unix.SchedGetAttr(tid, 0)
	if err != nil {
		return t, fmt.Errorf("read scheduling of thread %d: %w", tid, err)
	}
	t.sched = *sched
	if err := unix.SchedGetaffinity(tid, &t.cpus); err != nil {
		return t, fmt.Errorf("read CPU affinity of thread %d: %w", tid, err)
	}
	prio, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
	if errno != 0 {
		return t, fmt.Errorf("read I/O priority of thread %d: %w", tid, errno)
	}
	t.ioprio = effectiveIoprio(prio, &t.sched)
	return t, nil
}

// effectiveIoprio turns an I/O priority of class NONE into the one the
// kernel uses for it, which follows the thread's scheduling policy and
// nice value, as the kernel's task_nice_ioclass and task_nice_ioprio do.
// ioprio_get reports NONE for a thread that has done no I/O through a
// scheduler such as BFQ, and the derived priority once it has, so only an
// explicit ioprio_set changes the effective one.
func effectiveIoprio(prio uintptr, sched *unix.SchedAttr) uintptr {
	if prio>>ioprioClassShift != ioprioClassNone {
		return prio
	}
	class := uintptr(ioprioClassBE)
	switch sched.Policy {
	case unix.SCHED_IDLE:
		class = ioprioClassIdle
	case unix.SCHED_FIFO, unix.SCHED_RR, unix.SCHED_DEADLINE:
		class = ioprioClassRT
	}
	// Nice is from -20 to 19, so the level is from 0 to 7.
	level := (20 + sched.Nice) / 5
	if level < 0 || level > 7 {
		return prio
	}
	return class<<ioprioClassShift | uintptr(level)
}

// changed says what differs between s, read at startup, and now. Every
// thread now must match the first thread at startup, since the Go runtime
// starts and stops threads as it needs them.
func (s selfState) changed(now selfState) error {
	var diffs []string
	for i := range s.limits {
		if s.limits[i] != now.limits[i] {
			diffs = append(diffs, fmt.Sprintf("resource limit %d is %d/%d, was %d/%d", i, now.limits[i].Cur, now.limits[i].Max, s.limits[i].Cur, s.limits[i].Max))
		}
	}
	if len(s.threads) > 0 {
		base := s.threads[0]
		for i := range now.threads {
			t := &now.threads[i]
			if t.sched != base.sched || t.cpus != base.cpus || t.ioprio != base.ioprio {
				diffs = append(diffs, fmt.Sprintf("thread %d's scheduling, CPU affinity or I/O priority", t.tid))
			}
		}
	}
	if s.oomScoreAdj != now.oomScoreAdj {
		diffs = append(diffs, "the OOM score adjustment is "+now.oomScoreAdj+", was "+s.oomScoreAdj)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("a build changed the renderer itself: %s", strings.Join(diffs, "; "))
	}
	return nil
}

package render

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ipcObject is a System V IPC object: a shared memory segment, a message
// queue or a semaphore set. Each outlives the process that made it, until
// it is removed.
type ipcObject struct {
	// kind is shm, msg or sem, as /proc/sysvipc names it.
	kind string
	id   int
}

// listIPC returns the System V IPC objects that uid owns or created, from
// /proc/sysvipc.
func listIPC(uid int) ([]ipcObject, error) {
	dir, err := os.OpenRoot("/proc/sysvipc")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // A kernel built without System V IPC.
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var objs []ipcObject
	for _, kind := range []string{"shm", "msg", "sem"} {
		f, err := dir.Open(kind)
		if err != nil {
			return nil, err
		}
		found, err := parseSysvipc(f, kind, uid)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("read /proc/sysvipc/%s: %w", kind, err)
		}
		objs = append(objs, found...)
	}
	return objs, nil
}

// parseSysvipc reads the format of a /proc/sysvipc file, whose header names
// its columns, and returns the objects whose owner or creator is uid.
func parseSysvipc(r io.Reader, kind string, uid int) ([]ipcObject, error) {
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return nil, sc.Err()
	}
	header := strings.Fields(sc.Text())
	idCol := slices.IndexFunc(header, func(h string) bool { return h == "shmid" || h == "msqid" || h == "semid" })
	uidCol, cuidCol := slices.Index(header, "uid"), slices.Index(header, "cuid")
	if idCol < 0 || uidCol < 0 || cuidCol < 0 {
		return nil, fmt.Errorf("header %q", sc.Text())
	}
	want := strconv.Itoa(uid)
	var objs []ipcObject
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < len(header) {
			return nil, fmt.Errorf("line %q", sc.Text())
		}
		if fields[uidCol] != want && fields[cuidCol] != want {
			continue
		}
		id, err := strconv.Atoi(fields[idCol])
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", sc.Text(), err)
		}
		objs = append(objs, ipcObject{kind: kind, id: id})
	}
	return objs, sc.Err()
}

// removeIPC removes objs. One already gone is not an error.
func removeIPC(objs []ipcObject) (int, error) {
	removed := 0
	for _, o := range objs {
		var err error
		switch o.kind {
		case "shm":
			_, err = unix.SysvShmCtl(o.id, unix.IPC_RMID, nil)
		case "msg":
			_, _, errno := unix.Syscall(unix.SYS_MSGCTL, uintptr(o.id), unix.IPC_RMID, 0)
			err = errnoErr(errno)
		case "sem":
			_, _, errno := unix.Syscall6(unix.SYS_SEMCTL, uintptr(o.id), 0, unix.IPC_RMID, 0, 0, 0)
			err = errnoErr(errno)
		}
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EIDRM) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("remove System V %s %d: %w", o.kind, o.id, err)
		}
		removed++
	}
	return removed, nil
}

func errnoErr(errno unix.Errno) error {
	if errno == 0 {
		return nil
	}
	return errno
}

// cleanIPC removes every System V IPC object uid owns or created.
func cleanIPC(uid int) (int, error) {
	objs, err := listIPC(uid)
	if err != nil {
		return 0, err
	}
	return removeIPC(objs)
}

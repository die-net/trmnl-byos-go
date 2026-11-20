//go:build unix

package render

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// inProcessGroup runs cmd in a process group of its own, and makes a
// canceled build kill the whole group. trmnlp starts Firefox and
// geckodriver, which killing trmnlp alone would leave running.
func inProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
}

// killGroup kills what is left of cmd's process group, once it has started.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// killAll sends SIGKILL to every process this one may signal, but itself and
// process 1: every process of its user in its PID namespace. The kernel
// signals them all in one pass,
// so none can escape by forking while the signal goes out. As root it would
// signal every process on the host, so it refuses. As process 1 of a
// container, the shim is the parent of every orphan, so it then reaps what
// has exited, and the sweep lists zombies so it calls this again until all
// are reaped. That is safe only while no build runs, since it could reap a
// child a build waits for.
func killAll() error {
	if os.Geteuid() == 0 {
		return errors.New("refusing to kill every process as root")
	}
	err := syscall.Kill(-1, syscall.SIGKILL)
	if os.Getpid() == 1 {
		for {
			pid, werr := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			if pid <= 0 || werr != nil {
				break
			}
		}
	}
	if errors.Is(err, syscall.ESRCH) {
		return nil // Nothing left to signal.
	}
	return err
}

// openBuildFlags opens a file a build could have replaced without blocking,
// as opening a FIFO would until a writer came, and without following a
// link.
const openBuildFlags = syscall.O_NONBLOCK | syscall.O_NOFOLLOW

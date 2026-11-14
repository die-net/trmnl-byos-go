//go:build unix

package render

import (
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

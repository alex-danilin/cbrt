//go:build !windows

package projects

import (
	"os/exec"
	"syscall"
)

// killTree starts cmd in a process group of its own and makes cancelling
// it kill the whole group, so processes the agent started stop too.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

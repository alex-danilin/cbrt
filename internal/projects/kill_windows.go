//go:build windows

package projects

import (
	"os/exec"
	"strconv"
)

// killTree makes cancelling cmd kill its whole process tree by PID
// (taskkill /T): an agent run through a .cmd starts cmd.exe, which starts
// node, and killing only the direct child would leave node running. Never
// by image name: other agents may be running.
func killTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		tk := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := tk.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

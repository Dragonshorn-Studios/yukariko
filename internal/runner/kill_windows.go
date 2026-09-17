//go:build windows

package runner

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// setSysProcAttr keeps Windows console children quiet; Windows has no
// process groups in the POSIX sense, so tree termination is handled by
// killTree.
func setSysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killTree terminates the child and everything it spawned using the
// platform's taskkill with the tree flag, falling back to killing the child
// alone if taskkill is unavailable.
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err == nil {
		return nil
	}
	return p.Kill()
}

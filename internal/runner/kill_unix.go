//go:build unix

package runner

import (
	"os"
	"os/exec"
	"syscall"
)

// setSysProcAttr puts the child in its own process group so the whole tree
// can be signalled. Without this, grandchildren survive a kill of the child.
func setSysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree signals the child's process group with SIGKILL. The negative PID
// addresses the group; if group signalling fails the direct kill is a
// last-resort fallback.
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return p.Kill()
}

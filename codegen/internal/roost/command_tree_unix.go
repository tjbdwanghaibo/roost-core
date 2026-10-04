//go:build unix

package roost

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// treeInterruptSignals are the terminal and supervisor signals that no longer
// reach a command started in its own process group.
var treeInterruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// startAsTree puts the command in a new process group led by itself, so its
// pid names the group its children inherit.
func startAsTree(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

// killTree kills every process in the command's group, including children
// whose parent (the go tool) already exited.
func killTree(command *exec.Cmd) error {
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

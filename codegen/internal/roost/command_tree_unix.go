//go:build unix

package roost

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// interruptSignals are the terminal and supervisor signals the CLI entry owns
// (Main): a command started in its own process group no longer receives them.
var interruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

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

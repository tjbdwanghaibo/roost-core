//go:build windows

package roost

import (
	"os"
	"os/exec"
	"strconv"
)

// treeInterruptSignals is empty on Windows: the command stays in roost's
// console process group, so Ctrl-C reaches it and its children directly.
var treeInterruptSignals []os.Signal

func startAsTree(*exec.Cmd) {}

// killTree kills the command and its descendants. taskkill /T finds children
// by parent pid, so it has to run while the go process itself is still alive;
// if it fails (taskkill missing, process gone), fall back to killing the root.
func killTree(command *exec.Cmd) error {
	taskkill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid))
	if err := taskkill.Run(); err == nil {
		return nil
	}
	return command.Process.Kill()
}

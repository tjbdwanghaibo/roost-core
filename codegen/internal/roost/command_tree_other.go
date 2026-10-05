//go:build !unix && !windows

package roost

import (
	"os"
	"os/exec"
)

// Platforms without process groups or taskkill only get the WaitDelay bound.
var interruptSignals []os.Signal

func startAsTree(*exec.Cmd) {}

func killTree(command *exec.Cmd) error { return command.Process.Kill() }

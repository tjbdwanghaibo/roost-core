package roost

import (
	"context"
	"io"
	"os/exec"
	"time"
)

// commandTreeWaitDelay bounds how long a cancelled command may keep Wait busy:
// a process that escaped the tree kill but still holds the output pipe, or one
// that ignores the kill. After it, exec kills the process and closes the pipes.
const commandTreeWaitDelay = 5 * time.Second

// runCommandTree runs binary to completion in dir and treats it and everything
// it starts as one tree: when ctx ends, the whole tree is killed and the call
// returns within commandTreeWaitDelay.
//
// RR-20261004-13: roost runs the go tool here (doctor's go mod verify / go list
// / go test, project deps' and generate's go get / go mod tidy), and go starts
// compile / link / git processes of its own. exec.CommandContext alone kills
// only go: the orphans kept dir — often a .roost-deps-* / .roost-generate-*
// staging tree — as their working directory (undeletable on Windows, the
// RR-20261004-12 family), and with buffered output Wait waited for them to
// close the inherited pipe, so the timeout did not bound the call at all.
//
// On Unix the tree is a new process group, which also takes it out of the
// terminal's foreground group: Ctrl-C (and a hangup or SIGTERM) reaches only
// roost. The CLI entry (Main) owns those signals and cancels ctx; this
// function only has to kill the tree when ctx ends. It used to catch and
// re-raise the signals itself, which is what B6 removed.
func runCommandTree(ctx context.Context, dir string, env []string, stdout, stderr io.Writer, binary string, args ...string) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = dir
	command.Env = env
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = commandTreeWaitDelay
	startAsTree(command)
	command.Cancel = func() error { return killTree(command) }
	return command.Run()
}

package roost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// commandTreeWaitDelay bounds how long a cancelled command may keep Wait busy:
// a process that escaped the tree kill but still holds the output pipe, or one
// that ignores the kill. After it, exec kills the process and closes the pipes.
const commandTreeWaitDelay = 5 * time.Second

// reraisedSignalGrace is how long runCommandTree waits for a signal it
// re-raised on itself to end the process. The default action normally takes
// effect at once; the wait only runs out when something else in the process
// handles the signal, and then the call returns the interruption as an error.
const reraisedSignalGrace = 10 * time.Second

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
// terminal's foreground group: Ctrl-C (and a hangup or SIGTERM) now reaches
// only roost. So while the command runs roost catches those signals, kills the
// tree, and then re-raises the signal with its default action restored —
// roost still dies of the signal as before, just no longer leaving go behind.
func runCommandTree(ctx context.Context, dir string, env []string, stdout, stderr io.Writer, binary string, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = dir
	command.Env = env
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = commandTreeWaitDelay
	startAsTree(command)
	command.Cancel = func() error { return killTree(command) }

	interrupts := make(chan os.Signal, 1)
	if watched := watchedInterrupts(); len(watched) > 0 {
		signal.Notify(interrupts, watched...)
		defer signal.Stop(interrupts)
	}
	if err := command.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		return err
	case sig := <-interrupts:
		cancel()
		err := <-waited
		signal.Stop(interrupts)
		removeInterruptedStage(dir)
		if self, findErr := os.FindProcess(os.Getpid()); findErr == nil {
			_ = self.Signal(sig)
			// kill(2) on our own pid does not wait for the default action: in
			// a multi-threaded process another thread may take the signal, and
			// on a loaded machine the caller then ran on — rolling back, printing
			// the error, exiting with its own status — before the signal ended
			// the process. Wait for it instead of returning into that path.
			time.Sleep(reraisedSignalGrace)
		}
		// Reached only if something else in the process handles sig.
		return fmt.Errorf("interrupted by %v: %w", sig, err)
	}
}

// watchedInterrupts is the subset of treeInterruptSignals this process would
// otherwise die of. A signal the process was started ignoring (nohup) stays
// ignored: Notify on it would make it fatal to the command.
func watchedInterrupts() []os.Signal {
	var watched []os.Signal
	for _, sig := range treeInterruptSignals {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	return watched
}

// interruptStages are the staging trees (.roost-deps-*, .roost-generate-*)
// whose owners run go commands inside them. A registered tree is removed when
// runCommandTree catches an interrupt for a command running in it.
var interruptStages struct {
	sync.Mutex
	dirs map[string]int
}

// removeOnInterrupt registers stage for removal should roost be interrupted
// while a command runs inside it; release deregisters it. It complements, not
// replaces, the owner's defer os.RemoveAll(stage), which covers every exit
// that does not die of a signal.
func removeOnInterrupt(stage string) (release func()) {
	stage = filepath.Clean(stage)
	interruptStages.Lock()
	if interruptStages.dirs == nil {
		interruptStages.dirs = make(map[string]int)
	}
	interruptStages.dirs[stage]++
	interruptStages.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			interruptStages.Lock()
			defer interruptStages.Unlock()
			if interruptStages.dirs[stage]--; interruptStages.dirs[stage] <= 0 {
				delete(interruptStages.dirs, stage)
			}
		})
	}
}

// removeInterruptedStage deletes the registered staging tree that contains
// dir, best effort: the process is about to die of a signal and has no one to
// report a failure to.
func removeInterruptedStage(dir string) {
	if dir == "" {
		return
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	interruptStages.Lock()
	var owner string
	for stage := range interruptStages.dirs {
		rel, relErr := filepath.Rel(stage, dir)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			owner = stage
			break
		}
	}
	interruptStages.Unlock()
	if owner != "" {
		_ = os.RemoveAll(owner)
	}
}

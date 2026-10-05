package roost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// reraisedSignalGrace is how long the entry waits for a signal it re-raised
// on itself to end the process. The default action normally takes effect at
// once; the wait only runs out when something else in the process handles the
// signal, and then the process exits with the shell's 128+n status instead.
const reraisedSignalGrace = 10 * time.Second

// Main is the roost command-line entry: Run under one owner of the
// interrupting signals (维护者决定 B6).
//
// roost copies the project into a staging tree beside it, runs generators and
// go commands there and commits the result back; the go commands run as their
// own process group (RR-20261004-13), so Ctrl-C, a hangup or SIGTERM reaches
// only roost. Main catches those signals and turns the first one into the
// cancellation of the command's context: the command returns through its
// ordinary error path — the go tree is killed, dependency files and the
// manifest are rolled back, the staging tree is removed by the defer that
// created it, and a commit that already started is finished rather than cut
// in half. Then Main re-raises the signal, so roost still dies of it with the
// same exit status as before. A second signal ends roost at once.
//
// Before B6 the signal was caught inside runCommandTree, only while a go
// command ran, and re-raised from there; everything that went wrong around it
// (RR-20261004-12/13, cb11be90, RR-20261005-NC-70, N08 O6) came from dying in
// the middle of the call stack instead of after it.
func Main(args []string, stdout, stderr io.Writer) error {
	return runInterruptible(stderr, func(ctx context.Context) error {
		return RunContext(ctx, args, stdout, stderr)
	})
}

// runInterruptible runs command with a context that the first interrupting
// signal cancels. If a signal arrived, it reports the command's error on
// stderr and dies of that signal instead of returning; otherwise it returns
// the command's error.
func runInterruptible(stderr io.Writer, command func(context.Context) error) error {
	watched := watchedInterrupts()
	if len(watched) == 0 {
		return command(context.Background())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, watched...)

	var (
		mu     sync.Mutex
		caught os.Signal
	)
	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case sig := <-signals:
			mu.Lock()
			caught = sig
			mu.Unlock()
			cancel()
		case <-finished:
			return
		}
		// Rolling back is bounded (the go tree is already being killed), but
		// a user who presses Ctrl-C again asks to stop waiting for it.
		select {
		case sig := <-signals:
			dieOf(sig)
		case <-finished:
		}
	}()

	err := command(ctx)
	close(finished)
	<-watcherDone
	signal.Stop(signals)
	mu.Lock()
	sig := caught
	mu.Unlock()
	if sig == nil {
		return err
	}
	if err != nil {
		fmt.Fprintf(stderr, "roost: interrupted by %v: %v\n", sig, err)
	}
	dieOf(sig)
	return err // not reached
}

// watchedInterrupts is the subset of interruptSignals this process would
// otherwise die of. A signal the process was started ignoring (nohup) stays
// ignored: catching it would make it fatal to the command.
func watchedInterrupts() []os.Signal {
	var watched []os.Signal
	for _, sig := range interruptSignals {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	return watched
}

// dieOf ends the process by sig with its default action restored. kill(2) on
// our own pid does not wait for that action: another thread may take the
// signal a moment later, so the caller must not go on (rolling back again,
// printing, exiting with its own status) — dieOf never returns.
func dieOf(sig os.Signal) {
	signal.Reset(sig)
	if self, err := os.FindProcess(os.Getpid()); err == nil {
		_ = self.Signal(sig)
	}
	time.Sleep(reraisedSignalGrace)
	// Reached only if something else in the process handles sig.
	status := 1
	if number, ok := sig.(syscall.Signal); ok {
		status = 128 + int(number)
	}
	os.Exit(status)
}

// stagePhaseHook, when set by a test, runs at fixed points of a staged
// command — "copy" (after the first file is copied into the staging tree),
// "generator" (as each generator starts) and "commit" (after the last
// cancellation check, before the commit) — so a test can interrupt exactly
// there. It is nil in production.
var stagePhaseHook func(ctx context.Context, phase string)

func reachStagePhase(ctx context.Context, phase string) {
	if stagePhaseHook != nil {
		stagePhaseHook(ctx, phase)
	}
}

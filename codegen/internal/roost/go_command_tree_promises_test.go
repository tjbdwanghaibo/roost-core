//go:build unix

package roost

// RR-20261004-13：roost 替用户跑的 go 命令（doctor 的 go mod verify / go list / go test，
// project deps 与 generate 的 go get / go mod tidy）在超时或取消后，必须在有界时间内返回，
// 并且不留下任何还活着的子孙进程。go 自己会再起 compile / link / git 等子进程；旧实现用
// exec.CommandContext 却没设 WaitDelay、也不按进程组杀：ctx 到期只 kill go 本身，
// (1) 孙进程继续以命令的 Dir 为工作目录（project deps / generate 时是 .roost-deps-* /
// .roost-generate-* 暂存树，Windows 上因此删不掉，同族 RR-20261004-12）；
// (2) 输出写到 bytes.Buffer（doctor 的 CombinedOutput、generate 的暂存输出）时，Wait 要等
// 孙进程关掉继承的管道，超时形同虚设。
//
// 这里用一个 sh 替身代替 go：起一个长寿孙进程（继承 stdout / stderr 与工作目录）后 wait，
// 和被 kill 的 go 留下 compile 子进程是同一形态。
//
// 同步全部是确定性的：替身把孙进程 pid 原子地写进文件，测试等到这份报告之后才取消或让
// 期限到达；“等报告”“等孙进程消失”的时间上限只作兜底，放得很宽，满载时也不会误报。
// 唯一按时间断言的是承诺本身：上下文结束后命令要在 treeCommandReturnBound 内返回。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// treeCommandReturnBound is how long a cancelled command may take to return.
// It is the promise under test, twice the production WaitDelay.
const treeCommandReturnBound = 10 * time.Second

// treeStartBound and grandchildExitBound are backstops, not expectations: the
// tests wait for the fake's report and for the grandchild's death, and these
// only keep a broken run from hanging. A loaded machine (go test -race ./...
// beside other builds) can take seconds to exec a fresh script, so they are
// generous. The grandchild lives far longer than both, so a leaked one is
// still alive when it is checked.
const (
	treeStartBound      = 2 * time.Minute
	grandchildExitBound = 30 * time.Second
	grandchildLifetime  = "600"
)

type fakeGoTree struct {
	binary  string
	pidFile string
	dir     string
}

// newFakeGoTree writes a stand-in for the go tool that starts a long-lived
// grandchild sharing its stdout / stderr and working directory, records the
// grandchild's pid, and waits for it.
func newFakeGoTree(t *testing.T) fakeGoTree {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, ".roost-deps-fake")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(base, "grandchild.pid")
	binary := filepath.Join(base, "fake-go")
	script := "#!/bin/sh\n" +
		"echo fake go started\n" +
		"sleep " + grandchildLifetime + " &\n" +
		"echo $! > '" + pidFile + ".tmp' && mv '" + pidFile + ".tmp' '" + pidFile + "'\n" +
		"wait\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := fakeGoTree{binary: binary, pidFile: pidFile, dir: dir}
	t.Cleanup(func() {
		// Never leave the sleeper behind, whatever the outcome.
		if pid, ok := fake.grandchild(); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return fake
}

func (f fakeGoTree) grandchild() (int, bool) {
	body, err := os.ReadFile(f.pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	return pid, err == nil && pid > 0
}

// waitStarted waits for the fake's report that its grandchild is running. It
// gives up early if ended closes first (whatever runs the fake went away
// before the report, so it never will come), and otherwise only after the
// treeStartBound backstop.
func (f fakeGoTree) waitStarted(ended <-chan struct{}) (int, error) {
	backstop := time.NewTimer(treeStartBound)
	defer backstop.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if pid, ok := f.grandchild(); ok {
			return pid, nil
		}
		select {
		case <-tick.C:
		case <-ended:
			if pid, ok := f.grandchild(); ok {
				return pid, nil
			}
			return 0, errors.New("the command ended before fake go reported its grandchild")
		case <-backstop.C:
			return 0, fmt.Errorf("fake go did not report its grandchild within the %s backstop", treeStartBound)
		}
	}
}

// processGone reports whether pid no longer runs. A killed grandchild is
// reparented to init / launchd and stays a zombie until reaped there, which
// can lag under load; a zombie runs nothing, so it counts as gone.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(out))
	if err != nil {
		var exit *exec.ExitError
		return errors.As(err, &exit) && state == ""
	}
	return strings.HasPrefix(state, "Z")
}

// deadlineAtWill is a context whose deadline passes when the test says so, so
// a timeout path can be driven after the fake reported its grandchild instead
// of racing a timer against a slow exec.
type deadlineAtWill struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newDeadlineAtWill() *deadlineAtWill {
	return &deadlineAtWill{Context: context.Background(), done: make(chan struct{})}
}

func (c *deadlineAtWill) Done() <-chan struct{} { return c.done }

func (c *deadlineAtWill) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *deadlineAtWill) pass() { c.once.Do(func() { close(c.done) }) }

// awaitReturn waits for a command whose context ended at ctxEnd to return. If
// it does not return in time, the grandchild is killed so the call can unwind
// before the test ends, and the failure says so.
func (f fakeGoTree) awaitReturn(t *testing.T, what string, ctxEnd time.Time, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		return
	case <-time.After(time.Until(ctxEnd.Add(treeCommandReturnBound))):
	}
	pid, _ := f.grandchild()
	t.Errorf("%s was still blocked %s after its context ended: Wait is held by grandchild %d's copy of the output pipe", what, treeCommandReturnBound, pid)
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	<-done
}

// assertGrandchildGone fails if the grandchild still runs after the command
// returned. The tree kill was sent before Wait returned, so it is already
// dying; grandchildExitBound only absorbs scheduling delay under load.
func (f fakeGoTree) assertGrandchildGone(t *testing.T, what string) {
	t.Helper()
	pid, ok := f.grandchild()
	if !ok {
		t.Fatalf("%s: fake go never recorded its grandchild", what)
	}
	deadline := time.Now().Add(grandchildExitBound)
	for {
		if processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cwd := "unknown"
	if out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "n") {
				cwd = strings.TrimPrefix(line, "n")
			}
		}
	}
	t.Errorf("%s returned but grandchild %d is still alive %s later (working directory %s; command Dir was %s)", what, pid, grandchildExitBound, cwd, f.dir)
}

// The doctor's deadline passes only once the fake reported its grandchild: a
// real 3s timer raced the exec of the fake, and on a loaded machine it often
// fired before the grandchild existed, so the test proved nothing and failed
// with "fake go never recorded its grandchild".
func TestDoctorGoCommandTimeoutReturnsAndLeavesNoGrandchild(t *testing.T) {
	fake := newFakeGoTree(t)
	ctx := newDeadlineAtWill()
	t.Cleanup(ctx.pass)
	done := make(chan struct{})
	var item CheckItem
	go func() {
		defer close(done)
		item = runDoctorCommandUntil(ctx, fake.binary, fake.dir, "compile:go-test", 3*time.Second, "ok", "retry", "test", "./...")
	}()
	if _, err := fake.waitStarted(done); err != nil {
		t.Fatalf("runDoctorGoCommand: %v", err)
	}
	expired := time.Now()
	ctx.pass()
	fake.awaitReturn(t, "runDoctorGoCommand", expired, done)
	t.Logf("returned %s after its deadline: %s %s", time.Since(expired).Round(time.Millisecond), item.Status, item.Detail)
	if item.Status != StatusFail || !strings.Contains(item.Detail, "timed out after 3s") {
		t.Errorf("doctor item = %s %q, want a fail that says it timed out after 3s", item.Status, item.Detail)
	}
	fake.assertGrandchildGone(t, "runDoctorGoCommand")
}

// runDoctorCommand itself turns its timeout into the deadline: a real timer,
// so the fake may or may not have reached its grandchild when it fires. Either
// way the call returns in time, says it timed out, and leaves nothing running.
func TestDoctorGoCommandTimeoutIsItsOwnDeadline(t *testing.T) {
	fake := newFakeGoTree(t)
	done := make(chan struct{})
	var item CheckItem
	started := time.Now()
	go func() {
		defer close(done)
		item = runDoctorCommand(fake.binary, fake.dir, "compile:go-test", time.Second, "ok", "retry", "test", "./...")
	}()
	fake.awaitReturn(t, "runDoctorGoCommand", started.Add(time.Second), done)
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Errorf("returned after %s, before its 1s timeout", elapsed)
	}
	if item.Status != StatusFail || !strings.Contains(item.Detail, "timed out after 1s") {
		t.Errorf("doctor item = %s %q, want a fail that says it timed out after 1s", item.Status, item.Detail)
	}
	if _, ok := fake.grandchild(); ok {
		fake.assertGrandchildGone(t, "runDoctorGoCommand")
	}
}

// The generate path (TidyProjectDependencies inside GenerateTransactional)
// hands runDependencyCommand bytes.Buffers, so Wait copies the pipes.
func TestDependencyCommandCancelWithBufferedOutputReturnsAndLeavesNoGrandchild(t *testing.T) {
	fake := newFakeGoTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = runDependencyBinary(ctx, fake.binary, fake.dir, &stdout, &stderr, "mod", "tidy")
	}()
	if _, startErr := fake.waitStarted(done); startErr != nil {
		t.Fatalf("runDependencyCommand (buffered output): %v", startErr)
	}
	cancel()
	fake.awaitReturn(t, "runDependencyCommand (buffered output)", time.Now(), done)
	if err == nil {
		t.Errorf("cancelled dependency command returned nil")
	}
	fake.assertGrandchildGone(t, "runDependencyCommand (buffered output)")

	// The same directory is usable again with a fresh context, and removable.
	quick := filepath.Join(filepath.Dir(fake.binary), "fake-go-quick")
	if err := os.WriteFile(quick, []byte("#!/bin/sh\necho tidy ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runDependencyBinary(context.Background(), quick, fake.dir, &stdout, &stderr, "mod", "tidy"); err != nil {
		t.Fatalf("retry with a fresh context: %v", err)
	}
	if !strings.Contains(stdout.String(), "tidy ok") {
		t.Fatalf("retry output = %q", stdout.String())
	}
	if err := os.RemoveAll(fake.dir); err != nil {
		t.Fatalf("remove the staging directory after the retry: %v", err)
	}
}

// The CLI path (`roost project deps`) hands it the terminal's *os.File, so
// Wait has no pipe to copy and returns at once; the grandchild must still die.
func TestDependencyCommandCancelWithFileOutputLeavesNoGrandchild(t *testing.T) {
	fake := newFakeGoTree(t)
	out, err := os.Create(filepath.Join(t.TempDir(), "out.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDependencyBinary(ctx, fake.binary, fake.dir, out, out, "get", "example.com/x@latest")
	}()
	if _, startErr := fake.waitStarted(done); startErr != nil {
		t.Fatalf("runDependencyCommand (file output): %v", startErr)
	}
	cancel()
	fake.awaitReturn(t, "runDependencyCommand (file output)", time.Now(), done)
	if runErr == nil {
		t.Errorf("cancelled dependency command returned nil")
	}
	fake.assertGrandchildGone(t, "runDependencyCommand (file output)")
}

const interruptChildEnv = "ROOST_TEST_COMMAND_TREE_INTERRUPT"

// The go command runs in its own process group so it can be killed as a tree;
// that also takes it out of the terminal's foreground group, so Ctrl-C reaches
// only roost. Before the tree, one Ctrl-C killed roost and go together; it must
// still kill roost (by the signal, as before) and now take go's tree with it,
// or an interrupted `roost project deps` would leave `go get` running alone.
// The signal is fatal by design, so roost is played by a re-exec of this test.
func TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost(t *testing.T) {
	if binary := os.Getenv(interruptChildEnv); binary != "" {
		dir := os.Getenv(interruptChildEnv + "_DIR")
		err := runDependencyBinary(context.Background(), binary, dir, io.Discard, io.Discard, "mod", "tidy")
		// Only reached if the re-raised signal did not end the process.
		fmt.Fprintf(os.Stderr, "runDependencyBinary returned instead of dying of the signal: %v\n", err)
		os.Exit(3)
	}
	if signal.Ignored(os.Interrupt) {
		t.Skip("SIGINT is ignored in this process, so the child would inherit that")
	}
	fake := newFakeGoTree(t)
	var childStderr bytes.Buffer
	child := exec.Command(os.Args[0], "-test.run=^TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost$", "-test.count=1")
	child.Env = append(os.Environ(), interruptChildEnv+"="+fake.binary, interruptChildEnv+"_DIR="+fake.dir)
	child.Stderr = &childStderr
	// The child leads its own group, standing in for the terminal's foreground
	// process group that Ctrl-C signals as a whole.
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	childEnded := make(chan struct{})
	go func() {
		exited <- child.Wait()
		close(childEnded)
	}()
	// The child is a fresh exec of this (possibly -race) test binary, which a
	// loaded machine can take many seconds to bring up; wait for the report.
	if _, err := fake.waitStarted(childEnded); err != nil {
		select {
		case <-childEnded:
			t.Fatalf("roost (test child): %v; it ended with %v, stderr: %s", err, child.ProcessState, childStderr.String())
		default:
			_ = child.Process.Kill()
			<-childEnded
			t.Fatalf("roost (test child): %v", err)
		}
	}
	if err := syscall.Kill(-child.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(treeCommandReturnBound):
		_ = child.Process.Kill()
		<-exited
		t.Fatalf("roost (test child) still running %s after Ctrl-C", treeCommandReturnBound)
	}
	status, _ := child.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Errorf("roost (test child) ended with %v, want death by SIGINT as before; stderr: %s", child.ProcessState, childStderr.String())
	}
	fake.assertGrandchildGone(t, "interrupted roost")
}

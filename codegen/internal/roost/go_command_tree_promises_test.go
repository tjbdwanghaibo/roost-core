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
	"syscall"
	"testing"
	"time"
)

// treeCommandReturnBound is how long a cancelled command may take to return.
// It is well above the production WaitDelay; the grandchild lives 30s.
const treeCommandReturnBound = 10 * time.Second

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
		"sleep 30 &\n" +
		"echo $! > '" + pidFile + ".tmp' && mv '" + pidFile + ".tmp' '" + pidFile + "'\n" +
		"wait\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := fakeGoTree{binary: binary, pidFile: pidFile, dir: dir}
	t.Cleanup(func() {
		// Never leave the 30s sleeper behind, whatever the outcome.
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

func (f fakeGoTree) waitStarted(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := f.grandchild(); ok {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake go did not start its grandchild within 5s")
	return 0
}

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

// assertGrandchildGone fails if the grandchild is still alive shortly after the
// command returned (it is reparented and reaped, so allow a moment).
func (f fakeGoTree) assertGrandchildGone(t *testing.T, what string) {
	t.Helper()
	pid, ok := f.grandchild()
	if !ok {
		t.Fatalf("%s: fake go never recorded its grandchild", what)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
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
	t.Errorf("%s returned but grandchild %d is still alive 2s later (working directory %s; command Dir was %s)", what, pid, cwd, f.dir)
}

func TestDoctorGoCommandTimeoutReturnsAndLeavesNoGrandchild(t *testing.T) {
	fake := newFakeGoTree(t)
	done := make(chan struct{})
	var item CheckItem
	started := time.Now()
	go func() {
		defer close(done)
		// The timeout leaves the fake ample time to start its grandchild (the
		// first exec of a fresh script can be slow on macOS); the call itself
		// cannot be cancelled any other way.
		item = runDoctorCommand(fake.binary, fake.dir, "compile:go-test", 3*time.Second, "ok", "retry", "test", "./...")
	}()
	fake.awaitReturn(t, "runDoctorGoCommand", started.Add(3*time.Second), done)
	t.Logf("returned after %s: %s %s", time.Since(started).Round(time.Millisecond), item.Status, item.Detail)
	if item.Status != StatusFail || !strings.Contains(item.Detail, "timed out after 3s") {
		t.Errorf("doctor item = %s %q, want a fail that says it timed out after 3s", item.Status, item.Detail)
	}
	fake.assertGrandchildGone(t, "runDoctorGoCommand")
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
	fake.waitStarted(t)
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
	fake.waitStarted(t)
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
	go func() { exited <- child.Wait() }()
	fake.waitStarted(t)
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

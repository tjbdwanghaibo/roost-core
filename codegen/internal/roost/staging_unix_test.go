//go:build unix

package roost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// 暂存类命令（`roost project deps / sync / upgrade / new` 与 `roost generate`）先把工程复制进工程旁边的暂存树
// （.roost-deps-* / .roost-generate-*），在暂存树里跑 go get / go mod tidy，成功后才提交回工程。Ctrl-C（SIGINT /
// SIGTERM / SIGHUP）先杀 go 的进程树、再把信号重发给自己，roost 照旧死于该信号——死于信号的进程不跑 defer，
// 所以被中断命令自己的暂存树必须在重发信号之前删除，工程本身不被改动，不留 go 子树。
//
// roost 由重新执行的本测试二进制扮演（经 CLI 入口 Main 跑正式命令行，维护者决定 B6），PATH 上的替身 go 起长寿孙进程后
// 等待，测试等替身报告后才发信号，没有按时间赛跑的步骤。其余阶段（复制、生成器、提交）见本文件后面的阶段用例。

const interruptStageChildEnv = "ROOST_TEST_INTERRUPT_STAGE"

func TestInterruptedCommandRemovesItsStagingTree(t *testing.T) {
	if command := os.Getenv(interruptStageChildEnv); command != "" {
		root := os.Getenv(interruptStageChildEnv + "_ROOT")
		var err error
		switch command {
		case "deps":
			err = Main([]string{"project", "deps", "--root", root}, io.Discard, os.Stderr)
		case "generate":
			err = Main([]string{"generate", "--root", root}, io.Discard, os.Stderr)
		default:
			err = fmt.Errorf("unknown child command %q", command)
		}
		// Only reached if the re-raised signal did not end the process.
		fmt.Fprintf(os.Stderr, "%s returned instead of dying of the signal: %v\n", command, err)
		os.Exit(3)
	}
	if signal.Ignored(os.Interrupt) {
		t.Skip("SIGINT is ignored in this process, so the child would inherit that")
	}
	for _, tc := range []struct {
		command string
		stage   string
	}{
		{command: "deps", stage: ".roost-deps-"},
		{command: "generate", stage: ".roost-generate-"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			root := copyOfNewProject(t, "configdata")
			parent := filepath.Dir(root)
			goModBefore, err := os.ReadFile(filepath.Join(root, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			fake := newPathFakeGo(t)

			var childStderr bytes.Buffer
			child := exec.Command(os.Args[0], "-test.run=^TestInterruptedCommandRemovesItsStagingTree$", "-test.count=1")
			child.Env = append(os.Environ(),
				interruptStageChildEnv+"="+tc.command,
				interruptStageChildEnv+"_ROOT="+root,
				"PATH="+fake.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			child.Stderr = &childStderr
			// The child leads its own group, standing in for the terminal's
			// foreground process group that Ctrl-C signals as a whole.
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
			// The go command really runs inside a staging tree beside the
			// project: that tree is what must not survive the interrupt.
			cwd, err := os.ReadFile(fake.cwdFile)
			if err != nil {
				t.Fatal(err)
			}
			if stage := filepath.Base(strings.TrimSpace(string(cwd))); !strings.HasPrefix(stage, tc.stage) {
				t.Fatalf("fake go ran in %q, want a %s* staging tree beside the project", strings.TrimSpace(string(cwd)), tc.stage)
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
			fake.assertGrandchildGone(t, "interrupted roost "+tc.command)

			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".roost-") {
					t.Errorf("interrupted roost %s left its staging tree %s beside the project", tc.command, entry.Name())
				}
			}
			goModAfter, err := os.ReadFile(filepath.Join(root, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(goModBefore, goModAfter) {
				t.Errorf("interrupted roost %s changed the project's go.mod", tc.command)
			}

			// Rerunning after the interrupt completes and leaves nothing behind
			// (a go that succeeds at once stands in for the network coming back).
			okBin := filepath.Join(t.TempDir(), "bin")
			if err := os.Mkdir(okBin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(okBin, "go"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", okBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			switch tc.command {
			case "deps":
				manifest, loadErr := LoadManifest(root)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				err = UpdateFrameworkDependencies(context.Background(), root, manifest, io.Discard, io.Discard)
			case "generate":
				err = GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard}, io.Discard)
			}
			if err != nil {
				t.Fatalf("rerun %s after the interrupt: %v", tc.command, err)
			}
			if entries, err = os.ReadDir(parent); err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".roost-") {
					t.Errorf("rerun %s left %s beside the project", tc.command, entry.Name())
				}
			}
		})
	}
}

// pathFakeGo is fakeGoTree installed as `go` on PATH, so the formal entry
// points (which always run "go") reach it; it also records its working
// directory.
type pathFakeGo struct {
	fakeGoTree
	bin     string
	cwdFile string
}

func newPathFakeGo(t *testing.T) pathFakeGo {
	t.Helper()
	base := t.TempDir()
	bin := filepath.Join(base, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(base, "grandchild.pid")
	cwdFile := filepath.Join(base, "cwd")
	script := "#!/bin/sh\n" +
		"pwd > '" + cwdFile + "'\n" +
		"sleep " + grandchildLifetime + " &\n" +
		"echo $! > '" + pidFile + ".tmp' && mv '" + pidFile + ".tmp' '" + pidFile + "'\n" +
		"wait\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := pathFakeGo{fakeGoTree: fakeGoTree{binary: filepath.Join(bin, "go"), pidFile: pidFile, dir: "(a staging tree)"}, bin: bin, cwdFile: cwdFile}
	t.Cleanup(func() {
		body, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(body))); err == nil && pid > 0 {
			if killErr := syscall.Kill(pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
				t.Logf("kill grandchild %d: %v", pid, killErr)
			}
		}
	})
	return fake
}

// 暂存类命令在复制工程、跑进程内生成器、提交这三个阶段收到中断（维护者决定 B6，由 CLI 入口 Main 统一接管信号）：
// 命令按 ctx 走正常的错误 / 回滚路径，暂存树被删除；提交一旦开始就完整做完（不留半份提交），没开始的不提交；
// 最后进程照旧死于该信号。roost 由重新执行的本测试二进制扮演，测试钩子 stagePhaseHook 在指定阶段报告后等待，
// 测试收到报告才发信号，没有按时间赛跑的步骤。

const interruptPhaseChildEnv = "ROOST_TEST_INTERRUPT_PHASE"

// registryOutput is a generator output of the configdata fixture: the commit
// case deletes it so that generate has something to commit.
const registryOutput = "internal/registry/generated.go"

func TestInterruptInEveryStagePhaseRemovesTheStageAndDiesOfTheSignal(t *testing.T) {
	if phase := os.Getenv(interruptPhaseChildEnv); phase != "" {
		marker := os.Getenv(interruptPhaseChildEnv + "_MARKER")
		var once sync.Once
		stagePhaseHook = func(ctx context.Context, current string) {
			if current != phase {
				return
			}
			once.Do(func() {
				_ = os.WriteFile(marker+".tmp", []byte(current), 0o644)
				_ = os.Rename(marker+".tmp", marker)
				// The interrupt arrives while this phase is under way.
				select {
				case <-ctx.Done():
				case <-time.After(treeStartBound):
				}
			})
		}
		args := strings.Fields(os.Getenv(interruptPhaseChildEnv + "_ARGS"))
		err := Main(args, io.Discard, os.Stderr)
		// Only reached if the process did not die of the signal.
		fmt.Fprintf(os.Stderr, "roost %s returned instead of dying of the signal: %v\n", strings.Join(args, " "), err)
		os.Exit(3)
	}
	if signal.Ignored(os.Interrupt) {
		t.Skip("SIGINT is ignored in this process, so the child would inherit that")
	}
	for _, tc := range []struct {
		phase   string
		command string
		stage   string
	}{
		{phase: "copy", command: "project deps", stage: ".roost-deps-"},
		{phase: "generator", command: "generate", stage: ".roost-generate-"},
		{phase: "commit", command: "generate", stage: ".roost-generate-"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			root := copyOfNewProject(t, "configdata")
			parent := filepath.Dir(root)
			if tc.phase == "commit" {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(registryOutput))); err != nil {
					t.Fatal(err)
				}
			}
			before := projectDigest(t, root)
			// go mod tidy / go get succeed at once: the phase under test is
			// not a go command.
			okBin := filepath.Join(t.TempDir(), "bin")
			if err := os.Mkdir(okBin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(okBin, "go"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "phase-reached")

			var childStderr bytes.Buffer
			child := exec.Command(os.Args[0], "-test.run=^TestInterruptInEveryStagePhaseRemovesTheStageAndDiesOfTheSignal$", "-test.count=1")
			child.Env = append(os.Environ(),
				interruptPhaseChildEnv+"="+tc.phase,
				interruptPhaseChildEnv+"_MARKER="+marker,
				interruptPhaseChildEnv+"_ARGS="+tc.command+" --root "+root,
				"PATH="+okBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			child.Stderr = &childStderr
			// The child leads its own group, standing in for the terminal's
			// foreground process group that Ctrl-C signals as a whole.
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() {
				_ = child.Wait()
				close(exited)
			}()
			if err := waitForFile(marker, exited); err != nil {
				select {
				case <-exited:
					t.Fatalf("roost (test child) %s: %v; it ended with %v, stderr: %s", tc.command, err, child.ProcessState, childStderr.String())
				default:
					_ = child.Process.Kill()
					<-exited
					t.Fatalf("roost (test child) %s: %v", tc.command, err)
				}
			}
			// The phase really runs with a staging tree beside the project.
			if !hasStage(t, parent, tc.stage) {
				t.Fatalf("no %s* staging tree beside the project while in phase %s", tc.stage, tc.phase)
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
				t.Errorf("roost %s interrupted in phase %s ended with %v, want death by SIGINT; stderr: %s", tc.command, tc.phase, child.ProcessState, childStderr.String())
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".roost-") {
					t.Errorf("roost %s interrupted in phase %s left its staging tree %s beside the project", tc.command, tc.phase, entry.Name())
				}
			}
			after := projectDigest(t, root)
			if tc.phase != "commit" {
				if changed := diffDigests(before, after); len(changed) > 0 {
					t.Errorf("roost %s interrupted before its commit changed the project: %v", tc.command, changed)
				}
				return
			}
			// A commit that started runs to the end: the deleted output is
			// back and nothing generated is stale.
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(registryOutput))); err != nil {
				t.Errorf("roost generate interrupted during its commit did not finish it: %v", err)
			}
			if err := Generate(root, GenerateOptions{Check: true, Stdout: io.Discard}); err != nil {
				t.Errorf("roost generate interrupted during its commit left a partial commit: %v", err)
			}
		})
	}
}

// waitForFile waits for path to appear, giving up when ended closes first or
// after the treeStartBound backstop.
func waitForFile(path string, ended <-chan struct{}) error {
	backstop := time.NewTimer(treeStartBound)
	defer backstop.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-tick.C:
		case <-ended:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
			return fmt.Errorf("the command ended before reaching the phase")
		case <-backstop.C:
			return fmt.Errorf("the command did not reach the phase within the %s backstop", treeStartBound)
		}
	}
}

func hasStage(t *testing.T, parent, prefix string) bool {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return true
		}
	}
	return false
}

func projectDigest(t *testing.T, root string) map[string][sha256.Size]byte {
	t.Helper()
	out := make(map[string][sha256.Size]byte)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = sha256.Sum256(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffDigests(before, after map[string][sha256.Size]byte) []string {
	var changed []string
	for rel, sum := range before {
		if other, ok := after[rel]; !ok || other != sum {
			changed = append(changed, rel)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			changed = append(changed, rel)
		}
	}
	return changed
}

// N08 O2 / O3（随维护者决定 B6 一并处理）：
//
// O2：`roost project upgrade` 先把 roost.yaml 与模板升级、再解析依赖；依赖解析失败时工程处于
// “manifest 已升级、go.mod 未变”的半升级态，收敛它的命令是 `roost project deps`，旧错误文字
// 只说 “framework resolution failed”，不像 `project new` 那样指路（`project sync` 同形）。
//
// O3：generate / sync 在工程旁的暂存树里跑生成器，生成器报错时带的是暂存树里的路径
// （…/.roost-generate-*/…、…/.roost-sync-*/…），命令返回时那棵树已经删掉，用户照着路径找不到文件。
// 承诺：错误里的文件路径指向工程本身；errors.Is 链不受影响。

func TestStagedGeneratorErrorsNameTheProjectPath(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	broken := filepath.Join(root, "internal", "broken", "broken.go")
	if err := os.MkdirAll(filepath.Dir(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("package broken\n\nfunc broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"generate": func() error {
			return GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard}, io.Discard)
		},
		"sync": func() error {
			_, err := SyncProject(root)
			return err
		},
	} {
		err := run()
		if err == nil {
			t.Fatalf("%s with a file that does not parse succeeded", name)
		}
		if strings.Contains(err.Error(), ".roost-") || !strings.Contains(err.Error(), broken) {
			t.Errorf("%s error names a path that is gone when it is read, want %s:\n%v", name, broken, err)
		}
	}
	// The rewrite is only in the message.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := GenerateTransactional(ctx, root, GenerateOptions{Stdout: io.Discard}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled generate returned %v, want an error wrapping context.Canceled", err)
	}
}

func TestDependencyFailureAfterUpgradeOrSyncPointsAtProjectDeps(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\necho 'proxy unreachable' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, command := range []string{"upgrade", "sync"} {
		err := Run([]string{"project", command, "--root", root}, io.Discard, io.Discard)
		if err == nil {
			t.Fatalf("project %s with a failing go succeeded", command)
		}
		if want := "roost project deps --root " + root; !strings.Contains(err.Error(), want) {
			t.Errorf("project %s dependency failure does not say how to converge (%q):\n%v", command, want, err)
		}
	}
}

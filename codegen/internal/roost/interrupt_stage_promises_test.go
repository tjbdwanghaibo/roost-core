//go:build unix

package roost

// RR-20261005-NC-70：`roost project deps / sync / upgrade / new` 与 `roost generate` 先把工程
// 复制进工程旁边的暂存树（.roost-deps-* / .roost-generate-*），在暂存树里跑 go get / go mod tidy，
// 成功后才提交回工程；暂存树由 `defer os.RemoveAll(stage)` 收尾。RR-20261004-13 让 Ctrl-C
// （SIGINT / SIGTERM / SIGHUP）先杀 go 的进程树、再把信号重发给自己，roost 照旧死于该信号——
// 但死于信号的进程不跑 defer：旧实现每次在 go 命令窗口里被中断，都把一整份工程源码副本
// （含 configs、deploy 等应用自有文件）留在工程的父目录里，下次运行不会清理，文档也没提；
// 父目录本身是 Git 工作区（monorepo 子目录工程）时它还是一棵未跟踪的重复工程树。
//
// 承诺：中断照旧以该信号结束 roost、照旧不留 go 子树；被中断命令自己的暂存树在重发信号之前删除，
// 工程本身不被改动。roost 由重新执行的本测试二进制扮演，PATH 上的替身 go 起长寿孙进程后等待，
// 测试等替身报告后才发信号，没有按时间赛跑的步骤。
//
// B6（维护者决定，2026-10-06）之后信号由 CLI 入口 Main 统一接管：子进程经 Main 跑正式命令行，
// 断言不变。其余阶段（复制、生成器、提交）见 interrupt_phases_promises_test.go。

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

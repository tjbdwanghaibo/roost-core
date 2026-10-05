//go:build unix

package roost

// N08 O6（RR-20261005-NC-70 的残余窗口）与维护者决定 B6：暂存类命令（project deps / sync /
// upgrade / new、generate）把工程复制进工程旁边的暂存树，在里面跑生成器和 go 命令，最后提交回
// 工程。NC-70 只在“go 命令运行期间”接住信号并删暂存树；复制工程、跑进程内生成器、提交这三个阶段
// 收到 Ctrl-C 时进程直接死于信号，不跑 defer，暂存树（一整份工程副本）留在工程的父目录里；
// 提交阶段被打断还会留下一半提交。
//
// 承诺（B6 之后由 CLI 入口 Main 统一接管信号）：任何阶段收到中断，命令都按 ctx 走正常的错误 /
// 回滚路径，暂存树被删除；提交一旦开始就完整做完（不留半份提交），没开始的不提交；最后进程照旧
// 死于该信号。roost 由重新执行的本测试二进制扮演，测试钩子 stagePhaseHook 在指定阶段报告后
// 等待，测试收到报告才发信号，没有按时间赛跑的步骤。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

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

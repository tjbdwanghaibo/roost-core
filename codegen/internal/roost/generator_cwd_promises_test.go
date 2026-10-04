package roost

// RR-20261004-12：生成器运行期间不得把整个进程的工作目录移进被生成的工程（常是
// .roost-sync-* / .roost-generate-* 暂存树）。旧 runGenerators 用 os.Chdir 进入 root
// 再跑生成器，工作目录是进程级的：这段窗口里任何 goroutine 不设 exec.Cmd.Dir 启动的
// 子进程都继承暂存树为工作目录。Windows 上进程的工作目录不能删除，于是 SyncProject 的
// defer os.RemoveAll(stage) 失败（错误被忽略），暂存目录残留到调用方——windows-compatibility
// 上 demo 模板测试两次以 "TempDir RemoveAll cleanup: unlinkat …\.roost-sync-…: The process
// cannot access the file because it is being used by another process." 失败
// （W-2026-10-04-05）。macOS / Linux 允许删除别的进程的工作目录，所以只在 Windows 上可见；
// 本用例直接断言承诺本身（窗口内的子进程不在 root 里），任何平台都会红。

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGeneratorsDoNotMoveTheProcessIntoTheTreeTheyGenerate(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/planet\n\ngo "+generatedGoVersion+".0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var childGoMod, generatorWd, generatorRoot string
	probe := generator{Name: "probe", Always: true, Run: func(at string, _ io.Writer) error {
		generatorRoot = at
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		generatorWd = wd
		// A child started without Dir, as another goroutine of a library
		// caller (or a parallel test) would start one during this window.
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			return err
		}
		childGoMod = strings.TrimSpace(string(out))
		return nil
	}}
	if err := runGenerators(root, DefaultManifest("planet", "example.com/planet", nil, nil, nil), []generator{probe}, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if !sameDir(t, generatorRoot, root) {
		t.Errorf("the generator was handed %q, want the project root %q", generatorRoot, root)
	}
	if sameDir(t, generatorWd, root) {
		t.Errorf("the process working directory was the generated tree while generators ran: %s", generatorWd)
	}
	if childGoMod != "" && within(t, childGoMod, root) {
		t.Errorf("a child started without Dir while generators ran inherited the generated tree as its working directory (go env GOMOD = %s)", childGoMod)
	}
}

// The end-to-end shape of the same promise: while SyncProject runs, children
// that another goroutine starts without Dir never run inside the .roost-sync-*
// staging tree. Before the fix a few dozen of ~5000 such children over five
// syncs landed in a stage on macOS (probe in the RR record).
func TestSyncProjectStagesNeverBecomeAChildsWorkingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root}); err != nil {
		t.Fatal(err)
	}
	stagesParent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var mu sync.Mutex
	var inStage []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			out, err := exec.Command("go", "env", "GOMOD").Output()
			if err != nil {
				continue
			}
			goMod, evalErr := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
			if evalErr != nil {
				continue
			}
			if strings.HasPrefix(goMod, filepath.Join(stagesParent, ".roost-")) {
				mu.Lock()
				inStage = append(inStage, goMod)
				mu.Unlock()
			}
		}
	}()
	for i := 0; i < 3; i++ {
		if _, err := SyncProject(root); err != nil {
			stop.Store(true)
			<-done
			t.Fatal(err)
		}
	}
	stop.Store(true)
	<-done
	if len(inStage) > 0 {
		t.Errorf("%d children started without Dir ran inside a staging tree, e.g. %s", len(inStage), inStage[0])
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	if a == "" || b == "" {
		return false
	}
	ea, errA := filepath.EvalSymlinks(a)
	eb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ea == eb
}

func within(t *testing.T, path, dir string) bool {
	t.Helper()
	ep, errP := filepath.EvalSymlinks(path)
	ed, errD := filepath.EvalSymlinks(dir)
	if errP != nil || errD != nil {
		return false
	}
	rel, err := filepath.Rel(ed, ep)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

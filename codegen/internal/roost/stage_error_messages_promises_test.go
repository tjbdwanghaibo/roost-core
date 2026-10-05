//go:build unix

package roost

// N08 O2 / O3（随维护者决定 B6 一并处理）：
//
// O2：`roost project upgrade` 先把 roost.yaml 与模板升级、再解析依赖；依赖解析失败时工程处于
// “manifest 已升级、go.mod 未变”的半升级态，收敛它的命令是 `roost project deps`，旧错误文字
// 只说 “framework resolution failed”，不像 `project new` 那样指路（`project sync` 同形）。
//
// O3：generate / sync 在工程旁的暂存树里跑生成器，生成器报错时带的是暂存树里的路径
// （…/.roost-generate-*/…、…/.roost-sync-*/…），命令返回时那棵树已经删掉，用户照着路径找不到文件。
// 承诺：错误里的文件路径指向工程本身；errors.Is 链不受影响。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

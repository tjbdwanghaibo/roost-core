package roost

// RR-20261005-NC-206（N08 观察 O7 转确认）：生成器自己把工程运行期写进工程目录的东西
// 当成“不是工程”——skippedProjectDirectory 跳过 `.dev`（dev-run 日志）与 `data/wal`
// （DataEngine 默认 WAL 目录 data/wal/dataengine，第二个进程 data/wal/dataengine-<sid>）。
// 旧行为：生成的 .gitignore 有 `.dev/`，却没有 WAL 目录，`make dev-run` 之后 `git add -A`
// 会把 WAL 段（玩家数据、可能很大）提交进仓库。承诺：生成器认定的运行期输出在生成的
// .gitignore 里同样被忽略；configs/data（生成器输出、要提交）不能被顺带忽略。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedGitignoreIgnoresTheRuntimeOutputGenerationSkips(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := copyOfNewProject(t, "configdata")
	if out, err := exec.Command(git, "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	ignored := func(rel string) bool {
		cmd := exec.Command(git, "-C", root, "check-ignore", "-q", "--no-index", filepath.FromSlash(rel))
		err := cmd.Run()
		if err == nil {
			return true
		}
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return false
		}
		t.Fatalf("git check-ignore %s: %v", rel, err)
		return false
	}
	for _, rel := range []string{
		".dev/game.log",
		"data/wal/dataengine/000001.wal",
		"data/wal/dataengine-2/000001.wal", // deploy/dev/second-game.sh 的独立 WAL 目录
	} {
		dir := strings.SplitN(rel, "/", 3)
		if !skippedProjectDirectory(filepath.ToSlash(filepath.Join(dir[0], dir[1]))) && !skippedProjectDirectory(dir[0]) {
			t.Fatalf("fixture drift: generation no longer skips %s, so this promise no longer applies", rel)
		}
		if !ignored(rel) {
			t.Errorf("generated .gitignore does not ignore %s, which generation itself treats as runtime output", rel)
		}
	}
	for _, rel := range []string{"configs/data/item.json", "data/README.md"} {
		if ignored(rel) {
			t.Errorf("generated .gitignore ignores %s; only the WAL directory is runtime output", rel)
		}
	}
}

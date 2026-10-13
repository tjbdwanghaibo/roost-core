package roostcore_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// 示例实跑门禁：仓库里所有 examples/ 目录下的 main 包都编译出来真的跑一遍，退出码必须为 0，带超时。示例所在的模块独立于根模块，
// 根模块的 `go build ./...` 不包含它们，只编译不运行会漏掉运行即 panic 或依赖缺失。发现是穷尽的：新加的示例不在 exampleRuns 里
// 就失败，要么登记运行，要么写明跳过理由（需要外部依赖的示例才允许跳过）。

// exampleRuns 是全部可运行示例（相对仓库根，含 main 的目录）→ 跳过理由；空串表示必须运行。
// 目前六个示例都只用进程内或回环资源，没有一个需要外部依赖。
var exampleRuns = map[string]string{
	"examples/configgen":                   "",
	"examples/lubanreal":                   "",
	"examples/robotdemo":                   "", // 自带回环 TCP echo 服务器，不连外部服务
	"gameplay/skill/examples/combat":       "",
	"gameplay/skill/examples/fireball":     "",
	"gameplay/skill/examples/statusbridge": "",
}

const (
	exampleBuildTimeout = 5 * time.Minute
	exampleRunTimeout   = time.Minute
)

func TestExamplesRun(t *testing.T) {
	found := discoverExampleMains(t)
	var declared []string
	for dir := range exampleRuns {
		declared = append(declared, dir)
	}
	sort.Strings(declared)
	if strings.Join(found, "\n") != strings.Join(declared, "\n") {
		t.Fatalf("examples/ 下的 main 包与 exampleRuns 不一致：\n仓库里有：%v\n登记的：%v\n新示例要登记进 exampleRuns（必须运行，或写明需要哪个外部依赖而跳过）", found, declared)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("找不到 go 命令，示例门禁不能静默放行：%v", err)
	}
	bin := t.TempDir()
	for _, dir := range found {
		t.Run(dir, func(t *testing.T) {
			if reason := exampleRuns[dir]; reason != "" {
				t.Skip(reason)
			}
			t.Parallel()
			runExample(t, goTool, dir, filepath.Join(bin, strings.ReplaceAll(dir, "/", "_")))
		})
	}
}

// runExample 在示例自己的模块里编译（GOWORK=off，和发布后用户看到的一样），然后在示例目录里运行。
func runExample(t *testing.T, goTool, dir, binary string) {
	t.Helper()
	// Windows按可执行扩展名解析路径；无后缀产物能编译但os/exec无法启动。
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	moduleRoot := nearestModuleRoot(t, dir)
	rel, err := filepath.Rel(moduleRoot, dir)
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GOWORK=off")

	buildCtx, cancel := context.WithTimeout(context.Background(), exampleBuildTimeout)
	defer cancel()
	build := exec.CommandContext(buildCtx, goTool, "build", "-o", binary, "./"+filepath.ToSlash(rel))
	build.Dir, build.Env = moduleRoot, env
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("编译 %s（模块 %s）失败：%v\n%s", dir, moduleRoot, err, tail(out))
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), exampleRunTimeout)
	defer cancelRun()
	run := exec.CommandContext(runCtx, binary)
	run.Dir, run.Env = dir, env
	var output bytes.Buffer
	run.Stdout, run.Stderr = &output, &output
	err = run.Run()
	if runCtx.Err() != nil {
		t.Fatalf("运行 %s 超过 %s 没有退出\n%s", dir, exampleRunTimeout, tail(output.Bytes()))
	}
	if err != nil {
		t.Fatalf("运行 %s 失败：%v\n%s", dir, err, tail(output.Bytes()))
	}
}

var packageMainLine = regexp.MustCompile(`(?m)^package main\s*$`)

// discoverExampleMains 返回路径里含 examples 段、且有非测试 package main 源文件的目录（排序）。
func discoverExampleMains(t *testing.T) []string {
	t.Helper()
	set := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "artifacts", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		if !hasPathSegment(dir, "examples") || set[dir] {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if packageMainLine.Match(source) {
			set[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(set))
	for dir := range set {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

func hasPathSegment(path, segment string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == segment {
			return true
		}
	}
	return false
}

// nearestModuleRoot 从示例目录往上找最近的 go.mod（examples/ 与 skill/examples/ 是独立模块）。
func nearestModuleRoot(t *testing.T, dir string) string {
	t.Helper()
	for current := dir; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if current == "." || current == string(filepath.Separator) {
			t.Fatalf("%s 往上没有 go.mod", dir)
		}
	}
}

func tail(out []byte) string {
	const limit = 4000
	if len(out) > limit {
		return "…" + string(out[len(out)-limit:])
	}
	return string(out)
}

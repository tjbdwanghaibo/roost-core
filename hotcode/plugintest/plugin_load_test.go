//go:build darwin || linux || freebsd

// Package plugintest 用真实 .so 验证 hotcode.LoadPlugin。它是独立的测试包：hotcode 自己的
// 测试二进制里 hotcode 带着内部 _test.go 重新编译，与插件链接的 hotcode 不是同一版本，
// plugin.Open 会拒绝；这里按普通方式导入 hotcode，与插件一致。
package plugintest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/hotcode"
)

var (
	buildOnce sync.Once
	soPath    string
	buildSkip string
	buildErr  string
	buildDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// pluginPath 构建一次插件（同一进程不能加载两个包路径相同的插件，-count>1 时复用）。
// 构建条件不满足（无 go 工具链、工具链版本与测试二进制不同、cgo 关闭、覆盖率模式）时跳过。
func pluginPath(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		goTool, err := exec.LookPath("go")
		if err != nil {
			buildSkip = "go toolchain not on PATH"
			return
		}
		if testing.CoverMode() != "" {
			buildSkip = "coverage instrumentation changes package hashes; plugin would not match"
			return
		}
		env, err := exec.Command(goTool, "env", "GOVERSION", "CGO_ENABLED").Output()
		if err != nil {
			buildSkip = "go env failed: " + err.Error()
			return
		}
		fields := strings.Fields(string(env))
		if len(fields) != 2 || fields[0] != runtime.Version() {
			buildSkip = "go on PATH is " + strings.Join(fields, " ") + ", test binary is " + runtime.Version()
			return
		}
		if fields[1] != "1" {
			buildSkip = "plugins need cgo (CGO_ENABLED=" + fields[1] + ")"
			return
		}
		buildDir, err = os.MkdirTemp("", "hotcode-plugintest-")
		if err != nil {
			buildErr = err.Error()
			return
		}
		out := filepath.Join(buildDir, "bundle.so")
		args := []string{"build", "-buildmode=plugin"}
		if raceEnabled {
			args = append(args, "-race")
		}
		args = append(args, "-o", out, "./testdata/bundle")
		cmd := exec.Command(goTool, args...)
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			buildErr = err.Error() + ": " + string(output)
			return
		}
		soPath = out
	})
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	if buildErr != "" {
		t.Fatalf("build plugin: %s", buildErr)
	}
	return soPath
}

func original(v int) int { return v + 1 }

func resolveFirst() int { return hotcode.Resolve("plugintest.first", original)(1) }

func pointInfo(t *testing.T, name string) hotcode.PointInfo {
	t.Helper()
	for _, info := range hotcode.List() {
		if info.Name == name {
			return info
		}
	}
	t.Fatalf("point %s not listed", name)
	return hotcode.PointInfo{}
}

// RR-20261005-NC-244：插件按包注释把 PatchBundle 导出成 hotcode.Bundle 接口变量时，Lookup
// 得到的是 *hotcode.Bundle。LoadPlugin 为此专门有一个指针分支，但分支里 `ptr, ok := ...`
// 新声明了 ok，`ok = bundle != nil` 写的是内层变量，外层 ok 仍为 false，于是这种导出方式
// 一律报“does not implement hotcode.Bundle”。之前本仓不构建 .so，这个分支从没被执行过。
// 同一用例也是真实 .so 的控制：加载、Apply 生效、Bundle.Revert 回到原函数。
func TestLoadPluginAcceptsABundleExportedAsAnInterfaceVariable(t *testing.T) {
	path := pluginPath(t)
	hotcode.ResetForTest()
	t.Cleanup(hotcode.ResetForTest)
	if err := hotcode.Register("plugintest.first", original); err != nil {
		t.Fatal(err)
	}
	bundle, err := hotcode.LoadPlugin(path)
	if err != nil {
		t.Fatalf("LoadPlugin = %v", err)
	}
	if got := resolveFirst(); got != 101 {
		t.Fatalf("after load first(1) = %d, want 101 (the plugin's function)", got)
	}
	if info := pointInfo(t, "plugintest.first"); !info.Patched || info.Meta.Version != "plugintest-1" {
		t.Fatalf("after load %+v, want patched by plugintest-1", info)
	}
	if err := bundle.Revert(hotcode.Default); err != nil {
		t.Fatal(err)
	}
	if got := resolveFirst(); got != 2 {
		t.Fatalf("after Bundle.Revert first(1) = %d, want 2", got)
	}
}

// RR-20261005-NC-245（N10 观察 O-H1）：插件 Apply 中途失败时，已经替换的补丁点必须回到加载
// 前的状态（上一代函数与 Meta，不是原函数）。之前 LoadPlugin 只返回错误、丢掉 Bundle：运维
// 看到“加载失败”，first 却已经是插件里的函数，也拿不到 Bundle 去调它的 Revert；
// hotcode.revert 只能回到原函数，会把加载前已生效的那一代补丁一起撤掉。
func TestLoadPluginRollsBackAPartiallyAppliedBundle(t *testing.T) {
	path := pluginPath(t)
	hotcode.ResetForTest()
	t.Cleanup(hotcode.ResetForTest)
	if err := hotcode.Register("plugintest.first", original); err != nil {
		t.Fatal(err)
	}
	// 加载前已有一代补丁在生效。
	if err := hotcode.Replace("plugintest.first", func(v int) int { return v + 10 }, hotcode.Meta{Version: "v0"}); err != nil {
		t.Fatal(err)
	}
	// second 注册成不同签名，插件替换它时以 ErrTypeMismatch 失败。
	if err := hotcode.Register("plugintest.second", func(s string) string { return s }); err != nil {
		t.Fatal(err)
	}
	bundle, err := hotcode.LoadPlugin(path)
	if err == nil || bundle != nil {
		t.Fatalf("LoadPlugin = (%v, %v), want an apply error", bundle, err)
	}
	if !strings.Contains(err.Error(), "signature mismatch") {
		t.Fatalf("LoadPlugin error = %v, want the bundle's ErrTypeMismatch", err)
	}
	if got := resolveFirst(); got != 11 {
		t.Fatalf("after the failed load first(1) = %d, want 11: the partially applied patch was left in place (101) or rolled back past the previous generation (2)", got)
	}
	if info := pointInfo(t, "plugintest.first"); !info.Patched || info.Meta.Version != "v0" {
		t.Fatalf("after the failed load %+v, want the previous generation v0", info)
	}
}

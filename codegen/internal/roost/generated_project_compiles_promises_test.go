package roost

// 结构守卫（REFACTOR-2026-10-07-structural-guards §3）：新生成的 game-demo 必须能对着当前检出编译、vet 通过，
// 测试文件也算。
//
// 旧缺口：本包的用例只做文本断言与 parser.ParseFile，没有任何用例把生成工程交给类型检查器。RR-20261006-60
// （2a8b2e64）把 nest.DurabilityStrict 换成 dataengine 的 Durability 类型之后，demo 的两份测试模板
// enter_game_test.go.tmpl、guild_ids_test.go.tmpl 仍写 corenest.DurabilityStrict / nest.DurabilityStrict，
// 新生成的 game-demo `go vet` 报类型不符；codegen 的测试全绿，直到 saga 那一轮顺手修掉（133a4e88）才被发现。
// `go vet ./...` 会把 _test.go 一起类型检查，正好是 `go build` 看不到的那一半。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// generatedCoreModule 是生成工程 require 的框架模块，replace 到本检出。
const generatedCoreModule = "github.com/tjbdwanghaibo/roost-core"

func TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a whole generated project")
	}
	if runtime.GOOS == "windows" {
		// 生成形状与平台无关；windows-compatibility 作业的整包时长已经贴着 go test 的默认上限（RR-20260928-14）。
		t.Skip("the generated shape is platform independent; checked on the other runners")
	}
	t.Parallel()
	coreRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(coreRoot, "go.mod")); err != nil || !strings.HasPrefix(string(raw), "module "+generatedCoreModule+"\n") {
		t.Fatalf("%s is not the roost-core module root (%v)", coreRoot, err)
	}
	root := copyOfNewProject(t, "game-demo")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated game-demo: go %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("mod", "edit", "-replace", generatedCoreModule+"="+coreRoot)
	run("build", "./...")
	run("vet", "./...")
}

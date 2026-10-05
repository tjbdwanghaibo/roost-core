package roost

// A3（维护者决定 2026-10-05）/ RR-20261005-NC-83：生成的 player TCP 接入层（Server.Stop、Mod.StopWithContext）
// 套共用停机契约骨架 internal/stopcontract。
//
// 生成工程是另一个模块，不能导入 roost-core 的 internal 包，也不应为了测试把骨架变成公开 API 或改生成形状。
// 所以这里在 game-demo 夹具的私有副本上运行：go.mod 把 roost-core replace 到本仓库，骨架源码原样复制成生成
// 工程自己的 internal/stopcontract，再往生成的 tcp 包里加一个只在本用例存在的测试文件，复用生成测试里的
// stalledAuthServer（认证器不看 ctx、放行前一直卡住）。运行的是真实生成代码，不是文本断言。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const playerTCPStopContractTest = `package tcp

import (
	"context"
	"testing"

	"example.com/planet/internal/stopcontract"
)

// Server.Stop 自己不持有排空之后要释放的依赖：App 只在它返回 nil 之后继续停后面的 Mod。
func TestA3GeneratedServerStopContract(t *testing.T) {
	var server *Server
	var release chan struct{}
	var returned <-chan struct{}
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			server, release, _, returned = stalledAuthServer(t)
			stop, released = stopcontract.CallerReleases(server.Stop)
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return released() },
	})
	select {
	case <-returned:
	default:
		t.Fatal("Stop reported success while the authenticator was still running")
	}
}

// Mod.StopWithContext 在服务器排空之后才停会话关闭派发器（Runtime 的生命周期 goroutine）。
func TestA3GeneratedModStopContract(t *testing.T) {
	var mod *Mod
	var release chan struct{}
	runtime := &Runtime{}
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			var server *Server
			server, release, _, _ = stalledAuthServer(t)
			runtime.startLifecycle()
			runtime.server.Store(server)
			mod = &Mod{config: server.config, server: server, transportRuntime: runtime}
		},
		Stop:    func(ctx context.Context) error { return mod.StopWithContext(ctx) },
		Release: func() { close(release) },
		Released: func() bool {
			select {
			case <-runtime.lifecycleDone:
				return true
			default:
				return false
			}
		},
	})
}
`

func TestGeneratedPlayerTCPStopContract(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs the generated game-demo player TCP package")
	}
	t.Parallel()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "game-demo")
	goModPath := filepath.Join(root, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	replace := regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`)
	goMod = replace.ReplaceAll(goMod, nil)
	goMod = append(goMod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	skeleton, err := os.ReadFile(filepath.Join(repo, "internal", "stopcontract", "stopcontract.go"))
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string][]byte{
		"go.mod":                                goMod,
		"internal/stopcontract/stopcontract.go": skeleton,
		"internal/access/player/tcp/a3_stop_contract_test.go": []byte(playerTCPStopContractTest),
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "test", "-mod=mod", "-count=1", "-v", "-run", "TestA3Generated", "./internal/access/player/tcp/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated player TCP stop contract: %v\n%s", err, out)
	}
	for _, name := range []string{"TestA3GeneratedServerStopContract", "TestA3GeneratedModStopContract"} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run and pass:\n%s", name, out)
		}
	}
}

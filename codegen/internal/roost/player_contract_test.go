package roost

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
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

func generatedTestBodies(t *testing.T, root, rel string) map[string]string {
	t.Helper()
	source := readProjectFile(t, root, rel)
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Base(rel), source, 0)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	bodies := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// 单文件解析，FileSet 里只有这一个文件，Pos 减 1 就是字节偏移。
		bodies[fn.Name.Name] = source[fn.Body.Lbrace-1 : fn.Body.Rbrace]
	}
	return bodies
}

// firstIndex 返回 needles 里任一个在 body 中第一次出现的位置，都没有时返回 -1。
func firstIndex(body string, needles ...string) int {
	first := -1
	for _, needle := range needles {
		if at := strings.Index(body, needle); at >= 0 && (first < 0 || at < first) {
			first = at
		}
	}
	return first
}

func TestGeneratedPlayerTCPTestsWaitForTheSessionsTheyUse(t *testing.T) {
	t.Parallel()
	// 通用运行测试随实现迁到 gateway；保持相同等待契约，不能靠生成物复制测试。
	root := "../../.."
	rel := "infra/network/gateway/tcp_test.go"
	bodies := generatedTestBodies(t, root, rel)

	// 助手缺失时照样往下查每个用例，失败信息落在“哪个用例没等”上。
	helper, ok := bodies["waitRegistered"]
	if !ok {
		t.Errorf("%s has no waitRegistered helper", rel)
	}
	for _, want := range []string{"runtime.ActiveSessions(playerID) != want", "time.Now().Add(2 * time.Second)", "t.Fatalf("} {
		if ok && !strings.Contains(helper, want) {
			t.Errorf("waitRegistered is not a bounded wait on the registered count: missing %q", want)
		}
	}

	dials := []string{"dialAuthenticated(t, server,", "stalled(t, server,"}
	uses := []string{"runtime.PushPlayer(", "runtime.PushSession(", "runtime.CloseSessions(", "runtime.ActiveSessions(", "fillUntilOneFails("}
	checked := map[string]bool{}
	for name, body := range bodies {
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		use := firstIndex(body, uses...)
		if use < 0 || firstIndex(body, dials...) < 0 {
			continue
		}
		lastDial := -1
		for _, dial := range dials {
			lastDial = max(lastDial, strings.LastIndex(body[:use], dial))
		}
		wait := strings.Index(body, "waitRegistered(t, runtime, ")
		if wait < 0 || wait < lastDial || wait > use {
			t.Errorf("%s pushes, closes or counts the sessions it dialed without first waiting for them to be registered "+
				"(last dial at %d, wait at %d, first use at %d): the server acks before it registers", name, lastDial, wait, use)
		}
		checked[name] = true
	}
	for _, name := range []string{
		"TestAConnectionThatCannotTakeAPushIsClosedAndTheOthersKeepIt",
		"TestAPushNoConnectionTakesFailsAndClosesThemAll",
		"TestAPushThatFailsBeforeWritingClosesNoConnection",
		"TestASessionPushThatCannotBeWrittenClosesThatSession",
		"TestCloseSessionsCountsTheSessionsItClosed",
		"TestAnExpiringCallerDeadlineClosesNoHealthyConnection",
	} {
		if !checked[name] {
			t.Errorf("%s was not checked: it no longer dials and then uses the runtime, or it is gone", name)
		}
	}
}

func TestGeneratedSceneConnectionTestsWaitForEveryDialedPlayer(t *testing.T) {
	t.Parallel()
	root, err := generatedProjectFixture("game-demo")
	if err != nil {
		t.Fatal(err)
	}
	rel := "internal/service/game/scene_connections_test.go"
	bodies := generatedTestBodies(t, root, rel)
	dial := regexp.MustCompile(`dialScenePlayer\(t, addr, (\w+),`)
	uses := []string{"joinReady(", "stallPlayer(", "transport.PushPlayer("}
	dialed := 0
	for name, body := range bodies {
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		for _, match := range dial.FindAllStringSubmatchIndex(body, -1) {
			at, player := match[1], body[match[2]:match[3]]
			dialed++
			next := firstIndex(body[at:], uses...)
			if next < 0 {
				continue
			}
			if !strings.Contains(body[at:at+next], "waitActiveSessions(t, transport, "+player+", ") {
				t.Errorf("%s dials %s and uses the scene before waiting for that connection to be registered: "+
					"the transport acks before it registers, and a member without a registered connection is swept out", name, player)
			}
		}
	}
	if dialed < 6 {
		t.Errorf("%s: found %d dials, want the 6 of the two stalled-connection tests", rel, dialed)
	}
}

const playerTCPStopContractTest = `package tcp

import (
	"context"
	"testing"

	"example.com/planet/internal/stopcontract"
 "github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

// Server.Stop 自己不持有排空之后要释放的依赖：App 只在它返回 nil 之后继续停后面的 Mod。
func TestA3GeneratedServerStopContract(t *testing.T) {
	var server *gateway.TCPServer
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
	runtime := NewRuntime(nil,defaultLoginTimeout)
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			var server *gateway.TCPServer
			server, release, _, _ = stalledAuthServer(t)
			mod = &Mod{config: defaultConfig(), server: server, transportRuntime: runtime}
		},
		Stop:    func(ctx context.Context) error { return mod.StopWithContext(ctx) },
		Release: func() { close(release) },
		Released: func() bool {
			return mod.server == nil
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

package roost

// RR-20260928-15（REMAINING-2026-09-28 N70）：生成的 player TCP 服务器在 serveConnection 里先写认证 ack、
// 再 registerSession。客户端读到 ack 时，服务器可能还没把这个会话登记进 sessions / playerSessions，
// 所以“拨号返回”不等于“ActiveSessions / PushPlayer / PushSession / CloseSessions 已经能看到它”。
//
// RR-20260927-02 的后续更正只给 TestCloseSessionsCountsTheSessionsItClosed 加了有界等待；同一文件里
// 其余五个用例在拨号后直接推送 / 计数，高负载下偶发失败（framework-compat run 36367611936：
// server_gen_test.go:556: active sessions = 1, want 2）。game 服务的 scene_connections_test.go 也一样：
// 只等了被测玩家的连接登记，没等 mover 的，mover 未登记时会在下一个玩家入场的 sweep 中被移出场景。
//
// 这里只改生成的测试，不改服务器行为。本用例断言模板里的等待：凡是拨号之后要推送、关闭或计数的用例，
// 在最后一次拨号之后、第一次使用之前都先有界等待这些会话登记。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// generatedTestBodies 解析生成的测试文件，返回每个函数名到其函数体源码的映射。
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
	root, err := generatedProjectFixture("game-demo")
	if err != nil {
		t.Fatal(err)
	}
	rel := "internal/access/player/tcp/server_gen_test.go"
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

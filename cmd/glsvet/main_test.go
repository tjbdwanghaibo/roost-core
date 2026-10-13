package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vetSource(t *testing.T, source string) int {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := vetDirectory(token.NewFileSet(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func TestHandlerRawGoroutineIsRejected(t *testing.T) {
	if findings := vetSource(t, `package handler
//roost:nest rollback=undo durability=strict
func handlerMove() { go func() {}() }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandlerNamedGoroutineWrapperIsRejected(t *testing.T) {
	if findings := vetSource(t, `package handler
func launch() { go func() {}() }
//roost:nest rollback=undo durability=strict
func handlerMove() { launch() }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandlerErrgroupGoIsRejected(t *testing.T) {
	if findings := vetSource(t, `package handler
type group struct{}
func (*group) Go(func()) {}
//roost:nest rollback=undo durability=strict
func handlerMove(g *group) { g.Go(func(){}) }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandlerCoreWorkerPoolGoIsAllowed(t *testing.T) {
	if findings := vetSource(t, `package handler
import "github.com/tjbdwanghaibo/roost-core/infra/base/worker"
type task struct{}
func (task) OnRelease() {}
var pool *worker.Pool[task]
//roost:nest rollback=undo durability=strict
func handlerMove() { pool.Go(task{}, func(task){}) }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0", findings)
	}
}

func TestHandlerCoreWorkerPoolFieldGoIsAllowed(t *testing.T) {
	if findings := vetSource(t, `package handler
import "github.com/tjbdwanghaibo/roost-core/infra/base/worker"
type task struct{}
func (task) OnRelease() {}
type runtime struct { pool *worker.Pool[task] }
//roost:nest rollback=undo durability=strict
func handlerMove(r *runtime) { r.pool.Go(task{}, func(task){}) }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0", findings)
	}
}

func TestHandlerCoreWorkerPoolTryGoIsAllowedWhenAdmissionHandled(t *testing.T) {
	if findings := vetSource(t, `package handler
import "github.com/tjbdwanghaibo/roost-core/infra/base/worker"
type task struct{}
func (task) OnRelease() {}
var pool *worker.Pool[task]
//roost:nest rollback=undo durability=strict
func handlerMove() error { return pool.TryGo(task{}, func(task){}) }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0", findings)
	}
}

func TestIgnoredAdmissionResultIsRejected(t *testing.T) {
	if findings := vetSource(t, `package service
type client struct{}
func (*client) Dispatch() error { return nil }
func run(client *client) { client.Dispatch() }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandledAdmissionResultIsAllowed(t *testing.T) {
	if findings := vetSource(t, `package service
type client struct{}
func (*client) Dispatch() error { return nil }
func run(client *client) error { return client.Dispatch() }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0", findings)
	}
}

func TestVoidMethodWithAdmissionNameIsAllowed(t *testing.T) {
	if findings := vetSource(t, `package service
type publisher struct{}
func (*publisher) Publish() {}
func run(publisher *publisher) { publisher.Publish() }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0", findings)
	}
}

func TestUnknownInterfaceVoidPublishIsAllowed(t *testing.T) {
	if findings := vetSource(t, `package service
type publisher interface { Publish() }
func run(p publisher) { p.Publish() }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0", findings)
	}
}

func TestFrameworkTypedAdmissionResultIsRejected(t *testing.T) {
	if findings := vetSource(t, `package service
import corebus "github.com/tjbdwanghaibo/roost-core/infra/network/bus"
func run(p corebus.Bus) { p.Publish() }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandlerWorkerClosureCannotCaptureOuterState(t *testing.T) {
	if findings := vetSource(t, `package handler
import "github.com/tjbdwanghaibo/roost-core/infra/base/worker"
type task struct{ id int }
func (task) OnRelease() {}
var pool *worker.Pool[task]
//roost:nest rollback=undo durability=strict
func handlerMove(playerID int) { pool.Go(task{id: playerID}, func(current task){ _ = playerID }) }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandlerAfterCommitClosureIsAllowed(t *testing.T) {
	if findings := vetSource(t, `package handler
func AfterCommit(func()) bool { return true }
//roost:nest rollback=undo durability=strict
func handlerMove() { AfterCommit(func(){}) }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0", findings)
	}
}

// A1：组件方法里直接登记 undo 只给提示（不计入 findings、不改退出码）；DAO 自己的方法登记 undo 是生成 setter 的正常形状，不提示。
func TestComponentRecordingItsOwnUndoIsHinted(t *testing.T) {
	dir := t.TempDir()
	source := `package player
import (
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)
type AttributeComponent struct {
	entity.ComponentBase
	layers map[int]int
}
func (c *AttributeComponent) capture() {
	tx := nest.CurrentRollbackTx()
	_ = tx.RecordUndo(c, 1, func() error { return nil })
}
type Scheduler struct{ entity.ComponentBase }
func (s Scheduler) arm() { nest.RecordUndoToken(s, 1, 2, func() error { return nil }) }
type PlayerDao struct{ level int32 }
func (d *PlayerDao) SetLevel(v int32) {
	nest.RecordUndo(d, 1, func() error { return nil })
	d.level = v
}
`
	if err := os.WriteFile(filepath.Join(dir, "component.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	findings, err := vetDirectory(fileSet, dir)
	if err != nil {
		t.Fatal(err)
	}
	if findings != 0 {
		t.Fatalf("findings = %d, want 0: the component undo check is a hint, not a gate", findings)
	}
	packages, err := parser.ParseDir(fileSet, dir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	hints := componentUndoHints(fileSet, packages["player"])
	if len(hints) != 2 {
		t.Fatalf("hints = %q, want the two component methods (not the DAO setter)", hints)
	}
	for _, want := range []string{"AttributeComponent.capture", "Scheduler.arm"} {
		found := false
		for _, hint := range hints {
			found = found || strings.Contains(hint, want)
		}
		if !found {
			t.Fatalf("no hint for %s in %q", want, hints)
		}
	}
}

// 组件方法经同包 helper 函数登记 undo，与直接登记是同一个违例，也要提示（跟进一层，与停止类函数的提示同样做法）。
// 组件调 DAO 的 setter（DAO 方法自己登记 undo）是 A1 要求的正确写法，不能因为跟进而误报；helper 只跟一层。
func TestComponentRecordingUndoThroughHelperIsHinted(t *testing.T) {
	source := `package player
import (
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)
type BuffComponent struct {
	entity.ComponentBase
	dao   *PlayerDao
	stack []int
}
func (c *BuffComponent) push(v int) {
	c.stack = append(c.stack, v)
	rememberPop(c)
}
func (c *BuffComponent) level(v int32) { c.dao.SetLevel(v) }
func (c *BuffComponent) deep() { outer(c) }
func rememberPop(c *BuffComponent) {
	nest.CurrentRollbackTx().RecordUndo(c, 1, func() error { c.stack = c.stack[:len(c.stack)-1]; return nil })
}
func outer(c *BuffComponent) { rememberPop(c) }
type PlayerDao struct{ level int32 }
func (d *PlayerDao) SetLevel(v int32) {
	nest.RecordUndo(d, 1, func() error { return nil })
	d.level = v
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "component.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, dir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	hints := componentUndoHints(fileSet, packages["player"])
	if len(hints) != 1 || !strings.Contains(hints[0], "BuffComponent.push") || !strings.Contains(hints[0], "rememberPop") {
		t.Fatalf("hints = %q, want exactly one hint: BuffComponent.push registering undo through rememberPop "+
			"(not the DAO setter call, not the two-level outer → rememberPop chain)", hints)
	}
	if findings, err := vetDirectory(token.NewFileSet(), dir); err != nil || findings != 0 {
		t.Fatalf("findings = %d err = %v, want 0: the component undo check is a hint, not a gate", findings, err)
	}
}

// B4（维护者 2026-10-06）：skill.Runtime 的状态按决定不进事务，是 A1 的明确例外。A1 的提示只看
// 组件方法里的 undo 登记，skill 各包（含手写 CombatDao 自己登记逆操作的 combatcomponent）都不应
// 命中；命中说明提示的判定变了，需要先确认是否误报再决定豁免。
func TestSkillPackagesGetNoComponentUndoHint(t *testing.T) {
	for _, directory := range []string{"../../gameplay/skill", "../../gameplay/skill/combatcomponent", "../../gameplay/skill/combat", "../../gameplay/skill/skillsync"} {
		fileSet := token.NewFileSet()
		packages, err := parser.ParseDir(fileSet, directory, func(info os.FileInfo) bool {
			return !strings.HasSuffix(info.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(packages) == 0 {
			t.Fatalf("%s: no package parsed", directory)
		}
		for _, pkg := range packages {
			if hints := componentUndoHints(fileSet, pkg); len(hints) != 0 {
				t.Fatalf("%s: unexpected A1 hints %q", directory, hints)
			}
		}
	}
}

// 生成器接受方法 handler，检查器不能因 receiver 非空而跳过它。
func TestMethodHandlerConcurrencyChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   int
	}{
		{"pointer", `package p
type controller struct{}
//roost:nest rollback=undo durability=strict
func (*controller) Move() { go func(){}() }
`, 1},
		{"value", `package p
type controller struct{}
//roost:nest rollback=undo durability=strict
func (controller) Move() { go func(){}() }
`, 1},
		{"package helper with same name", `package p
type controller struct{}
func Move() { go func(){}() }
//roost:nest rollback=undo durability=strict
func (*controller) Move() { Move() }
`, 1},
		{"different receivers with same name", `package p
type first struct{}
type second struct{}
//roost:nest rollback=undo durability=strict
func (*first) Move() { go func(){}() }
//roost:nest rollback=undo durability=strict
func (*second) Move() { go func(){}() }
`, 2},
		{"receiver capture", `package p
import "github.com/tjbdwanghaibo/roost-core/infra/base/worker"
type task struct{}
func (task) OnRelease() {}
var pool *worker.Pool[task]
type controller struct { value int }
//roost:nest rollback=undo durability=strict
func (c *controller) Move() { pool.Go(task{}, func(task){ _ = c.value }) }
`, 1},
		{"explicit worker parameters", `package p
import "github.com/tjbdwanghaibo/roost-core/infra/base/worker"
type task struct{}
func (task) OnRelease() {}
var pool *worker.Pool[task]
type controller struct{}
//roost:nest rollback=undo durability=strict
func (*controller) Move() { pool.Go(task{}, func(task){}) }
`, 0},
		{"ordinary method", `package p
type controller struct{}
func (*controller) Move() { go func(){}() }
`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vetSource(t, tc.source); got != tc.want {
				t.Fatalf("findings = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBusinessWaitCheckerDistinguishesAwaitStages(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`client.Request(ctx, name, id, nil)`, 1},
		{`nest.RunLocal(ctx, func(){})`, 1},
		{`return nest.Await(func(ctx context.Context)(int,error){time.Sleep(time.Second);return 1,nil},func(p *Player,n int,err error)error{return nil})`, 0},
		{`return nest.Await(func(ctx context.Context)(int,error){return 1,nil},func(p *Player,n int,err error)error{time.Sleep(time.Second);return nil})`, 1},
	} {
		t.Run(tc.body, func(t *testing.T) {
			if got := vetSource(t, "package game\n//roost:nest durability=memory\nfunc Reward(){"+tc.body+"}\n"); got != tc.want {
				t.Fatalf("got=%d want=%d", got, tc.want)
			}
		})
	}
}

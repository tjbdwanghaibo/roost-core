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
import "github.com/tjbdwanghaibo/roost-core/worker"
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
import "github.com/tjbdwanghaibo/roost-core/worker"
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
import "github.com/tjbdwanghaibo/roost-core/worker"
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
import corebus "github.com/tjbdwanghaibo/roost-core/bus"
func run(p corebus.Bus) { p.Publish() }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandlerWorkerClosureCannotCaptureOuterState(t *testing.T) {
	if findings := vetSource(t, `package handler
import "github.com/tjbdwanghaibo/roost-core/worker"
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
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
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

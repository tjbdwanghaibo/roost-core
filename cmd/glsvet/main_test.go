package main

import (
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func vetSource(t *testing.T, source string) int {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return vetDirectory(token.NewFileSet(), dir)
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
import "github.com/tjbdwanghaibo/cube-core/worker"
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
import "github.com/tjbdwanghaibo/cube-core/worker"
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
import "github.com/tjbdwanghaibo/cube-core/worker"
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
import corebus "github.com/tjbdwanghaibo/cube-core/bus"
func run(p corebus.Bus) { p.Publish() }
`); findings != 1 {
		t.Fatalf("findings = %d, want 1", findings)
	}
}

func TestHandlerWorkerClosureCannotCaptureOuterState(t *testing.T) {
	if findings := vetSource(t, `package handler
import "github.com/tjbdwanghaibo/cube-core/worker"
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

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A3 ③：停止入口的复审提示只看“带 ctx 的停止类函数里不受 ctx 约束的通道接收”，并且不计入违例。
func stopHintsIn(t *testing.T, source string) int {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "stop.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	return reportStopHints(fileSet, file, collectFunctions([]*ast.File{file}))
}

func TestStopHintsFlagABareReceiveInAStopWithContext(t *testing.T) {
	// RR-20261005-NC-173 修前 Deregister 的形状：裸 <-done。
	if hints := stopHintsIn(t, `package p
import "context"
type d struct{ done chan struct{} }
func (x *d) Deregister(ctx context.Context) error { <-x.done; return nil }
`); hints != 1 {
		t.Fatalf("hints = %d, want 1", hints)
	}
	// select 没有 ctx.Done() 分支也没有 default，同样不受 ctx 约束。
	if hints := stopHintsIn(t, `package p
import "context"
func StopAll(ctx context.Context, a, b chan struct{}) { select { case <-a: case <-b: } }
`); hints != 2 {
		t.Fatalf("hints = %d, want 2", hints)
	}
}

// RR-20261005-NC-173 修前的真实形状：Deregister(ctx) 调用不带 ctx 的 waitLoopDone，后者裸 <-done。
func TestStopHintsFollowOneCallIntoAHelperThatCannotSeeTheContext(t *testing.T) {
	if hints := stopHintsIn(t, `package p
import "context"
type d struct{ done chan struct{} }
func (x *d) waitLoopDone() { if x.done != nil { <-x.done } }
func (x *d) Deregister(ctx context.Context) error { x.waitLoopDone(); return nil }
`); hints != 1 {
		t.Fatalf("hints = %d, want 1", hints)
	}
	// 把 ctx 传下去的 helper 由它自己的签名负责，不在调用处提示。
	if hints := stopHintsIn(t, `package p
import "context"
type d struct{ done chan struct{} }
func (x *d) waitLoopDone(ctx context.Context) error { select { case <-x.done: return nil; case <-ctx.Done(): return ctx.Err() } }
func (x *d) Deregister(ctx context.Context) error { return x.waitLoopDone(ctx) }
`); hints != 0 {
		t.Fatalf("hints = %d, want 0", hints)
	}
}

func TestStopHintsAcceptBoundedWaits(t *testing.T) {
	for name, source := range map[string]string{
		"select on ctx": `package p
import "context"
func (x *d) StopWithContext(ctx context.Context) error {
	select {
	case <-x.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
type d struct{ done chan struct{} }
`,
		"select with default": `package p
import "context"
func Close(ctx context.Context, done chan struct{}) bool { select { case <-done: return true; default: return false } }
`,
		"goroutine adapter": `package p
import ("context"; "sync")
func Shutdown(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { wg.Wait(); <-done }()
	select { case <-done: return nil; case <-ctx.Done(): return ctx.Err() }
}
`,
		"nil ctx is explicitly unbounded": `package p
import "context"
func Stop(ctx context.Context, done chan struct{}) error {
	if ctx == nil {
		<-done
		return nil
	}
	select { case <-done: return nil; case <-ctx.Done(): return ctx.Err() }
}
`,
		"no ctx parameter": `package p
func Stop(done chan struct{}) { <-done }
`,
		"not a stop function": `package p
import "context"
func Receive(ctx context.Context, done chan struct{}) { <-done }
`,
		"mutex lock is not hinted": `package p
import ("context"; "sync")
func StopWithContext(ctx context.Context, mu *sync.Mutex) { mu.Lock(); defer mu.Unlock() }
`,
	} {
		t.Run(name, func(t *testing.T) {
			if hints := stopHintsIn(t, source); hints != 0 {
				t.Fatalf("hints = %d, want 0", hints)
			}
		})
	}
}

// 提示不计入违例：含提示形状的目录仍是 0 个 finding，glsvet 以 0 退出。
func TestStopHintsDoNotCountAsFindings(t *testing.T) {
	if findings := vetSource(t, `package handler
import "context"
type d struct{ done chan struct{} }
func (x *d) Deregister(ctx context.Context) error { <-x.done; return nil }
`); findings != 0 {
		t.Fatalf("findings = %d, want 0 (hints never fail the run)", findings)
	}
}

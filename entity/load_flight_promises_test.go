package entity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// RR-20260919-08：一次 loader panic 不能让这个实体永远加载不了。
//
// 共享加载把并发的同一实体收敛成一次 LoadEntity。删除 flight 与 close(done)
// 写在调用之后、没有 defer，所以 loader（或它下面的 store、builder、反序列化、
// 初始化）里的一次 panic 会跳过这两步：map 里留着一个永远不会关闭的 flight，
// 之后每一个请求都等在那个 channel 上，直到自己的 ctx 超时——重试也不会再进
// loader，因为它们看到的是"已经有人在加载了"。
//
// 进程还活着（Nest 顶层会 recover handler 的 panic），所以这不是一次崩溃，
// 是一个实体从此不可用。

type panickingLoader struct {
	calls  int
	panics int
	value  IThreadSafeEntity
}

func (l *panickingLoader) LoadEntity(context.Context, int64, EntityKind) (IThreadSafeEntity, error) {
	l.calls++
	if l.calls <= l.panics {
		panic("loader blew up")
	}
	return l.value, nil
}

func TestALoaderPanicDoesNotWedgeTheEntity(t *testing.T) {
	access := &ManagerAccess{}
	loader := &panickingLoader{panics: 1}
	const fullID = int64(4242)

	// The first caller sees the panic — as a panic, so nothing pretends the
	// load succeeded — but the flight must be finished on the way out.
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("the loader panic was swallowed; a failed load must not look like a successful one")
			}
		}()
		_, _ = access.loadEntityShared(context.Background(), fullID, 1, loader)
	}()

	access.flightMu.Lock()
	_, stuck := access.flights[fullID]
	access.flightMu.Unlock()
	if stuck {
		t.Fatal("the flight survived the panic; every later request for this entity would wait forever")
	}

	// And the retry actually reaches the loader again.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := access.loadEntityShared(ctx, fullID, 1, loader)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the retry after a panic failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the retry hung: it is waiting on the flight the panic left behind")
	}
	if loader.calls != 2 {
		t.Fatalf("the loader was called %d times, want the panic and one real retry", loader.calls)
	}
}

// A waiter that arrived while the leader was panicking must not be left
// hanging either: it gets an error that says what happened.
func TestWaitersOfAPanickingLoadAreReleased(t *testing.T) {
	access := &ManagerAccess{}
	const fullID = int64(4243)
	entered := make(chan struct{})
	release := make(chan struct{})
	loader := loaderFunc(func(context.Context, int64, EntityKind) (IThreadSafeEntity, error) {
		close(entered)
		<-release
		panic("loader blew up")
	})

	leaderDone := make(chan struct{})
	go func() {
		defer func() { _ = recover(); close(leaderDone) }()
		_, _ = access.loadEntityShared(context.Background(), fullID, 1, loader)
	}()
	<-entered

	waiterCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := access.loadEntityShared(waiterCtx, fullID, 1, loader)
		waiter <- err
	}()
	// Give the waiter time to attach to the flight, then let the leader die.
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-leaderDone

	select {
	case err := <-waiter:
		if err == nil {
			t.Fatal("a waiter of a panicking load was told the load succeeded")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("the waiter was released by its own deadline, not by the flight finishing")
		}
		if !strings.Contains(err.Error(), "panic") {
			t.Errorf("the waiter's error does not say what happened: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter is still waiting on a flight nobody will close")
	}
}

type loaderFunc func(context.Context, int64, EntityKind) (IThreadSafeEntity, error)

func (f loaderFunc) LoadEntity(ctx context.Context, fullID int64, kind EntityKind) (IThreadSafeEntity, error) {
	return f(ctx, fullID, kind)
}

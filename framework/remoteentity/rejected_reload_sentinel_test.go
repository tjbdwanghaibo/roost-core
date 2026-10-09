package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-62：持久拒绝收尾（回滚 + 隔离）释放 gate 之后、仅内存卸载完成之前，下一写者碰到的是“正在重载”的可重试窗口，
// 不是真正的 fence。它必须得到可 errors.Is 的 entity.ErrRemoteEntityReloading，同时仍满足 errors.Is(err, ErrRemoteFenced)
// （兼容既有判断）；窗口结束后重试得到从权威重载的新实例。

func assertReloadingSentinel(t *testing.T, err error, where string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: next writer admitted while the rejected instance is still being reloaded", where)
	}
	if !errors.Is(err, entity.ErrRemoteEntityReloading) {
		t.Fatalf("%s: next writer err=%v, want the retryable entity.ErrRemoteEntityReloading (callers cannot tell this window from a real fence)", where, err)
	}
	if !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("%s: err=%v no longer satisfies errors.Is(ErrRemoteFenced)", where, err)
	}
}

func probeNextWriter(t *testing.T, f reloadFixture) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.id})
	if err == nil {
		_ = next.Abort(context.Background(), errors.New("probe"))
		_ = next.Close(context.Background())
	}
	return err
}

// finalizer 路径（Durability 0 从未到达 → 持久拒绝）：本地执行入口先放行回滚，再把卸载停住；gate 已释放、旧实例仍隔离在内存。
func TestRejectedWriteBeforeUnloadReturnsReloadingSentinel(t *testing.T) {
	f, live := newReloadFixture(t, 1981)
	var calls atomic.Int32
	unloadHeld, resumeUnload := make(chan struct{}), make(chan struct{})
	f.mgr.BindLocalExecutor(func(fn func()) error {
		// 第 1 次是拒绝收尾的回滚（settleRejectedRemoteEntries），第 2 次是 gate 释放之后的仅内存卸载。
		if calls.Add(1) == 2 {
			close(unloadHeld)
			<-resumeUnload
		}
		fn()
		return nil
	})
	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xE1), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-unloadHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("finalizer never reached the unload of the rejected instance")
	}
	assertReloadingSentinel(t, probeNextWriter(t, f), "gate released, unload pending")
	close(resumeUnload)
	f.assertReloadedFromAuthority(t, live)
}

// batch.Close 路径（Durability 0 被权威明确拒绝）：消息收尾里的卸载被快池拒绝投递，交给 finalizer 重试；重试前的窗口同样可重试。
func TestDefinitelyRejectedWriteBeforeUnloadReturnsReloadingSentinel(t *testing.T) {
	f, live := newReloadFixture(t, 1982)
	var paused atomic.Bool
	paused.Store(true)
	refuse := func(fn func()) error {
		if paused.Load() {
			return errors.New("test: fast pool paused")
		}
		fn()
		return nil
	}
	f.mgr.BindLocalExecutor(refuse)
	f.storage.reject.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xE2), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("commit err=%v, want version conflict", err)
	}
	if err := batch.Close(entity.WithLocalExecutor(context.Background(), refuse)); err != nil {
		t.Fatal(err)
	}
	f.storage.reject.Store(false)
	assertReloadingSentinel(t, probeNextWriter(t, f), "definite rejection, unload pending")
	paused.Store(false)
	f.assertReloadedFromAuthority(t, live)
}

// 卸载过程中（Destroy 已把实例移出索引、生命周期回调尚未结束）发起的重载被 EntityManager 以“being removed”拒绝：
// 下一写者得到同一可重试哨兵（原错误仍可 errors.Is(entity.ErrEntityRemoved)），而不是通用的加载失败。
func TestReloadDuringUnloadReturnsReloadingSentinel(t *testing.T) {
	f, live := newReloadFixture(t, 1983)
	destroying, resume := make(chan struct{}), make(chan struct{})
	live.duringDestroy = func() {
		close(destroying)
		<-resume
	}
	f.mgr.BindLocalExecutor(func(fn func()) error { fn(); return nil })
	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xE3), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-destroying:
	case <-time.After(5 * time.Second):
		t.Fatal("rejected instance was never unloaded")
	}
	f.storage.unreachable.Store(false)
	err := probeNextWriter(t, f)
	assertReloadingSentinel(t, err, "reload while the rejected instance is being destroyed")
	if !errors.Is(err, entity.ErrEntityRemoved) {
		t.Fatalf("err=%v lost the underlying entity.ErrEntityRemoved", err)
	}
	close(resume)
	f.assertReloadedFromAuthority(t, live)
}

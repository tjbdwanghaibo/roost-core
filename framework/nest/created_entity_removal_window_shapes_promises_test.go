package nest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// OPEN-ITEMS B18：RR-20260926-81 的另外两种形状。RR-81 的回归只覆盖“另一个事务撤销新建（revokeCreated）的收尾窗口”，
// 这里验证 Destroy 收尾窗口同样按新建锁冲突处理、不把 ErrEntityRemoved 交给业务：Nest 之外 Destroy X，Destroy 已摘索引、
// 置 removing、释放 X 的锁，正在跑销毁回调（LockManager 条目与 removing 在回调之后才回收）；此时 handler 内新建 X。
// 时序由销毁回调里的同步点钉住，不靠 sleep。可回滚事务得到 ErrLockTimeout + ErrCreatedEntityLockConflict，整条回滚后
// 重新准入；不能回滚的 memory 快路径得到只带 ErrCreatedEntityLockConflict 的冲突错误、不重排（RR-20260926-64）。
//
// 另一种形状（同一 Guard 内嵌套事务自己撤销后外层再建）不是别的持有者的暂时状态，改为确定失败，见
// created_entity_self_revoke_promises_test.go（RR-20260927-21）。

func assertRollbackableCreateConflict(t *testing.T, where string, err error) {
	t.Helper()
	if !errors.Is(err, ErrLockTimeout) || !errors.Is(err, ErrCreatedEntityLockConflict) || errors.Is(err, entity.ErrEntityRemoved) {
		t.Fatalf("%s: Create = %v; want ErrLockTimeout + ErrCreatedEntityLockConflict without ErrEntityRemoved", where, err)
	}
}

func assertNonRollbackCreateConflict(t *testing.T, where string, err error) {
	t.Helper()
	if !errors.Is(err, ErrCreatedEntityLockConflict) || errors.Is(err, ErrLockTimeout) || errors.Is(err, entity.ErrEntityRemoved) {
		t.Fatalf("%s: err = %v; want ErrCreatedEntityLockConflict without ErrLockTimeout / ErrEntityRemoved (RR-64 shape)", where, err)
	}
}

// destroyWindow 在 Nest 之外 Destroy X，并停在 X 的销毁回调里：X 已不在索引、锁已空闲、removing 仍在。
type destroyWindow struct {
	inWindow chan struct{}
	release  chan struct{}
	done     chan error
}

func openDestroyWindow(t *testing.T, manager *entity.EntityManager, access *entity.ManagerAccess, x int64) *destroyWindow {
	t.Helper()
	old, err := access.Create(createParam(x)) // Nest 之外创建并发布
	if err != nil {
		t.Fatal(err)
	}
	w := &destroyWindow{inWindow: make(chan struct{}), release: make(chan struct{}), done: make(chan error, 1)}
	old.Base().SetHooks(nil, func(entity.EntityDestroyReason) {
		close(w.inWindow)
		select {
		case <-w.release:
		case <-time.After(5 * time.Second):
		}
	})
	go func() { w.done <- access.Destroy(context.Background(), old, entity.EntityDestroyReason(0), false) }()
	select {
	case <-w.inWindow:
	case <-time.After(5 * time.Second):
		t.Fatal("Destroy never reached its destroy callbacks")
	}
	if got := manager.Get(x); got != nil {
		t.Fatalf("fixture: X is still indexed inside the destroy window: %v", got)
	}
	if !tryLockElsewhere(old) {
		t.Fatal("fixture: X's lock must be free inside the destroy window")
	}
	return w
}

func (w *destroyWindow) finish(t *testing.T) {
	t.Helper()
	close(w.release)
	select {
	case err := <-w.done:
		if err != nil {
			t.Fatalf("Destroy: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Destroy did not finish after its callbacks were released")
	}
}

func TestHandlerCreateInsideDestroyWindowIsTreatedAsLockConflict(t *testing.T) {
	t.Run("state_strict", func(t *testing.T) {
		manager := entity.NewEntityManager()
		pilots := addPilots(t, manager, 38100, 1)
		access := entity.NewManagerAccess(manager)
		x := mustBuildCastID(t, 38105, entity.EntityCategory(1), createdInScopeKind)
		mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
		attempts := &createAttempts{first: make(chan struct{})}
		name := NewHandlerName("b18_destroy_window_strict")
		mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
			_, err := access.Create(createParam(x))
			attempts.record(err)
			return "ok", err
		}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
		if err := mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mgr.Shutdown(context.Background()) }()

		window := openDestroyWindow(t, manager, access, x)
		out := make(chan requestResult, 1)
		sendRequest(mgr, "b", name, pilots[0], out)
		select {
		case <-attempts.first:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler never ran inside the destroy window")
		}
		window.finish(t)
		var b requestResult
		select {
		case b = <-out:
		case <-time.After(5 * time.Second):
			t.Fatalf("the handler did not finish after the destroy window closed; attempts=%v", attempts.snapshot())
		}
		errs := attempts.snapshot()
		if b.err != nil || b.ret != "ok" || len(errs) < 2 {
			t.Fatalf("reply ret=%v err=%v attempts=%v; want the rollbackable handler rolled back, re-admitted and then ok", b.ret, b.err, errs)
		}
		for i, err := range errs[:len(errs)-1] {
			assertRollbackableCreateConflict(t, fmt.Sprintf("attempt %d", i+1), err)
		}
		if err := errs[len(errs)-1]; err != nil {
			t.Fatalf("last attempt: Create = %v, want nil", err)
		}
		created := manager.Get(x)
		if created == nil || created.IsRemoved() || !tryLockElsewhere(created) {
			t.Fatalf("X after the handler committed: %v (want published and unlocked)", created)
		}
	})

	t.Run("memory", func(t *testing.T) {
		manager := entity.NewEntityManager()
		pilots := addPilots(t, manager, 38110, 1)
		access := entity.NewManagerAccess(manager)
		x := mustBuildCastID(t, 38115, entity.EntityCategory(1), createdInScopeKind)
		mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
		attempts := &createAttempts{first: make(chan struct{})}
		name := NewHandlerName("b18_destroy_window_memory")
		mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			es[0].(*rollbackTestEntity).dao.Value++ // 不能回滚的修改：重排就会重复生效
			_, err := access.Create(createParam(x))
			attempts.record(err)
			return "ok", err
		}, HandlerMeta{})
		if err := mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mgr.Shutdown(context.Background()) }()

		window := openDestroyWindow(t, manager, access, x)
		_, err := mgr.Request(context.Background(), name, pilots[0], nil)
		window.finish(t)
		assertNonRollbackCreateConflict(t, "memory reply inside the destroy window", err)
		if errs := attempts.snapshot(); len(errs) != 1 {
			t.Fatalf("memory handler was requeued: attempts=%v, want exactly 1", errs)
		}
		if v := manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value; v != 2 {
			t.Fatalf("declared entity value=%d, want 2 (one execution)", v)
		}
		if got := manager.Get(x); got != nil {
			t.Fatalf("X published although the only creator failed: %v", got)
		}
	})
}

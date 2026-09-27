package nest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// OPEN-ITEMS B18：RR-20260926-81 的另外两种形状。RR-81 的回归只覆盖“另一个事务撤销新建（revokeCreated）的收尾窗口”，
// 这里验证同一个 removing 分支上的另外两条路径同样按新建锁冲突处理、不把 ErrEntityRemoved 交给业务：
//
//  1. Destroy 收尾窗口：Nest 之外 Destroy X，Destroy 已摘索引、置 removing、释放 X 的锁，正在跑销毁回调
//     （LockManager 条目与 removing 在回调之后才回收）；此时 handler 内新建 X。
//  2. 同一 Guard 的嵌套撤销：handler 内嵌套 RunIsolatedTransaction 新建 X 后失败回滚，撤销收尾挂在同一 Guard 的
//     post-release 上、要等整个 handler 释放才跑；外层随后再新建 X。
//
// 两种形状都由事件顺序钉住（销毁回调里的同步点 / 同一 handler 内的先后），不靠 sleep。可回滚事务得到
// ErrLockTimeout + ErrCreatedEntityLockConflict，整条回滚后重新准入；不能回滚的 memory 快路径得到只带
// ErrCreatedEntityLockConflict 的冲突错误、不重排（RR-20260926-64）。

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
		mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
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
		mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
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

func TestOuterCreateAfterNestedRevokeInSameGuardIsTreatedAsLockConflict(t *testing.T) {
	t.Run("state_strict", func(t *testing.T) {
		manager := entity.NewEntityManager()
		pilots := addPilots(t, manager, 38120, 1)
		access := entity.NewManagerAccess(manager)
		x := mustBuildCastID(t, 38125, entity.EntityCategory(1), createdInScopeKind)
		committer := &recordingCommitter{}
		mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
		nestedBoom := errors.New("nested transaction failed after creating X")
		var runs int
		var outerErrs []error
		name := NewHandlerName("b18_nested_revoke_strict")
		mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
			runs++
			if runs == 1 {
				// 只在第一次执行里走嵌套撤销：重新准入后撤销收尾已在上一个 Guard 释放时完成。
				if _, err := RunIsolatedTransaction(context.Background(), committer, "b18_nested", func() (any, error) {
					if _, err := access.Create(createParam(x)); err != nil {
						return nil, err
					}
					return nil, nestedBoom
				}); !errors.Is(err, nestedBoom) {
					return nil, err
				}
			}
			_, err := access.Create(createParam(x))
			outerErrs = append(outerErrs, err)
			return "ok", err
		}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
		if err := mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mgr.Shutdown(context.Background()) }()

		ret, err := mgr.Request(context.Background(), name, pilots[0], nil)
		if err != nil || ret != "ok" || runs != 2 || len(outerErrs) != 2 {
			t.Fatalf("reply ret=%v err=%v runs=%d outer creates=%v; want one conflict, a re-admission, then ok", ret, err, runs, outerErrs)
		}
		assertRollbackableCreateConflict(t, "outer Create after the nested revoke (first run)", outerErrs[0])
		if outerErrs[1] != nil {
			t.Fatalf("outer Create after re-admission = %v, want nil", outerErrs[1])
		}
		created := manager.Get(x)
		if created == nil || created.IsRemoved() || !tryLockElsewhere(created) {
			t.Fatalf("X after the handler committed: %v (want published and unlocked)", created)
		}
	})

	t.Run("memory", func(t *testing.T) {
		manager := entity.NewEntityManager()
		pilots := addPilots(t, manager, 38130, 1)
		access := entity.NewManagerAccess(manager)
		x := mustBuildCastID(t, 38135, entity.EntityCategory(1), createdInScopeKind)
		committer := &recordingCommitter{}
		mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
		nestedBoom := errors.New("nested transaction failed after creating X")
		var runs int
		var outerErr error
		name := NewHandlerName("b18_nested_revoke_memory")
		mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			runs++
			es[0].(*rollbackTestEntity).dao.Value++
			if _, err := RunIsolatedTransaction(context.Background(), committer, "b18_nested_memory", func() (any, error) {
				if _, err := access.Create(createParam(x)); err != nil {
					return nil, err
				}
				return nil, nestedBoom
			}); !errors.Is(err, nestedBoom) {
				return nil, err
			}
			_, outerErr = access.Create(createParam(x))
			return "ok", outerErr
		}, HandlerMeta{})
		if err := mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mgr.Shutdown(context.Background()) }()

		_, err := mgr.Request(context.Background(), name, pilots[0], nil)
		assertNonRollbackCreateConflict(t, "memory outer Create after the nested revoke", outerErr)
		assertNonRollbackCreateConflict(t, "memory reply", err)
		if runs != 1 {
			t.Fatalf("memory handler ran %d times, want exactly 1 (not requeued)", runs)
		}
		if v := manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value; v != 2 {
			t.Fatalf("declared entity value=%d, want 2 (one execution)", v)
		}
		if got := manager.Get(x); got != nil {
			t.Fatalf("X published although both creations failed: %v", got)
		}
	})
}

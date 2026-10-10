package nest

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

type revokeWindowFixture struct {
	manager  *entity.EntityManager
	access   *entity.ManagerAccess
	mgr      *NestMgr
	pilots   []int64
	x        int64
	inWindow chan struct{}
	releaseA chan struct{}
	aName    HandlerName
	aBoom    error
}

func newRevokeWindowFixture(t *testing.T, unique int64) *revokeWindowFixture {
	t.Helper()
	f := &revokeWindowFixture{
		manager:  entity.NewEntityManager(),
		inWindow: make(chan struct{}),
		releaseA: make(chan struct{}),
		aBoom:    errors.New("boom"),
	}
	f.pilots = addPilots(t, f.manager, unique, 2)
	f.access = entity.NewManagerAccess(f.manager)
	f.x = mustBuildCastID(t, unique+5, entity.EntityCategory(1), createdInScopeKind)
	// A 停在 post-release 里占一个快 worker（测试同步点），B 需要另一个。
	f.mgr = NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}))
	f.aName = NewHandlerName("rr81_create_then_fail")
	f.mgr.MustRegisterHandlerWithMeta(f.aName, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if _, err := f.access.Create(createParam(f.x)); err != nil {
			return nil, err
		}
		// 注册在撤销之前（撤销发生在 handler 返回后的回滚里），所以先于撤销收尾运行。
		entity.CurrentGuardScope().Guard().AppendPostRelease(func() {
			close(f.inWindow)
			select {
			case <-f.releaseA:
			case <-time.After(5 * time.Second):
			}
		})
		return nil, f.aBoom
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	return f
}

// openWindow 发出 A 并等到撤销的中间态：X 的锁已释放、removing 标记仍在。
func (f *revokeWindowFixture) openWindow(t *testing.T) <-chan error {
	t.Helper()
	aErr := make(chan error, 1)
	go func() { _, err := f.mgr.Request(context.Background(), f.aName, f.pilots[0], nil); aErr <- err }()
	select {
	case <-f.inWindow:
	case <-time.After(5 * time.Second):
		t.Fatal("A never reached the revoke window")
	}
	if got := f.manager.Get(f.x); got != nil {
		t.Fatalf("fixture: X is still indexed inside the revoke window: %v", got)
	}
	return aErr
}

// createAttempts 记录 B 每次执行里 Create(X) 的错误；第一次执行结束时关闭 first。
type createAttempts struct {
	mu    sync.Mutex
	errs  []error
	n     atomic.Int64
	first chan struct{}
}

func (c *createAttempts) record(err error) {
	c.mu.Lock()
	c.errs = append(c.errs, err)
	c.mu.Unlock()
	if c.n.Add(1) == 1 {
		close(c.first)
	}
}

func (c *createAttempts) snapshot() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.errs...)
}

func TestHandlerCreateInsideRevokeWindowIsTreatedAsLockConflict(t *testing.T) {
	t.Run("state_strict", func(t *testing.T) {
		f := newRevokeWindowFixture(t, 9970)
		attempts := &createAttempts{first: make(chan struct{})}
		bName := NewHandlerName("rr81_state_strict_create")
		f.mgr.MustRegisterHandlerWithMeta(bName, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
			_, err := f.access.Create(createParam(f.x))
			attempts.record(err)
			if err != nil {
				return nil, err
			}
			return "ok", nil
		}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
		if err := f.mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.mgr.Shutdown(context.Background()) }()

		aErr := f.openWindow(t)
		bOut := make(chan requestResult, 1)
		sendRequest(f.mgr, "b", bName, f.pilots[1], bOut)
		select {
		case <-attempts.first:
		case <-time.After(5 * time.Second):
			t.Fatal("B never ran inside the revoke window")
		}
		close(f.releaseA)
		if err := <-aErr; !errors.Is(err, f.aBoom) {
			t.Fatalf("A: want boom, got %v", err)
		}
		var b requestResult
		select {
		case b = <-bOut:
		case <-time.After(5 * time.Second):
			t.Fatalf("B did not finish within 5s after the revoke window closed; attempts=%v", attempts.snapshot())
		}
		errs := attempts.snapshot()
		if b.err != nil || b.ret != "ok" {
			t.Fatalf("B created X inside another transaction's revoke window: reply ret=%v err=%v (ErrEntityRemoved=%v) attempts=%d; want the rollbackable handler rolled back, re-admitted and then ok",
				b.ret, b.err, errors.Is(b.err, entity.ErrEntityRemoved), len(errs))
		}
		if len(errs) < 2 {
			t.Fatalf("B was not re-admitted after hitting the revoke window: attempts=%v", errs)
		}
		for i, err := range errs[:len(errs)-1] {
			if !errors.Is(err, ErrLockTimeout) || !errors.Is(err, ErrCreatedEntityLockConflict) || errors.Is(err, entity.ErrEntityRemoved) {
				t.Fatalf("B attempt %d: Create inside the revoke window = %v; want ErrLockTimeout + ErrCreatedEntityLockConflict without ErrEntityRemoved", i+1, err)
			}
		}
		if err := errs[len(errs)-1]; err != nil {
			t.Fatalf("B last attempt: Create = %v, want nil", err)
		}
		created := f.manager.Get(f.x)
		if created == nil || created.IsRemoved() {
			t.Fatalf("X is not published after B committed: %v", created)
		}
		if !tryLockElsewhere(created) {
			t.Fatal("X is still locked after B returned")
		}
	})

	t.Run("memory", func(t *testing.T) {
		f := newRevokeWindowFixture(t, 9980)
		attempts := &createAttempts{first: make(chan struct{})}
		bName := NewHandlerName("rr81_memory_create")
		f.mgr.MustRegisterHandlerWithMeta(bName, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			es[0].(*rollbackTestEntity).dao.Value++
			_, err := f.access.Create(createParam(f.x))
			attempts.record(err)
			if err != nil {
				return nil, err
			}
			return "ok", nil
		}, HandlerMeta{})
		if err := f.mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.mgr.Shutdown(context.Background()) }()

		aErr := f.openWindow(t)
		bOut := make(chan requestResult, 1)
		sendRequest(f.mgr, "b", bName, f.pilots[1], bOut)
		select {
		case <-attempts.first:
		case <-time.After(5 * time.Second):
			t.Fatal("B never ran inside the revoke window")
		}
		var b requestResult
		select {
		case b = <-bOut:
		case <-time.After(5 * time.Second):
			t.Fatal("B did not reply while A's revoke was held")
		}
		close(f.releaseA)
		if err := <-aErr; !errors.Is(err, f.aBoom) {
			t.Fatalf("A: want boom, got %v", err)
		}
		errs := attempts.snapshot()
		if errors.Is(b.err, entity.ErrEntityRemoved) || !errors.Is(b.err, ErrCreatedEntityLockConflict) || errors.Is(b.err, ErrLockTimeout) {
			t.Fatalf("memory B inside the revoke window: reply err=%v (ErrEntityRemoved=%v); want ErrCreatedEntityLockConflict without ErrLockTimeout (RR-64 shape)",
				b.err, errors.Is(b.err, entity.ErrEntityRemoved))
		}
		if len(errs) != 1 {
			t.Fatalf("memory B was requeued: attempts=%v, want exactly 1", errs)
		}
		pilot := f.manager.Get(f.pilots[1]).(*rollbackTestEntity)
		if pilot.dao.Value != 2 {
			t.Fatalf("memory B's declared entity value=%d, want 2 (one execution)", pilot.dao.Value)
		}
		if got := f.manager.Get(f.x); got != nil {
			t.Fatalf("X published although both creators failed: %v", got)
		}
	})
}

func TestOuterCreateAfterNestedRevokeInSameGuardFailsDeterministically(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}},
		{"memory", HandlerMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilots := addPilots(t, manager, 38120+int64(i)*10, 1)
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 38125+int64(i)*10, entity.EntityCategory(1), createdInScopeKind)
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			nestedBoom := errors.New("nested transaction failed after creating X")
			var runs int
			var isoErr, outerErr error
			name := NewHandlerName("rr20260927_21_self_revoke_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				runs++
				_, isoErr = RunIsolatedTransaction(context.Background(), committer, "rr20260927_21_iso", func() (any, error) {
					if _, err := access.Create(createParam(x)); err != nil {
						return nil, err
					}
					return nil, nestedBoom
				})
				if _, outerErr = access.Create(createParam(x)); outerErr != nil {
					return nil, outerErr
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			_, err := mgr.Request(context.Background(), name, pilots[0], nil)
			if !errors.Is(isoErr, nestedBoom) {
				t.Fatalf("fixture: nested transaction err=%v, want its own failure", isoErr)
			}
			if runs != 1 {
				t.Fatalf("%s handler ran %d times: a create that collides with this handler's own revoke was requeued (reply %v)", tc.name, runs, err)
			}
			for where, got := range map[string]error{"outer Create": outerErr, "reply": err} {
				if !errors.Is(got, entity.ErrEntityRemoved) || errors.Is(got, ErrLockTimeout) || errors.Is(got, ErrCreatedEntityLockConflict) {
					t.Fatalf("%s: %s = %v; want errors.Is entity.ErrEntityRemoved without ErrLockTimeout / ErrCreatedEntityLockConflict", tc.name, where, got)
				}
				if msg := got.Error(); !strings.Contains(msg, "revoked earlier in this handler") || strings.Contains(msg, "another holder") {
					t.Fatalf("%s: %s text %q must say the id was revoked earlier in this handler, not blame another holder", tc.name, where, msg)
				}
			}
			if got := manager.Get(x); got != nil {
				t.Fatalf("X published although both creations failed: %v", got)
			}
			// 收尾随 handler 的 Guard 释放完成：同一 ID 之后可以重新创建。
			again, err := access.Create(createParam(x))
			if err != nil || manager.Get(x) != again {
				t.Fatalf("re-create after the handler released: %v", err)
			}
		})
	}
}

// RR-20260928-02（OPEN-ITEMS B43）：本 Guard 的撤销记录按 ID 记、不区分 EntityManager，而 removing 按 EntityManager 记。
// 同一 handler 在 Manager A 上新建 X 后被嵌套事务回滚撤销，随后在 Manager B 上新建同 ID 的 X，而 B 上的 X 正处在别的持有者的
// Destroy 收尾窗口（B.removing[X]、锁空闲）。B 的 removing 与本 handler 在 A 上的撤销无关，是别的持有者尚未交还的暂时状态，
// 承诺仍按 RR-81 给 ErrCreatedEntityLockConflict（不带 ErrEntityRemoved）；修复前按 ID 命中 A 上的撤销记录，误判成
// “本 handler 内已撤销”的确定失败（ErrEntityRemoved）。对照组不在 A 上撤销，两组结论应一致。
func TestCreateInOtherManagerAfterSameIDRevokeKeepsRemovalWindowConflict(t *testing.T) {
	for i, revokeInA := range []bool{false, true} {
		name := map[bool]string{false: "no_revoke_in_A", true: "revoked_in_A"}[revokeInA]
		t.Run(name, func(t *testing.T) {
			offset := int64(i) * 10
			managerA := entity.NewEntityManager()
			pilots := addPilots(t, managerA, 49800+offset, 1)
			accessA := entity.NewManagerAccess(managerA)
			managerB := entity.NewEntityManager()
			accessB := entity.NewManagerAccess(managerB)
			x := mustBuildCastID(t, 49805+offset, entity.EntityCategory(1), createdInScopeKind)
			window := openDestroyWindow(t, managerB, accessB, x) // B 上 X：别的持有者 Destroy 收尾中，removing 在、锁空闲
			defer window.finish(t)

			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(accessA), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			nestedBoom := errors.New("nested transaction failed after creating X in A")
			var isoErr, crossErr error
			var runs int
			handler := NewHandlerName("rr20260928_02_cross_manager_" + name)
			mgr.MustRegisterHandlerWithMeta(handler, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				runs++
				if revokeInA {
					_, isoErr = RunIsolatedTransaction(context.Background(), committer, "rr20260928_02_iso", func() (any, error) {
						if _, err := accessA.Create(createParam(x)); err != nil {
							return nil, err
						}
						return nil, nestedBoom // 独立事务回滚：A 上撤销 X，收尾挂在本 Guard 上
					})
				}
				_, crossErr = accessB.Create(createParam(x))
				return "done", nil
			}, HandlerMeta{}) // memory：不重排，只看这一次的分类
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			if _, err := mgr.Request(context.Background(), handler, pilots[0], nil); err != nil {
				t.Fatalf("request: %v", err)
			}
			if revokeInA && !errors.Is(isoErr, nestedBoom) {
				t.Fatalf("fixture: nested transaction err=%v, want its own failure", isoErr)
			}
			if runs != 1 {
				t.Fatalf("memory handler ran %d times", runs)
			}
			if !errors.Is(crossErr, ErrCreatedEntityLockConflict) || errors.Is(crossErr, entity.ErrEntityRemoved) {
				t.Fatalf("creating X in manager B inside another holder's destroy window returned %v; want the RR-81 conflict (ErrCreatedEntityLockConflict, no ErrEntityRemoved) — a revoke of the same id in manager A must not count", crossErr)
			}
			if strings.Contains(crossErr.Error(), "revoked earlier in this handler") {
				t.Fatalf("B.Create text %q blames a revoke that happened in another manager", crossErr)
			}
		})
	}
}

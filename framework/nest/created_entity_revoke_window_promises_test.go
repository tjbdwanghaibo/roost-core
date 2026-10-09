package nest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-81：handler 内新建的实体被事务撤销发布（RR-35）时，撤销分两段：锁内摘索引、置 removing 标记；
// Guard 释放全部锁之后的 post-release 里才跑销毁回调、回收 LockManager 条目、清除 removing。两段之间锁已经空闲，
// 另一个 handler 新建同 ID 能取到锁，TryAdd 却撞上 removing，得到 ErrEntityRemoved（“is being removed”）——
// 修前这是不重排的最终业务错误，RR-48 的交叉创建回归因此偶发失败。
//
// 承诺：Nest handler 内新建撞上撤销 / 销毁收尾中的同 ID，与 RR-48 的新建锁冲突同样处理——可回滚事务得到
// ErrLockTimeout + ErrCreatedEntityLockConflict，整条回滚后重新准入；不能回滚的 handler 按 RR-64 得到只带
// ErrCreatedEntityLockConflict 的冲突错误、不重排。两者都不再把 ErrEntityRemoved 交给业务 / 调用方。
//
// 时序由事件顺序钉住，不靠 sleep：A 在 strict handler 内新建 X 后失败，事务撤销发布；A 的 Guard 释放锁后、
// 撤销收尾（它的 post-release 排在 A 自己注册的回调之后）之前，A 注册的 post-release 停住——此刻 X 的锁空闲、
// removing 仍在。B 在这个窗口里第一次执行、新建 X；B 第一次执行结束后测试才放行 A 的收尾。

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

package nest

// RR-20261006-12：派发取锁只用调用方 Guard 作用域里的 Guard，取锁 / 组迁移重试 / 释放都不把 Guard 归还池。
//
// 旧行为：releaseDispatchLocks 在没有 Guard 作用域时调用 entity.EntityGuardRelease，把调用方传入的 Guard 整个放回池。
// lockDispatchEntitiesForHandlerWithStore 校验到锁组在取锁期间变了，会先 releaseLocks()（第一次归还）再拿同一个 Guard
// 重试，成功后调用方 releaseLocks()（第二次归还）——sync.Pool 里同一个指针两份，之后两个 NewGuardScope 取到同一个
// Guard，互相解对方的实体锁（收尾第 4 批 A2、W-2026-10-06-01）。修后取锁要求 Guard 属于当前 goroutine 的作用域，
// 否则不取任何锁、返回 errDispatchWithoutGuardScope；释放只放本次取得的实体锁，Guard 只由作用域结束时归还一次。

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// groupFlipMutex 第一次 Lock 成功后改实体的锁组：确定地落在“取锁之后、校验快照之前”，触发组迁移重试，不靠 sleep。
type groupFlipMutex struct {
	lock.Mutex
	once sync.Once
	flip func()
}

func (m *groupFlipMutex) Lock() {
	m.Mutex.Lock()
	m.once.Do(m.flip)
}

type groupFlipEntity struct {
	*mockEntity
	mu *groupFlipMutex
}

func (e *groupFlipEntity) GetMutex() lock.Mutex { return e.mu }

// newGroupFlipEntity 建一个初始不在锁组里的实体；第一次被 Lock 后它搬到 newGroup。
// uniqueID 4251～4259 只给本文件用（同包其他用例的 42xx 号见 group_lock_test.go）。
func newGroupFlipEntity(t *testing.T, uniqueID int64, newGroup int64) *groupFlipEntity {
	t.Helper()
	base := newMockEntity(mustBuildCastID(t, uniqueID, entity.EntityCategory(1), nestLocalKind), entity.EntityCategory(1))
	e := &groupFlipEntity{mockEntity: base}
	e.mu = &groupFlipMutex{Mutex: base.GetMutex(), flip: func() { base.Base().SetGroupLockIDForTest(newGroup) }}
	return e
}

func TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool(t *testing.T) {
	if entity.CurrentGuardScope() != nil {
		t.Fatal("test goroutine unexpectedly has a guard scope")
	}
	locks := newEntityLockGroupLockManager()
	e := newGroupFlipEntity(t, 4251, 9105)

	// 没有作用域：GetEntityGuard 从池里取一个独立 Guard，归还的责任在取得它的调用方。
	guard := entity.GetEntityGuard()
	var returned atomic.Int32
	countReturn := func() { returned.Add(1) }
	guard.AppendPostRelease(countReturn) // Guard 每被归还池一次，挂在上面的回调就跑一次（归还时清空）

	_, releaseLocks, err := lockDispatchEntitiesForHandler(locks, guard, []entity.IThreadSafeEntity{e})
	if err == nil {
		guard.AppendPostRelease(countReturn)
		releaseLocks()
	}
	if n := returned.Load(); n != 0 {
		t.Fatalf("dispatch locking returned the caller's guard to the pool %d time(s) during a lock-group retry (err=%v); "+
			"the guard belongs to its scope / caller and must be returned exactly once, by its owner", n, err)
	}
	if !errors.Is(err, errDispatchWithoutGuardScope) {
		t.Fatalf("lockDispatchEntitiesForHandler without a guard scope err = %v, want errDispatchWithoutGuardScope", err)
	}
	if !e.mu.Mutex.TryLock() {
		t.Fatal("refused dispatch locking still holds the entity lock")
	}
	e.mu.Mutex.Unlock()
	if CurrentEntityLockGroup() != nil {
		t.Fatal("refused dispatch locking leaked a lock-group scope")
	}

	entity.EntityGuardRelease(guard) // 调用方自己归还，正好一次
	if n := returned.Load(); n != 1 {
		t.Fatalf("caller release returned the guard %d time(s), want 1", n)
	}
}

func TestDispatchLockingGroupRetryInScopeKeepsGuardUntilScopeEnds(t *testing.T) {
	locks := newEntityLockGroupLockManager()
	e := newGroupFlipEntity(t, 4252, 9106)
	var returned atomic.Int32

	err := entity.WithGuardScope("dispatch-guard-scope-test", func(scope *entity.GuardScope) error {
		guard := scope.Guard()
		guard.AppendPostRelease(func() { returned.Add(1) })
		_, releaseLocks, err := lockDispatchEntitiesForHandler(locks, guard, []entity.IThreadSafeEntity{e})
		if err != nil {
			return err
		}
		if group := CurrentEntityLockGroup(); group == nil || group.GroupID() != 9106 {
			t.Errorf("dispatch did not retry into the new lock group, scope = %v", group)
		}
		releaseLocks()
		if n := returned.Load(); n != 0 {
			t.Errorf("guard returned to the pool %d time(s) before its scope ended", n)
		}
		if entity.GetEntityGuard() != guard {
			t.Error("scope guard changed after dispatch locking")
		}
		if guard.GuardedEntity(e) {
			t.Error("releaseLocks did not release the entity it acquired")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("lockDispatchEntitiesForHandler in scope: %v", err)
	}
	if n := returned.Load(); n != 1 {
		t.Fatalf("scope end returned the guard %d time(s), want 1", n)
	}
	if CurrentEntityLockGroup() != nil {
		t.Fatal("lock-group scope leaked past releaseLocks")
	}
}

// 生产入口（dispatchLoadedEntities、groupTransitionDispatch）都在 runNestLogic 建的作用域里；直接调用而没有作用域
// 是编程错误，取锁前就报错，不取池里的独立 Guard。
func TestDispatchEntriesRequireGuardScope(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)

	getter := newMockGetter()
	id := mustBuildCastID(t, 4253, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.groups.Add(e)
	getter.Add(e)
	name := NewHandlerName("test_dispatch_requires_guard_scope")
	ran := false
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		ran = true
		return "ok", nil
	})
	mgr := &NestMgr{getter: getter}

	if _, err := mgr.singleDispatch(name.String(), id, nil); !errors.Is(err, errDispatchWithoutGuardScope) {
		t.Fatalf("singleDispatch without guard scope err = %v, want errDispatchWithoutGuardScope", err)
	}
	if ran {
		t.Fatal("handler ran without a guard scope")
	}

	if !e.Base().BeginGroupTransition(entity.EntityGroupTransitionJoin, 9107) {
		t.Fatal("BeginGroupTransition should succeed")
	}
	_, err := mgr.groupTransitionDispatch(&GroupTransitionRequest{EntityID: id, TargetGroupID: 9107, State: entity.EntityGroupTransitionJoin})
	if !errors.Is(err, errDispatchWithoutGuardScope) {
		t.Fatalf("groupTransitionDispatch without guard scope err = %v, want errDispatchWithoutGuardScope", err)
	}
	if !e.GetMutex().TryLock() {
		t.Fatal("refused group transition still holds the entity lock")
	}
	e.GetMutex().Unlock()

	// 同样的调用放进作用域里照常执行。
	e.Base().ClearGroupTransition()
	err = entity.WithGuardScope("dispatch-guard-scope-test", func(*entity.GuardScope) error {
		_, err := mgr.singleDispatch(name.String(), id, nil)
		return err
	})
	if err != nil || !ran {
		t.Fatalf("singleDispatch in guard scope err = %v ran = %v", err, ran)
	}
}

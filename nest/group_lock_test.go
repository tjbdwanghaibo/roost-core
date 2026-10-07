package nest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestEntityLockGroupScopeAvailableForGroupedDispatch(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	groupID := int64(7001)
	id1 := mustBuildCastID(t, 4101, entity.EntityCategory(1), nestLocalKind)
	id2 := mustBuildCastID(t, 4102, entity.EntityCategory(1), nestLocalKind)
	idOtherGroup := mustBuildCastID(t, 4103, entity.EntityCategory(1), nestLocalKind)
	e1 := newMockEntity(id1, entity.EntityCategory(1))
	e2 := newMockEntity(id2, entity.EntityCategory(1))
	otherGroup := newMockEntity(idOtherGroup, entity.EntityCategory(1))
	e1.Base().SetGroupLockIDForTest(groupID)
	e2.Base().SetGroupLockIDForTest(groupID)
	otherGroup.Base().SetGroupLockIDForTest(groupID + 1)
	getter.groups.Add(e1)
	getter.groups.Add(e2)
	getter.groups.Add(otherGroup)
	getter.Add(e1)
	getter.Add(e2)
	getter.Add(otherGroup)

	name := NewHandlerName("test_entity_lock_group_scope_available")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		scope := CurrentEntityLockGroup()
		if scope == nil {
			return nil, errors.New("missing group scope")
		}
		if scope.GroupID() != groupID {
			return nil, errors.New("wrong group scope id")
		}
		got, ok := GroupEntityAs[*mockEntity](scope, id2)
		if !ok || got != e2 {
			return nil, errors.New("same group entity lookup failed")
		}
		if got, ok := GroupEntityAs[*mockEntity](scope, idOtherGroup); ok || got != nil {
			return nil, errors.New("different group entity should not be returned")
		}
		if len(es) != 1 || es[0] != e1 {
			return nil, errors.New("dispatch target mismatch")
		}
		return "ok", nil
	})

	mgr := &NestMgr{getter: getter}
	got, err := singleDispatchInGuardScope(mgr, name.String(), id1)
	if err != nil {
		t.Fatalf("singleDispatch: %v", err)
	}
	if got != "ok" {
		t.Fatalf("result = %v, want ok", got)
	}
	if CurrentEntityLockGroup() != nil {
		t.Fatal("group scope should be cleared after dispatch")
	}
}

func TestEntityLockGroupScopeNilForNormalDispatch(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 4201, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.Add(e)

	name := NewHandlerName("test_entity_lock_group_scope_nil")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if CurrentEntityLockGroup() != nil {
			return nil, errors.New("normal dispatch should not have group scope")
		}
		if len(es) != 1 || es[0] != e {
			return nil, errors.New("dispatch target mismatch")
		}
		return "ok", nil
	})

	mgr := &NestMgr{getter: getter}
	got, err := singleDispatchInGuardScope(mgr, name.String(), id)
	if err != nil {
		t.Fatalf("singleDispatch: %v", err)
	}
	if got != "ok" {
		t.Fatalf("result = %v, want ok", got)
	}
}

func TestEntityLockGroupSnapshotDetectsMembershipChange(t *testing.T) {
	id := mustBuildCastID(t, 4211, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	snapshots := captureDispatchGroupSnapshots([]entity.IThreadSafeEntity{e})

	e.Base().SetGroupLockIDForTest(9101)

	if err := validateDispatchGroupSnapshots(snapshots); !errors.Is(err, ErrEntityLockGroupChanged) {
		t.Fatalf("validateDispatchGroupSnapshots err = %v, want ErrEntityLockGroupChanged", err)
	}
}

func TestEntityLockGroupSnapshotDetectsPendingTransition(t *testing.T) {
	id := mustBuildCastID(t, 4212, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	snapshots := captureDispatchGroupSnapshots([]entity.IThreadSafeEntity{e})

	if !e.Base().BeginGroupTransition(entity.EntityGroupTransitionJoin, 9102) {
		t.Fatal("BeginGroupTransition should succeed")
	}

	if err := validateDispatchGroupSnapshots(snapshots); !errors.Is(err, ErrEntityGroupTransitionPending) {
		t.Fatalf("validateDispatchGroupSnapshots err = %v, want ErrEntityGroupTransitionPending", err)
	}
}

// singleDispatchInGuardScope 在当前 goroutine 上建 Guard 作用域再直接调用 singleDispatch，与 runNestLogic 一致：
// 派发取锁要求作用域（RR-20261006-12）。
func singleDispatchInGuardScope(mgr *NestMgr, name string, id int64) (got any, err error) {
	err = entity.WithGuardScope("group-lock-test", func(*entity.GuardScope) error {
		var dispatchErr error
		got, dispatchErr = mgr.singleDispatch(name, id, nil)
		return dispatchErr
	})
	return got, err
}

// withDispatchGuardScope 在当前 goroutine 上建 Guard 作用域再取锁，与快池派发（runNestLogic 先 NewGuardScope、
// dispatchLoadedEntities 再取锁）一致：Guard 归作用域所有，作用域结束时只归还池一次。
//
// 之前这几条用例在新 goroutine 里直接 entity.GetEntityGuard()：没有作用域时它从池里取一个独立 Guard，
// releaseDispatchLocks 的无作用域分支每次释放都把它 EntityGuardRelease 回池。组迁移重试会先释放一次、再拿同一个
// Guard 重新取锁并再释放一次，同一个 Guard 进池两次；之后同进程里两个快 worker 会取到同一个 Guard，互相解对方
// goroutine 持有的实体锁（“unlock of unowned mutex”），-shuffle 下排在后面的
// TestSymmetricCrossCreatePairsResolveWithinRequeueBudget 出现 winners=0、sync timeout 或停机挂死。
// 生产调用方都在 Guard 作用域里，不走那条分支（见 docs/bugfix/CLOSING-BATCH-4-2026-10-06.md A2）。之后 RR-20261006-12 删掉了
// 这条分支：没有作用域时派发取锁直接返回 errDispatchWithoutGuardScope，Guard 只由作用域归还。
// glsvet -tests 也因此不再报 “GetEntityGuard called inside a go statement”。
func withDispatchGuardScope(fn func(guard *entity.EntityGuard) error) error {
	return entity.WithGuardScope("group-lock-test", func(scope *entity.GuardScope) error {
		return fn(scope.Guard())
	})
}

func TestLockDispatchEntitiesForHandlerRetriesEpochChangeWhileWaiting(t *testing.T) {
	locks := newEntityLockGroupLockManager()
	id := mustBuildCastID(t, 4213, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	mu := e.GetMutex()
	mu.Lock()

	done := make(chan error, 1)
	go func() {
		done <- withDispatchGuardScope(func(guard *entity.EntityGuard) error {
			_, releaseLocks, err := lockDispatchEntitiesForHandler(locks, guard, []entity.IThreadSafeEntity{e})
			if err != nil {
				return err
			}
			defer releaseLocks()
			if scope := CurrentEntityLockGroup(); scope == nil || scope.GroupID() != 9103 {
				return errors.New("dispatch did not retry into the new group scope")
			}
			return nil
		})
	}()

	time.Sleep(20 * time.Millisecond)
	e.Base().SetGroupLockIDForTest(9103)
	mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("lockDispatchEntitiesForHandler: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lockDispatchEntitiesForHandler did not return")
	}
	if CurrentEntityLockGroup() != nil {
		t.Fatal("group scope should not leak to caller goroutine")
	}
}

func TestLockDispatchEntitiesForHandlerDoesNotBlockGroupOnBusyExtraEntity(t *testing.T) {
	locks := newEntityLockGroupLockManager()
	groupID := int64(9201)
	groupedID := mustBuildCastID(t, 4221, entity.EntityCategory(1), nestLocalKind)
	extraID := mustBuildCastID(t, 4222, entity.EntityCategory(1), nestLocalKind)
	grouped := newMockEntity(groupedID, entity.EntityCategory(1))
	extra := newMockEntity(extraID, entity.EntityCategory(1))
	grouped.Base().SetGroupLockIDForTest(groupID)

	extraLocked := make(chan struct{})
	releaseExtra := make(chan struct{})
	go func() {
		extra.GetMutex().Lock()
		close(extraLocked)
		<-releaseExtra
		extra.GetMutex().Unlock()
	}()
	<-extraLocked
	defer close(releaseExtra)

	done := make(chan error, 1)
	go func() {
		done <- withDispatchGuardScope(func(guard *entity.EntityGuard) error {
			_, releaseLocks, err := lockDispatchEntitiesForHandler(locks, guard, []entity.IThreadSafeEntity{grouped, extra})
			if err == nil {
				releaseLocks()
			}
			return err
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("lockDispatchEntitiesForHandler err = %v, want ErrLockTimeout", err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("lockDispatchEntitiesForHandler blocked while holding group lock")
	}
}

func TestLockDispatchEntitiesForHandlerDoesNotBlockOnBusyGroupLock(t *testing.T) {
	locks := newEntityLockGroupLockManager()
	groupID := int64(9203)
	groupedID := mustBuildCastID(t, 4241, entity.EntityCategory(1), nestLocalKind)
	grouped := newMockEntity(groupedID, entity.EntityCategory(1))
	grouped.Base().SetGroupLockIDForTest(groupID)

	groupLocked := make(chan struct{})
	releaseGroup := make(chan struct{})
	go func() {
		entry, ok := locks.acquire(groupID)
		if !ok {
			t.Error("acquire group lock")
			return
		}
		close(groupLocked)
		<-releaseGroup
		locks.release(entry)
	}()
	<-groupLocked
	defer close(releaseGroup)

	done := make(chan error, 1)
	go func() {
		done <- withDispatchGuardScope(func(guard *entity.EntityGuard) error {
			_, releaseLocks, err := lockDispatchEntitiesForHandler(locks, guard, []entity.IThreadSafeEntity{grouped})
			if err == nil {
				releaseLocks()
			}
			return err
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("lockDispatchEntitiesForHandler err = %v, want ErrLockTimeout", err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("lockDispatchEntitiesForHandler blocked on busy group lock")
	}
}

func TestEntityLockGroupDispatchRequeuesBusyGroupLock(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	groupID := int64(9204)
	groupedID := mustBuildCastID(t, 4242, entity.EntityCategory(1), nestLocalKind)
	grouped := newMockEntity(groupedID, entity.EntityCategory(1))
	grouped.Base().SetGroupLockIDForTest(groupID)
	getter.Add(grouped)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerNumAndMsgCap(1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_entity_lock_group_requeue_busy_group")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if len(es) != 1 || es[0] != grouped {
			return nil, errors.New("dispatch entity mismatch")
		}
		scope := CurrentEntityLockGroup()
		if scope == nil || scope.GroupID() != groupID {
			return nil, errors.New("missing group scope")
		}
		return "ok", nil
	})

	groupLocked := make(chan struct{})
	releaseGroup := make(chan struct{})
	go func() {
		entry, ok := Nest.groupLocks.acquire(groupID)
		if !ok {
			t.Error("acquire group lock")
			return
		}
		close(groupLocked)
		<-releaseGroup
		Nest.groupLocks.release(entry)
	}()
	<-groupLocked

	type syncResult struct {
		ret any
		err error
	}
	done := make(chan syncResult, 1)
	go func() {
		ret, err := Nest.Request(context.Background(), name, groupedID, nil)
		done <- syncResult{ret: ret, err: err}
	}()

	select {
	case got := <-done:
		close(releaseGroup)
		t.Fatalf("Sync returned before group lock released: ret=%v err=%v", got.ret, got.err)
	case <-time.After(30 * time.Millisecond):
	}

	close(releaseGroup)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Sync err = %v, want nil", got.err)
		}
		if got.ret != "ok" {
			t.Fatalf("Sync ret = %v, want ok", got.ret)
		}
	case <-time.After(time.Second):
		t.Fatal("Sync did not finish after group lock released")
	}
}

func TestEntityLockGroupDispatchRequeuesBusyExtraEntity(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	groupID := int64(9202)
	groupedID := mustBuildCastID(t, 4231, entity.EntityCategory(1), nestLocalKind)
	extraID := mustBuildCastID(t, 4232, entity.EntityCategory(1), nestLocalKind)
	grouped := newMockEntity(groupedID, entity.EntityCategory(1))
	extra := newMockEntity(extraID, entity.EntityCategory(1))
	grouped.Base().SetGroupLockIDForTest(groupID)
	getter.Add(grouped)
	getter.Add(extra)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerNumAndMsgCap(1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_entity_lock_group_requeue_busy_extra")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if len(es) != 2 || es[0] != grouped || es[1] != extra {
			return nil, errors.New("dispatch entities mismatch")
		}
		scope := CurrentEntityLockGroup()
		if scope == nil || scope.GroupID() != groupID {
			return nil, errors.New("missing group scope")
		}
		return "ok", nil
	})

	extraLocked := make(chan struct{})
	releaseExtra := make(chan struct{})
	go func() {
		extra.GetMutex().Lock()
		close(extraLocked)
		<-releaseExtra
		extra.GetMutex().Unlock()
	}()
	<-extraLocked

	type syncResult struct {
		ret any
		err error
	}
	done := make(chan syncResult, 1)
	go func() {
		ret, err := Nest.RequestMulti(context.Background(), name, []int64{groupedID, extraID}, nil)
		done <- syncResult{ret: ret, err: err}
	}()

	select {
	case got := <-done:
		close(releaseExtra)
		t.Fatalf("MultiSync returned before extra lock released: ret=%v err=%v", got.ret, got.err)
	case <-time.After(30 * time.Millisecond):
	}

	close(releaseExtra)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("MultiSync err = %v, want nil", got.err)
		}
		if got.ret != "ok" {
			t.Fatalf("MultiSync ret = %v, want ok", got.ret)
		}
	case <-time.After(time.Second):
		t.Fatal("MultiSync did not finish after extra lock released")
	}
}

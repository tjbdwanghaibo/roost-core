// 实体锁组：分组派发的组作用域与快照校验、组锁 / 额外实体忙时不阻塞而重排、组迁移请求的准入与执行。

package nest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
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
	snapshots := captureDispatchGroupSnapshots([]entity.IThreadSafeEntity{e}, nil)

	e.Base().SetGroupLockIDForTest(9101)

	if err := validateDispatchGroupSnapshots(snapshots); !errors.Is(err, ErrEntityLockGroupChanged) {
		t.Fatalf("validateDispatchGroupSnapshots err = %v, want ErrEntityLockGroupChanged", err)
	}
}

func TestEntityLockGroupSnapshotDetectsPendingTransition(t *testing.T) {
	id := mustBuildCastID(t, 4212, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	snapshots := captureDispatchGroupSnapshots([]entity.IThreadSafeEntity{e}, nil)

	if !e.Base().BeginGroupTransition(entity.EntityGroupTransitionJoin, 9102) {
		t.Fatal("BeginGroupTransition should succeed")
	}

	if err := validateDispatchGroupSnapshots(snapshots); !errors.Is(err, ErrEntityGroupTransitionPending) {
		t.Fatalf("validateDispatchGroupSnapshots err = %v, want ErrEntityGroupTransitionPending", err)
	}
}

// singleDispatchInGuardScope 在当前 goroutine 上建 Guard 作用域再直接调用 singleDispatch，与 runNestLogic 一致：
// 派发取锁要求作用域。
func singleDispatchInGuardScope(mgr *NestMgr, name string, id int64) (got any, err error) {
	err = entity.WithGuardScope("group-lock-test", func(*entity.GuardScope) error {
		var dispatchErr error
		got, dispatchErr = mgr.singleDispatch(name, id, nil)
		return dispatchErr
	})
	return got, err
}

// withDispatchGuardScope 在当前 goroutine 上建 Guard 作用域再取锁，与业务池派发（runNestLogic 先 NewGuardScope、
// dispatchLoadedEntities 再取锁）一致：Guard 归作用域所有，作用域结束时只归还池一次。没有作用域时派发取锁直接返回
// errDispatchWithoutGuardScope（见 dispatch_test.go），所以用例不能在裸 goroutine 里直接 GetEntityGuard。
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
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
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
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
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

func TestGroupReleasePanicStillReleasesGroupLockAndScope(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "normal-release"
		if partial {
			name = "partial-acquire"
		}
		t.Run(name, func(t *testing.T) {
			testGroupReleasePanic(t, partial)
		})
	}
}

func testGroupReleasePanic(t *testing.T, partial bool) {
	id := mustBuildCastID(t, 9810, entity.EntityCategory(1), nestLocalKind)
	ent := newMockEntity(id, entity.EntityCategory(1))
	manager := entity.NewEntityManager()
	if err := manager.TryAdd(ent); err != nil {
		t.Fatal(err)
	}
	defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { panic("release hook failed") })()
	scope, closeScope := entity.NewGuardScope("group-release-panic")
	defer closeScope()
	locks := newEntityLockGroupLockManager()
	entities := []entity.IThreadSafeEntity{ent}
	if partial {
		busyID := mustBuildCastID(t, 9811, entity.EntityCategory(1), nestLocalKind)
		busy := newMockEntity(busyID, entity.EntityCategory(1))
		locked, unlock, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			busy.GetMutex().Lock()
			close(locked)
			<-unlock
			busy.GetMutex().Unlock()
			close(done)
		}()
		<-locked
		defer func() { close(unlock); <-done }()
		entities = append(entities, busy)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, release, err := lockDispatchEntitiesWithGroup(locks, scope.Guard(), entities, 9800, nil)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		release()
	}()
	if recovered == nil {
		t.Fatal("release panic should reach the dispatch recovery boundary")
	}
	if group := CurrentEntityLockGroup(); group != nil {
		// 先清理测试线程的残留 scope，避免影响其它测试。
		entityLockGroupScopes.Delete(goroutine.GoID())
		t.Errorf("group scope leaked: %d", group.GroupID())
	}
	if scope.Guard().Guarded(id) {
		t.Error("entity remained guarded")
	}
	locks.mu.Lock()
	remaining := len(locks.locks)
	locks.mu.Unlock()
	if remaining != 0 {
		t.Errorf("group lock entries leaked: %d", remaining)
	}
}

// Lock-group transitions are requested by entity id; the guards in front of
// the request are what keep a zero group id, an unknown entity, or a second
// concurrent request from reaching the dispatcher. Each is pinned.
func TestGroupTransitionRequestsRefuseZeroGroupsUnknownEntitiesAndOverlaps(t *testing.T) {
	const kind entity.EntityKind = 171
	const category entity.EntityCategory = 1
	entity.MustRegisterEntityKindCategory(kind, category)
	getter := newMockGetter()
	mgr := NewEngine(NestOptionWithGetter(getter))
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(nil) })

	id, err := entity.BuildEntityID(9001, kind)
	if err != nil {
		t.Fatal(err)
	}
	ent := newMockEntityWithKind(id, category, kind)
	getter.Add(ent)
	missing, err := entity.BuildEntityID(9002, kind)
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.RequestJoinEntityLockGroup(id, 0); !errors.Is(err, ErrInvalidEntityLockGroup) {
		t.Fatalf("join group 0 = %v", err)
	}
	if err := mgr.RequestMoveEntityLockGroup(id, 0); !errors.Is(err, ErrInvalidEntityLockGroup) {
		t.Fatalf("move to group 0 = %v", err)
	}
	if err := mgr.RequestJoinEntityLockGroup(missing, 5); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("join for an entity the getter does not know = %v", err)
	}
	if ent.Base().GroupTransitionPending() {
		t.Fatal("refused requests must not leave a transition pending on the entity")
	}

	// Hold the entity's lock so the dispatched transition cannot complete:
	// under the race detector the worker otherwise finishes the join before
	// the second request is issued, and "pending" is no longer observable.
	mu := ent.GetMutex()
	mu.Lock()
	t.Cleanup(mu.Unlock)
	if err := mgr.RequestJoinEntityLockGroup(id, 5); err != nil {
		t.Fatalf("first join request: %v", err)
	}
	if !ent.Base().GroupTransitionPending() {
		t.Fatal("the first join must be pending while the entity lock is held")
	}
	if err := mgr.RequestMoveEntityLockGroup(id, 6); !errors.Is(err, ErrEntityGroupTransitionPending) {
		t.Fatalf("second request while the first is pending = %v", err)
	}
	if err := mgr.RequestLeaveEntityLockGroup(id); !errors.Is(err, ErrEntityGroupTransitionPending) {
		t.Fatalf("leave while a join is pending = %v", err)
	}
}

func TestEntityLockGroupTransitionJoinMoveLeaveUpdatesState(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 4301, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.groups.Add(e)
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	if err := Nest.RequestJoinEntityLockGroup(id, 8101); err != nil {
		t.Fatalf("RequestJoinEntityLockGroup: %v", err)
	}
	waitForNestCondition(t, func() bool {
		return e.Base().GroupLockID() == 8101 &&
			e.Base().GroupEpoch() == 1 &&
			!e.Base().GroupTransitionPending() &&
			getter.groups.GetGroupEntity(8101, id) == e
	})

	if err := Nest.RequestMoveEntityLockGroup(id, 8102); err != nil {
		t.Fatalf("RequestMoveEntityLockGroup: %v", err)
	}
	waitForNestCondition(t, func() bool {
		return e.Base().GroupLockID() == 8102 &&
			e.Base().GroupEpoch() == 2 &&
			!e.Base().GroupTransitionPending() &&
			getter.groups.GetGroupEntity(8101, id) == nil &&
			getter.groups.GetGroupEntity(8102, id) == e
	})

	if err := Nest.RequestLeaveEntityLockGroup(id); err != nil {
		t.Fatalf("RequestLeaveEntityLockGroup: %v", err)
	}
	waitForNestCondition(t, func() bool {
		return e.Base().GroupLockID() == 0 &&
			e.Base().GroupEpoch() == 3 &&
			!e.Base().GroupTransitionPending() &&
			getter.groups.GetGroupEntity(8102, id) == nil
	})
}

func TestEntityLockGroupTransitionPendingGatesNormalDispatch(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 4401, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	if !e.Base().BeginGroupTransition(entity.EntityGroupTransitionJoin, 8201) {
		t.Fatal("BeginGroupTransition should succeed")
	}
	getter.Add(e)

	name := NewHandlerName("test_group_transition_pending_gate")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return nil, errors.New("handler should not run while transition is pending")
	})

	mgr := &NestMgr{getter: getter}
	_, err := mgr.singleDispatch(name.String(), id, nil)
	if !errors.Is(err, ErrEntityGroupTransitionPending) {
		t.Fatalf("singleDispatch err = %v, want ErrEntityGroupTransitionPending", err)
	}
}

func TestEntityLockGroupTransitionPendingRequeuesSyncDispatch(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 4451, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	if !e.Base().BeginGroupTransition(entity.EntityGroupTransitionJoin, 8251) {
		t.Fatal("BeginGroupTransition should succeed")
	}
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_group_transition_pending_requeue_sync")
	called := make(chan struct{}, 1)
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		called <- struct{}{}
		return "ok", nil
	})

	time.AfterFunc(30*time.Millisecond, func() {
		e.Base().ClearGroupTransition()
	})

	got, err := Nest.Request(context.Background(), name, id, nil)
	if err != nil {
		t.Fatalf("Nest.Sync err = %v, want nil", err)
	}
	if got != "ok" {
		t.Fatalf("Nest.Sync result = %v, want ok", got)
	}
	select {
	case <-called:
	default:
		t.Fatal("handler should run after pending clears")
	}
}

func TestEntityLockGroupTransitionContinuationReturnsSyncResult(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 4501, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.groups.Add(e)
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	startName := NewHandlerName("test_group_transition_continuation_start")
	continuationName := NewHandlerName("test_group_transition_continuation_after_join")
	MustRegisterMemoryHandler(startName, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if err := Nest.RequestJoinEntityLockGroup(
			id,
			8301,
			GroupTransitionWithContinuation(continuationName, NewParams("ready")),
		); err != nil {
			return nil, err
		}
		return nil, ErrEntityGroupTransitionScheduled
	})
	MustRegisterMemoryHandler(continuationName, func(es []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
		if len(params) != 1 || params[0] != "ready" {
			return nil, errors.New("continuation params mismatch")
		}
		if len(es) != 1 || es[0] != e {
			return nil, errors.New("continuation entity mismatch")
		}
		scope := CurrentEntityLockGroup()
		if scope == nil || scope.GroupID() != 8301 {
			return nil, errors.New("continuation did not run under target group")
		}
		return "joined", nil
	})

	got, err := Nest.Request(context.Background(), startName, id, nil)
	if err != nil {
		t.Fatalf("Nest.Sync: %v", err)
	}
	if got != "joined" {
		t.Fatalf("sync result = %v, want joined", got)
	}
	if e.Base().GroupLockID() != 8301 {
		t.Fatalf("group id = %d, want 8301", e.Base().GroupLockID())
	}
}

func TestEntityLockGroupTransitionRetriesWhenEntityLockBusy(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	id := mustBuildCastID(t, 4601, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.groups.Add(e)
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	mu := e.GetMutex()
	mu.Lock()
	if err := Nest.RequestJoinEntityLockGroup(id, 8401); err != nil {
		mu.Unlock()
		t.Fatalf("RequestJoinEntityLockGroup: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if !e.Base().GroupTransitionPending() {
		mu.Unlock()
		t.Fatal("transition should remain pending while entity lock is busy")
	}
	mu.Unlock()

	waitForNestCondition(t, func() bool {
		return e.Base().GroupLockID() == 8401 &&
			!e.Base().GroupTransitionPending() &&
			getter.groups.GetGroupEntity(8401, id) == e
	})
}

func TestEntityLockGroupTransitionTimeoutClearsPending(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 4602, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.groups.Add(e)
	getter.Add(e)

	if !e.Base().BeginGroupTransition(entity.EntityGroupTransitionJoin, 8402) {
		t.Fatal("BeginGroupTransition should succeed")
	}

	mgr := &NestMgr{getter: getter}
	mu := e.GetMutex()
	locked := make(chan struct{})
	release := make(chan struct{})
	go func() {
		mu.Lock()
		close(locked)
		<-release
		mu.Unlock()
	}()
	awaitChan(t, locked, "the group transition to acquire the entity lock")
	// 派发取锁要求 Guard 作用域（RR-20261006-12），与 runNestLogic 一致。
	err := entity.WithGuardScope("group-transition-test", func(*entity.GuardScope) error {
		_, err := mgr.groupTransitionDispatch(&GroupTransitionRequest{
			EntityID:      id,
			TargetGroupID: 8402,
			State:         entity.EntityGroupTransitionJoin,
			Attempts:      entityGroupTransitionRetryMax,
			Deadline:      time.Now().Add(-time.Millisecond),
		})
		return err
	})
	close(release)

	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("groupTransitionDispatch err = %v, want ErrLockTimeout", err)
	}
	if e.Base().GroupTransitionPending() {
		t.Fatal("transition pending should be cleared after timeout")
	}
}

func TestRequeueTransientDispatchHandlesGroupChanged(t *testing.T) {
	dispatcher := NewDispatcher("test_group_changed_requeue", 1, 16, nil)
	dispatcher.OnInit()
	defer dispatcher.OnDestroy()

	mgr := &NestMgr{dispatcher: dispatcher}
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name = "test_group_changed_requeue"
	msg.Tid = 4701

	if !requeueTransientDispatch(mgr, msg, ErrEntityLockGroupChanged) {
		t.Fatal("ErrEntityLockGroupChanged should be requeued as a transient dispatch error")
	}
	if msg.RetChan != nil {
		t.Fatal("original message RetChan should be moved to the requeued clone")
	}
	select {
	case got := <-ch:
		t.Fatalf("requeue should not complete sync channel immediately: %v", got)
	default:
	}
}

func waitForNestCondition(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// awaitChan receives from ch with an upper bound. A bare receive made a broken
// property fail as a go test timeout — a stack dump after the default ten
// minutes, naming no expectation — so every wait that IS the assertion is
// bounded and says what it was waiting for.
func awaitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func TestCurrentDispatchMessageNestedScopes(t *testing.T) {
	first, second := &Msg{Name: "first"}, &Msg{Name: "second"}
	func() {
		release := pushCurrentNestDispatchMsg(first)
		defer release()
		func() {
			same := pushCurrentNestDispatchMsg(first)
			defer same()
			func() {
				defer func() {
					if recover() != "nested" {
						t.Error("missing nested panic")
					}
				}()
				other := pushCurrentNestDispatchMsg(second)
				defer other()
				if currentNestDispatchMsg() != second {
					t.Error("nested identity")
				}
				panic("nested")
			}()
			if currentNestDispatchMsg() != first {
				t.Error("outer identity not restored")
			}
		}()
		if currentNestDispatchMsg() != first {
			t.Error("same-message scope removed outer identity")
		}
	}()
	if currentNestDispatchMsg() != nil {
		t.Fatal("message identity leaked")
	}
}

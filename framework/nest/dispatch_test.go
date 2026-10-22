// 派发：Guard 作用域所有权、参数与目标顺序、缺失实体、多目标首目标必需、handler 内禁止嵌套派发。

package nest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// 派发取锁只用调用方 Guard 作用域里的 Guard：取锁、组迁移重试、释放都不把 Guard 归还池，否则同一个 Guard
// 会被两个作用域取到、互相解对方的实体锁。没有作用域时不取任何锁，返回 errDispatchWithoutGuardScope；
// 释放只放本次取得的实体锁，Guard 只由作用域结束时归还一次。
// 历史：RR-20261006-12。

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

type dispatchLifetimeEntity struct {
	*entity.EntityBase
	touches   int
	untouches int
}

func (e *dispatchLifetimeEntity) Base() *entity.EntityBase { return e.EntityBase }

func newDispatchLifetimeEntity(t *testing.T, unique int64) *dispatchLifetimeEntity {
	t.Helper()
	id := mustBuildCastID(t, unique, entity.EntityCategory(1), nestLocalKind)
	return &dispatchLifetimeEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind)}
}

func (e *dispatchLifetimeEntity) Touch() bool {
	if !e.EntityBase.Touch() {
		return false
	}
	e.touches++
	return true
}

func (e *dispatchLifetimeEntity) UnTouch() {
	e.untouches++
	e.EntityBase.UnTouch()
}

func TestBroadcastReleasePanicBalancesTouchesAndContinues(t *testing.T) {
	a, b := newDispatchLifetimeEntity(t, 9960), newDispatchLifetimeEntity(t, 9961)
	manager := entity.NewEntityManager()
	if err := manager.TryAdd(a); err != nil {
		t.Fatal(err)
	}
	defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { panic("release hook") })()
	getter := nilForMissingGetter{present: map[int64]entity.IThreadSafeEntity{a.ID(): a, b.ID(): b}}
	mgr := NewEngine(NestOptionWithGetter(getter))
	name := NewHandlerName("broadcast_release_panic")
	var calls []int64
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		calls = append(calls, es[0].ID())
		return nil, nil
	}, HandlerMeta{})
	scope, end := entity.NewGuardScope("broadcast-release")
	defer end()
	var escaped any
	func() {
		defer func() { escaped = recover() }()
		mgr.broadcastDispatch(name.String(), []int64{a.ID(), b.ID()}, nil)
	}()
	if escaped != nil {
		t.Errorf("release panic escaped per-entity boundary: %v", escaped)
	}
	if !slices.Equal(calls, []int64{a.ID(), b.ID()}) {
		t.Errorf("broadcast calls = %v, want both entities", calls)
	}
	for _, e := range []*dispatchLifetimeEntity{a, b} {
		if e.touches != 1 || e.untouches != 1 {
			t.Errorf("entity %d touches/untouches = %d/%d, want 1/1", e.ID(), e.touches, e.untouches)
		}
		if scope.Guard().Guarded(e.GUId()) {
			t.Errorf("entity %d remained locked", e.ID())
		}
	}
}

// 锁顺序可以排序，但业务参数的顺序、重复项和分组中的空位置必须保留。
// handler 即使重写自己的参数切片，框架也必须按原取得的资源完成清理。
func TestDispatchRoutesPreserveArgumentsAndReleaseOnEveryOutcome(t *testing.T) {
	for _, route := range []string{"single", "multi", "groups"} {
		for _, outcome := range []string{"success", "error", "panic"} {
			t.Run(route+"/"+outcome, func(t *testing.T) {
				a, b := newDispatchLifetimeEntity(t, 9971), newDispatchLifetimeEntity(t, 9970)
				missing := mustBuildCastID(t, 9972, entity.EntityCategory(1), nestLocalKind)
				mgr := NewEngine(NestOptionWithGetter(nilForMissingGetter{present: map[int64]entity.IThreadSafeEntity{a.ID(): a, b.ID(): b}}))
				name := NewHandlerName("route_lifecycle")
				failure := errors.New("business failed")
				mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, opts ...HandlerOption) (any, error) {
					want := []entity.IThreadSafeEntity{a}
					if route != "single" {
						want = []entity.IThreadSafeEntity{a, nil, b, a}
					}
					if !slices.Equal(es, want) {
						t.Errorf("handler argument order = %v, want %v", es, want)
					}
					var options HandlerOptionParam
					for _, opt := range opts {
						opt(&options)
					}
					if route == "groups" && (!options.IsGroup || !slices.Equal(options.GroupLen, []int{2, 0, 2})) {
						t.Errorf("group options = %+v", options)
					}
					if route != "groups" && options.IsGroup {
						t.Error("ordinary dispatch received group options")
					}
					clear(es)
					switch outcome {
					case "error":
						return nil, failure
					case "panic":
						panic(failure)
					}
					return "ok", nil
				}, HandlerMeta{})
				scope, end := entity.NewGuardScope("route-lifecycle")
				defer end()
				var ret any
				var err error
				var panicked any
				func() {
					defer func() { panicked = recover() }()
					switch route {
					case "single":
						ret, err = mgr.singleDispatch(name.String(), a.ID(), nil)
					case "multi":
						ret, err = mgr.multiDispatch(name.String(), []int64{a.ID(), missing, b.ID(), a.ID()}, nil)
					case "groups":
						ret, err = mgr.multiGroupDispatch(name.String(), [][]int64{{a.ID(), missing}, {}, {b.ID(), a.ID()}}, nil)
					}
				}()
				if outcome == "success" && (ret != "ok" || err != nil || panicked != nil) {
					t.Fatalf("success = %v/%v/%v", ret, err, panicked)
				}
				if outcome == "error" && (!errors.Is(err, failure) || panicked != nil) {
					t.Fatalf("error = %v/%v", err, panicked)
				}
				if outcome == "panic" && panicked != failure {
					t.Fatalf("panic = %v", panicked)
				}
				wantA, wantB := 2, 1
				if route == "single" {
					wantA, wantB = 1, 0
				}
				if a.touches != wantA || a.untouches != wantA || b.touches != wantB || b.untouches != wantB {
					t.Errorf("unbalanced touches: a=%d/%d, b=%d/%d", a.touches, a.untouches, b.touches, b.untouches)
				}
				if scope.Guard().GuardedCount() != 0 {
					t.Error("dispatch retained entity locks")
				}
			})
		}
	}
}

// 缺失实体：生产 ManagerAccess.Get 未命中且没有 loader 时返回 (nil, nil)。single 分派返回 ErrEntityNotFound（不是
// nil 解引用），broadcast 跳过缺失目标、后面的目标照常执行。替身必须是生产的 (nil, nil) 形状。
// 历史：RR-20260919-09。

// nilForMissingGetter is the production contract: not found is (nil, nil).
type nilForMissingGetter struct {
	present map[int64]entity.IThreadSafeEntity
}

func (g nilForMissingGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.present[id], nil
}

func (g nilForMissingGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		out[i] = g.present[id]
	}
	return out, nil
}

func TestASingleDispatchToAMissingEntitySaysNotFound(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	id := mustBuildCastID(t, 7301, entity.EntityCategory(1), nestLocalKind)
	name := NewHandlerName("test_missing_entity_single")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		t.Error("the handler ran for an entity that does not exist")
		return nil, nil
	})

	mgr := &NestMgr{getter: nilForMissingGetter{}}
	_, err := mgr.singleDispatch(name.String(), id, nil)
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("dispatch to a missing entity = %v, want ErrEntityNotFound", err)
	}
}

// One missing id must not take the rest of the broadcast with it.
func TestABroadcastSkipsMissingEntitiesAndKeepsGoing(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	presentID := mustBuildCastID(t, 7303, entity.EntityCategory(1), nestLocalKind)
	present := newMockEntity(presentID, entity.EntityCategory(1))
	missingID := mustBuildCastID(t, 7302, entity.EntityCategory(1), nestLocalKind)

	calls := 0
	name := NewHandlerName("test_missing_entity_broadcast")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		calls++
		if len(es) != 1 || es[0] != present {
			t.Errorf("the handler ran for %v, want only the entity that exists", es)
		}
		return nil, nil
	})

	mgr := &NestMgr{getter: nilForMissingGetter{present: map[int64]entity.IThreadSafeEntity{presentID: present}}}
	// The missing one is FIRST: that is the order in which it used to abort
	// everything behind it.
	mgr.broadcastDispatch(name.String(), []int64{missingID, presentID}, nil)
	if calls != 1 {
		t.Fatalf("the handler ran %d times, want once: a missing id aborted the rest of the broadcast", calls)
	}
}

// 广播在一个作业里逐目标执行：前一个目标的 handler fence 了引擎（或它的提交结果未知）之后，还没开始的目标不再执行业务
// （广播没有交回，跳过即可）。与批量作业共用逐目标的开始检查 checkHandlerStart（RV-20261022-01）。
// 变异核对：broadcastDispatch 不逐目标调用 checkHandlerStart 时第二、三个目标照常执行，本用例变红。
func TestBroadcastSkipsUnstartedTargetsAfterFence(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	ids := make([]int64, 3)
	for i := range ids {
		ids[i] = mustBuildCastID(t, int64(7311+i), entity.EntityCategory(1), nestLocalKind)
		getter.Add(newMockEntity(ids[i], entity.EntityCategory(1)))
	}
	mgr := &NestMgr{getter: getter}
	var ran []int64
	name := NewHandlerName("test_broadcast_fence")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		ran = append(ran, es[0].ID())
		if es[0].ID() == ids[0] {
			mgr.Fence(errors.New("test: persistence result unknown"))
		}
		return nil, nil
	})
	mgr.broadcastDispatch(name.String(), ids, nil)
	if !slices.Equal(ran, ids[:1]) {
		t.Fatalf("broadcast ran %v after the first target fenced the engine, want only %v", ran, ids[:1])
	}
}

func TestMultiDispatchRequiresFirstEntity(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	missingID := mustBuildCastID(t, 7100, entity.EntityCategory(1), nestLocalKind)
	existingID := mustBuildCastID(t, 7101, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(existingID, entity.EntityCategory(1)))

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_multi_requires_first")
	called := false
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		called = true
		return len(es), nil
	})

	ret, err := Nest.RequestMulti(context.Background(), name, []int64{missingID, existingID}, nil)
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("MultiSync err = %v, want %v", err, ErrEntityNotFound)
	}
	if ret != nil {
		t.Fatalf("MultiSync ret = %v, want nil", ret)
	}
	if called {
		t.Fatal("handler should not be called when first entity is missing")
	}
}

func TestMultiDispatchAllowsMissingNonFirstEntity(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	firstID := mustBuildCastID(t, 7110, entity.EntityCategory(1), nestLocalKind)
	missingID := mustBuildCastID(t, 7111, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(firstID, entity.EntityCategory(1)))

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_multi_allows_missing_non_first")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if len(es) != 2 {
			return nil, fmt.Errorf("entities len=%d want 2", len(es))
		}
		if es[0] == nil || es[0].ID() != firstID {
			return nil, errors.New("first entity missing")
		}
		if es[1] != nil {
			return nil, errors.New("second entity should be nil")
		}
		return "ok", nil
	})

	ret, err := Nest.RequestMulti(context.Background(), name, []int64{firstID, missingID}, nil)
	if err != nil {
		t.Fatalf("MultiSync err = %v", err)
	}
	if ret != "ok" {
		t.Fatalf("MultiSync ret = %v, want ok", ret)
	}
}

func TestMultiGroupDispatchRequiresFirstEntity(t *testing.T) {
	ResetHandlersForTest()
	defer ResetHandlersForTest()

	getter := newMockGetter()
	missingID := mustBuildCastID(t, 7120, entity.EntityCategory(1), nestLocalKind)
	existingID := mustBuildCastID(t, 7121, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(existingID, entity.EntityCategory(1)))

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_multi_group_requires_first")
	called := false
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		called = true
		return len(es), nil
	})

	ret, err := Nest.RequestMultiGroup(context.Background(), name, [][]int64{{missingID}, {existingID}}, nil)
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("MultiGroupSync err = %v, want %v", err, ErrEntityNotFound)
	}
	if ret != nil {
		t.Fatalf("MultiGroupSync ret = %v, want nil", ret)
	}
	if called {
		t.Fatal("handler should not be called when first grouped entity is missing")
	}
}

func TestNestHandlerRejectsNestedSyncDispatch(t *testing.T) {
	cases := []struct {
		name string
		run  func(target HandlerName, id1 int64, id2 int64) (any, error)
	}{
		{
			name: "sync",
			run: func(target HandlerName, id1 int64, _ int64) (any, error) {
				return Nest.Request(context.Background(), target, id1, nil)
			},
		},
		{
			name: "multi_sync",
			run: func(target HandlerName, id1 int64, id2 int64) (any, error) {
				return Nest.RequestMulti(context.Background(), target, []int64{id1, id2}, nil)
			},
		},
		{
			name: "multi_group_sync",
			run: func(target HandlerName, id1 int64, id2 int64) (any, error) {
				return Nest.RequestMultiGroup(context.Background(), target, [][]int64{{id1}, {id2}}, nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetHandlersForTest()
			t.Cleanup(ResetHandlersForTest)

			getter := newMockGetter()
			id1 := mustBuildCastID(t, 13, entity.EntityCategory(1), nestLocalKind)
			id2 := mustBuildCastID(t, 14, entity.EntityCategory(1), nestLocalKind)
			getter.Add(newMockEntity(id1, entity.EntityCategory(1)))
			getter.Add(newMockEntity(id2, entity.EntityCategory(1)))

			InitNest(
				NestOptionWithGetter(getter),
				NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
				NestOptionWithTickDuration(100*time.Millisecond),
			)
			t.Cleanup(StopNest)

			called := make(chan string, 1)
			targetName := NewHandlerName("test_nested_sync_target_" + tc.name)
			outerName := NewHandlerName("test_nested_sync_outer_" + tc.name)
			MustRegisterMemoryHandler(targetName, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				called <- "handler"
				return "inner-ok", nil
			})
			MustRegisterMemoryHandler(outerName, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				return tc.run(targetName, id1, id2)
			})

			ret, err := Nest.Request(context.Background(), outerName, id1, nil)
			if !errors.Is(err, ErrSyncInHandler) {
				t.Fatalf("outer sync err = %v, want %v", err, ErrSyncInHandler)
			}
			if ret != nil {
				t.Fatalf("outer ret = %v, want nil", ret)
			}
			select {
			case got := <-called:
				t.Fatalf("nested sync dispatch %s should be rejected, got %s", tc.name, got)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestNestHandlerRejectsNestedAsyncDispatch(t *testing.T) {
	cases := []struct {
		name string
		run  func(target HandlerName, id int64) error
	}{
		{
			name: "send",
			run: func(target HandlerName, id int64) error {
				return Nest.Dispatch(context.Background(), target, id, nil)
			},
		},
		{
			name: "multi_send",
			run: func(target HandlerName, id int64) error {
				return Nest.DispatchMulti(context.Background(), target, []int64{id}, nil)
			},
		},
		{
			name: "multi_group_send",
			run: func(target HandlerName, id int64) error {
				return Nest.DispatchMultiGroup(context.Background(), target, [][]int64{{id}}, nil)
			},
		},
		{
			name: "broadcast",
			run: func(target HandlerName, id int64) error {
				return Nest.DispatchBroadcast(context.Background(), target, []int64{id}, nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetHandlersForTest()
			t.Cleanup(ResetHandlersForTest)

			getter := newMockGetter()
			id := mustBuildCastID(t, 16, entity.EntityCategory(1), nestLocalKind)
			getter.Add(newMockEntity(id, entity.EntityCategory(1)))

			InitNest(
				NestOptionWithGetter(getter),
				NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
				NestOptionWithTickDuration(100*time.Millisecond),
			)
			t.Cleanup(StopNest)

			called := make(chan string, 1)
			targetName := NewHandlerName("test_nested_async_target_" + tc.name)
			outerName := NewHandlerName("test_nested_async_outer_" + tc.name)
			MustRegisterMemoryHandler(targetName, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				called <- "handler"
				return nil, nil
			})
			MustRegisterMemoryHandler(outerName, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if err := tc.run(targetName, id); err != nil {
					return nil, err
				}
				return "outer-ok", nil
			})

			ret, err := Nest.Request(context.Background(), outerName, id, nil)
			if !errors.Is(err, ErrAsyncInHandler) {
				t.Fatalf("outer sync err = %v, want %v", err, ErrAsyncInHandler)
			}
			if ret != nil {
				t.Fatalf("outer ret = %v, want nil", ret)
			}
			select {
			case got := <-called:
				t.Fatalf("nested async dispatch %s should be rejected, got %s", tc.name, got)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

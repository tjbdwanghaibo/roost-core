package entity

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type lateLocalStepLoader struct {
	manager *EntityManager
	release chan struct{} // 领头方 Get 返回后由用例关闭
	late    chan error
	ran     atomic.Bool
}

func (l *lateLocalStepLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	if err := l.manager.TryAdd(value); err != nil {
		return nil, err
	}
	go func() {
		<-l.release
		l.late <- RunLocal(ctx, func() { l.ran.Store(true) })
	}()
	return value, nil
}

func TestLateRunLocalAfterNonNestLeaderLoadCompletedDoesNotBlock(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "unbound"
		if bound {
			name = "bound_executor"
		}
		t.Run(name, func(t *testing.T) {
			manager := NewEntityManager()
			access := NewManagerAccess(manager)
			var boundRuns atomic.Int32
			if bound {
				access.BindLocalExecutor(func(fn func()) error {
					boundRuns.Add(1)
					fn()
					return nil
				})
			}
			x := newMgrTestEntity(mustBuildTestEntityID(t, 5591, testEntityCategoryPlayer, EntityKindNone), testEntityCategoryPlayer)
			if err := manager.TryAdd(x); err != nil {
				t.Fatal(err)
			}
			loader := &lateLocalStepLoader{manager: manager, release: make(chan struct{}), late: make(chan error, 1)}
			if _, err := access.ConfigureLoader(loader); err != nil {
				t.Fatal(err)
			}
			y := mustBuildTestEntityID(t, 5592, testEntityCategoryPlayer, EntityKindNone)
			// 领头方持有 X 的锁，所以 flight 带 leaderSteps / leaderAway（RR-27 的交回领头方路径）。
			err := WithGuardScope("late_local_step_leader", func(scope *GuardScope) error {
				if !scope.Guard().RequireEntity(x) {
					return errors.New("lock x")
				}
				_, err := access.Get(context.Background(), y, EntityCategoryNone)
				return err
			})
			if err != nil {
				t.Fatalf("leader Get: %v", err)
			}
			close(loader.release)
			select {
			case lateErr := <-loader.late:
				if lateErr != nil {
					t.Fatalf("late RunLocal returned %v", lateErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("late RunLocal (after the leader left with the completed load) still blocked after 3s: leaderAway was never closed")
			}
			if !loader.ran.Load() {
				t.Fatal("late RunLocal returned without running the step")
			}
			want := int32(0)
			if bound {
				want = 1
			}
			if got := boundRuns.Load(); got != want {
				t.Fatalf("bound executor runs = %d, want %d", got, want)
			}
		})
	}
}

type publishUnderOwnGuardLoader struct {
	manager *EntityManager
	locked  chan bool // 发布时领头方持有的 X 是否可被加载 goroutine 取得（应为 false）
	x       IThreadSafeEntity
}

func (l *publishUnderOwnGuardLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	var addErr error
	if err := RunLocal(ctx, func() {
		l.locked <- lockableElsewhere(l.x)
		addErr = WithGuardScope("load_publish", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(value) {
				return errors.New("publish could not lock the loaded entity")
			}
			return l.manager.TryAdd(value)
		})
	}); err != nil {
		return nil, err
	}
	return value, addErr
}

func TestNonNestLeaderHoldingAnotherLockColdLoadsWithoutDeadlock(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	x := newMgrTestEntity(mustBuildTestEntityID(t, 5501, testEntityCategoryPlayer, EntityKindNone), testEntityCategoryPlayer)
	if err := manager.TryAdd(x); err != nil {
		t.Fatal(err)
	}
	loader := &publishUnderOwnGuardLoader{manager: manager, locked: make(chan bool, 1), x: x}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	y := mustBuildTestEntityID(t, 5502, testEntityCategoryPlayer, EntityKindNone)

	type leaderResult struct {
		value IThreadSafeEntity
		err   error
	}
	done := make(chan leaderResult, 1)
	go func() {
		var got leaderResult
		got.err = WithGuardScope("non_nest_leader", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(x) {
				return errors.New("leader could not lock X")
			}
			var err error
			got.value, err = access.Get(context.Background(), y, EntityCategoryNone)
			return err
		})
		done <- got
	}()
	var got leaderResult
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the non-Nest leader holding X never got Y back: the cold load's publish deadlocked")
	}
	if got.err != nil || got.value == nil {
		t.Fatalf("leader Get(Y): value=%v err=%v", got.value, got.err)
	}
	if xFreeDuringPublish := <-loader.locked; xFreeDuringPublish {
		t.Fatal("X was lockable by the load goroutine while the leader held it")
	}
	if published := manager.Get(y); published != got.value {
		t.Fatalf("Y not published: manager has %v, leader got %v", published, got.value)
	}
	if !lockableElsewhere(got.value) || !lockableElsewhere(x) {
		t.Fatalf("locks leaked: Y free=%v X free=%v", lockableElsewhere(got.value), lockableElsewhere(x))
	}
}

// RR-20260927-27（B20 相邻形状）：loader 的发布还要锁领头方正持有的 X。RR-54 把发布从领头方 goroutine 挪到加载 goroutine 后，
// 加载 goroutine 在发布里等 X，领头方在等加载；领头方自己的 ctx 到期也离不开——leaderLeft 要等持有 leaderMu 的在途发布结束，
// 永久死锁。修后：Nest 之外、没有指定执行器且持有实体锁的领头方仍在等待时，发布交回领头方自己的 goroutine 执行（修前的位置，
// 可重入锁不阻塞）；领头方先按自己的 ctx 离开时，发布改在加载 goroutine 上执行，等领头方放锁后完成。
type publishLocksLeaderHeldLoader struct {
	manager *EntityManager
	x       IThreadSafeEntity
	gate    chan struct{} // 非 nil 时 LoadEntity 在发布前等它（模拟慢 I/O），用于让领头方先离开
	panics  bool          // 发布步骤 panic：交回领头方执行的步骤 panic 仍按加载 panic 处理
}

func (l *publishLocksLeaderHeldLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	if l.gate != nil {
		<-l.gate
	}
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	var addErr error
	if err := RunLocal(ctx, func() {
		addErr = WithGuardScope("load_publish_locks_x", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(l.x) {
				return errors.New("publish could not lock X")
			}
			if l.panics {
				panic("publish step panicked")
			}
			if !scope.Guard().RequireEntity(value) {
				return errors.New("publish could not lock the loaded entity")
			}
			return l.manager.TryAdd(value)
		})
	}); err != nil {
		return nil, err
	}
	return value, addErr
}

func TestNonNestLeaderHoldingLockNeededByPublishDoesNotDeadlock(t *testing.T) {
	setup := func(t *testing.T, gate chan struct{}, panics bool) (*EntityManager, *ManagerAccess, IThreadSafeEntity, int64) {
		t.Helper()
		manager := NewEntityManager()
		access := NewManagerAccess(manager)
		x := newMgrTestEntity(mustBuildTestEntityID(t, 5511, testEntityCategoryPlayer, EntityKindNone), testEntityCategoryPlayer)
		if err := manager.TryAdd(x); err != nil {
			t.Fatal(err)
		}
		if _, err := access.ConfigureLoader(&publishLocksLeaderHeldLoader{manager: manager, x: x, gate: gate, panics: panics}); err != nil {
			t.Fatal(err)
		}
		return manager, access, x, mustBuildTestEntityID(t, 5512, testEntityCategoryPlayer, EntityKindNone)
	}
	type leaderResult struct {
		value IThreadSafeEntity
		err   error
	}
	leadWhileHoldingX := func(access *ManagerAccess, x IThreadSafeEntity, y int64, budget time.Duration, done chan<- leaderResult) {
		var got leaderResult
		got.err = WithGuardScope("non_nest_leader_holding_x", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(x) {
				return errors.New("leader could not lock X")
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			var err error
			got.value, err = access.Get(ctx, y, EntityCategoryNone)
			return err
		})
		done <- got
	}

	t.Run("publish_while_leader_waits", func(t *testing.T) {
		manager, access, x, y := setup(t, nil, false)
		done := make(chan leaderResult, 1)
		go leadWhileHoldingX(access, x, y, 300*time.Millisecond, done)
		var got leaderResult
		select {
		case got = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("leader (own ctx 300ms) still stuck after 3s: the load goroutine's publish waits for X held by the leader, and the leader cannot leave because leaderLeft waits for that publish (deadlock)")
		}
		if got.err != nil || got.value == nil {
			t.Fatalf("leader Get(Y): value=%v err=%v", got.value, got.err)
		}
		if published := manager.Get(y); published != got.value {
			t.Fatalf("Y not published: manager has %v, leader got %v", published, got.value)
		}
		if !lockableElsewhere(got.value) || !lockableElsewhere(x) {
			t.Fatalf("locks leaked: Y free=%v X free=%v", lockableElsewhere(got.value), lockableElsewhere(x))
		}
	})

	t.Run("leader_leaves_before_publish", func(t *testing.T) {
		gate := make(chan struct{})
		manager, access, x, y := setup(t, gate, false)
		done := make(chan leaderResult, 1)
		go leadWhileHoldingX(access, x, y, 100*time.Millisecond, done)
		var got leaderResult
		select {
		case got = <-done:
		case <-time.After(3 * time.Second):
			close(gate)
			t.Fatal("leader did not leave on its own ctx while the load was still in I/O")
		}
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("leader err=%v, want its own context.DeadlineExceeded", got.err)
		}
		close(gate) // 领头方已离开并放掉 X：发布在加载 goroutine 上取 X、Y 后完成
		deadline := time.Now().Add(3 * time.Second)
		for manager.Get(y) == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		published := manager.Get(y)
		if published == nil {
			t.Fatal("Y was never published after the leader left and released X")
		}
		value, err := access.Get(context.Background(), y, EntityCategoryNone)
		if err != nil || value != published {
			t.Fatalf("Get(Y) after publish: value=%v err=%v", value, err)
		}
		if !lockableElsewhere(published) || !lockableElsewhere(x) {
			t.Fatalf("locks leaked: Y free=%v X free=%v", lockableElsewhere(published), lockableElsewhere(x))
		}
	})
	t.Run("publish_panics_on_leader", func(t *testing.T) {
		manager, access, x, y := setup(t, nil, true)
		panicked := make(chan any, 1)
		go func() {
			defer func() { panicked <- recover() }()
			done := make(chan leaderResult, 1)
			leadWhileHoldingX(access, x, y, 2*time.Second, done)
		}()
		select {
		case r := <-panicked:
			if r == nil {
				t.Fatal("the leader returned normally although its load's publish step panicked")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("leader stuck after the publish step panicked")
		}
		if manager.Get(y) != nil {
			t.Fatal("Y was published although the publish step panicked")
		}
		if !lockableElsewhere(x) {
			t.Fatal("X still locked after the leader's scope unwound")
		}
		access.flightMu.Lock()
		_, inFlight := access.flights[y]
		access.flightMu.Unlock()
		if inFlight {
			t.Fatal("the panicked load left its flight behind")
		}
	})
}

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
		_, _ = access.loadEntityShared(context.Background(), fullID, 1, entityLoaderBinding{loader: loader})
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
		_, err := access.loadEntityShared(ctx, fullID, 1, entityLoaderBinding{loader: loader})
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
		_, _ = access.loadEntityShared(context.Background(), fullID, 1, entityLoaderBinding{loader: loader})
	}()
	<-entered

	waiterCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := access.loadEntityShared(waiterCtx, fullID, 1, entityLoaderBinding{loader: loader})
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

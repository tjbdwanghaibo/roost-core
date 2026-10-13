// 冷加载：非 Nest 领头方持锁时不死锁、loader panic 不卡住实体、共享加载与领头方预算解耦、框架超时与停机原因传给等待方。

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

// loader 的发布还要锁领头方正持有的 X：Nest 之外、没有指定执行器且持有实体锁的领头方仍在等待时，发布交回领头方自己的 goroutine
// 执行（可重入锁不阻塞）；领头方先按自己的 ctx 离开时，发布改在加载 goroutine 上执行，等领头方放锁后完成。不死锁。
// 历史：RR-20260927-27。
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

// 共享加载运行在与调用方解耦的 ctx 上（WithoutCancel 派生，只受框架加载超时与停机取消约束）：每个等待方按自己的 ctx 离开；领头方
// 截止不连带预算更长的等待方；最后一个等待方离开也不取消在途加载，结果照常进入 EntityManager。
// 历史：RR-20260926-54。

// gatedLoader 在 release 关闭前阻塞，期间尊重自己拿到的 ctx；放行后经 RunLocal 发布实体，
// 与 DataEngine 仓库的冷加载顺序一致。
type gatedLoader struct {
	manager *EntityManager
	entered chan struct{}
	release chan struct{}
	loads   atomic.Int32
	// ctxErr 记录 LoadEntity 返回时自己的 ctx 状态。
	ctxErr atomic.Pointer[error]
}

func newGatedLoader(manager *EntityManager) *gatedLoader {
	return &gatedLoader{manager: manager, entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (l *gatedLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	l.loads.Add(1)
	l.entered <- struct{}{}
	select {
	case <-l.release:
	case <-ctx.Done():
	}
	err := ctx.Err()
	l.ctxErr.Store(&err)
	if err != nil {
		return nil, err
	}
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	var addErr error
	if runErr := RunLocal(ctx, func() { addErr = l.manager.TryAdd(value) }); runErr != nil {
		return nil, runErr
	}
	if addErr != nil {
		return nil, addErr
	}
	return value, nil
}

func (l *gatedLoader) loaderCtxErr() error {
	if p := l.ctxErr.Load(); p != nil {
		return *p
	}
	return nil
}

func waitEntered(t *testing.T, loader *gatedLoader) {
	t.Helper()
	select {
	case <-loader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the load never reached the loader")
	}
}

type getResult struct {
	value IThreadSafeEntity
	err   error
}

func goGet(access *ManagerAccess, ctx context.Context, id int64) <-chan getResult {
	done := make(chan getResult, 1)
	go func() {
		value, err := access.Get(ctx, id, EntityCategoryNone)
		done <- getResult{value, err}
	}()
	return done
}

// loginBudget 代表登录预算；缩短只为让回归快，时序关系不变。
const loginBudget = 200 * time.Millisecond

func TestSharedLoadOutlivesTheLeadersBudget(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const id = int64(5401)

	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), loginBudget)
	defer cancelLeader()
	leader := goGet(access, leaderCtx, id)
	waitEntered(t, loader)

	// 同一 flight 中预算更长的等待方。
	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWaiter()
	waiter := goGet(access, waiterCtx, id)

	select {
	case got := <-leader:
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("leader: value=%v err=%v, want its own login budget to expire", got.value, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader did not leave when its own budget expired")
	}
	close(loader.release)

	select {
	case got := <-waiter:
		if got.err != nil || got.value == nil {
			t.Fatalf("waiter with a longer budget: value=%v err=%v (loader ctx at return: %v); the leader's budget ended the shared load",
				got.value, got.err, loader.loaderCtxErr())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the shared load's result")
	}
	if n := loader.loads.Load(); n != 1 {
		t.Fatalf("loads=%d, want 1: the waiter must share the flight the leader started, not start a new one", n)
	}
	if manager.Get(id) == nil {
		t.Fatal("the loaded entity was not published into the EntityManager")
	}
}

func TestLastWaiterLeavingDoesNotCancelTheLoad(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const id = int64(5402)

	ctx, cancel := context.WithTimeout(context.Background(), loginBudget)
	defer cancel()
	only := goGet(access, ctx, id)
	waitEntered(t, loader)
	if got := <-only; !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("the only waiter: value=%v err=%v, want its own deadline", got.value, got.err)
	}
	// 没有任何等待方了；放行后加载仍完成并进入 EntityManager。
	close(loader.release)
	deadline := time.Now().Add(5 * time.Second)
	for manager.Get(id) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the load was abandoned when its last waiter left (loader ctx at return: %v)", loader.loaderCtxErr())
		}
		time.Sleep(5 * time.Millisecond)
	}
	value, err := access.Get(context.Background(), id, EntityCategoryNone)
	if err != nil || value == nil {
		t.Fatalf("Get after the load finished: value=%v err=%v", value, err)
	}
	if n := loader.loads.Load(); n != 1 {
		t.Fatalf("loads=%d, want the one load whose result was published", n)
	}
}

// 停机：DataEngine Runtime 停止时注销 loader，在途共享加载随之取消，等待方拿到结果而不是
// 各自等到自己的截止时间。
func TestUnregisteringTheLoaderCancelsInFlightLoads(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	unregister, err := access.ConfigureLoader(loader)
	if err != nil {
		t.Fatal(err)
	}
	const id = int64(5403)
	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWaiter()
	waiter := goGet(access, waiterCtx, id)
	waitEntered(t, loader)

	unregister()
	select {
	case got := <-waiter:
		if got.err == nil || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("waiter after the loader stopped: value=%v err=%v, want the load cancelled", got.value, got.err)
		}
		if waiterCtx.Err() != nil {
			t.Fatal("the waiter was released by its own deadline, not by the loader stopping")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the loader did not cancel the in-flight load; its waiters wait out their own deadlines")
	}
	if !errors.Is(loader.loaderCtxErr(), context.Canceled) {
		t.Fatalf("loader ctx at return: %v, want cancelled", loader.loaderCtxErr())
	}
	if manager.Get(id) != nil {
		t.Fatal("a cancelled load published its entity")
	}
}

// 框架加载上限：没有等待方、loader 也不返回的加载最终结束并释放 flight，Cause 为 ErrEntityLoadTimeout；
// 之后的 Get 重新发起加载。默认上限为 DefaultEntityLoadTimeout。
func TestSharedLoadIsBoundedByTheFrameworkLoadTimeout(t *testing.T) {
	if DefaultEntityLoadTimeout != 30*time.Second {
		t.Fatalf("DefaultEntityLoadTimeout=%v, want 30s", DefaultEntityLoadTimeout)
	}
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	access.ConfigureLoadTimeout(100 * time.Millisecond)
	loader := newGatedLoader(manager)
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const id = int64(5404)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := <-goGet(access, ctx, id)
	waitEntered(t, loader)
	if !errors.Is(got.err, context.DeadlineExceeded) || !errors.Is(got.err, ErrEntityLoadTimeout) || ctx.Err() != nil {
		t.Fatalf("value=%v err=%v, want the framework load timeout (not the caller's own deadline)", got.value, got.err)
	}
	access.flightMu.Lock()
	_, stuck := access.flights[id]
	access.flightMu.Unlock()
	if stuck {
		t.Fatal("the timed-out load left its flight behind")
	}
	close(loader.release)
	if value, err := access.Get(ctx, id, EntityCategoryNone); err != nil || value == nil {
		t.Fatalf("retry after the timeout: value=%v err=%v", value, err)
	}
	if n := loader.loads.Load(); n != 2 {
		t.Fatalf("loads=%d, want the timed-out load and one fresh retry", n)
	}
}

// 停机取消给出可判别的 Cause；注销可重复调用。
func TestLoaderStopCauseReachesWaiters(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	unregister, err := access.ConfigureLoader(loader)
	if err != nil {
		t.Fatal(err)
	}
	waiter := goGet(access, context.Background(), 5405)
	waitEntered(t, loader)
	unregister()
	unregister()
	if got := <-waiter; !errors.Is(got.err, ErrEntityLoaderStopped) || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err=%v, want ErrEntityLoaderStopped wrapped with context.Canceled", got.err)
	}
}

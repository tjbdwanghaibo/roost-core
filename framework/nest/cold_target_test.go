// 冷目标准入：声明的冷目标自动走 I/O 预加载，动态冷目标返回可降级错误，准入判定不在发送方做 I/O。

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
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// 声明目标里有未加载、可由 loader 加载的实体时，Nest 在统一准入处自动走 I/O 阶段预加载：业务代码与生成 sender
// 不需要 SendOptionIO，同 ID 顺序仍在统一准入处建立，loader 只在 I/O worker 上执行。准入之后、handler 取得
// Guard 之前目标被驱逐时，同一条已准入请求原位转到 I/O 准备阶段，不重新排队，也不在业务 worker 上冷加载。
// 历史：RR-20260926-25。

func newColdTargetEngine(t *testing.T, fast, slow WorkerPoolConfig) (*NestMgr, *entity.EntityManager, *stageLoader) {
	t.Helper()
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(fast, WorkerPoolConfig{}, slow))
	return mgr, manager, loader
}

func startColdTargetEngine(t *testing.T, mgr *NestMgr) {
	t.Helper()
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
}

func TestDefaultSenderPreparesColdDeclaredTargetsOnIOWorker(t *testing.T) {
	mgr, manager, loader := newColdTargetEngine(t, WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8})
	hot := mustBuildCastID(t, 8401, entity.EntityCategory(1), nestLocalKind)
	coldSingle := mustBuildCastID(t, 8402, entity.EntityCategory(1), nestLocalKind)
	coldMulti := mustBuildCastID(t, 8403, entity.EntityCategory(1), nestLocalKind)
	coldGroup := mustBuildCastID(t, 8404, entity.EntityCategory(1), nestLocalKind)
	if err := manager.TryAdd(newMockEntityWithKind(hot, entity.EntityCategory(1), nestLocalKind)); err != nil {
		t.Fatal(err)
	}
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("grant"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if !fctx.InShortWorker() {
			return nil, errors.New("handler left the business pool")
		}
		for _, e := range es {
			if e == nil {
				return nil, errors.New("declared target missing in handler")
			}
		}
		return len(es), nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	startColdTargetEngine(t, mgr)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// 生成 sender 的默认形态：不带任何 SendOpt。
	if ret, err := mgr.Request(ctx, NewHandlerName("grant"), coldSingle, nil); err != nil || ret != 1 {
		t.Fatalf("single request to cold target: ret=%v err=%v loads=%d", ret, err, loader.calls.Load())
	}
	if ret, err := mgr.RequestMulti(ctx, NewHandlerName("grant"), []int64{hot, coldMulti}, nil); err != nil || ret != 2 {
		t.Fatalf("multi request with a cold target: ret=%v err=%v", ret, err)
	}
	if ret, err := mgr.RequestMultiGroup(ctx, NewHandlerName("grant"), [][]int64{{hot}, {coldGroup}}, nil); err != nil || ret != 2 {
		t.Fatalf("multi-group request with a cold target: ret=%v err=%v", ret, err)
	}
	// stageLoader 在业务 worker 上被调用会返回错误，所以三次成功即证明加载只在I/O worker。
	if loads := loader.calls.Load(); loads != 3 {
		t.Fatalf("loads=%d, want one per cold declared target", loads)
	}
	// 目标已加载后回到业务池直接执行，不再经过I/O 阶段。
	slowBefore := mgr.Stats().Queue.IO.Started
	if ret, err := mgr.Request(ctx, NewHandlerName("grant"), coldSingle, nil); err != nil || ret != 1 {
		t.Fatalf("warm request: ret=%v err=%v", ret, err)
	}
	if slowAfter := mgr.Stats().Queue.IO.Started; slowAfter != slowBefore || loader.calls.Load() != 3 {
		t.Fatalf("loaded target still used the I/O preparation stage: io started %d -> %d loads=%d", slowBefore, slowAfter, loader.calls.Load())
	}
}

// evictingEntity 在被武装后的第一次 Touch 里把自己从 EntityManager 驱逐，模拟闲置回收恰好
// 落在“读取之后、引用之前”或“引用之后、取锁之前”两个窗口。
type evictingEntity struct {
	*mockEntity
	evict      atomic.Pointer[func()]
	afterTouch bool
}

func (e *evictingEntity) Touch() bool {
	evict := e.evict.Swap(nil)
	if evict != nil && !e.afterTouch {
		(*evict)()
	}
	ok := e.mockEntity.Touch()
	if evict != nil && e.afterTouch {
		(*evict)()
	}
	return ok
}

func TestDeclaredTargetEvictedAfterAdmissionMovesToIOKeepingOrder(t *testing.T) {
	for _, window := range []string{"before lookup", "between lookup and touch", "between touch and lock"} {
		t.Run(window, func(t *testing.T) {
			// 1 个业务 worker：先用 blocker 占住，保证两条请求都在目标仍加载时准入并走业务池，
			// 然后在 handler 取 Guard 之前把目标驱逐。第二条请求用来证明第一条没有被重新
			// 准入到同 ID 队尾。
			mgr, manager, loader := newColdTargetEngine(t, WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8})
			blockerID := mustBuildCastID(t, 8411, entity.EntityCategory(1), nestLocalKind)
			targetID := mustBuildCastID(t, 8412, entity.EntityCategory(1), nestLocalKind)
			target := &evictingEntity{mockEntity: newMockEntityWithKind(targetID, entity.EntityCategory(1), nestLocalKind), afterTouch: window == "between touch and lock"}
			for _, e := range []entity.IThreadSafeEntity{newMockEntityWithKind(blockerID, entity.EntityCategory(1), nestLocalKind), target} {
				if err := manager.TryAdd(e); err != nil {
					t.Fatal(err)
				}
			}
			entered, gate := make(chan struct{}), make(chan struct{})
			mgr.MustRegisterHandlerWithMeta(NewHandlerName("block"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				close(entered)
				<-gate
				return "released", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			var orderMu sync.Mutex
			var order []string
			mgr.MustRegisterHandlerWithMeta(NewHandlerName("record"), func(es []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
				if !fctx.InShortWorker() || len(es) != 1 || es[0] == nil || es[0] == entity.IThreadSafeEntity(target) {
					return nil, errors.New("handler did not run on the business pool with the reloaded target")
				}
				orderMu.Lock()
				order = append(order, params[0].(string))
				orderMu.Unlock()
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			startColdTargetEngine(t, mgr)

			send := func(name string, id int64, params ...any) <-chan any {
				t.Helper()
				msg, ch := GenSyncMsg(MsgTypeSingle)
				msg.Name, msg.Tid, msg.Params = name, id, params
				if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
					t.Fatal(err)
				}
				return ch
			}
			blocked := send("block", blockerID)
			stagedSignal(t, entered)
			first := send("record", targetID, "first")
			second := send("record", targetID, "second")
			if fast := mgr.Stats().Queue.Short; fast.Ready+fast.BlockedOnPredecessor != 2 {
				t.Fatalf("both requests should be admitted to the business lane while the target is loaded: %+v", fast)
			}
			evict := func() {
				if err := manager.Destroy(context.Background(), target, entity.DestroyReasonCommon, false); err != nil {
					t.Errorf("evict: %v", err)
				}
			}
			if window == "before lookup" {
				evict()
			} else {
				target.evict.Store(&evict)
			}
			close(gate)
			if got := stagedWait(t, blocked); got != "released" {
				t.Fatalf("blocker: %v", got)
			}
			if got := stagedWait(t, first); got != "ok" {
				t.Fatalf("first request after eviction: %v (loads=%d)", got, loader.calls.Load())
			}
			if got := stagedWait(t, second); got != "ok" {
				t.Fatalf("second request: %v", got)
			}
			orderMu.Lock()
			defer orderMu.Unlock()
			if !slices.Equal(order, []string{"first", "second"}) {
				t.Fatalf("same-ID order changed after moving to the I/O preparation stage: %v", order)
			}
			if loads := loader.calls.Load(); loads != 1 {
				t.Fatalf("loads=%d, want one slow-stage reload", loads)
			}
			if manager.Get(targetID) == nil {
				t.Fatal("reloaded target is not published")
			}
		})
	}
}

// 准入判定只用不做加载的查询接口（entity.LoadedChecker）：自定义 Getter.Get 只在 worker 上被调用，
// 不在发送方或延迟派发 goroutine 上同步 I/O；未实现 LoadedChecker 时准入不判冷、不自动转慢。
// 历史：RR-20260926-47。

// probeIOGetter 模拟不遵守 LoadedEntitiesOnly 的自定义 Getter：在任何 worker 之外被调用时同步“加载”200ms。
type probeIOGetter struct {
	entity.Getter
	outsideWorker atomic.Int32
}

func (g *probeIOGetter) Get(ctx context.Context, id int64, c entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if fctx.CurrentContext() == nil {
		g.outsideWorker.Add(1)
		time.Sleep(200 * time.Millisecond) // 模拟 I/O：发生在发送方 / 延迟派发 goroutine 上即为缺陷
	}
	return g.Getter.Get(ctx, id, c)
}

func TestAdmissionNeverRunsCustomGetterOnSender(t *testing.T) {
	id, e := newAsyncPilotEntity(t, 9800, 1)
	base := newMockGetter()
	base.Add(e)
	g := &probeIOGetter{Getter: base}
	mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("admission_custom_getter")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name, msg.Tid = name.String(), id
	started := time.Now()
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	sendTook := time.Since(started)
	if got := stagedWait(t, ch); got != "ok" {
		t.Fatalf("reply=%v", got)
	}
	if n := g.outsideWorker.Load(); n != 0 {
		t.Fatalf("admission called the custom Getter %d time(s) on the sender goroutine; TrySendMsg took %v", n, sendTook)
	}
}

func TestDelayedAdmissionIsNotSerializedByCustomGetter(t *testing.T) {
	id, e := newAsyncPilotEntity(t, 9810, 1)
	base := newMockGetter()
	base.Add(e)
	g := &probeIOGetter{Getter: base}
	mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 4, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("admission_custom_getter_delayed")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	const n = 3
	done := make(chan time.Duration, n)
	begin := time.Now()
	for range n {
		msg, ch := GenSyncMsg(MsgTypeSingle)
		msg.Name, msg.Tid = name.String(), id
		if err := mgr.dispatcher.TryDelaySendMsg(10*time.Millisecond, msg); err != nil {
			t.Fatal(err)
		}
		go func() { <-ch; done <- time.Since(begin) }()
	}
	var last time.Duration
	for range n {
		select {
		case d := <-done:
			last = max(last, d)
		case <-time.After(3 * time.Second):
			t.Fatal("delayed messages did not complete")
		}
	}
	if calls := g.outsideWorker.Load(); calls != 0 {
		t.Fatalf("delay loop called the custom Getter %d time(s); last of %d 10ms-delayed messages finished at %v", calls, n, last)
	}
	// 基线上三条依次完成于约 212 / 413 / 614ms；不再在 delay loop 上做 I/O 后应远低于一次 200ms “加载”。
	if last >= 190*time.Millisecond {
		t.Fatalf("delayed admissions look serialized: last finished at %v", last)
	}
}

// 未实现 LoadedChecker 的自定义 Getter：准入不判冷、不自动转慢，冷目标返回 ErrColdLoadInLogic，loader 不被调用。
type contractGetter struct{ access *entity.ManagerAccess }

func (g contractGetter) Get(ctx context.Context, id int64, c entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.access.Get(ctx, id, c)
}
func (g contractGetter) GetMany(ctx context.Context, ids []int64, cs []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	return g.access.GetMany(ctx, ids, cs)
}

// loadedCheckerGetter 在 contractGetter 之上实现 IsLoaded，并记录 worker 之外的 Get 调用。
type loadedCheckerGetter struct {
	contractGetter
	isLoaded, outsideWorker atomic.Int32
}

func (g *loadedCheckerGetter) Get(ctx context.Context, id int64, c entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if fctx.CurrentContext() == nil {
		g.outsideWorker.Add(1)
	}
	return g.contractGetter.Get(ctx, id, c)
}
func (g *loadedCheckerGetter) IsLoaded(id int64) bool {
	g.isLoaded.Add(1)
	return g.access.Manager().Get(id) != nil // 本测试总配置了 loader：不在内存即需要加载
}

func newLoaderAccess(t *testing.T) (*entity.ManagerAccess, *entity.EntityManager, *stageLoader) {
	t.Helper()
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	return access, manager, loader
}

func TestAdmissionColdProbeRequiresLoadedChecker(t *testing.T) {
	t.Run("custom_getter_without_checker_keeps_old_behavior", func(t *testing.T) {
		access, _, loader := newLoaderAccess(t)
		mgr := NewEngine(NestOptionWithGetter(contractGetter{access: access}), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{2, 8}))
		mgr.MustRegisterHandlerWithMeta(NewHandlerName("old_cold"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
		startColdTargetEngine(t, mgr)
		cold := mustBuildCastID(t, 9821, entity.EntityCategory(1), nestLocalKind)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := mgr.Request(ctx, NewHandlerName("old_cold"), cold, nil)
		if !errors.Is(err, entity.ErrColdLoadInLogic) || loader.calls.Load() != 0 || mgr.Stats().Queue.IO.Started != 0 {
			t.Fatalf("custom Getter without IsLoaded was probed and moved to the I/O preparation stage: err=%v loads=%d io started=%d", err, loader.calls.Load(), mgr.Stats().Queue.IO.Started)
		}
	})
	t.Run("custom_getter_with_checker_auto_slows", func(t *testing.T) {
		access, _, loader := newLoaderAccess(t)
		g := &loadedCheckerGetter{contractGetter: contractGetter{access: access}}
		mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{2, 8}))
		mgr.MustRegisterHandlerWithMeta(NewHandlerName("checked_cold"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			if es[0] == nil {
				return nil, errors.New("declared target missing in handler")
			}
			return "ok", nil
		}, HandlerMeta{})
		startColdTargetEngine(t, mgr)
		cold := mustBuildCastID(t, 9831, entity.EntityCategory(1), nestLocalKind)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if ret, err := mgr.Request(ctx, NewHandlerName("checked_cold"), cold, nil); err != nil || ret != "ok" {
			t.Fatalf("cold target with IsLoaded getter: ret=%v err=%v loads=%d", ret, err, loader.calls.Load())
		}
		if loader.calls.Load() != 1 || g.isLoaded.Load() == 0 {
			t.Fatalf("loads=%d isLoaded calls=%d, want one slow-stage load decided by IsLoaded", loader.calls.Load(), g.isLoaded.Load())
		}
		if n := g.outsideWorker.Load(); n != 0 {
			t.Fatalf("admission called Getter.Get %d time(s) outside any worker", n)
		}
	})
}

// handler 在业务阶段用动态 Cast 访问未加载目标：业务拿到可 errors.Is 的 ErrColdLoadInLogic 并可降级，不做 I/O。
// 历史：RR-20260926-26。
func TestShortCastColdTargetLetsBusinessFallBack(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	withCastGroupFunc(t)
	hot := mustBuildCastID(t, 8301, castPlayerCategory, castPlayerKind)
	cold := mustBuildCastID(t, 8302, castAllianceCategory, castAllianceKind)
	if err := manager.TryAdd(newMockEntityWithKind(hot, castPlayerCategory, castPlayerKind)); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{1, 8}))
	fallback := false
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("optional_friend"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if _, err := guardFixtureCastOne[entity.IThreadSafeEntity](cold); err != nil {
			if errors.Is(err, entity.ErrColdLoadInLogic) {
				fallback = true
				return "friend offline", nil
			}
			return nil, err
		}
		return "friend online", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ret, err := mgr.Request(ctx, NewHandlerName("optional_friend"), hot, nil)
	if err != nil || ret != "friend offline" || !fallback {
		t.Fatalf("ret=%v err=%v fallback=%v: fast-stage cold Cast miss did not reach the business as ErrColdLoadInLogic", ret, err, fallback)
	}
	if loads := loader.calls.Load(); loads != 0 {
		t.Fatalf("undeclared dynamic Cast target was loaded %d times", loads)
	}
}

// I/O 阶段冷加载由先到的请求领头：领头请求截止后结束、Msg 回收，同一 flight 里预算更长的等待方仍拿到结果；
// 加载不随领头方的 ctx 取消，发布走 Nest 绑定给 ManagerAccess 的 RunLocal，仍在业务 worker 上执行。
// 历史：RR-20260926-54。

type gatedStageLoader struct {
	manager      *entity.EntityManager
	entered      chan struct{}
	release      chan struct{}
	loads        atomic.Int32
	publishedOff atomic.Bool
}

func (l *gatedStageLoader) LoadEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	l.loads.Add(1)
	if fctx.InShortWorker() {
		return nil, errors.New("loader ran in business worker")
	}
	l.entered <- struct{}{}
	select {
	case <-l.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var value entity.IThreadSafeEntity
	var createErr error
	err := entity.RunLocal(ctx, func() {
		if !fctx.InShortWorker() {
			l.publishedOff.Store(true)
		}
		value = newMockEntityWithKind(id, entity.ResolveEntityID(id).Category, kind)
		createErr = l.manager.TryAdd(value)
	})
	return value, errors.Join(err, createErr)
}

func TestIOPreparationLeaderTimeoutDoesNotCutTheSharedLoad(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &gatedStageLoader{manager: manager, entered: make(chan struct{}, 4), release: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{2, 8}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("touch"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return len(es), nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	startColdTargetEngine(t, mgr)
	cold := mustBuildCastID(t, 8501, entity.EntityCategory(1), nestLocalKind)

	// 领头方：Nest 请求，预算很短（代表 RR-36 的登录预算）。
	leaderCtx, cancelLeader := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancelLeader()
	leader := make(chan error, 1)
	go func() {
		_, err := mgr.Request(leaderCtx, NewHandlerName("touch"), cold, nil)
		leader <- err
	}()
	select {
	case <-loader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow preparation never reached the loader")
	}
	// 等待方：同一实体、预算更长，直接经 ManagerAccess 加入同一 flight。
	waiterCtx, cancelWaiter := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelWaiter()
	waiter := make(chan error, 1)
	go func() {
		value, err := access.Get(waiterCtx, cold, entity.EntityCategoryNone)
		if err == nil && value == nil {
			err = errors.New("nil entity")
		}
		waiter <- err
	}()
	select {
	case err := <-leader:
		if err == nil {
			t.Fatal("the leader finished although its load is still gated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader did not return when its budget expired")
	}
	// 等领头请求在I/O 池、业务池都彻底结束（Msg 已回收），再放行加载：发布只能走绑定的业务池入口。
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		queue := mgr.Stats().Queue
		if queue.IO.Running == 0 && queue.Short.Running == 0 && queue.ContinuationRunning == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the leading request is still running: %+v", queue)
		}
	}
	close(loader.release)
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatalf("the waiter with a longer budget failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the shared load's result")
	}
	if loader.publishedOff.Load() {
		t.Fatal("the loaded entity was published off the business pool")
	}
	if n := loader.loads.Load(); n != 1 {
		t.Fatalf("loads=%d, want the waiter to share the leader's flight", n)
	}
	// 实体已发布，后续请求直接在业务池执行。
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if ret, err := mgr.Request(ctx, NewHandlerName("touch"), cold, nil); err != nil || ret != 1 {
		t.Fatalf("request after the shared load: ret=%v err=%v", ret, err)
	}
}

// absentLoader 按 AggregateLoader 契约报告“权威里没有这个实体”（DataEngine 的 ErrEntityAggregateNotFound
// 也是这样满足 entity.ErrAuthorityEntityNotFound 的）。
type absentLoader struct{ calls atomic.Int32 }

func (l *absentLoader) LoadEntity(_ context.Context, id int64, _ entity.EntityKind) (entity.IThreadSafeEntity, error) {
	l.calls.Add(1)
	return nil, fmt.Errorf("%w: entity=%d", entity.ErrAuthorityEntityNotFound, id)
}

// 首登前就发业务请求：声明目标在权威里不存在。回复必须带稳定码 errcode.CodeEntityNotFound（客户端据此
// 先登录 / 创角），不能坍缩成 CodeInternal；原错误仍可 errors.Is 判别（生成 lifecycle 的 GetOrCreate 依赖它）。
// 单目标、多目标、分组多目标三种准入形态都经 I/O 准备冷加载，各自覆盖。DESIGN-TODO D7。
func TestColdTargetMissingInAuthorityAnswersEntityNotFoundCode(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &absentLoader{}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{1, 8}))
	ran := atomic.Int32{}
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("buy"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		ran.Add(1)
		return nil, nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	startColdTargetEngine(t, mgr)

	absent := mustBuildCastID(t, 8701, entity.EntityCategory(1), nestLocalKind)
	other := mustBuildCastID(t, 8702, entity.EntityCategory(1), nestLocalKind)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cases := map[string]func() error{
		"single": func() error { _, err := mgr.Request(ctx, NewHandlerName("buy"), absent, nil); return err },
		"multi": func() error {
			_, err := mgr.RequestMulti(ctx, NewHandlerName("buy"), []int64{absent, other}, nil)
			return err
		},
		"group": func() error {
			_, err := mgr.RequestMultiGroup(ctx, NewHandlerName("buy"), [][]int64{{absent}, {other}}, nil)
			return err
		},
	}
	for name, request := range cases {
		err := request()
		if err == nil {
			t.Fatalf("%s: request to an absent entity succeeded", name)
		}
		if code, reason := errcode.ClientError(err); code != errcode.CodeEntityNotFound {
			t.Errorf("%s: ClientError = (%d, %q), want CodeEntityNotFound=%d; err=%v", name, code, reason, errcode.CodeEntityNotFound, err)
		}
		if !errors.Is(err, entity.ErrAuthorityEntityNotFound) {
			t.Errorf("%s: the loader's not-found is no longer errors.Is-visible: %v", name, err)
		}
	}
	if ran.Load() != 0 {
		t.Fatalf("handler ran %d time(s) for an absent target", ran.Load())
	}
	if loader.calls.Load() == 0 {
		t.Fatal("the absent target was never cold-loaded; the test did not reach the path it guards")
	}
}

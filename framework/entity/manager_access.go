package entity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// ManagerAccess adapts the in-memory EntityManager to the execution and remote
// loading contracts without installing package globals. A service owns one
// instance and injects it into Nest and Remote Entity mods.
type ManagerAccess struct {
	manager         *EntityManager
	loaderMu        sync.RWMutex
	loader          AggregateLoader
	loaderStopped   context.Context // 当前 loader 注册被注销时结束（停机取消在途共享加载）
	loaderID        uint64
	loadConcurrency int
	loadTimeout     time.Duration
	// localExecutor 是 Nest 绑定的快池入口（NestMgr.RunLocal，经 LocalExecutorBinder）：领头调用方
	// 离开后，共享加载经它把发布实体交回快池；卸载后重载（RR-20260926-59）也经它发布。
	localExecutor atomic.Pointer[func(func()) error]
	flightMu      sync.Mutex
	flights       map[int64]*entityLoadFlight
	// resync：卸载后仍有订阅者时的主动重载（ConfigureUnloadResync，RR-20260926-59）；nil 表示未接线。
	resync atomic.Pointer[unloadResync]
}

// DefaultEntityLoadTimeout 是一次共享冷加载的框架上限（RR-20260926-54）。加载与调用方解耦后，
// 调用方的截止时间不再约束它；这个上限保证没有等待方的加载（投影持续失败、store 不作答）
// 最终结束、释放 flight，之后的请求重新发起加载。取值远大于各调用方预算（Nest 请求、登录、
// dispatch 默认 2～3s），只作兜底。
const DefaultEntityLoadTimeout = 30 * time.Second

var (
	// ErrEntityLoadTimeout 是共享加载超过框架加载上限时 ctx 的 Cause；等待方拿到的错误同时满足
	// errors.Is(err, context.DeadlineExceeded)。
	ErrEntityLoadTimeout = errors.New("entity manager access: shared entity load exceeded the framework load timeout")
	// ErrEntityLoaderStopped 是 loader 被注销（DataEngine Runtime 停机）时在途共享加载 ctx 的 Cause；
	// 等待方拿到的错误同时满足 errors.Is(err, context.Canceled)。
	ErrEntityLoaderStopped = errors.New("entity manager access: aggregate loader stopped; in-flight load cancelled")
)

// entityLoadFlight deduplicates concurrent cold loads of one entity: the
// first caller starts LoadEntity on its own goroutine, everyone (the first
// caller included) waits on done or leaves on its own context. Without it a
// hot entity's cache miss stampedes the database with one load per caller.
type entityLoadFlight struct {
	done     chan struct{}
	value    IThreadSafeEntity
	err      error
	panicked bool

	// leaderMu 保护领头调用方的本地执行器交接：领头方仍在等待时，加载里的 RunLocal 走它自己
	// ctx 上的执行器（Nest 慢阶段即本条消息的快续行，与修前相同）；领头方离开时置 leaderGone，
	// 之后改走 ManagerAccess 绑定的快池入口。离开要等正在进行的续行结束——续行仍在使用领头方
	// 的 Msg，Msg 在领头方返回后会被回收。
	leaderMu   sync.Mutex
	leaderRun  func(func()) error
	leaderGone bool
	leaveOnce  sync.Once

	// leaderSteps / leaderAway（RR-20260927-27）：领头方在 Nest 之外、没有指定执行器、并且持有实体锁时非 nil。
	// 它仍在等待时，加载里的本地步骤经 leaderSteps 交回领头方自己的 goroutine 执行——RR-54 之前发布就在那里，
	// 可重入锁不阻塞；领头方离开时关闭 leaderAway，之后的步骤改走绑定的快池入口或就地执行。
	leaderSteps chan *leaderLocalStep
	leaderAway  chan struct{}
}

// leaderLocalStep 是交给领头方 goroutine 执行的一个本地步骤；fn 的 panic 带回加载 goroutine，按加载 panic 处理。
type leaderLocalStep struct {
	fn        func()
	done      chan struct{}
	panicked  bool
	recovered any
}

func (step *leaderLocalStep) run() {
	defer close(step.done)
	defer func() {
		if r := recover(); r != nil {
			step.panicked, step.recovered = true, r
		}
	}()
	step.fn()
}

func NewManagerAccess(manager *EntityManager) *ManagerAccess {
	return &ManagerAccess{manager: manager, loadConcurrency: 8, flights: make(map[int64]*entityLoadFlight)}
}

// ConfigureLoadTimeout 设置一次共享冷加载的框架上限；<=0 忽略，未设置时为 DefaultEntityLoadTimeout。
// 对之后开始的加载生效。
func (access *ManagerAccess) ConfigureLoadTimeout(timeout time.Duration) {
	if access == nil || timeout <= 0 {
		return
	}
	access.loaderMu.Lock()
	access.loadTimeout = timeout
	access.loaderMu.Unlock()
}

// BindLocalExecutor 实现 nest.LocalExecutorBinder：Nest 构造时把 NestMgr.RunLocal 交给作为
// Getter 的 ManagerAccess。共享加载的领头调用方离开后，加载经它把实体发布交回快池；未绑定
// （没有 Nest）时就地执行，与独立使用 Entity 时的 RunLocal 相同。
func (access *ManagerAccess) BindLocalExecutor(run func(func()) error) {
	if access == nil || run == nil {
		return
	}
	access.localExecutor.Store(&run)
}

func (access *ManagerAccess) ConfigureLoadConcurrency(concurrency int) {
	if access == nil || concurrency <= 0 {
		return
	}
	access.loaderMu.Lock()
	access.loadConcurrency = concurrency
	access.loaderMu.Unlock()
}

func (access *ManagerAccess) Manager() *EntityManager {
	if access == nil {
		return nil
	}
	return access.manager
}

func (access *ManagerAccess) RegisterOnEntityRelease(hook func(IThreadSafeEntity)) (func(), error) {
	if access == nil || access.manager == nil {
		return nil, ErrEntityNotManaged
	}
	return access.manager.RegisterOnEntityRelease(hook), nil
}

func (access *ManagerAccess) RegisterDeleteAdmitter(admitter DeleteAdmitter) (func(), error) {
	if access == nil || access.manager == nil {
		return nil, ErrEntityNotManaged
	}
	return access.manager.RegisterDeleteAdmitter(admitter)
}

// ConfigureLoader 安装冷加载器，返回的函数注销它。注销同时取消这次注册下所有在途的共享加载
// （Cause 为 ErrEntityLoaderStopped）：DataEngine Runtime 停机时先注销 loader，加载不会在停机后
// 继续等投影或读 store。注销可重复调用；loader 已被新注册替换时只取消自己的在途加载。
func (access *ManagerAccess) ConfigureLoader(loader AggregateLoader) (func(), error) {
	if access == nil || access.manager == nil || loader == nil {
		return nil, fmt.Errorf("entity manager access: aggregate loader is required")
	}
	stopped, stop := context.WithCancelCause(context.Background())
	access.loaderMu.Lock()
	access.loaderID++
	id := access.loaderID
	access.loader = loader
	access.loaderStopped = stopped
	access.loaderMu.Unlock()
	return func() {
		access.loaderMu.Lock()
		if access.loaderID == id {
			access.loader = nil
			access.loaderStopped = nil
		}
		access.loaderMu.Unlock()
		stop(ErrEntityLoaderStopped)
	}, nil
}

// entityLoaderBinding 是一次冷加载开始时读到的 loader 注册：loader 本身、它的停机信号与框架加载上限。
type entityLoaderBinding struct {
	loader  AggregateLoader
	stopped context.Context
	timeout time.Duration
}

func (access *ManagerAccess) currentLoader() entityLoaderBinding {
	access.loaderMu.RLock()
	defer access.loaderMu.RUnlock()
	return entityLoaderBinding{loader: access.loader, stopped: access.loaderStopped, timeout: access.loadTimeout}
}

// loadContext 为共享加载派生与调用方解耦的 ctx：保留领头调用方 ctx 的值（trace 等），去掉它的
// 取消与截止（context.WithoutCancel），只受框架加载上限与 loader 注销约束。
func (binding entityLoaderBinding) loadContext(leader context.Context) (context.Context, func()) {
	timeout := binding.timeout
	if timeout <= 0 {
		timeout = DefaultEntityLoadTimeout
	}
	timed, cancelTimeout := context.WithTimeoutCause(context.WithoutCancel(leader), timeout,
		fmt.Errorf("%w (%v)", ErrEntityLoadTimeout, timeout))
	if binding.stopped == nil {
		return timed, cancelTimeout
	}
	loadCtx, cancelLoad := context.WithCancelCause(timed)
	stopped := binding.stopped
	stopWatch := context.AfterFunc(stopped, func() { cancelLoad(context.Cause(stopped)) })
	return loadCtx, func() {
		stopWatch()
		cancelLoad(nil)
		cancelTimeout()
	}
}

func (access *ManagerAccess) Get(ctx context.Context, id int64, category EntityCategory) (IThreadSafeEntity, error) {
	if access == nil || access.manager == nil || id == 0 {
		return nil, nil
	}
	meta := ResolveEntityID(id)
	if value := access.manager.GetWithCategory(meta.FullID, category); value != nil {
		return value, nil
	}
	binding := access.currentLoader()
	if binding.loader == nil {
		return nil, nil
	}
	if LoadedEntitiesOnly(ctx) {
		return nil, coldLoadInLogicError("entity.ManagerAccess.Get", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	value, err := access.loadEntityShared(ctx, meta.FullID, meta.Kind, binding)
	if err != nil {
		return nil, err
	}
	if value != nil && category != EntityCategoryNone && value.GetEntityCategory() != category {
		return nil, fmt.Errorf("entity manager access: loaded entity %d category mismatch", id)
	}
	return value, nil
}

// IsLoaded 实现 LoadedChecker：一次分片 map 查找，未命中时再读一次加载器配置；
// 不加载、不等待 singleflight、不 Touch，可在任何 goroutine 上调用。
func (access *ManagerAccess) IsLoaded(id int64) bool {
	if access == nil || access.manager == nil || id == 0 {
		return true
	}
	if access.manager.Get(id) != nil {
		return true
	}
	access.loaderMu.RLock()
	hasLoader := access.loader != nil
	access.loaderMu.RUnlock()
	return !hasLoader
}

// coldLoadInLogicError 是快阶段冷缺失的唯一答复：它发生在任何 I/O、加载 goroutine 和
// singleflight 等待之前，本身不阻塞，所以返回可 errors.Is 判别的 ErrColdLoadInLogic，
// 让业务按“目标未加载”降级（RR-02 契约）。RR-06 曾在这里 panic，业务因此失去降级能力
// （RR-20260926-26）；fail-fast 只留给真正会等待的入口（Repository 冷加载、投影等待、
// Remote 准备等）。在快 worker 上额外包裹 fctx.ErrBlockingInFastWorker，便于定位入口与 handler。
func coldLoadInLogicError(operation string, id int64) error {
	if err := fctx.BlockingError(operation); err != nil {
		return fmt.Errorf("%w: entity %d: %w", ErrColdLoadInLogic, id, err)
	}
	return fmt.Errorf("%w: entity %d", ErrColdLoadInLogic, id)
}

// loadEntityShared collapses concurrent loads of the same entity into one
// LoadEntity call. Results (including errors) are shared with every waiter;
// the flight is removed before done closes, so a retry after a failure
// starts a fresh load.
//
// 加载与调用方解耦（RR-20260926-54）：第一个调用方（领头方）在独立 goroutine 上启动 LoadEntity，
// ctx 由 entityLoaderBinding.loadContext 派生——保留领头方 ctx 的值，不随它取消或截止，只受框架
// 加载上限与 loader 注销（停机）约束。每个等待方（含领头方）只按自己的 ctx 离开；最后一个等待方
// 离开也不取消在途加载，加载完成后结果照常经 loader 发布进 EntityManager，之后的 Get 直接命中。
// 修前 LoadEntity 直接用领头方的 ctx，EnterGame 以 2s 登录预算领头时（RR-20260926-36），同一 flight
// 里预算更长的等待方（同一玩家的 Nest 慢阶段准备）在 2s 时一起得到 DeadlineExceeded。
//
// Finishing the flight is a DEFER, and that is the load-bearing part
// (RR-20260919-08). A panic anywhere under LoadEntity — the store, the
// builder, a decoder, an OnInitFinish — used to skip the removal and the
// close, leaving a flight nobody would ever finish: every later request for
// that entity waited on a channel that never closed and got back its own
// deadline, and the retry never reached the loader because a load "was
// already in flight". The load goroutine recovers the panic (re-panicking
// there would take the process down), logs it with its stack, and every
// waiter gets a real error; the leader, if it is still waiting, keeps seeing
// it as a panic, because a caller whose load blew up must not be told it
// worked.
func (access *ManagerAccess) loadEntityShared(ctx context.Context, fullID int64, kind EntityKind, binding entityLoaderBinding) (IThreadSafeEntity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	access.flightMu.Lock()
	flight, joined := access.flights[fullID]
	if !joined {
		// 调用方查过内存之后、拿到 flightMu 之前，上一轮加载可能刚发布并撤掉 flight（flight 在发布之后才删除）：
		// 在同一把锁下复查，不为已发布的实体再读一次权威（卸载后主动重载与业务访问并发时即此窗口，RR-20260926-59）。
		if access.manager != nil {
			if value := access.manager.Get(fullID); value != nil {
				access.flightMu.Unlock()
				return value, nil
			}
		}
		flight = &entityLoadFlight{done: make(chan struct{}), leaderRun: localExecutorOf(ctx)}
		if flight.leaderRun == nil && currentGoroutineHoldsEntityLocks() {
			flight.leaderSteps = make(chan *leaderLocalStep)
			flight.leaderAway = make(chan struct{})
		}
		if access.flights == nil {
			access.flights = make(map[int64]*entityLoadFlight)
		}
		access.flights[fullID] = flight
	}
	access.flightMu.Unlock()
	if !joined {
		go access.runEntityLoad(ctx, fctx.CaptureSnapshot(), fullID, kind, binding, flight)
	}

	// 只有领头方接收交回的本地步骤；nil channel 的分支永不就绪。领头方执行步骤期间不看 ctx，
	// 所以它按自己的 ctx 离开最多晚一个本地步骤（与 Nest 领头方等快续行结束相同）。
	var steps chan *leaderLocalStep
	if !joined {
		steps = flight.leaderSteps
		// RR-20260927-29：领头方无论怎样离开（自己的 ctx 到期、加载正常完成、加载 panic 重新抛出）都要登记离开。
		// 之前只有 ctx 分支调用 leaderLeft：加载正常完成后 loader 另起的 goroutine 若仍用加载 ctx 调 RunLocal，
		// 持锁的非 Nest 领头方已不再接收 leaderSteps、leaderAway 又从未关闭，runOnLeader 永久阻塞；Nest 领头方
		// 则会把迟到步骤交给已返回（Msg 将被回收）的那条消息的快续行。离开后迟到步骤改走绑定执行器或就地执行。
		defer flight.leaderLeft()
	}
	for {
		select {
		case <-flight.done:
			if !joined && flight.panicked {
				panic(flight.err)
			}
			return flight.value, flight.err
		case step := <-steps:
			step.run()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// currentGoroutineHoldsEntityLocks 报告当前 goroutine 的 GuardScope 是否持有实体锁（RR-20260927-27）。
// 不持锁的领头方，加载里的发布无论在哪个 goroutine 上都等不到它，保持 RR-54 的就地执行。
func currentGoroutineHoldsEntityLocks() bool {
	scope := CurrentGuardScope()
	return scope != nil && scope.guard != nil && scope.guard.GuardedCount() > 0
}

// runEntityLoad 在自己的 goroutine 上执行一次共享加载并结束 flight。它不是快 worker：LoadEntity 里
// 需要 Entity 锁的发布步骤经 flight.runLocal 交回本地执行池。fctx 请求数据（meta、trace）沿用领头方
// 的快照，Base 换成解耦后的加载 ctx。
func (access *ManagerAccess) runEntityLoad(leader context.Context, snapshot fctx.ContextSnapshot, fullID int64, kind EntityKind, binding entityLoaderBinding, flight *entityLoadFlight) {
	loadCtx, cancel := binding.loadContext(leader)
	defer cancel()
	loadCtx = WithLocalExecutor(loadCtx, flight.runLocal(access))
	current, release := fctx.NewContext(fctx.WithSnapshot(snapshot))
	current.Base = loadCtx
	defer release()

	settled := false
	defer func() {
		if !settled {
			recovered := recover()
			flight.value, flight.err, flight.panicked = nil, fmt.Errorf(
				"entity manager access: loading entity %d panicked: %v", fullID, recovered), true
			slog.Error("entity manager access: shared entity load panicked", "entity", fullID, "panic", recovered, "stack", string(debug.Stack()))
		}
		access.flightMu.Lock()
		if access.flights[fullID] == flight {
			delete(access.flights, fullID)
		}
		access.flightMu.Unlock()
		close(flight.done)
	}()

	value, err := binding.loader.LoadEntity(loadCtx, fullID, kind)
	if err != nil && loadCtx.Err() != nil {
		// 加载被框架上限或停机结束：把 Cause 带给等待方，便于区分“自己的截止”与“加载被结束”。
		if cause := context.Cause(loadCtx); cause != nil && !errors.Is(err, cause) {
			err = fmt.Errorf("%w (%w)", err, cause)
		}
	}
	flight.value, flight.err = value, err
	settled = true
}

// localExecutorOf 返回 ctx 上由 WithLocalExecutor 指定的本地执行器，未指定为 nil。
func localExecutorOf(ctx context.Context) func(func()) error {
	if ctx == nil {
		return nil
	}
	run, _ := ctx.Value(localExecutorKey{}).(func(func()) error)
	return run
}

// runLocal 是共享加载 ctx 上的本地执行器。领头方仍在等待时：
//   - 它指定了执行器（Nest 慢阶段的快续行）：沿用它，执行期间持有 leaderMu，领头方此时离开要等这一步结束；
//   - 它在 Nest 之外、持有实体锁（leaderSteps 非 nil）：把步骤交回领头方 goroutine 执行（RR-20260927-27）；
//   - 其余（Nest 之外、不持锁）：就地执行。
//
// 领头方离开后改走 ManagerAccess 绑定的快池入口，未绑定时就地执行。
//
// RR-20260927-27：之前 Nest 之外的领头方也是“持 leaderMu 就地执行”。发布若要锁领头方正持有的实体，加载 goroutine
// 在发布里等那把锁，领头方在等加载；它的 ctx 到期后 leaderLeft 又要等 leaderMu——永久死锁，RR-54“领头方按自己的
// ctx 离开”不成立。RR-54 之前发布在领头方 goroutine 上，可重入锁不阻塞；现在交回那里执行，不持 leaderMu 等待。
// 领头方不会是快 worker（快阶段 Getter 不冷加载），在它的 goroutine 上执行本地步骤不属于快池内等待。
func (flight *entityLoadFlight) runLocal(access *ManagerAccess) func(func()) error {
	return func(fn func()) error {
		if flight.leaderSteps != nil {
			if flight.runOnLeader(fn) {
				return nil
			}
		} else {
			flight.leaderMu.Lock()
			if !flight.leaderGone {
				defer flight.leaderMu.Unlock()
				if flight.leaderRun != nil {
					return flight.leaderRun(fn)
				}
				fn()
				return nil
			}
			flight.leaderMu.Unlock()
		}
		if run := access.localExecutor.Load(); run != nil {
			return (*run)(fn)
		}
		fn()
		return nil
	}
}

// runOnLeader 把 fn 交给仍在等待的领头方 goroutine 执行并等它完成；领头方已离开、没有接手时返回 false，
// 由调用方改走领头方离开后的路径。fn 的 panic 在加载 goroutine 上重新抛出，由 runEntityLoad 按加载 panic 处理。
func (flight *entityLoadFlight) runOnLeader(fn func()) bool {
	step := &leaderLocalStep{fn: fn, done: make(chan struct{})}
	select {
	case flight.leaderSteps <- step:
	case <-flight.leaderAway:
		return false
	}
	<-step.done
	if step.panicked {
		panic(step.recovered)
	}
	return true
}

// leaderLeft 在领头方离开 loadEntityShared 时调用（任何返回方式，含 panic；leaveOnce 保证只生效一次）：之后的本地步骤
// 不再使用它的执行器，也不再交给它的 goroutine。取 leaderMu 会等正在用领头方执行器的续行结束（续行仍在使用领头方的 Msg）；
// leaderSteps 路径的步骤不持 leaderMu，领头方离开不等它们。
func (flight *entityLoadFlight) leaderLeft() {
	flight.leaveOnce.Do(func() {
		flight.leaderMu.Lock()
		flight.leaderGone = true
		flight.leaderRun = nil
		flight.leaderMu.Unlock()
		if flight.leaderAway != nil {
			close(flight.leaderAway)
		}
	})
}

func (access *ManagerAccess) GetMany(ctx context.Context, ids []int64, categories []EntityCategory) ([]IThreadSafeEntity, error) {
	result := make([]IThreadSafeEntity, len(ids))
	if access == nil || access.manager == nil {
		return result, nil
	}
	type loadRequest struct {
		id      int64
		indices []int
	}
	requestsByID := make(map[int64]*loadRequest)
	requests := make([]*loadRequest, 0, len(ids))
	for index, id := range ids {
		meta := ResolveEntityID(id)
		if value := access.manager.Get(meta.FullID); value != nil {
			if index < len(categories) && categories[index] != EntityCategoryNone && value.GetEntityCategory() != categories[index] {
				return nil, fmt.Errorf("entity manager access: entity %d category mismatch", id)
			}
			result[index] = value
			continue
		}
		request := requestsByID[meta.FullID]
		if request == nil {
			request = &loadRequest{id: meta.FullID}
			requestsByID[meta.FullID] = request
			requests = append(requests, request)
		}
		request.indices = append(request.indices, index)
	}
	if len(requests) == 0 {
		return result, nil
	}
	// 快阶段只读内存，缺失实体也不能创建加载 goroutine 或等待 singleflight。
	if LoadedEntitiesOnly(ctx) {
		access.loaderMu.RLock()
		hasLoader := access.loader != nil
		access.loaderMu.RUnlock()
		if hasLoader {
			return nil, coldLoadInLogicError("entity.ManagerAccess.GetMany", requests[0].id)
		}
		return result, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	access.loaderMu.RLock()
	concurrency := access.loadConcurrency
	access.loaderMu.RUnlock()
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(requests) {
		concurrency = len(requests)
	}
	loadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *loadRequest)
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	var firstErr error
	worker := func() {
		defer wg.Done()
		for request := range jobs {
			value, err := access.Get(loadCtx, request.id, EntityCategoryNone)
			resultMu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
			if err == nil {
				for _, index := range request.indices {
					if value != nil && index < len(categories) && categories[index] != EntityCategoryNone && value.GetEntityCategory() != categories[index] {
						if firstErr == nil {
							firstErr = fmt.Errorf("entity manager access: loaded entity %d category mismatch", request.id)
							cancel()
						}
						continue
					}
					result[index] = value
				}
			}
			resultMu.Unlock()
		}
	}
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go worker()
	}
dispatchLoop:
	for _, request := range requests {
		select {
		case jobs <- request:
		case <-loadCtx.Done():
			break dispatchLoop
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (access *ManagerAccess) LoadRemoteEntity(ctx context.Context, id int64, kind EntityKind) (IThreadSafeRemoteEntity, error) {
	if access == nil || access.manager == nil || id == 0 {
		return nil, nil
	}
	value, err := access.Get(ctx, id, EntityCategoryNone)
	if err != nil {
		return nil, err
	}
	if value == nil || (kind != EntityKindNone && value.GetEntityKind() != kind) {
		return nil, nil
	}
	remote, _ := value.(IThreadSafeRemoteEntity)
	return remote, nil
}

func (access *ManagerAccess) LookupLocalRemoteEntity(id int64, kind EntityKind) IThreadSafeRemoteEntity {
	if access == nil || access.manager == nil || id == 0 {
		return nil
	}
	value := access.manager.Get(id)
	if value == nil || (kind != EntityKindNone && value.GetEntityKind() != kind) {
		return nil
	}
	remote, _ := value.(IThreadSafeRemoteEntity)
	return remote
}

func (access *ManagerAccess) Len() int {
	if access == nil || access.manager == nil {
		return 0
	}
	return access.manager.Len()
}

func (access *ManagerAccess) Range(fn func(IThreadSafeEntity) bool) {
	if access != nil && access.manager != nil && fn != nil {
		access.manager.Range(fn)
	}
}

func (access *ManagerAccess) GetGroupEntity(groupID, entityID int64) IThreadSafeEntity {
	if access == nil || access.manager == nil {
		return nil
	}
	return access.manager.GetGroupEntity(groupID, entityID)
}

func (access *ManagerAccess) GetGroupEntities(groupID int64) []IThreadSafeEntity {
	if access == nil || access.manager == nil {
		return nil
	}
	return access.manager.GetGroupEntities(groupID)
}

func (access *ManagerAccess) UpdateEntityGroup(value IThreadSafeEntity, groupID int64) error {
	if access == nil || access.manager == nil {
		return ErrEntityNotManaged
	}
	return access.manager.UpdateEntityGroup(value, groupID)
}

func (access *ManagerAccess) Create(param *EntityCreateParam) (IThreadSafeEntity, error) {
	if access == nil || access.manager == nil {
		return nil, ErrEntityNotManaged
	}
	return access.manager.Create(param)
}

func (access *ManagerAccess) CreateInScope(scope *GuardScope, param *EntityCreateParam) (IThreadSafeEntity, error) {
	if access == nil || access.manager == nil {
		return nil, ErrEntityNotManaged
	}
	return access.manager.CreateInScope(scope, param)
}

func (access *ManagerAccess) Destroy(ctx context.Context, value IThreadSafeEntity, reason EntityDestroyReason, deleteFromDB bool) error {
	if access == nil || access.manager == nil {
		return ErrEntityNotManaged
	}
	return access.manager.Destroy(ctx, value, reason, deleteFromDB)
}

// Unload 把实例从本进程内存卸载、不删持久数据：内存状态已不可信、须从权威重建时使用（DataEngine 驱逐
// 被 lease fence 跳过的原生步骤留下的实体，RR-20260926-30；Remote 事务被持久拒绝后的实例，RR-20260926-39）。
// 内部是 EntityManager.Destroy(deleteFromDB=false, DestroyReasonMemoryUnload)：自己取实体锁、先从索引删除，
// 在途引用由 Touch 计数保护（归零才清理），Guard 对已移除实体拒绝加锁。之后 Get 未命中、经已配置的 loader
// 从权威重新加载。实例已被卸载或已不是当前托管实例时返回 nil（幂等）。
//
// Sync：实例一离开 EntityManager 就关闭它的 SubjectSyncState（不等在途引用归零后的 ClearBase），被丢弃的内容
// 不再捕获；订阅保持登记。若该 subject 仍有订阅者且已接上 ConfigureUnloadResync，框架在快池之外主动从权威
// 重载并 Rebind 全量，重载不了时退回 remove（RR-20260926-59）；这里只做登记，不做 I/O、不等待。未接线时
// 订阅者保留原对象，直到实体经 kit 的 EntityRepository.OnEntityLoaded → Rebind 被重新加载。
// 需要实体锁：调用方在本地执行入口（快池，或 Nest 未装配时就地）调用。
func (access *ManagerAccess) Unload(ctx context.Context, value IThreadSafeEntity) error {
	if access == nil || access.manager == nil {
		return ErrEntityNotManaged
	}
	if value == nil {
		return nil
	}
	id := value.ID()
	var state *SubjectSyncState
	if base := value.Base(); base != nil {
		state = base.Sync()
	}
	// defer：业务 OnDestroy panic 时实例也已离开 EntityManager，同样要关闭同步状态并登记重载。
	defer func() {
		if !value.IsRemoved() {
			return
		}
		state.Close()
		if resync := access.resync.Load(); resync != nil {
			resync.schedule(id)
		}
	}()
	err := access.manager.Destroy(ctx, value, DestroyReasonMemoryUnload, false)
	if errors.Is(err, ErrEntityRemoved) || errors.Is(err, ErrEntityNotManaged) {
		return nil
	}
	return err
}

// UnloadRemoteEntity 实现 IRemoteEntityUnloader，与 Unload 同一路径。
func (access *ManagerAccess) UnloadRemoteEntity(ctx context.Context, value IThreadSafeRemoteEntity) error {
	if value == nil {
		return nil
	}
	return access.Unload(ctx, value)
}

func (access *ManagerAccess) ConfigureIDGenerator(generator func() (uint64, error)) error {
	if access == nil || access.manager == nil {
		return ErrEntityNotManaged
	}
	return access.manager.ConfigureIDGenerator(generator)
}

var _ Getter = (*ManagerAccess)(nil)
var _ LoadedChecker = (*ManagerAccess)(nil)
var _ IRemoteEntityLoader = (*ManagerAccess)(nil)
var _ IRemoteEntityLocalLookup = (*ManagerAccess)(nil)
var _ IRemoteEntityUnloader = (*ManagerAccess)(nil)

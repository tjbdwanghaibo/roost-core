package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// ErrRuntimeStopped 表示 Runtime 或其组件已开始关闭，不再接受新操作。
var ErrRuntimeStopped = errors.New("dataengine runtime: shutdown has started")

// Runtime 是启动完成的 DataEngine 组合：Store、WAL、Projector、Outbox 与实体仓库。
// 它在恢复屏障之后向 EntityManager 安装加载器与删除准入，并为 Nest 提供提交器选项。
type Runtime struct {
	Store      *MongoStore
	WAL        *nestwal.WAL
	Projector  *Projector
	Outbox     *OutboxWorker
	Repository *EntityRepository

	access           *entity.ManagerAccess
	unregisterLoader func()
	unregisterDelete func()
	remoteManager    entity.IRemoteEntityManager
	onFatal          func(error)
	ready            atomic.Bool
	pipelined        PipelinedRuntimeConfig

	// 启动和关闭共享所有权；等待时遵守调用者 deadline，stopping 是终态。
	lifecycleGate   operationGate
	stopping        bool
	drainAttempted  bool
	projectorClosed bool
	walClosed       bool
	outboxClosed    bool
}

// PipelinedRuntimeConfig 是 Runtime 交给 Nest 的 pipelined 提交配置：允许名单与异步完成池。
type PipelinedRuntimeConfig struct {
	Allowlist     []string
	Async         bool
	AsyncWorkers  int
	AsyncQueueCap int
}

func NewRuntime(store *MongoStore, wal *nestwal.WAL, projector *Projector, outbox *OutboxWorker, access *entity.ManagerAccess, remoteManager entity.IRemoteEntityManager, onFatal func(error), pipelined PipelinedRuntimeConfig) (*Runtime, error) {
	if store == nil || wal == nil || projector == nil || outbox == nil || access == nil || access.Manager() == nil {
		return nil, errors.New("dataengine runtime: store, WAL, projector, outbox and entity access are required")
	}
	runtime := &Runtime{Store: store, WAL: wal, Projector: projector, Outbox: outbox, access: access, remoteManager: remoteManager, onFatal: onFatal, pipelined: pipelined}
	repository, err := NewEntityRepository(access.Manager(), store, runtime)
	if err != nil {
		return nil, err
	}
	runtime.Repository = repository
	return runtime, nil
}

// Start 先执行恢复屏障（排空历史 WAL 投影），再向业务流量开放实体加载器与事务提交器。
func (runtime *Runtime) Start(ctx context.Context) error {
	if runtime == nil || runtime.Projector == nil || runtime.Repository == nil {
		return errors.New("dataengine runtime: not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := runtime.lifecycleGate.acquire(ctx); err != nil {
		return err
	}
	defer runtime.lifecycleGate.release()
	if runtime.stopping {
		return ErrRuntimeStopped
	}
	if runtime.Ready() {
		return nil
	}
	if err := runtime.drainAtStartup(ctx); err != nil {
		return fmt.Errorf("dataengine runtime: startup projection recovery: %w", err)
	}
	unregister, err := runtime.access.ConfigureLoader(runtime.Repository)
	if err != nil {
		return err
	}
	runtime.unregisterLoader = unregister
	unregisterDelete, err := runtime.access.RegisterDeleteAdmitter(runtime.admitEntityDelete)
	if err != nil {
		runtime.unregisterLoader()
		runtime.unregisterLoader = nil
		runtime.ready.Store(false)
		return err
	}
	runtime.unregisterDelete = unregisterDelete
	runtime.Outbox.Start(context.Background())
	// 两个框架入口都安装后才发布就绪：并发的健康检查或提交器查询不能看到
	// 只有加载器、没有持久删除门的半装配 Runtime。
	runtime.ready.Store(true)
	return nil
}

// startupDrainLimit 是调用方没给期限时启动排空重试的上限（设计 T-A 第 3 条：120s）。装配经 wiring 启动时期限是
// dataengine.startup_timeout（缺省 60s，saga 设计第 5 批维护者决定），不用这个上限。
const startupDrainLimit = 2 * time.Minute

// startupDrainContext 决定启动排空能重试多久：调用方给了期限（wiring 的 dataengine.startup_timeout）就只用它，
// 不论比上限长还是短；没给期限（直接装配、测试）才套 startupDrainLimit，永久错误不会让启动一直重试。
func startupDrainContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, startupDrainLimit)
}

// drainAtStartup 把 WAL 里没投影的记录全部投影进 Mongo，在 ctx（wiring 的 dataengine.startup_timeout）内有界重试。
//
// 为什么要重试（saga 设计 T-A，DESIGN-2026-10-28 §10）：被 kill -9 的进程留下的投影事务在服务端保持打开、持锁，直到
// transactionLifetimeLimitSeconds（部署要求 20s，服务端缺省 60s）才回收；这期间对同一文档的写一直 WriteConflict。驱动在
// 一次 WithTransaction 里重跑暂时性错误，但只到 mongo.transaction_timeout 为止；之后 Flush 返回错误。这里在启动期限内退避
// 再 Flush，所以排空能等的上限是 startup_timeout，而不是两者中较小的那个。
//
// 不重试的：投影器已经 fence（致命冲突，重试只会重复同一结论）、ctx 结束、关闭开始。其余错误（含权限、网络这类重试也
// 不会好的）同样重试到期限，再带最后一次错误失败：启动期限内多等一会儿，换来遗留事务回收后能起来，不用区分驱动错误的种类。
func (runtime *Runtime) drainAtStartup(ctx context.Context) error {
	ctx, cancel := startupDrainContext(ctx)
	defer cancel()
	backoff := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := runtime.Projector.Flush(ctx)
		if err == nil {
			return nil
		}
		if runtime.Projector.fatal() != nil || ctx.Err() != nil || errors.Is(err, ErrRuntimeStopped) {
			return err
		}
		slog.Warn("dataengine runtime: startup drain failed; retrying within the startup timeout (a killed process's leftover Mongo transaction holds its locks until transactionLifetimeLimitSeconds)",
			"attempt", attempt, "retry_in", backoff, "err", err)
		metrics.IncCounter("dataengine.startup_drain_retry_total", nil, 1)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		backoff = min(2*backoff, 2*time.Second)
	}
}

func (runtime *Runtime) Ready() bool {
	return runtime != nil && runtime.ready.Load()
}

func (runtime *Runtime) NestOptions() []corenest.NestOption {
	if runtime == nil || runtime.Projector == nil || !runtime.Ready() {
		return nil
	}
	options := []corenest.NestOption{corenest.NestOptionWithTransactionCommitter(runtime.Projector)}
	if len(runtime.pipelined.Allowlist) > 0 {
		options = append(options, corenest.NestOptionWithPipelinedAllowlist(runtime.pipelined.Allowlist...))
	}
	if runtime.pipelined.Async {
		options = append(options, corenest.NestOptionWithPipelinedAsyncCompletion(runtime.pipelined.AsyncWorkers, runtime.pipelined.AsyncQueueCap))
	}
	return options
}

func (runtime *Runtime) Flush(ctx context.Context) error {
	if runtime == nil || runtime.Projector == nil {
		return nil
	}
	return runtime.Projector.Flush(ctx)
}

// Shutdown 先停 Projector，再停 Outbox worker。ctx 到期导致未完成时可再次调用：
// 组件的 Close 返回 nil 后才标记为已停止，重试只等待仍在运行的组件。关闭 Projector
// 之前的 Flush 只尝试一次；Projector 关闭后没有可 Flush 的对象，未投影的记录留在
// WAL 中由下次启动恢复。
func (runtime *Runtime) Shutdown(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := runtime.lifecycleGate.acquire(ctx); err != nil {
		return err
	}
	defer runtime.lifecycleGate.release()
	return runtime.stop(ctx, true)
}

// stop 由持有 lifecycleGate 的 Shutdown，或尚未发布 Runtime 的 Assembly 调用。
// 启动失败不再排空 WAL，只停止资源；未投影记录留给下次恢复。
func (runtime *Runtime) stop(ctx context.Context, drain bool) error {
	runtime.stopping = true
	runtime.ready.Store(false)
	if runtime.unregisterDelete != nil {
		runtime.unregisterDelete()
		runtime.unregisterDelete = nil
	}
	if runtime.unregisterLoader != nil {
		runtime.unregisterLoader()
		runtime.unregisterLoader = nil
	}
	// 服务先于 Mod 停止，此时不会再进入新的 Nest handler 或 Guard。先让已准入的
	// WAL 记录全部投影可见，再停止新的 outbox 认领；已暂存的 effect 留在 Mongo 等下次启动。
	var flushErr, projectorErr, walErr, outboxErr error
	if !runtime.drainAttempted {
		runtime.drainAttempted = true
		if drain && runtime.Projector != nil {
			flushErr = runtime.Projector.Flush(ctx)
		}
	}
	if !runtime.projectorClosed {
		if runtime.Projector != nil {
			projectorErr = runtime.Projector.Close(ctx)
		}
		runtime.projectorClosed = projectorErr == nil
	}
	// Runtime/Assembly 拥有 WAL；即使 Projector 配置为不关闭 WAL，也必须回收。
	// 先等重放退出，避免 writer 关闭与重放仍在执行的 ack 交错。
	if runtime.projectorClosed && !runtime.walClosed {
		if runtime.WAL != nil {
			walErr = runtime.WAL.Close(ctx)
		}
		runtime.walClosed = walErr == nil
	}
	if !runtime.outboxClosed {
		if runtime.Outbox != nil {
			outboxErr = runtime.Outbox.Close(ctx)
		}
		runtime.outboxClosed = outboxErr == nil
	}
	return errors.Join(flushErr, projectorErr, walErr, outboxErr)
}

var _ coredata.Store = (*MongoStore)(nil)

// WaitEntityProjection 是冷加载前的实体级可见性屏障；调用者在 I/O 准备阶段等待。
func (runtime *Runtime) WaitEntityProjection(ctx context.Context, id int64) error {
	if !runtime.Ready() {
		return coredata.ErrRecoveryIncomplete
	}
	return runtime.Projector.WaitEntityProjection(ctx, id)
}

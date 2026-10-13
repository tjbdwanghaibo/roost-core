package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
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
	// 被跳过的原生步骤要驱逐 Runtime 管理的内存实体；Projector 自己不认识 EntityManager。
	projector.evictEntities = runtime.evictStaleEntities
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
	if err := runtime.Projector.Flush(ctx); err != nil {
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

// WaitEntityProjection 是冷加载前的实体级可见性屏障；调用者在慢阶段等待。
func (runtime *Runtime) WaitEntityProjection(ctx context.Context, id int64) error {
	if !runtime.Ready() {
		return coredata.ErrRecoveryIncomplete
	}
	return runtime.Projector.WaitEntityProjection(ctx, id)
}

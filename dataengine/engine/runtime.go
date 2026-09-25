package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

var ErrRuntimeStopped = errors.New("dataengine runtime: shutdown has started")

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
	migration, err := NewMigrationRunner(projector)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{Store: store, WAL: wal, Projector: projector, Outbox: outbox, access: access, remoteManager: remoteManager, onFatal: onFatal, pipelined: pipelined}
	repository, err := NewEntityRepository(access.Manager(), store, migration, runtime)
	if err != nil {
		return nil, err
	}
	runtime.Repository = repository
	return runtime, nil
}

// Start performs the recovery barrier before making the aggregate loader or
// transaction committer available to service traffic.
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
	// Publish readiness only after both framework entry points are installed.
	// A concurrent health/committer lookup must never observe a half-wired
	// runtime with a loader but no durable delete gate.
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

// Shutdown stops the projector, then the outbox worker. It may be called again
// after an incomplete attempt (a bounded ctx that ran out): each component is
// marked stopped once its Close has returned nil, and a retry only waits on
// the ones still running. The flush that precedes the projector's close is
// attempted once — after the projector is closed there is nothing left to
// flush, and the records it could not make visible stay durable in the WAL
// for the next start.
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
	// Services stop before mods, so no new Nest handlers or guards can enter.
	// First make every admitted WAL record visible, then stop new outbox claims;
	// already staged effects remain durable in Mongo for the next start.
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

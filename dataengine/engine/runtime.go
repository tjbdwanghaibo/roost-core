package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

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

	// shutdownMu serializes Shutdown and guards the two completion marks, so
	// a retried shutdown only waits on what has not stopped yet.
	shutdownMu      sync.Mutex
	projectorClosed bool
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
	if err := runtime.Projector.Flush(ctx); err != nil {
		return fmt.Errorf("dataengine runtime: startup projection recovery: %w", err)
	}
	runtime.Outbox.Start(context.Background())
	unregister, err := runtime.access.ConfigureLoader(runtime.Repository)
	if err != nil {
		_ = runtime.Outbox.Close(ctx)
		return err
	}
	runtime.unregisterLoader = unregister
	unregisterDelete, err := runtime.access.RegisterDeleteAdmitter(runtime.admitEntityDelete)
	if err != nil {
		runtime.unregisterLoader()
		runtime.unregisterLoader = nil
		runtime.ready.Store(false)
		_ = runtime.Outbox.Close(ctx)
		return err
	}
	runtime.unregisterDelete = unregisterDelete
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
	runtime.shutdownMu.Lock()
	defer runtime.shutdownMu.Unlock()
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
	var projectorErr error
	if runtime.Projector != nil && !runtime.projectorClosed {
		flushErr := runtime.Projector.Flush(ctx)
		closeErr := runtime.Projector.Close(ctx)
		if closeErr == nil {
			runtime.projectorClosed = true
		}
		projectorErr = errors.Join(flushErr, closeErr)
	}
	var outboxErr error
	if runtime.Outbox != nil && !runtime.outboxClosed {
		outboxErr = runtime.Outbox.Close(ctx)
		if outboxErr == nil {
			runtime.outboxClosed = true
		}
	}
	return errors.Join(projectorErr, outboxErr)
}

var _ coredata.Store = (*MongoStore)(nil)

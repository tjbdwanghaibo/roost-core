package dataengine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	"github.com/tjbdwanghaibo/cube-core/entity"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"github.com/tjbdwanghaibo/cube-kit/nestwal"
)

type Runtime struct {
	Store      *MongoStore
	WAL        *nestwal.WAL
	Projector  *Projector
	Outbox     *OutboxWorker
	Repository *EntityRepository

	access           *entity.ManagerAccess
	unregisterLoader func()
	ready            atomic.Bool
	pipelined        pipelinedRuntimeConfig
}

type pipelinedRuntimeConfig struct {
	Allowlist     []string
	Async         bool
	AsyncWorkers  int
	AsyncQueueCap int
}

func newRuntime(store *MongoStore, wal *nestwal.WAL, projector *Projector, outbox *OutboxWorker, access *entity.ManagerAccess, pipelined pipelinedRuntimeConfig) (*Runtime, error) {
	if store == nil || wal == nil || projector == nil || outbox == nil || access == nil || access.Manager() == nil {
		return nil, errors.New("dataengine runtime: store, WAL, projector, outbox and entity access are required")
	}
	migration, err := NewMigrationRunner(projector)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{Store: store, WAL: wal, Projector: projector, Outbox: outbox, access: access, pipelined: pipelined}
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
	runtime.ready.Store(true)
	unregister, err := runtime.access.ConfigureLoader(runtime.Repository)
	if err != nil {
		runtime.ready.Store(false)
		_ = runtime.Outbox.Close(ctx)
		return err
	}
	runtime.unregisterLoader = unregister
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

func (runtime *Runtime) Shutdown(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	runtime.ready.Store(false)
	if runtime.unregisterLoader != nil {
		runtime.unregisterLoader()
		runtime.unregisterLoader = nil
	}
	// Services stop before mods, so no new Nest handlers or guards can enter.
	// First make every admitted WAL record visible, then stop new outbox claims;
	// already staged effects remain durable in Mongo for the next start.
	var projectorErr error
	if runtime.Projector != nil {
		projectorErr = runtime.Projector.Shutdown(ctx)
	}
	var outboxErr error
	if runtime.Outbox != nil {
		outboxErr = runtime.Outbox.Close(ctx)
	}
	return errors.Join(projectorErr, outboxErr)
}

var _ coredata.Store = (*MongoStore)(nil)

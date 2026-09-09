package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// AssemblyConfig is everything the data engine needs to build itself once the
// process has parsed its configuration. The kit Mod fills it from viper; tests
// fill it directly.
type AssemblyConfig struct {
	Mongo        MongoStoreConfig
	WAL          nestwal.Options
	Projector    ProjectorOptions
	Outbox       OutboxWorkerOptions
	EffectPrefix string
	EffectStream fnats.JetStreamConfig
	Pipelined    PipelinedRuntimeConfig
}

// AssemblyDeps are the capabilities the data engine consumes. RemoteStore and
// RemoteManager are set together when remote projection is enabled; the
// manager must then implement entity.RemoteCommitApplier.
type AssemblyDeps struct {
	Mongo         fmongo.IMongo
	JetStream     fnats.IJetStream
	Access        *entity.ManagerAccess
	RemoteStore   RemoteProjectionStore
	RemoteManager entity.IRemoteEntityManager
	OnFatal       func(error)
}

// Assembly owns the construction order and the failure rollback of the data
// engine: store at Provide time; WAL, projector, outbox and runtime at Start,
// each earlier component closed when a later one fails. The kit Mod only
// forwards lifecycle calls and reads Runtime() (P3b).
type Assembly struct {
	Store *MongoStore

	deps AssemblyDeps
	cfg  AssemblyConfig

	runtimeMu sync.RWMutex
	runtime   *Runtime
}

// Assemble validates the dependencies and builds the Mongo store, binding the
// remote projection when it is configured. Nothing is opened or written yet.
func Assemble(deps AssemblyDeps, cfg AssemblyConfig) (*Assembly, error) {
	if deps.Access == nil || deps.Access.Manager() == nil {
		return nil, errors.New("dataengine: service-scoped entity access is required")
	}
	if deps.Mongo == nil {
		return nil, errors.New("dataengine: mongo client is required")
	}
	if deps.JetStream == nil {
		return nil, errors.New("dataengine: jetstream client is required")
	}
	if (deps.RemoteStore == nil) != (deps.RemoteManager == nil) {
		return nil, errors.New("dataengine: remote projection needs both the remote manager and its atomic store")
	}
	store, err := NewMongoStore(deps.Mongo, cfg.Mongo)
	if err != nil {
		return nil, err
	}
	if deps.RemoteManager != nil {
		applier, ok := deps.RemoteManager.(entity.RemoteCommitApplier)
		if !ok {
			return nil, errors.New("dataengine: remote manager has no commit applier")
		}
		if err := store.SetRemoteProjection(deps.RemoteStore, applier); err != nil {
			return nil, err
		}
	}
	return &Assembly{Store: store, deps: deps, cfg: cfg}, nil
}

// Start ensures the Mongo infrastructure and the effect stream, opens the WAL
// and brings up projector, outbox and runtime in dependency order. When a step
// fails every component opened before it is closed, so a failed Start leaves
// no WAL handle or worker behind. ctx bounds the whole startup.
func (a *Assembly) Start(ctx context.Context) error {
	if a == nil || a.Store == nil {
		return errors.New("dataengine: not assembled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.Store.EnsureInfrastructure(ctx); err != nil {
		return fmt.Errorf("dataengine: ensure mongo infrastructure: %w", err)
	}
	if err := a.deps.JetStream.EnsureStream(ctx, a.cfg.EffectStream); err != nil {
		return fmt.Errorf("dataengine: ensure effect stream: %w", err)
	}
	wal, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		return err
	}
	projector, err := NewProjector(wal, a.Store, a.cfg.Projector)
	if err != nil {
		_ = wal.Close(ctx)
		return err
	}
	outboxStore, err := NewMongoOutboxStore(a.Store)
	if err != nil {
		_ = projector.Close(ctx)
		return err
	}
	publisher := &jetStreamOutboxPublisher{client: a.deps.JetStream, prefix: a.cfg.EffectPrefix}
	outbox, err := NewOutboxWorker(outboxStore, publisher, a.cfg.Outbox)
	if err != nil {
		_ = projector.Close(ctx)
		return err
	}
	runtime, err := NewRuntime(a.Store, wal, projector, outbox, a.deps.Access, a.deps.RemoteManager, a.deps.OnFatal, a.cfg.Pipelined)
	if err != nil {
		_ = projector.Close(ctx)
		return err
	}
	if err := runtime.Start(ctx); err != nil {
		_ = runtime.Shutdown(ctx)
		return err
	}
	a.runtimeMu.Lock()
	a.runtime = runtime
	a.runtimeMu.Unlock()
	return nil
}

// Runtime is the started runtime, or nil before Start / after Shutdown.
func (a *Assembly) Runtime() *Runtime {
	if a == nil {
		return nil
	}
	a.runtimeMu.RLock()
	defer a.runtimeMu.RUnlock()
	return a.runtime
}

// Shutdown stops the runtime (projector first, then outbox) and, once that
// has completed, forgets it. A call before Start, or after a completed
// shutdown, does nothing.
//
// "Completed" is the point: when ctx runs out while a worker is still
// draining, Shutdown reports the error and KEEPS the runtime, so a retry waits
// on the same components. Forgetting it on any outcome let the retry find nil
// and answer success while the outbox worker was still active — a caller that
// then released the underlying connections would pull them from under a live
// worker (RR-20260909-03). Runtime.Shutdown remembers which components have
// stopped, so the retry does not re-flush or re-close what is already down.
func (a *Assembly) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.runtimeMu.RLock()
	runtime := a.runtime
	a.runtimeMu.RUnlock()
	if runtime == nil {
		return nil
	}
	if err := runtime.Shutdown(ctx); err != nil {
		return err
	}
	a.runtimeMu.Lock()
	if a.runtime == runtime {
		a.runtime = nil
	}
	a.runtimeMu.Unlock()
	return nil
}

// jetStreamOutboxPublisher publishes staged effects to JetStream under the
// effect prefix, with the effect ID as the broker deduplication key.
type jetStreamOutboxPublisher struct {
	client fnats.IJetStream
	prefix string
}

func (publisher *jetStreamOutboxPublisher) Publish(ctx context.Context, item OutboxItem) error {
	if publisher == nil || publisher.client == nil || item.Effect.ID == "" || item.Effect.Topic == "" {
		return errors.New("dataengine outbox: invalid JetStream publish")
	}
	payload, err := json.Marshal(nestwal.EffectEnvelope{
		TransactionID: item.TransactionID, EffectID: item.Effect.ID, Topic: item.Effect.Topic,
		Key: item.Effect.Key, Headers: item.Effect.Headers, Payload: item.Effect.Payload,
	})
	if err != nil {
		return err
	}
	_, err = publisher.client.Publish(ctx, publisher.prefix+"."+strings.Trim(item.Effect.Topic, "."), payload, fnats.JetStreamPublishOptions{MsgID: item.Effect.ID})
	return err
}

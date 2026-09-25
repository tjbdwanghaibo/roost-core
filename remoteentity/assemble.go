package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// AssemblyDeps are the capabilities Remote Entity consumes. Redis is always
// required (coordination locks and snapshot L2). Durable ownership is stored with commits. The backend is
// either supplied directly or built from Loader on Mongo.
type AssemblyDeps struct {
	Redis   fredis.IRedis
	Mongo   fmongo.IMongo
	Loader  entity.IRemoteEntityLoader
	Backend entity.IRemoteEntityBackend
	OnFatal func(error)
}

// MongoBackendConfig configures the Mongo committer built from Loader.
type MongoBackendConfig struct {
	Database       string
	TransactionTTL time.Duration
}

// Assembly owns the manager's construction, dependency sealing, storage
// initialisation, outbox recovery and replication lifecycle. The kit Mod
// only parses configuration, publishes the three capabilities and forwards
// lifecycle calls (P3b).
type Assembly struct {
	Manager     *Manager
	LockFactory fredis.IVersionedLockFactory
	AtomicStore AtomicCommitStore

	cfg         *Config
	snapshotRep *mirror.Replicator
	interestRep *mirror.Replicator

	// 启停持有整个操作的所有权；等待者可取消，但不能提前回收持有者的资源。
	lifecycleMu   sync.Mutex
	operationDone chan struct{}
	started       bool
	stopping      bool
}

// ErrAssemblyStopped 表示 finalizer 已进入不可逆的停止流程；重新运行须重新 Assemble。
var ErrAssemblyStopped = errors.New("remote_entity: assembly is stopping or stopped")

// Assemble builds the manager with its lock factory, snapshot L2, backend and
// ownership store. localSid must be non-zero: it fences ownership.
func Assemble(deps AssemblyDeps, cfg *Config, localSid int32, mongoCfg MongoBackendConfig) (*Assembly, error) {
	if deps.Redis == nil {
		return nil, errors.New("remote_entity: redis client is required")
	}
	if localSid == 0 {
		return nil, errors.New("remote_entity: non-zero sid is required for ownership fencing")
	}
	if cfg == nil {
		cfg = DefaultConfig()
	}
	backend := deps.Backend
	if backend == nil && deps.Loader != nil {
		if deps.Mongo == nil {
			return nil, errors.New("remote_entity: mongo client is required for the Mongo storage backend")
		}
		if mongoCfg.Database == "" {
			mongoCfg.Database = "remote_entity"
		}
		storage := NewMongoCommitter(deps.Mongo, mongoCfg.Database, localSid, mongoCfg.TransactionTTL)
		built, err := NewBackend(deps.Loader, storage)
		if err != nil {
			return nil, err
		}
		backend = built
	}
	if backend == nil {
		return nil, errors.New("remote_entity: atomic storage backend is required")
	}
	atomicStore, _ := backend.(AtomicCommitStore)
	if atomicStore == nil {
		return nil, errors.New("remote_entity: backend must support caller-owned atomic transactions")
	}
	provider, ok := backend.(WriteAuthorityProvider)
	if !ok {
		return nil, errors.New("remote_entity: backend must provide durable write authority")
	}
	authority := provider.WriteAuthority()
	if authority == nil {
		return nil, errors.New("remote_entity: backend must provide durable write authority")
	}
	lockFactory := NewVersionedLockFactory(deps.Redis, authority)
	manager := NewManager(lockFactory, cfg, localSid, NewSnapshotL2Store(deps.Redis, cfg.SnapshotL2TTL))
	if err := manager.LockFactoryError(); err != nil {
		return nil, err
	}
	if deps.OnFatal != nil {
		manager.SetFatalHandler(deps.OnFatal)
	}
	manager.SetBackend(backend)
	manager.SetOwnershipStore(authority)
	return &Assembly{Manager: manager, LockFactory: lockFactory, AtomicStore: atomicStore, cfg: cfg}, nil
}

// Start validates and seals the dependencies, binds and starts snapshot and
// interest replication on bus, initialises remote storage when the backend
// supports it, recovers the outbox and starts the finalizer. Any failure
// stops the replicators again. ctx is the parent for the bounded storage and
// recovery calls (each limited to cfg.OpTimeout).
// 重复启动幂等；失败重试复用首次绑定的 bus，替换 bus 需要新的 Assembly。
func (a *Assembly) Start(ctx context.Context, bus fsyncbus.ISyncBus) error {
	if a == nil || a.Manager == nil {
		return errors.New("remote_entity: not assembled")
	}
	if bus == nil {
		return errors.New("remote_entity: sync bus is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer a.releaseLifecycle()
	if a.stopping {
		return ErrAssemblyStopped
	}
	if a.started {
		return nil
	}
	if err := a.Manager.ValidateDependencies(); err != nil {
		return err
	}
	// 第一次绑定后保留同一组 replicator；失败重试只重订阅，不改动已封存的依赖。
	if a.snapshotRep == nil {
		a.snapshotRep, a.interestRep = a.Manager.BindSync(bus)
	}
	started := false
	defer func() {
		if !started {
			a.stopReplicators()
		}
	}()
	if err := a.snapshotRep.Start(); err != nil {
		return fmt.Errorf("remote_entity: start snapshot replica: %w", err)
	}
	if err := a.interestRep.Start(); err != nil {
		return fmt.Errorf("remote_entity: start interest replica: %w", err)
	}
	a.Manager.SealDependencies()
	if initializer, ok := a.Manager.Backend().(entity.IRemoteStorageInitializer); ok {
		storageCtx, cancel := context.WithTimeout(ctx, a.cfg.OpTimeout)
		err := initializer.EnsureRemoteStorage(storageCtx)
		cancel()
		if err != nil {
			return err
		}
	}
	recoverCtx, cancel := context.WithTimeout(ctx, a.cfg.OpTimeout)
	err := a.Manager.RecoverOutbox(recoverCtx)
	cancel()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.Manager.StartFinalizer()
	started = true
	a.started = true
	return nil
}

// Stop stops the finalizer within ctx and then the replicators. A timeout keeps
// replication available for accepted work; a later Stop finishes the cleanup.
func (a *Assembly) Stop(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer a.releaseLifecycle()
	// finalizer 是单次生命周期；一旦开始停止，同一个 Assembly 就不能再次启动。
	a.stopping = true
	if a.Manager != nil {
		if err := a.Manager.StopFinalizer(ctx); err != nil {
			return err
		}
	}
	a.stopReplicators()
	a.started = false
	return nil
}

func (a *Assembly) stopReplicators() {
	if a.snapshotRep != nil {
		a.snapshotRep.Stop()
	}
	if a.interestRep != nil {
		a.interestRep.Stop()
	}
}

func (a *Assembly) acquireLifecycle(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.lifecycleMu.Lock()
		if a.operationDone == nil {
			a.operationDone = make(chan struct{})
			a.lifecycleMu.Unlock()
			return nil
		}
		done := a.operationDone
		a.lifecycleMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *Assembly) releaseLifecycle() {
	a.lifecycleMu.Lock()
	close(a.operationDone)
	a.operationDone = nil
	a.lifecycleMu.Unlock()
}

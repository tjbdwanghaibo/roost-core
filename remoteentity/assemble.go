package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/mirror"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
)

// AssemblyDeps are the capabilities Remote Entity consumes. Redis is always
// required (versioned locks, ownership markers, snapshot L2). The backend is
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
}

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
	lockFactory := NewVersionedLockFactory(deps.Redis)
	manager := NewManager(lockFactory, cfg, localSid, NewSnapshotL2Store(deps.Redis, cfg.SnapshotL2TTL))
	if err := manager.LockFactoryError(); err != nil {
		return nil, err
	}
	if deps.OnFatal != nil {
		manager.SetFatalHandler(deps.OnFatal)
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
	manager.SetBackend(backend)
	atomicStore, _ := backend.(AtomicCommitStore)
	if atomicStore == nil {
		return nil, errors.New("remote_entity: backend must support caller-owned atomic transactions")
	}
	manager.SetOwnershipStore(NewRedisMarker(deps.Redis, ""))
	return &Assembly{Manager: manager, LockFactory: lockFactory, AtomicStore: atomicStore, cfg: cfg}, nil
}

// Start validates and seals the dependencies, binds and starts snapshot and
// interest replication on bus, initialises remote storage when the backend
// supports it, recovers the outbox and starts the finalizer. Any failure
// stops the replicators again. ctx is the parent for the bounded storage and
// recovery calls (each limited to cfg.OpTimeout).
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
	if err := a.Manager.ValidateDependencies(); err != nil {
		return err
	}
	snapshotRep, interestRep := a.Manager.BindSync(bus)
	if err := snapshotRep.Start(); err != nil {
		return fmt.Errorf("remote_entity: start snapshot replica: %w", err)
	}
	if err := interestRep.Start(); err != nil {
		snapshotRep.Stop()
		return fmt.Errorf("remote_entity: start interest replica: %w", err)
	}
	a.snapshotRep, a.interestRep = snapshotRep, interestRep
	started := false
	defer func() {
		if !started {
			a.stopReplicators()
		}
	}()
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
	a.Manager.StartFinalizer()
	started = true
	return nil
}

// Stop stops the finalizer within ctx and then the replicators.
func (a *Assembly) Stop(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	if a.Manager != nil {
		err = a.Manager.StopFinalizer(ctx)
	}
	a.stopReplicators()
	return err
}

func (a *Assembly) stopReplicators() {
	if a.snapshotRep != nil {
		a.snapshotRep.Stop()
		a.snapshotRep = nil
	}
	if a.interestRep != nil {
		a.interestRep.Stop()
		a.interestRep = nil
	}
}

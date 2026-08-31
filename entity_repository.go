package dataengine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	corecheckpoint "github.com/tjbdwanghaibo/cube-core/checkpoint"
	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	"github.com/tjbdwanghaibo/cube-core/entity"
)

var (
	ErrEntityAggregateNotFound = errors.New("dataengine repository: entity aggregate not found")
	ErrEntityAggregateCorrupt  = errors.New("dataengine repository: entity aggregate is incomplete or corrupt")
)

type RecoveryGate interface {
	Ready() bool
}

type entityLoadFlight struct {
	done  chan struct{}
	value entity.IThreadSafeEntity
	err   error
}

type EntityRepository struct {
	manager   *entity.EntityManager
	store     coredata.Store
	migration *MigrationRunner
	gate      RecoveryGate

	flightMu sync.Mutex
	flights  map[int64]*entityLoadFlight
}

func NewEntityRepository(manager *entity.EntityManager, store coredata.Store, migration *MigrationRunner, gate RecoveryGate) (*EntityRepository, error) {
	return newEntityRepository(manager, store, migration, gate)
}

func newEntityRepository(manager *entity.EntityManager, store coredata.Store, migration *MigrationRunner, gate RecoveryGate) (*EntityRepository, error) {
	if manager == nil || store == nil {
		return nil, errors.New("dataengine repository: entity manager and store are required")
	}
	return &EntityRepository{
		manager: manager, store: store, migration: migration, gate: gate,
		flights: make(map[int64]*entityLoadFlight),
	}, nil
}

func (repository *EntityRepository) LoadEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	if repository == nil || repository.manager == nil || repository.store == nil {
		return nil, coredata.ErrStoreRequired
	}
	if repository.gate != nil && !repository.gate.Ready() {
		return nil, coredata.ErrRecoveryIncomplete
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fullID, err := entity.NormalizeFullID(id, kind)
	if err != nil {
		return nil, err
	}
	if loaded := repository.manager.Get(fullID); loaded != nil {
		return loaded, nil
	}

	repository.flightMu.Lock()
	if flight := repository.flights[fullID]; flight != nil {
		repository.flightMu.Unlock()
		select {
		case <-flight.done:
			return flight.value, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &entityLoadFlight{done: make(chan struct{})}
	repository.flights[fullID] = flight
	repository.flightMu.Unlock()

	flight.value, flight.err = repository.loadAggregate(ctx, fullID, kind)
	repository.flightMu.Lock()
	delete(repository.flights, fullID)
	close(flight.done)
	repository.flightMu.Unlock()
	return flight.value, flight.err
}

type loadedDAO struct {
	dao     entity.DaoInterface
	doc     coredata.RawDocument
	payload []byte
	schema  uint32
}

func (repository *EntityRepository) loadAggregate(ctx context.Context, fullID int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	if loaded := repository.manager.Get(fullID); loaded != nil {
		return loaded, nil
	}
	builder := entity.GetEntityBuilderParam(kind)
	if builder == nil {
		return nil, fmt.Errorf("dataengine repository: no builder for entity kind %d", kind)
	}
	if builder.NoPersist || len(builder.DaoBuilders) == 0 {
		return nil, fmt.Errorf("%w: entity kind %d has no persistent DAO", ErrEntityAggregateNotFound, kind)
	}

	for migrationAttempt := 0; migrationAttempt < 3; migrationAttempt++ {
		loaded, remoteVector, err := repository.readAggregate(ctx, builder, fullID)
		if err != nil {
			return nil, err
		}
		migrated := false
		for _, item := range loaded {
			descriptor, ok := item.dao.(coredata.Descriptor)
			if !ok || descriptor.SchemaVersion() == item.schema {
				continue
			}
			if migrationAttempt == 2 {
				return nil, fmt.Errorf("%w: resource=%s entity=%d stored_schema=%d target_schema=%d", coredata.ErrMigrationConflict, item.dao.CollName(), fullID, item.schema, descriptor.SchemaVersion())
			}
			if repository.migration == nil {
				return nil, fmt.Errorf("%w: resource=%s entity=%d", ErrMigrationUnsupported, item.dao.CollName(), fullID)
			}
			migrationDoc := item.doc
			migrationDoc.Schema = item.schema
			changed, err := repository.migration.Migrate(ctx, item.dao, migrationDoc)
			if err != nil {
				return nil, err
			}
			migrated = migrated || changed
		}
		if migrated {
			continue
		}

		daos := make(map[string]entity.DaoInterface, len(loaded))
		for _, item := range loaded {
			hydrator, ok := item.dao.(entity.PersistedDaoLoader)
			if !ok {
				return nil, fmt.Errorf("%w: resource=%s does not implement PersistedDaoLoader", ErrEntityAggregateCorrupt, item.dao.CollName())
			}
			if err := hydrator.RestorePersisted(item.payload, item.schema, item.doc.Version); err != nil {
				return nil, fmt.Errorf("dataengine repository: restore %s/%d: %w", item.dao.CollName(), fullID, err)
			}
			if item.dao.Id() != fullID {
				return nil, fmt.Errorf("%w: resource=%s decoded id=%d want=%d", ErrEntityAggregateCorrupt, item.dao.CollName(), item.dao.Id(), fullID)
			}
			daos[item.dao.CollName()] = item.dao
		}
		param := &entity.EntityCreateParam{
			IsCreate: false, Category: builder.Category, Kind: kind, Id: fullID,
			Dao: daos, Lifetime: builder.Lifetime,
		}
		if builder.RemotePolicy.RemoteManaged() {
			if remoteVector.StateVersion == 0 {
				return nil, fmt.Errorf("%w: remote entity %d has no version envelope", ErrEntityAggregateCorrupt, fullID)
			}
			param.RemoteRestore = &remoteVector
		}
		created, err := repository.manager.Create(param)
		if err != nil {
			if errors.Is(err, entity.ErrEntityExists) {
				if existing := repository.manager.Get(fullID); existing != nil {
					return existing, nil
				}
			}
			return nil, err
		}
		return created, nil
	}
	return nil, coredata.ErrMigrationConflict
}

func (repository *EntityRepository) readAggregate(ctx context.Context, builder *entity.EntityBuilderParam, fullID int64) ([]loadedDAO, entity.RemoteVersionVector, error) {
	loaded := make([]loadedDAO, 0, len(builder.DaoBuilders))
	var remoteVector entity.RemoteVersionVector
	err := repository.store.ReadConsistent(ctx, func(readCtx context.Context) error {
		for index, buildDAO := range builder.DaoBuilders {
			if buildDAO == nil {
				return fmt.Errorf("%w: DAO builder %d is nil", ErrEntityAggregateCorrupt, index)
			}
			dao := buildDAO()
			if dao == nil || dao.CollName() == "" {
				return fmt.Errorf("%w: DAO builder %d returned invalid DAO", ErrEntityAggregateCorrupt, index)
			}
			for _, existing := range loaded {
				if existing.dao.CollName() == dao.CollName() {
					return fmt.Errorf("%w: duplicate DAO resource %q", ErrEntityAggregateCorrupt, dao.CollName())
				}
			}
			scope := coredata.DatabaseGlobal
			if corecheckpoint.ResolveDatabaseScope(dao) == corecheckpoint.DatabaseScopeServer {
				scope = coredata.DatabaseServer
			}
			docs, err := repository.store.Load(readCtx, coredata.LoadSpec{
				Database: dao.DbName(), Scope: scope, Resource: dao.CollName(), Filter: map[string]any{"_id": fullID}, BatchSize: 1,
			})
			if err != nil {
				return fmt.Errorf("dataengine repository: load %s/%d: %w", dao.CollName(), fullID, err)
			}
			if len(docs) == 0 || len(docs) == 1 && docs[0].Deleted {
				return fmt.Errorf("%w: resource=%s entity=%d", ErrEntityAggregateNotFound, dao.CollName(), fullID)
			}
			if len(docs) != 1 || docs[0].Key.ID != fullID {
				return fmt.Errorf("%w: resource=%s entity=%d documents=%d", ErrEntityAggregateCorrupt, dao.CollName(), fullID, len(docs))
			}
			doc := docs[0]
			payload, schema, err := persistedPayload(doc)
			if err != nil {
				return fmt.Errorf("%w: resource=%s entity=%d: %v", ErrEntityAggregateCorrupt, dao.CollName(), fullID, err)
			}
			loaded = append(loaded, loadedDAO{dao: dao, doc: doc, payload: payload, schema: schema})
			if doc.Enveloped && doc.Version >= remoteVector.StateVersion {
				remoteVector = entity.RemoteVersionVector{
					StateVersion: doc.Version, MarkerEpoch: doc.MarkerEpoch,
					LockFence: doc.LockFence, RouteEpoch: doc.RouteEpoch,
				}
			}
		}
		return nil
	})
	return loaded, remoteVector, err
}

var _ entity.AggregateLoader = (*EntityRepository)(nil)

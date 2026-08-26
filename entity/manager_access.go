package entity

import (
	"context"
	"fmt"
	"sync"
)

// ManagerAccess adapts the in-memory EntityManager to the execution and remote
// loading contracts without installing package globals. A service owns one
// instance and injects it into Nest and Remote Entity mods.
type ManagerAccess struct {
	manager         *EntityManager
	loaderMu        sync.RWMutex
	loader          AggregateLoader
	loaderID        uint64
	loadConcurrency int
	flightMu        sync.Mutex
	flights         map[int64]*entityLoadFlight
}

// entityLoadFlight deduplicates concurrent cold loads of one entity: the
// first caller performs LoadEntity, everyone else waits on done. Without it a
// hot entity's cache miss stampedes the database with one load per caller.
type entityLoadFlight struct {
	done  chan struct{}
	value IThreadSafeEntity
	err   error
}

func NewManagerAccess(manager *EntityManager) *ManagerAccess {
	return &ManagerAccess{manager: manager, loadConcurrency: 8, flights: make(map[int64]*entityLoadFlight)}
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

func (access *ManagerAccess) RegisterDeleteAdmitter(admitter func(context.Context, IThreadSafeEntity) error) (func(), error) {
	if access == nil || access.manager == nil {
		return nil, ErrEntityNotManaged
	}
	return access.manager.RegisterDeleteAdmitter(admitter)
}

func (access *ManagerAccess) ConfigureLoader(loader AggregateLoader) (func(), error) {
	if access == nil || access.manager == nil || loader == nil {
		return nil, fmt.Errorf("entity manager access: aggregate loader is required")
	}
	access.loaderMu.Lock()
	access.loaderID++
	id := access.loaderID
	access.loader = loader
	access.loaderMu.Unlock()
	return func() {
		access.loaderMu.Lock()
		if access.loaderID == id {
			access.loader = nil
		}
		access.loaderMu.Unlock()
	}, nil
}

func (access *ManagerAccess) Get(ctx context.Context, id int64, category EntityCategory) (IThreadSafeEntity, error) {
	if access == nil || access.manager == nil || id == 0 {
		return nil, nil
	}
	meta := ResolveEntityID(id)
	if value := access.manager.GetWithCategory(meta.FullID, category); value != nil {
		return value, nil
	}
	access.loaderMu.RLock()
	loader := access.loader
	access.loaderMu.RUnlock()
	if loader == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	value, err := access.loadEntityShared(ctx, meta.FullID, meta.Kind, loader)
	if err != nil {
		return nil, err
	}
	if value != nil && category != EntityCategoryNone && value.GetEntityCategory() != category {
		return nil, fmt.Errorf("entity manager access: loaded entity %d category mismatch", id)
	}
	return value, nil
}

// loadEntityShared collapses concurrent loads of the same entity into one
// LoadEntity call. Results (including errors) are shared with every waiter;
// the flight is removed before done closes, so a retry after a failure
// starts a fresh load. A waiter whose own context is cancelled stops waiting
// without affecting the in-flight load.
func (access *ManagerAccess) loadEntityShared(ctx context.Context, fullID int64, kind EntityKind, loader AggregateLoader) (IThreadSafeEntity, error) {
	access.flightMu.Lock()
	if flight, ok := access.flights[fullID]; ok {
		access.flightMu.Unlock()
		select {
		case <-flight.done:
			return flight.value, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &entityLoadFlight{done: make(chan struct{})}
	if access.flights == nil {
		access.flights = make(map[int64]*entityLoadFlight)
	}
	access.flights[fullID] = flight
	access.flightMu.Unlock()
	flight.value, flight.err = loader.LoadEntity(ctx, fullID, kind)
	access.flightMu.Lock()
	delete(access.flights, fullID)
	access.flightMu.Unlock()
	close(flight.done)
	return flight.value, flight.err
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

func (access *ManagerAccess) ConfigureIDGenerator(generator func() (uint64, error)) error {
	if access == nil || access.manager == nil {
		return ErrEntityNotManaged
	}
	return access.manager.ConfigureIDGenerator(generator)
}

var _ Getter = (*ManagerAccess)(nil)
var _ IRemoteEntityLoader = (*ManagerAccess)(nil)
var _ IRemoteEntityLocalLookup = (*ManagerAccess)(nil)

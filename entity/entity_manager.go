package entity

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/cube-core/lock"
	flog "github.com/tjbdwanghaibo/cube-core/log"
	"github.com/tjbdwanghaibo/cube-core/misc"
)

const defaultBucketCnt = 64

// EntityManager is the central registry for all entities.
// Uses sharded buckets for high-concurrency access with hundreds of thousands of entities.
type EntityManager struct {
	entities       *misc.BucketHolder[int64, IThreadSafeEntity]
	idGen          func() (uint64, error)
	locks          *lock.LockManager
	configMu       sync.RWMutex
	addMu          sync.Mutex
	removing       map[int64]struct{}
	groupMu        sync.RWMutex
	groups         map[int64]map[int64]IThreadSafeEntity
	hookMu         sync.RWMutex
	nextHookID     uint64
	releaseHooks   []entityReleaseHook
	deleteAdmitter entityDeleteAdmitter
}

var (
	ErrEntityNil            = errors.New("entity manager: nil entity")
	ErrEntityRemoved        = errors.New("entity manager: entity removed")
	ErrEntityExists         = errors.New("entity manager: entity already exists")
	ErrEntityNotManaged     = errors.New("entity manager: entity not managed")
	ErrIDGeneratorRequired  = errors.New("entity manager: id generator is required for new entities")
	ErrDeleteAdmitterExists = errors.New("entity manager: delete admitter already registered")
	ErrDeleteAdmitterNeeded = errors.New("entity manager: delete admitter is required")
	ErrDeleteIndeterminate  = errors.New("entity manager: delete admission outcome is indeterminate")
)

// NewEntityManager creates an EntityManager with default bucket count.
type EntityManagerOption func(*EntityManager)

func WithEntityIDGenerator(generator func() (uint64, error)) EntityManagerOption {
	return func(manager *EntityManager) {
		manager.idGen = generator
	}
}

func WithEntityLockManager(manager *lock.LockManager) EntityManagerOption {
	return func(entityManager *EntityManager) {
		entityManager.locks = manager
	}
}

// ConfigureIDGenerator installs the instance generator before any entity is
// published. It is safe against concurrent Create calls and refuses runtime
// replacement once the manager contains state.
func (m *EntityManager) ConfigureIDGenerator(generator func() (uint64, error)) error {
	if m == nil || generator == nil {
		return fmt.Errorf("entity manager: id generator is required")
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	if m.Len() != 0 {
		return fmt.Errorf("entity manager: id generator cannot change after entity publication")
	}
	m.idGen = generator
	return nil
}

func NewEntityManager(options ...EntityManagerOption) *EntityManager {
	return NewEntityManagerWithBuckets(defaultBucketCnt, options...)
}

// NewEntityManagerWithBuckets creates an EntityManager with specified bucket count.
func NewEntityManagerWithBuckets(bucketCnt int, options ...EntityManagerOption) *EntityManager {
	manager := &EntityManager{
		entities: misc.NewBucketHolder[int64, IThreadSafeEntity](bucketCnt, nil, false),
		groups:   make(map[int64]map[int64]IThreadSafeEntity),
		locks:    lock.NewLockManager(nil),
		removing: make(map[int64]struct{}),
	}
	for _, option := range options {
		if option != nil {
			option(manager)
		}
	}
	if manager.locks == nil {
		manager.locks = lock.NewLockManager(nil)
	}
	return manager
}

// Add registers an entity. Panics on duplicate ID.
func (m *EntityManager) Add(e IThreadSafeEntity) {
	if err := m.TryAdd(e); err != nil {
		panic(fmt.Sprintf("entity manager: add failed: %v", err))
	}
}

// TryAdd registers an entity and reports duplicate IDs as an error.
func (m *EntityManager) TryAdd(e IThreadSafeEntity) error {
	if e == nil || e.Base() == nil {
		return ErrEntityNil
	}
	id := e.ID()
	m.addMu.Lock()
	defer m.addMu.Unlock()
	if _, removing := m.removing[id]; removing {
		return fmt.Errorf("%w: %d is being removed", ErrEntityRemoved, id)
	}
	existing := m.entities.Get(id)
	if existing != nil {
		return fmt.Errorf("%w: %d", ErrEntityExists, id)
	}
	m.entities.Add(id, e)
	e.Base().setOwner(m)
	m.addGroupIndexLockedByManager(e)
	return nil
}

type entityDeleteAdmitter struct {
	id uint64
	fn DeleteAdmitter
}

// DeleteAdmission tells EntityManager whether durable admission completed in
// this call or was attached to the active transaction. Deferred admission is
// finalized by the admitter only after that transaction reaches its commit
// point; rollback must leave the entity live.
type DeleteAdmission uint8

const (
	DeleteAdmissionImmediate DeleteAdmission = iota
	DeleteAdmissionDeferred
	// DeleteAdmissionIndeterminate means persistence may already have accepted
	// the delete. The entity must be removed from memory and the error returned
	// so callers cannot continue serving possibly deleted state.
	DeleteAdmissionIndeterminate
)

// DeleteAdmitter owns the persistence-specific delete transaction. The reason
// is supplied so a deferred admission can perform the same in-memory lifecycle
// transition after the durable commit point.
type DeleteAdmitter func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error)

// RegisterDeleteAdmitter installs the single durable admission gate used by
// persistent deletion. Multiple independent gates are rejected because they
// cannot form one atomic deletion decision.
func (m *EntityManager) RegisterDeleteAdmitter(admitter DeleteAdmitter) (func(), error) {
	if m == nil || admitter == nil {
		return nil, ErrDeleteAdmitterNeeded
	}
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	if m.deleteAdmitter.fn != nil {
		return nil, ErrDeleteAdmitterExists
	}
	m.nextHookID++
	id := m.nextHookID
	m.deleteAdmitter = entityDeleteAdmitter{id: id, fn: admitter}
	return func() {
		m.hookMu.Lock()
		if m.deleteAdmitter.id == id {
			m.deleteAdmitter = entityDeleteAdmitter{}
		}
		m.hookMu.Unlock()
	}, nil
}

func (m *EntityManager) admitDelete(ctx context.Context, e IThreadSafeEntity, reason EntityDestroyReason) (DeleteAdmission, error) {
	m.hookMu.RLock()
	admitter := m.deleteAdmitter.fn
	m.hookMu.RUnlock()
	if admitter == nil {
		return DeleteAdmissionImmediate, ErrDeleteAdmitterNeeded
	}
	return admitter(ctx, e, reason)
}

// Destroy durably admits a versioned delete tombstone while holding the entity
// mutex, then removes the entity from memory. A definitive admission failure
// leaves the entity live. An indeterminate result removes it defensively so
// callers cannot serve state that persistence may already have deleted.
func (m *EntityManager) Destroy(ctx context.Context, e IThreadSafeEntity, reason EntityDestroyReason, deleteFromDB bool) error {
	if m == nil || e == nil || e.Base() == nil {
		return ErrEntityNil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.Touch() {
		return ErrEntityRemoved
	}
	mu := e.GetMutex()
	if mu == nil {
		e.UnTouch()
		return ErrEntityNil
	}
	mu.Lock()
	if e.IsRemoved() || e.IsClear() {
		mu.Unlock()
		e.UnTouch()
		return ErrEntityRemoved
	}
	m.addMu.Lock()
	managed := m.entities.Get(e.ID()) == e
	m.addMu.Unlock()
	if !managed {
		mu.Unlock()
		e.UnTouch()
		return ErrEntityNotManaged
	}
	var admissionErr error
	if deleteFromDB {
		admission, err := m.admitDelete(ctx, e, reason)
		if admission == DeleteAdmissionIndeterminate && err == nil {
			err = ErrDeleteIndeterminate
		}
		if err != nil && admission != DeleteAdmissionIndeterminate {
			mu.Unlock()
			e.UnTouch()
			flog.Warn("entity manager: durable delete admission failed", "id", e.ID(), "category", e.GetEntityCategory(), "kind", e.GetEntityKind(), "reason", reason, "err", err)
			return err
		}
		admissionErr = err
		if admission == DeleteAdmissionDeferred {
			// The admitter registered the lifecycle transition on the current
			// transaction. It owns finalization after durable admission.
			mu.Unlock()
			e.UnTouch()
			return nil
		}
		if admission != DeleteAdmissionImmediate && admission != DeleteAdmissionIndeterminate {
			mu.Unlock()
			e.UnTouch()
			return fmt.Errorf("entity manager: invalid delete admission %d", admission)
		}
	}
	id := e.ID()
	category := e.GetEntityCategory()
	kind := e.GetEntityKind()

	// Prevent new readers and remove the entity from the global index while the
	// entity mutex is held. Lifecycle callbacks run after unlock so they can
	// safely coordinate with other entities through EntityGuard lock ordering.
	m.addMu.Lock()
	if m.entities.Get(id) != e {
		m.addMu.Unlock()
		mu.Unlock()
		e.UnTouch()
		return ErrEntityNotManaged
	}
	m.removing[id] = struct{}{}
	e.SetRemoved()
	m.entities.Del(e.ID())
	m.removeGroupIndex(e)
	m.addMu.Unlock()
	defer func() {
		if m.locks != nil {
			m.locks.ReleaseLock(id)
		}
		m.addMu.Lock()
		delete(m.removing, id)
		m.addMu.Unlock()
	}()
	mu.Unlock()
	defer func() {
		e.UnTouch()
		e.ClearBase()
		flog.Debug("entity manager: removed", "id", id, "category", category, "kind", kind, "reason", reason, "delete_db", deleteFromDB)
	}()

	e.Base().DestroyAll(reason)

	e.OnDestroy(reason)
	return admissionErr
}

// Get returns the entity with the given ID, or nil if not found.
func (m *EntityManager) Get(id int64) IThreadSafeEntity {
	return m.entities.Get(id)
}

// GetWithCategory returns the entity with the given ID and category check.
// Returns nil if not found or type mismatch.
func (m *EntityManager) GetWithCategory(id int64, category EntityCategory) IThreadSafeEntity {
	e := m.entities.Get(id)
	if e != nil && category != EntityCategoryNone && e.GetEntityCategory() != category {
		return nil
	}
	return e
}

// GetMany returns entities matching the given IDs.
// Missing entities are skipped.
func (m *EntityManager) GetMany(ids []int64) []IThreadSafeEntity {
	result := make([]IThreadSafeEntity, 0, len(ids))
	for _, id := range ids {
		if e := m.entities.Get(id); e != nil {
			result = append(result, e)
		}
	}
	return result
}

// Exists checks if an entity with the given ID is in memory.
func (m *EntityManager) Exists(id int64) bool {
	return m.entities.Get(id) != nil
}

// Len returns the total number of managed entities.
func (m *EntityManager) Len() int {
	return m.entities.Count()
}

// Range iterates all entities across all buckets. Return false from fn to stop early.
func (m *EntityManager) Range(fn func(IThreadSafeEntity) bool) {
	m.entities.RangeAll(func(_ int64, e IThreadSafeEntity) bool {
		return fn(e)
	})
}

// RangeByCategory iterates entities of a specific category.
func (m *EntityManager) RangeByCategory(category EntityCategory, fn func(IThreadSafeEntity) bool) {
	m.entities.RangeAll(func(_ int64, e IThreadSafeEntity) bool {
		if e.GetEntityCategory() == category {
			return fn(e)
		}
		return true
	})
}

// CountByCategory returns the number of entities of a specific category.
func (m *EntityManager) CountByCategory(category EntityCategory) int {
	count := 0
	m.entities.RangeAll(func(_ int64, e IThreadSafeEntity) bool {
		if e.GetEntityCategory() == category {
			count++
		}
		return true
	})
	return count
}

func (m *EntityManager) addGroupIndexLockedByManager(e IThreadSafeEntity) {
	if m == nil || e == nil || e.Base() == nil {
		return
	}
	groupID := e.Base().GroupLockID()
	if groupID == 0 {
		return
	}
	m.groupMu.Lock()
	defer m.groupMu.Unlock()
	m.addGroupIndexLocked(groupID, e)
}

func (m *EntityManager) addGroupIndexLocked(groupID int64, e IThreadSafeEntity) {
	if groupID == 0 || e == nil {
		return
	}
	bucket := m.groups[groupID]
	if bucket == nil {
		bucket = make(map[int64]IThreadSafeEntity)
		m.groups[groupID] = bucket
	}
	bucket[e.ID()] = e
}

func (m *EntityManager) removeGroupIndex(e IThreadSafeEntity) {
	if m == nil || e == nil || e.Base() == nil {
		return
	}
	groupID := e.Base().GroupLockID()
	if groupID == 0 {
		return
	}
	m.groupMu.Lock()
	defer m.groupMu.Unlock()
	m.removeGroupIndexLocked(groupID, e.ID())
}

func (m *EntityManager) removeGroupIndexLocked(groupID int64, entityID int64) {
	if groupID == 0 {
		return
	}
	bucket := m.groups[groupID]
	if bucket == nil {
		return
	}
	delete(bucket, entityID)
	if len(bucket) == 0 {
		delete(m.groups, groupID)
	}
}

// UpdateEntityGroup updates EntityBase group state and the manager's derived
// group membership index. Callers are responsible for holding the correct
// entity/group serialization lock.
func (m *EntityManager) UpdateEntityGroup(e IThreadSafeEntity, groupID int64) error {
	if m == nil || e == nil || e.Base() == nil {
		return ErrEntityNil
	}
	if m.entities.Get(e.ID()) != e {
		return ErrEntityNotManaged
	}
	oldGroupID := e.Base().GroupLockID()
	if oldGroupID == groupID {
		return nil
	}
	m.groupMu.Lock()
	defer m.groupMu.Unlock()
	m.removeGroupIndexLocked(oldGroupID, e.ID())
	if groupID != 0 {
		m.addGroupIndexLocked(groupID, e)
	}
	e.Base().setGroupLockID(groupID)
	return nil
}

func (m *EntityManager) GetGroupEntity(groupID int64, entityID int64) IThreadSafeEntity {
	if m == nil || groupID == 0 || entityID == 0 {
		return nil
	}
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()
	bucket := m.groups[groupID]
	if bucket == nil {
		return nil
	}
	return bucket[entityID]
}

func (m *EntityManager) GetGroupEntities(groupID int64) []IThreadSafeEntity {
	if m == nil || groupID == 0 {
		return nil
	}
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()
	bucket := m.groups[groupID]
	if len(bucket) == 0 {
		return nil
	}
	ret := make([]IThreadSafeEntity, 0, len(bucket))
	for _, e := range bucket {
		ret = append(ret, e)
	}
	return ret
}

func (m *EntityManager) RangeGroupEntities(groupID int64, fn func(IThreadSafeEntity) bool) {
	if m == nil || groupID == 0 || fn == nil {
		return
	}
	for _, e := range m.GetGroupEntities(groupID) {
		if !fn(e) {
			return
		}
	}
}

package nest

import (
	"errors"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"github.com/tjbdwanghaibo/roost-core/lock"
)

const entityLockGroupDispatchRetryMax = 4

type EntityLockGroupScope struct {
	groupID int64
	store   entityGroupStore
	prev    *EntityLockGroupScope
}

type entityGroupStore interface {
	GetGroupEntity(groupID, entityID int64) entity.IThreadSafeEntity
	GetGroupEntities(groupID int64) []entity.IThreadSafeEntity
	UpdateEntityGroup(entity.IThreadSafeEntity, int64) error
}

func groupStoreOf(getter entity.Getter) entityGroupStore {
	store, _ := getter.(entityGroupStore)
	return store
}

var entityLockGroupScopes sync.Map // map[int64]*EntityLockGroupScope

func CurrentEntityLockGroup() *EntityLockGroupScope {
	if value, ok := entityLockGroupScopes.Load(goroutine.GoID()); ok {
		if scope, ok := value.(*EntityLockGroupScope); ok {
			return scope
		}
	}
	return nil
}

func (s *EntityLockGroupScope) GroupID() int64 {
	if s == nil {
		return 0
	}
	return s.groupID
}

func (s *EntityLockGroupScope) Get(entityID int64) entity.IThreadSafeEntity {
	if s == nil || s.groupID == 0 || entityID == 0 || s.store == nil {
		return nil
	}
	ent := s.store.GetGroupEntity(s.groupID, entityID)
	if ent == nil || ent.Base() == nil || ent.Base().GroupLockID() != s.groupID {
		return nil
	}
	return ent
}

func (s *EntityLockGroupScope) Range(fn func(entity.IThreadSafeEntity) bool) {
	if s == nil || s.groupID == 0 || fn == nil || s.store == nil {
		return
	}
	for _, ent := range s.store.GetGroupEntities(s.groupID) {
		if ent == nil || ent.Base() == nil || ent.Base().GroupLockID() != s.groupID {
			continue
		}
		if !fn(ent) {
			return
		}
	}
}

func GroupEntityAs[T entity.IThreadSafeEntity](scope *EntityLockGroupScope, entityID int64) (T, bool) {
	var zero T
	ent := scope.Get(entityID)
	if ent == nil {
		return zero, false
	}
	typed, ok := ent.(T)
	if !ok {
		return zero, false
	}
	return typed, true
}

func pushEntityLockGroupScope(groupID int64, store entityGroupStore) func() {
	if groupID == 0 {
		return func() {}
	}
	prev := CurrentEntityLockGroup()
	scope := &EntityLockGroupScope{groupID: groupID, store: store, prev: prev}
	entityLockGroupScopes.Store(goroutine.GoID(), scope)
	return func() {
		cur := CurrentEntityLockGroup()
		if cur != scope {
			entityLockGroupScopes.Delete(goroutine.GoID())
			return
		}
		if scope.prev != nil {
			entityLockGroupScopes.Store(goroutine.GoID(), scope.prev)
		} else {
			entityLockGroupScopes.Delete(goroutine.GoID())
		}
		scope.prev = nil
	}
}

type entityLockGroupLockManager struct {
	mu    sync.Mutex
	locks map[int64]*entityLockGroupLockEntry
}

type entityLockGroupLockEntry struct {
	groupID int64
	mu      lock.Mutex
	refs    int
}

func newEntityLockGroupLockManager() *entityLockGroupLockManager {
	return &entityLockGroupLockManager{locks: make(map[int64]*entityLockGroupLockEntry)}

}

func (m *entityLockGroupLockManager) acquire(groupID int64) (*entityLockGroupLockEntry, bool) {
	if m == nil || groupID == 0 {
		return nil, false
	}
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[int64]*entityLockGroupLockEntry)
	}
	entry := m.locks[groupID]
	if entry == nil {
		entry = &entityLockGroupLockEntry{groupID: groupID, mu: lock.NewReentrantMutex(-groupID)}
		m.locks[groupID] = entry
	}
	entry.refs++
	m.mu.Unlock()
	if entry.mu.TryLock() {
		return entry, true
	}
	m.releaseRef(entry)
	return nil, false
}

func (m *entityLockGroupLockManager) release(entry *entityLockGroupLockEntry) {
	if m == nil || entry == nil {
		return
	}
	entry.mu.Unlock()
	m.releaseRef(entry)
}

func (m *entityLockGroupLockManager) releaseRef(entry *entityLockGroupLockEntry) {
	m.mu.Lock()
	entry.refs--
	if entry.refs == 0 && m.locks[entry.groupID] == entry {
		delete(m.locks, entry.groupID)
	}
	m.mu.Unlock()
}

func resolveDispatchGroupID(lockEs []entity.IThreadSafeEntity) (int64, error) {
	return resolveDispatchGroupIDFromSnapshots(captureDispatchGroupSnapshots(lockEs))
}

type dispatchGroupSnapshot struct {
	ent      entity.IThreadSafeEntity
	groupID  int64
	epoch    uint64
	state    entity.EntityGroupTransitionState
	targetID int64
}

func captureDispatchGroupSnapshots(lockEs []entity.IThreadSafeEntity) []dispatchGroupSnapshot {
	ret := make([]dispatchGroupSnapshot, 0, len(lockEs))
	for _, ent := range lockEs {
		if ent == nil || ent.Base() == nil {
			continue
		}
		base := ent.Base()
		ret = append(ret, dispatchGroupSnapshot{
			ent:      ent,
			groupID:  base.GroupLockID(),
			epoch:    base.GroupEpoch(),
			state:    base.GroupTransitionState(),
			targetID: base.GroupTransitionTargetID(),
		})
	}
	return ret
}

func resolveDispatchGroupIDFromSnapshots(snapshots []dispatchGroupSnapshot) (int64, error) {
	var groupID int64
	for _, snap := range snapshots {
		next := snap.groupID
		if next == 0 {
			continue
		}
		if groupID == 0 {
			groupID = next
			continue
		}
		if groupID != next {
			return 0, ErrEntityLockGroupMix
		}
	}
	return groupID, nil
}

func validateDispatchGroupSnapshots(snapshots []dispatchGroupSnapshot) error {
	for _, snap := range snapshots {
		if snap.ent == nil || snap.ent.Base() == nil {
			return ErrEntityLockGroupChanged
		}
		base := snap.ent.Base()
		if base.GroupTransitionPending() {
			return ErrEntityGroupTransitionPending
		}
		if base.GroupLockID() != snap.groupID ||
			base.GroupEpoch() != snap.epoch ||
			base.GroupTransitionState() != snap.state ||
			base.GroupTransitionTargetID() != snap.targetID {
			return ErrEntityLockGroupChanged
		}
	}
	return nil
}

func lockDispatchEntitiesForHandler(locks *entityLockGroupLockManager, guard *entity.EntityGuard, lockEs []entity.IThreadSafeEntity) ([]entity.IThreadSafeEntity, func(), error) {
	return lockDispatchEntitiesForHandlerWithStore(locks, guard, lockEs, nil)
}

// errDispatchWithoutGuardScope 表示派发取锁时当前 goroutine 没有 Guard 作用域，或传入的 Guard 不是作用域里的那一个。
// 这是调用方的编程错误：生产入口都在 runNestLogic 建的作用域里（RR-20261006-12）。
var errDispatchWithoutGuardScope = errors.New("nest: dispatch locking requires the caller's entity guard scope")

// dispatchScopeGuard 返回当前 goroutine Guard 作用域里的 Guard，没有作用域时返回 errDispatchWithoutGuardScope。
//
// 派发取锁只用作用域的 Guard（RR-20261006-12）：Guard 归作用域所有，作用域结束时释放它上面的全部实体锁、跑解锁后回调并
// 归还池，正好一次；取锁、组迁移重试和 releaseDispatchEntities 只放本次取得的实体锁，从不归还 Guard。之前没有作用域时
// 用 entity.GetEntityGuard() 从池里取独立 Guard，releaseDispatchLocks 每次释放都把它整个归还池；组迁移重试先释放一次、
// 再拿同一个 Guard 重新取锁，成功后调用方再释放一次，同一 Guard 进池两次，之后两个快 worker 共用它、互相解对方的实体锁
// （收尾第 4 批 A2、W-2026-10-06-01）。没有采用“无作用域时调用方持有、重试不归还”：handler 新建实体的锁、被取代实例和
// 解锁后回调都挂在 Guard 上，只有完整的作用域释放能收尾，调用方等于要自己再写一遍作用域。
func dispatchScopeGuard() (*entity.EntityGuard, error) {
	scope := entity.CurrentGuardScope()
	if scope == nil || scope.Guard() == nil {
		return nil, errDispatchWithoutGuardScope
	}
	return scope.Guard(), nil
}

// lockDispatchEntitiesForHandlerWithStore 按锁组取 lockEs 的实体锁，取锁期间锁组变了就放掉已取的锁再重试。
// guard 必须是当前 goroutine Guard 作用域里的 Guard（见 dispatchScopeGuard），否则不取任何锁、返回
// errDispatchWithoutGuardScope；返回的释放函数只放本次取得的实体锁和组锁，Guard 留给作用域。
func lockDispatchEntitiesForHandlerWithStore(locks *entityLockGroupLockManager, guard *entity.EntityGuard, lockEs []entity.IThreadSafeEntity, store entityGroupStore) ([]entity.IThreadSafeEntity, func(), error) {
	if scopeGuard, err := dispatchScopeGuard(); err != nil || scopeGuard != guard {
		return nil, nil, errDispatchWithoutGuardScope
	}
	var lastErr error
	for attempt := 0; attempt < entityLockGroupDispatchRetryMax; attempt++ {
		snapshots := captureDispatchGroupSnapshots(lockEs)
		groupID, err := resolveDispatchGroupIDFromSnapshots(snapshots)
		if err != nil {
			return nil, nil, err
		}
		acquired, releaseLocks, err := lockDispatchEntitiesWithGroup(locks, guard, lockEs, groupID, store)
		if err != nil {
			return nil, nil, err
		}
		if err := validateDispatchGroupSnapshots(snapshots); err != nil {
			releaseLocks()
			if errors.Is(err, ErrEntityGroupTransitionPending) {
				return nil, nil, err
			}
			lastErr = err
			continue
		}
		return acquired, releaseLocks, nil
	}
	if lastErr == nil {
		lastErr = ErrEntityLockGroupChanged
	}
	return nil, nil, lastErr
}

func lockDispatchEntitiesWithGroup(locks *entityLockGroupLockManager, guard *entity.EntityGuard, lockEs []entity.IThreadSafeEntity, groupID int64, store entityGroupStore) ([]entity.IThreadSafeEntity, func(), error) {
	if groupID == 0 {
		acquired, err := lockDispatchEntities(guard, lockEs)
		if err != nil {
			return nil, nil, err
		}
		return acquired, func() {
			releaseDispatchEntities(guard, acquired)
		}, nil
	}
	groupEntry, ok := locks.acquire(groupID)
	if !ok {
		return nil, nil, ErrLockTimeout
	}
	releaseScope := pushEntityLockGroupScope(groupID, store)
	releaseGroup := func() {
		defer locks.release(groupEntry)
		releaseScope()
	}
	handedOff := false
	defer func() {
		// 部分 Entity 加锁失败时也会调用 release hook，panic 不能泄漏组锁。
		if !handedOff {
			releaseGroup()
		}
	}()
	acquired, err := tryLockDispatchEntities(guard, lockEs)
	if err != nil {
		return nil, nil, err
	}
	handedOff = true
	return acquired, func() {
		// Entity release hook 可能 panic；组锁和 goroutine scope 仍必须释放。
		defer releaseGroup()
		releaseDispatchEntities(guard, acquired)
	}, nil
}

func tryLockDispatchEntities(guard *entity.EntityGuard, lockEs []entity.IThreadSafeEntity) ([]entity.IThreadSafeEntity, error) {
	if guard == nil {
		return nil, ErrLockTimeout
	}
	acquired := make([]entity.IThreadSafeEntity, 0, len(lockEs))
	for _, ent := range lockEs {
		if ent == nil {
			continue
		}
		if guard.GuardedEntity(ent) {
			continue
		}
		if !tryRequireDispatchEntity(guard, ent) {
			releaseDispatchEntities(guard, acquired)
			return nil, ErrLockTimeout
		}
		acquired = append(acquired, ent)
	}
	return acquired, nil
}

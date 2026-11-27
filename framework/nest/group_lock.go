package nest

import (
	"errors"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// entityLockGroupDispatchRetryMax 是取锁期间锁组变化时重新取锁的最大次数。
const entityLockGroupDispatchRetryMax = 4

// EntityLockGroupScope 只在所属 handler 的 goroutine、Guard 与事务里授予成员访问权。
// 其成员取自已加锁、已纳入本次事务的快照，不是可按 ID 查询任意实体的仓库。
// Get/Range 返回的是借用的业务指针；不得保存、交给其他 goroutine 或在 handler 结束后使用。
type EntityLockGroupScope struct {
	groupID int64
	guard   *entity.GuardScope
	tx      *RollbackTx
	frame   *nestDispatchFrame
	ctx     *fctx.Context
	members map[int64]dispatchGroupSnapshot
	anchors []dispatchGroupSnapshot // 显式目标；全已失效就不再借出其他成员。
	domains map[int64]entity.CompanionDomainSnapshot
	ids     []int64
}

// ErrGroupMemberUnavailable 表示当前没有唯一、有效的同组成员授权，或实例的类型不符。
var ErrGroupMemberUnavailable = errors.New("nest: group member unavailable in current handler scope")

// ErrPeerGroupUnsupported 表示当前对等锁组不能安全组成事务（例如含 Remote 或已绑定的伴生域）。
var ErrPeerGroupUnsupported = errors.New("nest: unsupported peer group dispatch")

// entityGroupStore 是 Getter 可选实现的锁组查询与迁移能力（ManagerAccess 实现）。
type entityGroupStore interface {
	GetGroupEntity(groupID, entityID int64) entity.IThreadSafeEntity
	GetGroupEntities(groupID int64) []entity.IThreadSafeEntity
	UpdateEntityGroup(entitypermit.Token, entity.IThreadSafeEntity, int64) error
}

func groupStoreOf(getter entity.Getter) entityGroupStore {
	store, _ := getter.(entityGroupStore)
	return store
}

var entityLockGroupScopes sync.Map // map[int64]*EntityLockGroupScope

// CurrentEntityLockGroup 返回当前 goroutine 持有的锁组作用域；不在锁组内时为 nil。
func CurrentEntityLockGroup() *EntityLockGroupScope {
	if value, ok := entityLockGroupScopes.Load(goroutine.GoID()); ok {
		if scope, ok := value.(*EntityLockGroupScope); ok && (scope.members == nil ||
			(scope.frame != nil && scope.frame == currentNestDispatchFrame() && scope.frame.guard == scope.guard &&
				scope.guard == entity.CurrentGuardScope() && scope.ctx == fctx.CurrentContext() && scope.tx == CurrentRollbackTx())) {
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

// Get 返回同组中的实体；实体不在本组（或已迁出）时返回 nil。
func (s *EntityLockGroupScope) Get(entityID int64) entity.IThreadSafeEntity {
	// 先验证当前 goroutine 的登记，再接触 Guard、成员和实体；跨 goroutine 的旧 scope 不能读别人的 Guard。
	if s == nil || s != CurrentEntityLockGroup() || entityID == 0 || s.members == nil {
		return nil
	}
	snap, ok := s.members[entityID]
	if !ok || !s.validMember(snap) || !slices.ContainsFunc(s.anchors, s.validMember) {
		return nil
	}
	return snap.ent
}

func (s *EntityLockGroupScope) validMember(snap dispatchGroupSnapshot) bool {
	e := snap.ent
	if snap.groupID != s.groupID || e.IsRemoved() || e.IsClear() || e.Base().GroupLockID() != s.groupID ||
		e.Base().GroupEpoch() != snap.epoch || e.Base().GroupTransitionPending() || !s.guard.Guard().GuardedEntity(e) {
		return false
	}
	d, tracked := s.domains[e.GUId()]
	return !tracked || d.Valid()
}

// Range 遍历仍属于本组的实体，fn 返回 false 时停止。
func (s *EntityLockGroupScope) Range(fn func(entity.IThreadSafeEntity) bool) {
	if s == nil || fn == nil || s != CurrentEntityLockGroup() {
		return
	}
	for _, id := range s.ids {
		if ent := s.Get(id); ent != nil && !fn(ent) {
			return
		}
	}
}

// GroupEntityAs 按类型取同组实体；不存在或类型不符时返回零值与 false。
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

// GetGroupMember 取得本 handler 唯一显式锁组中已登记参与当前事务的本地成员。
// 未分组目标不影响该唯一组；多个显式组仍可一起派发，但此无组参数的查询拒绝歧义。
func GetGroupMember[T entity.IThreadSafeEntity](id int64) (T, error) {
	v, ok := GroupEntityAs[T](CurrentEntityLockGroup(), id)
	if !ok {
		return v, ErrGroupMemberUnavailable
	}
	return v, nil
}

// installPeerGroupScope(nil) 在独立事务的整个生命期遮蔽外层授权，包括它的快照和提交钩子。
func installPeerGroupScope(scope *EntityLockGroupScope) func() {
	gid := goroutine.GoID()
	prev, had := entityLockGroupScopes.Load(gid)
	if scope != nil {
		entityLockGroupScopes.Store(gid, scope)
	} else if had {
		entityLockGroupScopes.Delete(gid)
	} else {
		return func() {}
	}
	return func() {
		if had {
			entityLockGroupScopes.Store(gid, prev)
		} else {
			entityLockGroupScopes.Delete(gid)
		}
	}
}

func suspendPeerGroupScope() func() { return installPeerGroupScope(nil) }

func (s *EntityLockGroupScope) beginHandler() func() {
	s.guard, s.tx, s.frame, s.ctx = entity.CurrentGuardScope(), CurrentRollbackTx(), currentNestDispatchFrame(), fctx.CurrentContext()
	restore := installPeerGroupScope(s)
	return func() {
		restore()
		// 捕获出去的 scope 不延长实例与事务的寿命；Get/Range 总在身份检查成功后才读这些字段。
		s.members, s.ids, s.anchors, s.domains = nil, nil, nil, nil
		s.guard, s.tx, s.frame, s.ctx = nil, nil, nil, nil
	}
}

// 低层兼容取锁入口仅报告所持组的 ID；没有事务成员快照，不授予原始指针访问权。
func pushEntityLockGroupScope(groupID int64, _ entityGroupStore) func() {
	return installPeerGroupScope(&EntityLockGroupScope{groupID: groupID})
}

// entityLockGroupLockManager 管理按组 ID 引用计数的组锁；最后一个引用释放时删除条目。组锁只 TryLock，不等待。
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

// dispatchGroupSnapshot 是取锁前采集的实体锁组状态，取锁后据此校验锁组没有变化。
type dispatchGroupSnapshot struct {
	ent      entity.IThreadSafeEntity
	groupID  int64
	epoch    uint64
	state    entity.EntityGroupTransitionState
	targetID int64
}

func captureDispatchGroupSnapshots(lockEs []entity.IThreadSafeEntity, storage []dispatchGroupSnapshot) []dispatchGroupSnapshot {
	ret := slices.Grow(storage[:0], len(lockEs))
	for _, ent := range lockEs {
		if ent == nil || ent.Base() == nil {
			continue
		}
		ret = capturePeerSnapshot(ret, ent)
	}
	return ret
}

// resolveDispatchGroupIDFromSnapshots 返回目标共同所属的锁组（0 表示都不在组内）；属于不同组时返回 ErrEntityLockGroupMix。
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
	// 伴生派发是在目标无显式组时选中的。若在它重新拍关系快照前实体入组，不能以旧入口只锁
	// 伴生域、漏掉对等成员；明确拒绝这次组合，正常的后续派发会依据实际组选择入口。
	if groupID != 0 {
		for _, s := range snapshots {
			if entity.IsCompanionParticipant(s.ent.GetEntityKind()) {
				return 0, ErrPeerGroupUnsupported
			}
		}
	}
	return groupID, nil
}

// validateDispatchGroupSnapshots 在取锁后确认每个实体的锁组状态与快照一致：组迁移待定返回 ErrEntityGroupTransitionPending，
// 其余变化返回 ErrEntityLockGroupChanged（调用方放锁重试）。
func validateDispatchGroupSnapshots(snapshots []dispatchGroupSnapshot) error {
	for _, snap := range snapshots {
		if snap.ent == nil || snap.ent.Base() == nil || snap.ent.IsRemoved() || snap.ent.IsClear() {
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
// 这是调用方的编程错误：生产入口都在 runNestLogic 建的作用域里。
var errDispatchWithoutGuardScope = errors.New("nest: dispatch locking requires the caller's entity guard scope")

// dispatchScopeGuard 返回当前 goroutine Guard 作用域里的 Guard，没有作用域时返回 errDispatchWithoutGuardScope。
//
// 派发取锁只用作用域的 Guard：Guard 归作用域所有，作用域结束时释放它上面的全部实体锁、跑解锁后回调并归还池，
// 正好一次；取锁、组迁移重试和 releaseDispatchEntities 只放本次取得的实体锁，从不归还 Guard。若派发自己从池里取
// Guard 并在每次释放时归还，组迁移重试会让同一 Guard 进池两次，之后两个业务 worker 共用它、互相解对方的实体锁。
// 也不采用“无作用域时调用方持有、重试不归还”：handler 新建实体的锁、被取代实例和解锁后回调都挂在 Guard 上，
// 只有完整的作用域释放能收尾，调用方等于要自己再写一遍作用域。历史：RR-20261006-12。
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
	// 快照只用于本次同步取锁后的校验，不交给 release 闭包或 Remote 续行。
	// 常见 1～4 实体使用栈上存储；更大集合按需扩容，重试仍重新采集状态。
	var storage [4]dispatchGroupSnapshot
	snapshots := storage[:0]
	for attempt := 0; attempt < entityLockGroupDispatchRetryMax; attempt++ {
		snapshots = captureDispatchGroupSnapshots(lockEs, snapshots)
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

// lockDispatchEntitiesWithGroup 取一次锁：无锁组时按锁序等待实体锁；有锁组时先 TryLock 组锁并登记组作用域，
// 再 TryLock 每个实体锁。返回的释放函数先放实体锁、再放组作用域与组锁（实体释放 hook panic 时组锁仍会释放）。
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

// tryLockDispatchEntities 在持有组锁时逐个 TryLock 实体锁（已持有的跳过），任何一把取不到就放掉本次已取的锁
// 并返回 ErrLockTimeout。
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

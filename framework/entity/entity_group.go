package entity

import (
	"errors"

	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

// ErrEntityGroupDomain：Remote、合上的伴生域或正在变更的伴生域不能成为对等组成员。
var ErrEntityGroupDomain = errors.New("entity: instance is ineligible for peer group membership")

// checkGroupMemberKindLocked 与 manager 的关系增删在同一 addMu 下复核，防止异步请求入队后关系已经改变。
func checkGroupMemberKindLocked(e IThreadSafeEntity) error {
	_, remote := e.(IThreadSafeRemoteEntity)
	if remote || IsEntityKindRemoteCapable(e.GetEntityKind()) || ResolveEntityID(e.GUId()).RemoteCapable {
		return ErrEntityGroupDomain
	}
	if g := gateOf(e); g != nil && (g.parent != nil || len(g.children) != 0 || g.reservations != 0 || g.retiring != 0) {
		return ErrEntityGroupDomain
	}
	return nil
}

// CheckEntityGroupMember 在当前 owner 的登记锁内核对精确实例及关系。调用方已 Touch；
// 检查不是锁或业务授权，实际索引更新仍须复核。
func CheckEntityGroupMember(e IThreadSafeEntity) error {
	if e == nil || e.Base() == nil {
		return ErrEntityNil
	}
	m := e.Base().owningManager()
	if m == nil {
		return ErrEntityNotManaged
	}
	m.addMu.Lock()
	defer m.addMu.Unlock()
	if m.entities.Get(e.ID()) != e || e.IsRemoved() || e.IsClear() {
		return ErrEntityNotManaged
	}
	return checkGroupMemberKindLocked(e)
}

// EntityGroupTransitionState 描述框架级的待定锁组迁移。它是派发门，不是锁：待定期间该实体的普通派发重新准入。
type EntityGroupTransitionState int32

const (
	EntityGroupTransitionNone EntityGroupTransitionState = iota
	EntityGroupTransitionJoin
	EntityGroupTransitionLeave
	EntityGroupTransitionMove
)

// GroupLockID 返回实体所属的锁组；0 表示实体只由自己的实体锁串行。
func (e *EntityBase) GroupLockID() int64 {
	if e == nil {
		return 0
	}
	return e.groupLockID.Load()
}

// GroupEpoch 在实体锁组成员关系每次变化时递增。
func (e *EntityBase) GroupEpoch() uint64 {
	if e == nil {
		return 0
	}
	return e.groupEpoch.Load()
}

// SetGroupLockIDForTest 直接设置锁组，只供测试。生产代码使用 EntityManager.UpdateEntityGroup，让组索引保持一致。
func (e *EntityBase) SetGroupLockIDForTest(permit entitypermit.Token, groupID int64) {
	if !permit.Valid() {
		panic(ErrEntityLockScope)
	}
	e.setGroupLockID(groupID)
}

func (e *EntityBase) setGroupLockID(groupID int64) {
	if e == nil {
		return
	}
	if e.groupLockID.Swap(groupID) != groupID {
		e.groupEpoch.Add(1)
	}
}

// GroupTransitionState 返回待定的锁组迁移类型；没有时为 None。
func (e *EntityBase) GroupTransitionState() EntityGroupTransitionState {
	if e == nil {
		return EntityGroupTransitionNone
	}
	return EntityGroupTransitionState(e.groupState.Load())
}

// GroupTransitionTargetID 返回待定迁移的目标锁组。
func (e *EntityBase) GroupTransitionTargetID() int64 {
	if e == nil {
		return 0
	}
	return e.groupTargetID.Load()
}

// BeginGroupTransition 登记一次待定迁移；已有待定迁移时返回 false。
func (e *EntityBase) BeginGroupTransition(permit entitypermit.Token, state EntityGroupTransitionState, targetGroupID int64) bool {
	if !permit.Valid() {
		panic(ErrEntityLockScope)
	}
	if e == nil || state == EntityGroupTransitionNone {
		return false
	}
	if !e.groupState.CompareAndSwap(int32(EntityGroupTransitionNone), int32(state)) {
		return false
	}
	e.groupTargetID.Store(targetGroupID)
	return true
}

// ClearGroupTransition 清除待定迁移。
func (e *EntityBase) ClearGroupTransition(permit entitypermit.Token) {
	if !permit.Valid() {
		panic(ErrEntityLockScope)
	}
	if e == nil {
		return
	}
	e.groupTargetID.Store(0)
	e.groupState.Store(int32(EntityGroupTransitionNone))
}

// GroupTransitionPending 报告是否有待定迁移。
func (e *EntityBase) GroupTransitionPending() bool {
	return e != nil && e.GroupTransitionState() != EntityGroupTransitionNone
}

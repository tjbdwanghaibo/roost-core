package entity

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/lock"
	"math"
	"sync/atomic"
)

// IThreadSafeRemoteEntity extends IThreadSafeEntity with remote entity capabilities.
// Remote entities can be shared across servers and require distributed locking.
type IThreadSafeRemoteEntity interface {
	IThreadSafeEntity
	EntityVersion() int64
	// SetEntityVersion 只改 StateVersion、沿用当前 fence；同一 fence 下不能回退（RR-20260930-13），
	// 更小的版本返回 ErrRemoteVersionConflict 且不写入。
	SetEntityVersion(int64) error
	// ExcludeSId is the current ownership sid. Zero means the entity is
	// remotely shared/owned; a positive sid means that server owns the local
	// fast path. It is not the source of truth for whether the entity is
	// registered as remotely shared.
	ExcludeSId() int32
	// SetExcludeSId updates the current ownership sid.
	SetExcludeSId(int32)
	RemoteVersionVector() RemoteVersionVector
	SetRemoteVersionVector(RemoteVersionVector) error
	RemoteOwnershipState() RemoteOwnershipState
	TransitionRemoteOwnership(RemoteOwnershipState) error
}

// RemoteEntityBase extends EntityBase with remote entity fields.
// Embed this instead of EntityBase for entities that support remote access.
type RemoteEntityBase struct {
	EntityBase
	version    atomic.Pointer[RemoteVersionVector]
	excludeSId atomic.Int32
	ownerState atomic.Uint32
}

func (r *RemoteEntityBase) EntityVersion() int64 {
	version := r.RemoteVersionVector().StateVersion
	if version > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(version)
}

// SetEntityVersion 只改 StateVersion，其余维度（fence、marker、route）沿用当前值。
//
// 同一 fence 下 StateVersion 不能回退（RR-20260930-13，与 SetRemoteVersionVector 的 RR-20260927-15 同一规则）：这个入口
// 从不换 fence，所以“同 fence 回退”在这里就是“写更小的版本”。之前不做任何检查，迟到的调用能把已推进的版本写回旧值，
// RR-15 的判据因此有一条绕过路径。更小的版本返回 ErrRemoteVersionConflict、向量不变；相等或更大照常 CAS 写入。
// 负数按 0 处理（与之前一致），所以已推进过的实体写负数同样被拒绝。
func (r *RemoteEntityBase) SetEntityVersion(v int64) error {
	if v < 0 {
		v = 0
	}
	for {
		current := r.version.Load()
		next := RemoteVersionVector{StateVersion: uint64(v)}
		if current != nil {
			if uint64(v) < current.StateVersion {
				return ErrRemoteVersionConflict
			}
			next = *current
			next.StateVersion = uint64(v)
		}
		if r.version.CompareAndSwap(current, &next) {
			return nil
		}
	}
}

func (r *RemoteEntityBase) ExcludeSId() int32 {
	return r.excludeSId.Load()
}

func (r *RemoteEntityBase) SetExcludeSId(sid int32) {
	r.excludeSId.Store(sid)
}

// RemoteVersionVector returns the four independent version dimensions.
func (r *RemoteEntityBase) RemoteVersionVector() RemoteVersionVector {
	current := r.version.Load()
	if current == nil {
		return RemoteVersionVector{}
	}
	return *current
}

// SetRemoteVersionVector rejects a stale fence even if a caller still holds a
// Go reference to this entity after its distributed lease expired.
//
// 同一 fence 下 StateVersion 也不能回退（RR-20260927-15）：非 authority 的兼容装配里本节点的每笔写都沿用同一个
// fence，只比 fence 挡不住已提交事务的迟到重放把版本写回旧值。返回 ErrRemoteVersionConflict，调用方按过期处理
// （acknowledgeRemoteCommit 会再次核对 remoteReceiptObsolete）。正式装配每次写都由 GrantWrite 递增 fence，不走到这里。
func (r *RemoteEntityBase) SetRemoteVersionVector(version RemoteVersionVector) error {
	if version.StateVersion > math.MaxInt64 {
		return ErrRemoteVersionConflict
	}
	for {
		current := r.version.Load()
		if current != nil && version.LockFence < current.LockFence {
			return ErrRemoteFenced
		}
		if current != nil && version.LockFence == current.LockFence && version.StateVersion < current.StateVersion {
			return ErrRemoteVersionConflict
		}
		next := version
		if r.version.CompareAndSwap(current, &next) {
			return nil
		}
	}
}

// NewRemoteEntityBase creates a remote-capable entity base with one coherent
// version vector. The vector is published through a single atomic pointer, so
// readers can never observe fields from different ownership generations.
func NewRemoteEntityBase(id int64, category EntityCategory, notAutoPersist bool, kind EntityKind) *RemoteEntityBase {
	return NewRemoteEntityBaseWithMutex(id, category, notAutoPersist, nil, kind)
}

func NewRemoteEntityBaseWithMutex(id int64, category EntityCategory, notAutoPersist bool, mu lock.Mutex, kind EntityKind) *RemoteEntityBase {
	base := &RemoteEntityBase{EntityBase: *NewEntityBaseWithMutex(id, category, notAutoPersist, mu, kind)}
	initial := &RemoteVersionVector{MarkerEpoch: 1, RouteEpoch: 1}
	base.version.Store(initial)
	base.ownerState.Store(uint32(RemoteOwnershipUnknown))
	return base
}

func (r *RemoteEntityBase) RemoteOwnershipState() RemoteOwnershipState {
	return RemoteOwnershipState(r.ownerState.Load())
}

func (r *RemoteEntityBase) TransitionRemoteOwnership(to RemoteOwnershipState) error {
	for {
		from := RemoteOwnershipState(r.ownerState.Load())
		if !ValidRemoteOwnershipTransition(from, to) {
			return fmt.Errorf("%w: %s -> %s", ErrRemoteInvalidStateTransition, from, to)
		}
		if r.ownerState.CompareAndSwap(uint32(from), uint32(to)) {
			return nil
		}
	}
}

// IsRemoteCapable returns true if this entity's ID has the remote-capable bit.
// It does not check the runtime remote marker store.
func (r *RemoteEntityBase) IsRemoteCapable() bool {
	return IsRemoteCapableEntityID(r.GUId())
}

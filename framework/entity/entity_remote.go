package entity

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// IThreadSafeRemoteEntity 在 IThreadSafeEntity 之上加入 Remote 实体能力。Remote 实体可在多个服务之间共享，写入需要
// 分布式所有权保护。
type IThreadSafeRemoteEntity interface {
	IThreadSafeEntity
	EntityVersion() int64
	// SetEntityVersion 只改 StateVersion、沿用当前 fence；同一 fence 下不能回退，
	// 更小的版本返回 ErrRemoteVersionConflict 且不写入。
	SetEntityVersion(int64) error
	// ExcludeSId 是当前所有者 sid。0 表示实体处于 Remote 共享 / 托管；正数表示该服务独占本地快路径。它不是
	// “实体是否登记为 Remote 共享”的权威来源。
	ExcludeSId() int32
	// SetExcludeSId 更新当前所有者 sid。
	SetExcludeSId(int32)
	RemoteVersionVector() RemoteVersionVector
	SetRemoteVersionVector(RemoteVersionVector) error
	RemoteOwnershipState() RemoteOwnershipState
	TransitionRemoteOwnership(RemoteOwnershipState) error
}

// RemoteEntityBase 在 EntityBase 之上加入 Remote 实体字段。支持 Remote 访问的实体嵌入它而不是 EntityBase。
type RemoteEntityBase struct {
	EntityBase
	version    atomic.Pointer[RemoteVersionVector]
	excludeSId atomic.Int32
	ownerState atomic.Uint32
}

// EntityVersion 返回 StateVersion（超过 int64 时饱和）。
func (r *RemoteEntityBase) EntityVersion() int64 {
	version := r.RemoteVersionVector().StateVersion
	if version > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(version)
}

// SetEntityVersion 只改 StateVersion，其余维度（fence、marker、route）沿用当前值。
//
// 同一 fence 下 StateVersion 不能回退（与 SetRemoteVersionVector 同一规则）：这个入口
// 从不换 fence，所以“同 fence 回退”在这里就是“写更小的版本”：迟到的调用不能把已推进的版本写回旧值。更小的版本
// 返回 ErrRemoteVersionConflict、向量不变；相等或更大照常 CAS 写入。负数按 0 处理，所以已推进过的实体写负数同样被拒绝。
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

// ExcludeSId 返回当前所有者 sid。
func (r *RemoteEntityBase) ExcludeSId() int32 {
	return r.excludeSId.Load()
}

// SetExcludeSId 更新当前所有者 sid。
func (r *RemoteEntityBase) SetExcludeSId(sid int32) {
	r.excludeSId.Store(sid)
}

// RemoteVersionVector 返回四个相互独立的版本维度。
func (r *RemoteEntityBase) RemoteVersionVector() RemoteVersionVector {
	current := r.version.Load()
	if current == nil {
		return RemoteVersionVector{}
	}
	return *current
}

// SetRemoteVersionVector 原子地替换版本向量。fence 比当前旧时返回 ErrRemoteFenced——即使调用方在分布式租约过期后
// 仍持有本实体的引用也写不进来。
//
// 同一 fence 下 StateVersion 也不能回退：非 authority 的兼容装配里本节点的每笔写都沿用同一个
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

// NewRemoteEntityBase 创建带一致版本向量的 Remote 实体基座。版本向量经单个原子指针发布，读者不会看到来自不同所有权
// 代际的字段组合。
func NewRemoteEntityBase(id int64, category EntityCategory, notAutoPersist bool, kind EntityKind) *RemoteEntityBase {
	return NewRemoteEntityBaseWithMutex(id, category, notAutoPersist, nil, kind)
}

// NewRemoteEntityBaseWithMutex 与 NewRemoteEntityBase 相同，但使用指定的实体锁。初始版本向量的 marker / 路由代际为 1，
// 所有权状态为 Unknown。
func NewRemoteEntityBaseWithMutex(id int64, category EntityCategory, notAutoPersist bool, mu lock.Mutex, kind EntityKind) *RemoteEntityBase {
	base := &RemoteEntityBase{EntityBase: *NewEntityBaseWithMutex(id, category, notAutoPersist, mu, kind)}
	initial := &RemoteVersionVector{MarkerEpoch: 1, RouteEpoch: 1}
	base.version.Store(initial)
	base.ownerState.Store(uint32(RemoteOwnershipUnknown))
	return base
}

// RemoteOwnershipState 返回当前所有权状态。
func (r *RemoteEntityBase) RemoteOwnershipState() RemoteOwnershipState {
	return RemoteOwnershipState(r.ownerState.Load())
}

// TransitionRemoteOwnership 按合法转换表把所有权状态 CAS 到 to；非法转换返回 ErrRemoteInvalidStateTransition。
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

// IsRemoteCapable 报告实体 ID 是否具有 Remote 能力；不查运行期的 Remote 标记存储。
func (r *RemoteEntityBase) IsRemoteCapable() bool {
	return IsRemoteCapableEntityID(r.GUId())
}

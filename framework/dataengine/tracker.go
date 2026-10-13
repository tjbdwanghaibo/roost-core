package dataengine

import "sync/atomic"

// Tracker 保存 DAO 已接受的持久版本和两类同步脏掩码：syncDirty 供服务间同步
// 消费，entitySyncDirty 供客户端 EntitySync 消费，二者由 MarkSync 同时置位、各自取走。
// 持久化的脏状态属于事务本地，不在这里记录。
type Tracker struct {
	version         atomic.Uint64
	syncDirty       atomic.Uint64
	entitySyncDirty atomic.Uint64
	syncVersion     atomic.Uint64
}

type TrackerSnapshot struct {
	Version         uint64
	SyncDirty       uint64
	EntitySyncDirty uint64
	SyncVersion     uint64
}

func (t *Tracker) Version() uint64 { return t.version.Load() }

func (t *Tracker) SetVersion(version uint64) { t.version.Store(version) }

// AcceptVersion 以 CAS 把版本从 expected 推进到 expected+1，否则返回 ErrInvalidVersion。
func (t *Tracker) AcceptVersion(expected, next uint64) error {
	if next == 0 || next != expected+1 || !t.version.CompareAndSwap(expected, next) {
		return ErrInvalidVersion
	}
	return nil
}

// AdvanceVersion 接受远端实体的权威版本，只允许前进或保持。同一远端聚合里
// 中间事务可能只改了另一个 DAO，因此本 DAO 的版本可以合法跳号。
func (t *Tracker) AdvanceVersion(next uint64) error {
	if next == 0 {
		return ErrInvalidVersion
	}
	for {
		current := t.version.Load()
		if current == next {
			return nil
		}
		if current > next {
			return ErrInvalidVersion
		}
		if t.version.CompareAndSwap(current, next) {
			return nil
		}
	}
}

func (t *Tracker) MarkSync(mask uint64) {
	if mask != 0 {
		t.syncDirty.Or(mask)
		t.entitySyncDirty.Or(mask)
	}
}

func (t *Tracker) SyncDirtyMask() uint64 { return t.syncDirty.Load() }

func (t *Tracker) HasSyncDirty() bool { return t.SyncDirtyMask() != 0 }

func (t *Tracker) Dirty() bool { return t.HasSyncDirty() }

func (t *Tracker) TakeSyncDirty() uint64 { return t.syncDirty.Swap(0) }

// TakeEntitySyncDirty 独立消费客户端同步变化，不影响服务间同步掩码。
func (t *Tracker) TakeEntitySyncDirty() uint64 { return t.entitySyncDirty.Swap(0) }

func (t *Tracker) SyncVersion() uint64 { return t.syncVersion.Load() }

func (t *Tracker) IncSyncVersion() uint64 { return t.syncVersion.Add(1) }

func (t *Tracker) SetSyncVersion(version uint64) { t.syncVersion.Store(version) }

func (t *Tracker) SelfCleanSync() { t.syncDirty.Store(0) }

func (t *Tracker) SelfClean() { t.SelfCleanSync() }

func (t *Tracker) Snapshot() TrackerSnapshot {
	if t == nil {
		return TrackerSnapshot{}
	}
	return TrackerSnapshot{
		Version:         t.version.Load(),
		SyncDirty:       t.syncDirty.Load(),
		EntitySyncDirty: t.entitySyncDirty.Load(),
		SyncVersion:     t.syncVersion.Load(),
	}
}

func (t *Tracker) Restore(snapshot TrackerSnapshot) {
	if t == nil {
		return
	}
	t.version.Store(snapshot.Version)
	t.syncDirty.Store(snapshot.SyncDirty)
	t.entitySyncDirty.Store(snapshot.EntitySyncDirty)
	t.syncVersion.Store(snapshot.SyncVersion)
}

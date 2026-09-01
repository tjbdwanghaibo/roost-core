package dataengine

import "sync/atomic"

// Tracker holds accepted persistence version and cross-server synchronization
// state. Persistence dirty state is transaction-local and intentionally absent.
type Tracker struct {
	version     atomic.Uint64
	syncDirty   atomic.Uint64
	syncVersion atomic.Uint64
}

type TrackerSnapshot struct {
	Version     uint64
	SyncDirty   uint64
	SyncVersion uint64
}

func (t *Tracker) Version() uint64 { return t.version.Load() }

func (t *Tracker) SetVersion(version uint64) { t.version.Store(version) }

func (t *Tracker) AcceptVersion(expected, next uint64) error {
	if next == 0 || next != expected+1 || !t.version.CompareAndSwap(expected, next) {
		return ErrInvalidVersion
	}
	return nil
}

// AdvanceVersion accepts an authoritative remote-entity version. A DAO can
// legitimately skip entity versions when another DAO in the same remote
// aggregate was the only document changed by an intervening transaction.
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
	}
}

func (t *Tracker) SyncDirtyMask() uint64 { return t.syncDirty.Load() }

func (t *Tracker) HasSyncDirty() bool { return t.SyncDirtyMask() != 0 }

func (t *Tracker) Dirty() bool { return t.HasSyncDirty() }

func (t *Tracker) TakeSyncDirty() uint64 { return t.syncDirty.Swap(0) }

func (t *Tracker) CommitSync(_ uint64) {}

func (t *Tracker) RollbackSync(mask uint64) { t.MarkSync(mask) }

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
		Version:     t.version.Load(),
		SyncDirty:   t.syncDirty.Load(),
		SyncVersion: t.syncVersion.Load(),
	}
}

func (t *Tracker) Restore(snapshot TrackerSnapshot) {
	if t == nil {
		return
	}
	t.version.Store(snapshot.Version)
	t.syncDirty.Store(snapshot.SyncDirty)
	t.syncVersion.Store(snapshot.SyncVersion)
}

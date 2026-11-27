package remoteentity

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

// AllowsExclusiveCompanionLocked is the optional Nest-facing proof for an implicit
// remote member. Call under the guards for the validated relation, on the batch
// prepared for THIS dispatch. This is neither an ownership query nor a lease that
// can be saved for the next dispatch. It loads nothing and never takes a write gate.
func (b *remoteWriteBatch) AllowsExclusiveCompanionLocked(member entity.IThreadSafeRemoteEntity) bool {
	if b == nil || member == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finalized || b.closed || b.aborted || b.indeterminate || b.committed || b.rejected {
		return false
	}
	for _, entry := range b.entries {
		if entry.entity == member {
			return b.currentExclusiveEntryLocked(entry)
		}
	}
	return false
}

// Called with b.mu and the admitted entity's lock held. Require both a durable
// authority and an exact, loaded-only manager lookup; the wrapper's old pointer
// alone is not evidence that this is still the instance managed for that ID.
func (b *remoteWriteBatch) currentExclusiveEntryLocked(entry *remoteWriteEntry) bool {
	if b.mgr == nil || b.mgr.FatalError() != nil || entry == nil || entry.distLock || entry.entity == nil || entry.wrapper == nil {
		return false
	}
	if _, durable := b.mgr.ownershipStore.(WriteAuthority); !durable {
		return false
	}
	local, ok := b.mgr.backend.(entity.IRemoteEntityLocalLookup)
	if !ok {
		return false
	}
	lease, e := entry.lease, entry.entity
	if !lease.Valid() || lease.OwnerSID == 0 || lease.OwnerSID != b.mgr.localSid || lease.Mode != entity.RemoteWriteOwnerRouted || lease.State != entity.RemoteOwnershipLocalOwned || e.RemoteOwnershipState() != entity.RemoteOwnershipLocalOwned {
		return false
	}
	if e.IsRemoved() || e.IsClear() || e.GUId() != lease.EntityID || e.GetEntityKind() != entry.wrapper.kind || !entity.IsEntityKindRemoteManaged(entry.wrapper.kind) || entity.RemoteKindShared(entry.wrapper.kind) || local.LookupLocalRemoteEntity(lease.EntityID, entry.wrapper.kind) != e {
		return false
	}
	v := e.RemoteVersionVector()
	return v.MarkerEpoch == lease.MarkerEpoch && v.RouteEpoch == lease.RouteEpoch && v.LockFence == lease.LockFence && v.StateVersion == lease.BaseVersion
}

// ConfirmEmptyCompanionLocked is the empty branch of Commit, at the only point
// at which Nest can expose a new relation under the original guards: AFTER
// definite local admission. An actual Remote mutation needs the normal unlocked
// commit/finalizer path and cannot accompany a relation change. No storage I/O
// or gate release here: Close still owns all the resources obtained by Prepare.
func (b *remoteWriteBatch) ConfirmEmptyCompanionLocked() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.finalized || b.closed || b.aborted || b.indeterminate || b.committed || b.rejected || len(b.entries) == 0 {
		return false
	}
	for _, e := range b.entries {
		if e.finalized || !b.currentExclusiveEntryLocked(e) {
			return false
		}
	}
	id := b.outcome.TransactionID
	b.mgr.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitCommitted})
	b.committed = true
	return true
}

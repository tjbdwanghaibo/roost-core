package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/redis"
)

type remoteWriteEntry struct {
	wrapper   *remoteEntityWrapper
	entity    entity.IThreadSafeRemoteEntity
	lease     entity.RemoteWriteLease
	distLock  bool
	finalized bool
	commit    entity.RemoteCommit
}

type remoteWriteBatch struct {
	mgr     *remoteEntityManager
	entries []*remoteWriteEntry
	ids     []int64

	mu            sync.Mutex
	outcome       entity.RemoteTransactionOutcome
	finalized     bool
	committed     bool
	aborted       bool
	indeterminate bool
	closed        bool
	reserved      bool
}

var _ entity.RemoteWriteBatch = (*remoteWriteBatch)(nil)

func (m *remoteEntityManager) PrepareRemoteWriteBatch(ctx context.Context, ids []int64) (_ entity.RemoteWriteBatch, err error) {
	started := time.Now()
	batchClass := "single"
	if len(ids) > 1 {
		batchClass = "multi"
	}
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		labels := metrics.Labels{"result": result, "batch": batchClass}
		metrics.IncCounter("remote_entity.remote.prepare_total", labels, 1)
		metrics.ObserveDuration("remote_entity.remote.prepare_latency", labels, time.Since(started))
	}()
	if m == nil || m.cfg == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if fatal := m.fatalError(); fatal != nil {
		return nil, errors.Join(entity.ErrRemoteFenced, fatal)
	}
	m.startRemoteFinalizer()
	ordered, err := entity.ValidateRemoteWriteBatchIDs(ids)
	if err != nil {
		return nil, err
	}
	if len(ordered) == 0 {
		return &remoteWriteBatch{mgr: m}, nil
	}
	if m.cfg.MaxWriteBatch > 0 && len(ordered) > m.cfg.MaxWriteBatch {
		return nil, fmt.Errorf("%w: batch=%d max=%d", entity.ErrRemoteOverloaded, len(ordered), m.cfg.MaxWriteBatch)
	}
	if m.backend == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if !m.reserveRemoteFinalizeSlot() {
		return nil, entity.ErrRemoteOverloaded
	}
	if ctx == nil {
		ctx = context.Background()
	}
	batch := &remoteWriteBatch{mgr: m, ids: ordered, entries: make([]*remoteWriteEntry, 0, len(ordered)), reserved: true}
	for _, id := range ordered {
		meta := entity.ResolveEntityID(id)
		wrapper := m.getOrCreate(meta.FullID, meta.Category, meta.Kind)
		if wrapper == nil {
			_ = batch.Abort(ctx, entity.ErrRemoteRejected)
			_ = batch.Close(ctx)
			return nil, fmt.Errorf("%w: wrapper capacity reached for %d", entity.ErrRemoteOverloaded, id)
		}
		entry, beginErr := wrapper.beginWrite(ctx)
		if beginErr != nil {
			wrapper.release()
			_ = batch.Abort(ctx, beginErr)
			_ = batch.Close(ctx)
			return nil, beginErr
		}
		batch.entries = append(batch.entries, entry)
	}
	return batch, nil
}

func (w *remoteEntityWrapper) beginWrite(parent context.Context) (*remoteWriteEntry, error) {
	if w == nil || w.mgr == nil {
		return nil, entity.ErrRemoteRejected
	}
	gateStarted := time.Now()
	select {
	case w.writeGate <- struct{}{}:
		metrics.ObserveDuration("remote_entity.remote.write_gate_wait", nil, time.Since(gateStarted))
	case <-parent.Done():
		return nil, parent.Err()
	}
	w.ownershipMu.RLock()
	release := func() {
		w.ownershipMu.RUnlock()
		<-w.writeGate
	}
	ctx := parent
	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, w.mgr.cfg.OpTimeout)
		defer cancel()
	}
	if err := w.ensureMarker(ctx); err != nil {
		release()
		return nil, fmt.Errorf("remote_entity: refresh marker %d: %w", w.id, err)
	}
	if _, found := w.cachedOwnership(); !found {
		lease, err := w.mgr.ownershipStore.ClaimOwnership(ctx, w.id, w.mgr.localSid)
		if err != nil {
			// Refresh after a failed CAS so the error reports the actual owner
			// that won the claim instead of treating absence as local ownership.
			_ = w.refreshMarked(ctx)
			current, _ := w.cachedOwnership()
			release()
			return nil, errors.Join(ownershipFenceError(w.id, current), err)
		}
		if !validOwnershipLease(lease) || lease.OwnerSid != w.mgr.localSid {
			release()
			return nil, fmt.Errorf("%w: invalid ownership claim for entity=%d", entity.ErrRemoteFenced, w.id)
		}
		w.applyOwnership(lease)
	}
	marked := w.isMarked()
	if !marked && !w.isLocalOwner() {
		release()
		return nil, fmt.Errorf("%w: entity=%d owner=%d", entity.ErrRemoteFenced, w.id, w.leaseOwner())
	}
	distLocked := false
	lockFence := uint64(0)
	if marked {
		if err := w.rMu.Lock(ctx); err != nil {
			release()
			return nil, fmt.Errorf("remote_entity: shared lock %d: %w", w.id, err)
		}
		distLocked = true
		// The public contract, not a duck type: the manager refuses a factory
		// whose locks lack it at construction, so this assertion holds for
		// every wrapper that exists. The zero check below stays as the
		// last-line guard against a lock that has the method and lies.
		if provider, ok := w.rMu.(redis.IFencedVersionedLock); ok {
			lockFence = provider.Fence()
		}
		if lockFence == 0 {
			_ = w.unlockObserved(ctx, w.rMu.Version())
			release()
			return nil, fmt.Errorf("%w: shared lock did not allocate fence", entity.ErrRemoteFenced)
		}
		if err := w.refreshMarked(ctx); err != nil || !w.isMarked() {
			_ = w.unlockObserved(ctx, w.rMu.Version())
			release()
			return nil, errors.Join(entity.ErrRemoteOwnerTransition, err)
		}
	}

	w.markerMu.RLock()
	marker := w.markerLease
	w.markerMu.RUnlock()
	if !validOwnershipLease(marker) {
		if distLocked {
			_ = w.unlockObserved(ctx, w.rMu.Version())
		}
		release()
		return nil, ownershipFenceError(w.id, marker)
	}

	remoteEntity := w.lookupLocalEntity()
	if remoteEntity == nil || (marked && (remoteEntity.EntityVersion() != w.rMu.Version() || w.rMu.Version() <= 0)) {
		var loadErr error
		remoteEntity, loadErr = w.loadEntity(ctx)
		if loadErr != nil {
			if distLocked {
				_ = w.unlockObserved(ctx, w.rMu.Version())
			}
			release()
			return nil, fmt.Errorf("remote_entity: load entity %d: %w", w.id, loadErr)
		}
	}
	if remoteEntity == nil {
		if distLocked {
			_ = w.unlockObserved(ctx, w.rMu.Version())
		}
		release()
		return nil, fmt.Errorf("%w: entity %d not found", entity.ErrRemoteRejected, w.id)
	}
	if remote, ok := remoteEntity.(entity.IThreadSafeRemoteEntity); ok {
		state := remote.RemoteOwnershipState()
		switch state {
		case entity.RemoteOwnershipDraining, entity.RemoteOwnershipFenced, entity.RemoteOwnershipQuarantined:
			if distLocked {
				_ = w.unlockObserved(ctx, w.rMu.Version())
			}
			release()
			cause := entity.ErrRemoteOwnerTransition
			if state == entity.RemoteOwnershipFenced || state == entity.RemoteOwnershipQuarantined {
				cause = entity.ErrRemoteFenced
			}
			return nil, fmt.Errorf("%w: entity=%d state=%s", cause, w.id, state)
		}
	}
	w.attachEntity(remoteEntity)
	stateVersion := remoteEntity.EntityVersion()
	if stateVersion < 0 {
		stateVersion = 0
	}
	if current, ok := remoteEntity.(entity.IThreadSafeRemoteEntity); ok && !marked {
		lockFence = current.RemoteVersionVector().LockFence
	}
	state := entity.RemoteOwnershipLocalOwned
	mode := entity.RemoteWriteOwnerRouted
	if marked {
		state = entity.RemoteOwnershipShared
		mode = entity.RemoteWriteSharedLock
	}
	lease := entity.RemoteWriteLease{
		EntityID: w.id, OwnerSID: w.mgr.localSid, Mode: mode, State: state,
		BaseVersion: uint64(stateVersion), MarkerEpoch: marker.MarkerEpoch, LockFence: lockFence,
		RouteEpoch: marker.RouteEpoch, AcquiredAt: time.Now().UnixNano(),
	}
	if remote, ok := remoteEntity.(entity.IThreadSafeRemoteEntity); ok {
		if err := remote.SetRemoteVersionVector(entity.RemoteVersionVector{
			StateVersion: lease.BaseVersion, MarkerEpoch: lease.MarkerEpoch,
			LockFence: lease.LockFence, RouteEpoch: lease.RouteEpoch,
		}); err != nil {
			if distLocked {
				_ = w.unlockObserved(ctx, w.rMu.Version())
			}
			release()
			return nil, fmt.Errorf("remote_entity: update version vector %d: %w", w.id, err)
		}
		if err := remote.TransitionRemoteOwnership(state); err != nil {
			if distLocked {
				_ = w.unlockObserved(ctx, w.rMu.Version())
			}
			release()
			return nil, fmt.Errorf("remote_entity: update ownership state %d: %w", w.id, err)
		}
	}
	return &remoteWriteEntry{wrapper: w, entity: remoteEntity, lease: lease, distLock: distLocked}, nil
}

func (b *remoteWriteBatch) EntityIDs() []int64 {
	if b == nil {
		return nil
	}
	return append([]int64(nil), b.ids...)
}

func (b *remoteWriteBatch) FinalizeLocked(outcome entity.RemoteTransactionOutcome) error {
	if b == nil || b.mgr == nil || !outcome.Succeeded || outcome.TransactionID.IsZero() {
		return entity.ErrRemoteCommitNotFinalized
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finalized || b.committed || b.aborted || b.closed {
		return entity.ErrRemoteCommitNotFinalized
	}
	for _, entry := range b.entries {
		if entry.entity == nil {
			continue
		}
		participant, ok := entry.entity.(entity.IRemoteCommitParticipant)
		if !ok {
			b.rollbackFinalizedLocked()
			return fmt.Errorf("%w: entity %d has no generated remote commit participant", entity.ErrRemoteWriteCapabilityDisabled, entry.lease.EntityID)
		}
		deleteRequested := outcome.DeleteIntents != nil && outcome.DeleteIntents.RemoteDeleteRequested(entry.lease.EntityID)
		if !deleteRequested {
			if transactional, ok := participant.(entity.IRemoteCommitChangeParticipant); ok {
				if !transactional.HasRemoteCommitLocked(outcome) {
					continue
				}
			} else if !hasEntityDirty(entry.entity) {
				continue
			}
		}
		commit, err := participant.BuildRemoteCommitLocked(entry.lease, outcome)
		if err != nil {
			b.rollbackFinalizedLocked()
			return fmt.Errorf("remote_entity: build commit %d: %w", entry.lease.EntityID, err)
		}
		commit.TransactionID = outcome.TransactionID
		commit.EntityID = entry.lease.EntityID
		commit.Kind = entry.entity.GetEntityKind()
		if commit.Delete != deleteRequested {
			participant.RollbackRemoteCommit(commit)
			b.rollbackFinalizedLocked()
			return fmt.Errorf("remote_entity: delete intent mismatch for entity %d", entry.lease.EntityID)
		}
		commit.BaseVersion = entry.lease.BaseVersion
		commit.NextVersion = entry.lease.BaseVersion + 1
		commit.MarkerEpoch = entry.lease.MarkerEpoch
		commit.LockFence = entry.lease.LockFence
		commit.RouteEpoch = entry.lease.RouteEpoch
		for i := range commit.Snapshots {
			commit.Snapshots[i].BaseVersion = commit.BaseVersion
			commit.Snapshots[i].StateVersion = commit.NextVersion
			commit.Snapshots[i].MarkerEpoch = commit.MarkerEpoch
			commit.Snapshots[i].RouteEpoch = commit.RouteEpoch
		}
		if err := commit.Validate(); err != nil {
			participant.RollbackRemoteCommit(commit)
			b.rollbackFinalizedLocked()
			return fmt.Errorf("remote_entity: validate commit %d: %w", entry.lease.EntityID, err)
		}
		entry.commit = commit.Clone()
		entry.finalized = true
	}
	if err := b.mgr.trackRemoteTransaction(outcome.TransactionID); err != nil {
		b.rollbackFinalizedLocked()
		return err
	}
	b.outcome = outcome
	b.finalized = true
	return nil
}

func (b *remoteWriteBatch) rollbackFinalizedLocked() {
	for _, entry := range b.entries {
		if !entry.finalized || entry.entity == nil {
			continue
		}
		if participant, ok := entry.entity.(entity.IRemoteCommitParticipant); ok {
			participant.RollbackRemoteCommit(entry.commit.Clone())
		}
		entry.commit = entity.RemoteCommit{}
		entry.finalized = false
	}
}

func (b *remoteWriteBatch) Commits() []entity.RemoteCommit {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	commits := make([]entity.RemoteCommit, 0, len(b.entries))
	for _, entry := range b.entries {
		if entry.finalized {
			commits = append(commits, entry.commit.Clone())
		}
	}
	return commits
}

func (b *remoteWriteBatch) Commit(ctx context.Context) ([]entity.RemoteCommitReceipt, error) {
	if b == nil || b.mgr == nil {
		return nil, entity.ErrRemoteCommitNotFinalized
	}
	b.mu.Lock()
	if !b.finalized || b.aborted || b.closed {
		b.mu.Unlock()
		return nil, entity.ErrRemoteCommitNotFinalized
	}
	if b.committed {
		commits := b.commitsLocked()
		b.mu.Unlock()
		status, err := b.mgr.RemoteCommitStatus(ctx, b.outcome.TransactionID)
		if err != nil {
			return nil, err
		}
		if len(status.Receipts) > 0 {
			return status.Receipts, nil
		}
		return speculativeReceipts(commits), nil
	}
	commits := b.commitsLocked()
	outcome := b.outcome
	b.mu.Unlock()

	var receipts []entity.RemoteCommitReceipt
	var err error
	if len(commits) == 0 {
		b.mgr.completeRemoteTransaction(outcome.TransactionID, entity.RemoteCommitStatus{TransactionID: outcome.TransactionID, State: entity.RemoteCommitCommitted})
	} else if outcome.Durability == 0 {
		receipts, err = b.mgr.ApplyRemoteCommits(ctx, outcome.TransactionID, commits)
	} else if outcome.Durability == 1 {
		receipts = speculativeReceipts(commits)
	} else {
		var status entity.RemoteCommitStatus
		status, err = b.mgr.waitRemoteTransaction(ctx, outcome.TransactionID)
		receipts = status.Receipts
	}
	if err != nil {
		if outcome.Durability == 2 || errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
			b.mu.Lock()
			b.indeterminate = true
			b.mu.Unlock()
		} else if outcome.Durability == 0 {
			b.mgr.rollbackRemoteEntries(b.entries)
		}
		return nil, errors.Join(err, b.mgr.quarantineEntries(b.entries, err))
	}
	b.mu.Lock()
	b.committed = true
	b.mu.Unlock()
	return receipts, nil
}

func (b *remoteWriteBatch) Abort(_ context.Context, cause error) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.committed || b.indeterminate || b.closed {
		b.mu.Unlock()
		return nil
	}
	b.aborted = true
	if b.finalized {
		b.rollbackFinalizedLocked()
	}
	txID := b.outcome.TransactionID
	b.mu.Unlock()
	if !txID.IsZero() && b.mgr != nil {
		b.mgr.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitRejected, Cause: errorString(cause)})
	}
	return nil
}

func (b *remoteWriteBatch) Indeterminate(_ context.Context, _ error) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.aborted || !b.finalized {
		return entity.ErrRemoteCommitNotFinalized
	}
	b.indeterminate = true
	return nil
}

func (b *remoteWriteBatch) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	entries := append([]*remoteWriteEntry(nil), b.entries...)
	deferred := b.indeterminate || (b.committed && b.outcome.Durability == 1 && len(b.commitsLocked()) > 0)
	txID := b.outcome.TransactionID
	b.entries = nil
	b.mu.Unlock()
	if deferred {
		if err := b.mgr.deferRemoteClose(deferredRemoteClose{txID: txID, entries: entries}); err == nil {
			return nil
		}
	}
	err := b.mgr.releaseRemoteEntries(ctx, entries)
	if b.reserved {
		b.mgr.releaseRemoteFinalizeSlot()
		b.reserved = false
	}
	return err
}

func (b *remoteWriteBatch) commitsLocked() []entity.RemoteCommit {
	commits := make([]entity.RemoteCommit, 0, len(b.entries))
	for _, entry := range b.entries {
		if entry.finalized {
			commits = append(commits, entry.commit.Clone())
		}
	}
	return commits
}

func speculativeReceipts(commits []entity.RemoteCommit) []entity.RemoteCommitReceipt {
	receipts := make([]entity.RemoteCommitReceipt, len(commits))
	for i, commit := range commits {
		receipts[i] = entity.RemoteCommitReceipt{
			TransactionID: commit.TransactionID, EntityID: commit.EntityID,
			StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch,
			LockFence: commit.LockFence, RouteEpoch: commit.RouteEpoch,
		}
	}
	return receipts
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

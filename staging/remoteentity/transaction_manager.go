package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

type remoteTransactionTracker struct {
	done     chan struct{}
	status   entity.RemoteCommitStatus
	closed   bool
	closedAt int64
}

type remoteVersionWaiter struct {
	version uint64
	done    chan struct{}
}

type remoteState struct {
	cache                 *entity.RemoteSnapshotCache
	interests             *remoteInterestRegistry
	localInterestMu       sync.Mutex
	localInterestLocks    [64]sync.Mutex
	localInterests        map[entity.RemoteSnapshotKey]int64
	localInterestOps      atomic.Uint64
	localInterestCapacity int

	txMu sync.Mutex
	txs  map[entity.RemoteTransactionID]*remoteTransactionTracker

	versionMu    sync.Mutex
	versions     map[int64]uint64
	versionOrder []int64
	maxVersions  int
	waiters      map[int64][]remoteVersionWaiter
	waiterCount  int
	maxWaiters   int
	txCapacity   int
	txTTL        time.Duration

	finalizeCtx    context.Context
	finalizeCancel context.CancelFunc
	finalizeQueue  chan deferredRemoteClose
	finalizeSlots  chan struct{}
	finalizeDone   chan struct{}
	finalizeWG     sync.WaitGroup
	retryWG        sync.WaitGroup
	retryMu        sync.Mutex
	stopping       bool
	finalizeOnce   sync.Once
}

type deferredRemoteClose struct {
	txID    entity.RemoteTransactionID
	entries []*remoteWriteEntry
	attempt int
}

func newRemoteState(mgr *remoteEntityManager, cfg *Config, snapshotL2 ...cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]) *remoteState {
	finalizeCtx, finalizeCancel := context.WithCancel(context.Background())
	capacity := cfg.AsyncFinalizeCapacity
	if capacity <= 0 {
		capacity = 4096
	}
	state := &remoteState{
		txs:                   make(map[entity.RemoteTransactionID]*remoteTransactionTracker),
		txCapacity:            cfg.TransactionTrackLimit,
		txTTL:                 cfg.TransactionTrackTTL,
		versions:              make(map[int64]uint64),
		maxVersions:           cfg.SnapshotCacheEntries,
		waiters:               make(map[int64][]remoteVersionWaiter),
		maxWaiters:            cfg.SnapshotMaxWaiters,
		interests:             newRemoteInterestRegistry(cfg.SnapshotInterestKeys, cfg.SnapshotInterestSubs),
		localInterests:        make(map[entity.RemoteSnapshotKey]int64),
		localInterestCapacity: cfg.SnapshotInterestKeys,
		finalizeCtx:           finalizeCtx, finalizeCancel: finalizeCancel,
		finalizeQueue: make(chan deferredRemoteClose, capacity),
		finalizeSlots: make(chan struct{}, capacity), finalizeDone: make(chan struct{}),
	}
	var l2 cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]
	if len(snapshotL2) > 0 {
		l2 = snapshotL2[0]
	}
	state.cache = entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{
		Shards: cfg.SnapshotCacheShards, MaxEntries: cfg.SnapshotCacheEntries,
		MaxBytes: cfg.SnapshotCacheBytes, TTL: cfg.SnapshotCacheTTL,
		LoadTimeout: cfg.SnapshotLoadTimeout, MaxWaiters: cfg.SnapshotMaxWaiters,
	}, l2, func(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		if mgr == nil || mgr.backend == nil {
			return entity.RemoteSnapshotEnvelope{}, false, nil
		}
		return mgr.backend.LoadRemoteSnapshot(ctx, key, consistency, minVersion)
	})
	return state
}

func (m *remoteEntityManager) startRemoteFinalizer() {
	if m == nil || m.remote == nil {
		return
	}
	workers := m.cfg.AsyncFinalizeWorkers
	if workers <= 0 {
		workers = 16
	}
	m.remote.finalizeOnce.Do(func() { go m.runRemoteFinalizers(m.remote, workers) })
}

func (m *remoteEntityManager) reserveRemoteFinalizeSlot() bool {
	if m == nil || m.remote == nil {
		return false
	}
	select {
	case m.remote.finalizeSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *remoteEntityManager) releaseRemoteFinalizeSlot() {
	if m == nil || m.remote == nil {
		return
	}
	select {
	case <-m.remote.finalizeSlots:
	default:
	}
}

func (m *remoteEntityManager) deferRemoteClose(item deferredRemoteClose) error {
	select {
	case m.remote.finalizeQueue <- item:
		return nil
	case <-m.remote.finalizeCtx.Done():
		return m.remote.finalizeCtx.Err()
	}
}

func (m *remoteEntityManager) runRemoteFinalizers(state *remoteState, workers int) {
	state.finalizeWG.Add(workers)
	for range workers {
		go m.runRemoteFinalizerWorker(state)
	}
	state.finalizeWG.Wait()
	state.retryWG.Wait()
	// Retry goroutines send without holding a lock, so one may race the
	// workers' final drain and land an item after they exited. All senders
	// are done here (retryWG), so one last drain guarantees every deferred
	// close releases its entries and slot before finalizeDone is published.
	for {
		select {
		case item := <-state.finalizeQueue:
			if quarantineErr := m.quarantineEntries(item.entries, state.finalizeCtx.Err()); quarantineErr != nil {
				metrics.IncCounter("remote_entity.quarantine_error_total", nil, 1)
			}
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteFinalizeSlot()
		default:
			close(state.finalizeDone)
			return
		}
	}
}

func (m *remoteEntityManager) runRemoteFinalizerWorker(state *remoteState) {
	defer state.finalizeWG.Done()
	for {
		select {
		case item := <-state.finalizeQueue:
			m.processDeferredRemoteClose(state, item)
		case <-state.finalizeCtx.Done():
			for {
				select {
				case item := <-state.finalizeQueue:
					if quarantineErr := m.quarantineEntries(item.entries, state.finalizeCtx.Err()); quarantineErr != nil {
						metrics.IncCounter("remote_entity.quarantine_error_total", nil, 1)
					}
					m.releaseRemoteEntriesObserved(context.Background(), item.entries)
					m.releaseRemoteFinalizeSlot()
				default:
					return
				}
			}
		}
	}
}

func (m *remoteEntityManager) processDeferredRemoteClose(state *remoteState, item deferredRemoteClose) {
	status, err := m.RemoteCommitStatus(state.finalizeCtx, item.txID)
	if err == nil && status.State == entity.RemoteCommitApplied {
		err = m.publishAppliedRemoteTransaction(state.finalizeCtx, status)
		if err == nil {
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteFinalizeSlot()
			return
		}
	}
	if err == nil && status.State == entity.RemoteCommitCommitted {
		err = m.reconcileRemoteEntries(state.finalizeCtx, item.entries, status.Receipts)
		if err == nil {
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteFinalizeSlot()
			return
		}
	}
	if status.State == entity.RemoteCommitRejected {
		m.rollbackRemoteEntries(item.entries)
		m.releaseRemoteEntriesObserved(context.Background(), item.entries)
		m.releaseRemoteFinalizeSlot()
		return
	}
	// One failed/pending transaction must not monopolize a finalizer worker.
	// Keep its write gate and fence, quarantine the live object, and retry with
	// bounded exponential backoff on a timer.
	if quarantineErr := m.quarantineEntries(item.entries, err); quarantineErr != nil {
		metrics.IncCounter("remote_entity.quarantine_error_total", nil, 1)
	}
	metrics.IncCounter("remote_entity.finalize_retry_total", nil, 1)
	item.attempt++
	delay := m.cfg.FinalizeRetryInterval
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	for i := 1; i < item.attempt && delay < 5*time.Second; i++ {
		delay *= 2
	}
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	state.retryWG.Add(1)
	go func() {
		defer state.retryWG.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-state.finalizeCtx.Done():
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteFinalizeSlot()
			return
		case <-timer.C:
		}
		state.retryMu.Lock()
		stopping := state.stopping
		state.retryMu.Unlock()
		if stopping {
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteFinalizeSlot()
			return
		}
		// Send outside the lock: holding retryMu across a bounded-queue send
		// stalls Stop behind a full queue. The Done branch and the final
		// drain in runRemoteFinalizers together guarantee the item is always
		// consumed or released, whichever side wins the race.
		select {
		case state.finalizeQueue <- item:
		case <-state.finalizeCtx.Done():
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteFinalizeSlot()
		}
	}()
}

func (m *remoteEntityManager) reconcileRemoteEntries(ctx context.Context, entries []*remoteWriteEntry, receipts []entity.RemoteCommitReceipt) error {
	byEntity := make(map[int64]entity.RemoteCommitReceipt, len(receipts))
	for _, receipt := range receipts {
		byEntity[receipt.EntityID] = receipt
	}
	for _, entry := range entries {
		if entry == nil || !entry.finalized {
			continue
		}
		receipt, ok := byEntity[entry.commit.EntityID]
		if !ok {
			return fmt.Errorf("%w: committed transaction missing receipt for entity %d", entity.ErrRemotePersistenceIndeterminate, entry.commit.EntityID)
		}
		if err := m.afterRemoteCommit(ctx, entry.commit.Clone(), receipt); err != nil {
			return err
		}
	}
	return nil
}

func (m *remoteEntityManager) stopRemoteFinalizer(ctx context.Context) error {
	if m == nil || m.remote == nil {
		return nil
	}
	m.startRemoteFinalizer()
	m.remote.retryMu.Lock()
	m.remote.stopping = true
	m.remote.finalizeCancel()
	m.remote.retryMu.Unlock()
	select {
	case <-m.remote.finalizeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *remoteEntityManager) releaseRemoteEntries(parent context.Context, entries []*remoteWriteEntry) error {
	if parent == nil {
		parent = context.Background()
	}
	var joined error
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry == nil || entry.wrapper == nil {
			continue
		}
		if entry.distLock && entry.wrapper.rMu.IsAcquired() {
			version := int64(entry.lease.BaseVersion)
			if entry.finalized {
				version = int64(entry.commit.NextVersion)
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), m.cfg.OpTimeout)
			err := entry.wrapper.rMu.UnlockWithRetry(ctx, version, m.cfg.VersionTTL, m.cfg.UnlockRetryCount, m.cfg.UnlockRetryInterval)
			cancel()
			if err != nil {
				joined = errors.Join(joined, entity.ErrRemoteReleaseIncomplete, err)
			}
		}
		entry.wrapper.ownershipMu.RUnlock()
		<-entry.wrapper.writeGate
		entry.wrapper.release()
	}
	return joined
}

func (m *remoteEntityManager) releaseRemoteEntriesObserved(parent context.Context, entries []*remoteWriteEntry) {
	if err := m.releaseRemoteEntries(parent, entries); err != nil {
		m.recordReleaseFailure(err)
	}
}

func (m *remoteEntityManager) ApplyRemoteCommits(ctx context.Context, txID entity.RemoteTransactionID, commits []entity.RemoteCommit) (receipts []entity.RemoteCommitReceipt, err error) {
	started := time.Now()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		labels := metrics.Labels{"result": result}
		metrics.IncCounter("remote_entity.remote.apply_total", labels, 1)
		metrics.ObserveDuration("remote_entity.remote.apply_latency", labels, time.Since(started))
	}()
	if m == nil || m.backend == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if txID.IsZero() {
		return nil, entity.ErrRemoteRejected
	}
	if err := m.trackRemoteTransaction(txID); err != nil {
		return nil, err
	}
	cloned := make([]entity.RemoteCommit, len(commits))
	for i, commit := range commits {
		if commit.TransactionID != txID {
			return nil, fmt.Errorf("%w: transaction mismatch", entity.ErrRemoteRejected)
		}
		if err := commit.Validate(); err != nil {
			return nil, err
		}
		cloned[i] = commit.Clone()
	}
	if len(cloned) > 1 {
		receipts, err = m.backend.CommitRemoteBatch(ctx, cloned)
	} else if len(cloned) == 1 {
		var receipt entity.RemoteCommitReceipt
		receipt, err = m.backend.CommitRemote(ctx, cloned[0])
		receipts = []entity.RemoteCommitReceipt{receipt}
	}
	if err != nil {
		state := entity.RemoteCommitIndeterminate
		if errors.Is(err, entity.ErrRemoteFenced) || errors.Is(err, entity.ErrRemoteVersionConflict) || errors.Is(err, entity.ErrRemoteRejected) {
			state = entity.RemoteCommitRejected
		}
		m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: state, Cause: err.Error()})
		return nil, err
	}
	if len(receipts) != len(cloned) {
		err = fmt.Errorf("%w: receipt count=%d commits=%d", entity.ErrRemotePersistenceIndeterminate, len(receipts), len(cloned))
		m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitIndeterminate, Cause: err.Error()})
		return nil, err
	}
	for i := range cloned {
		if err := m.afterRemoteCommit(ctx, cloned[i], receipts[i]); err != nil {
			m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitIndeterminate, Receipts: receipts, Cause: err.Error()})
			return nil, errors.Join(entity.ErrRemotePersistenceIndeterminate, err)
		}
	}
	if err := m.backend.MarkRemoteCommitPublished(ctx, txID); err != nil {
		m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitIndeterminate, Receipts: receipts, Commits: cloned, Cause: err.Error()})
		return nil, errors.Join(entity.ErrRemotePersistenceIndeterminate, err)
	}
	status := entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitCommitted, Receipts: append([]entity.RemoteCommitReceipt(nil), receipts...)}
	m.completeRemoteTransaction(txID, status)
	return receipts, nil
}

func (m *remoteEntityManager) recoverRemoteOutbox(ctx context.Context) error {
	outbox := m.backend
	for {
		pending, err := outbox.PendingRemoteCommits(ctx, 256)
		if err != nil {
			return fmt.Errorf("remote_entity: load commit outbox: %w", err)
		}
		if len(pending) == 0 {
			return nil
		}
		for _, status := range pending {
			if err := m.publishAppliedRemoteTransaction(ctx, status); err != nil {
				return fmt.Errorf("remote_entity: recover transaction %s: %w", status.TransactionID, err)
			}
		}
		if len(pending) < 256 {
			return nil
		}
	}
}

func (m *remoteEntityManager) publishAppliedRemoteTransaction(ctx context.Context, status entity.RemoteCommitStatus) error {
	if status.State != entity.RemoteCommitApplied || status.TransactionID.IsZero() || len(status.Commits) == 0 || len(status.Commits) != len(status.Receipts) {
		return fmt.Errorf("remote_entity: corrupt commit outbox transaction %s", status.TransactionID)
	}
	if m.backend == nil {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	for i := range status.Commits {
		commit := status.Commits[i].Clone()
		if commit.TransactionID != status.TransactionID {
			return fmt.Errorf("remote_entity: outbox transaction mismatch %s", status.TransactionID)
		}
		if err := m.afterRemoteCommit(ctx, commit, status.Receipts[i]); err != nil {
			return err
		}
	}
	if err := m.backend.MarkRemoteCommitPublished(ctx, status.TransactionID); err != nil {
		return err
	}
	m.completeRemoteTransaction(status.TransactionID, entity.RemoteCommitStatus{
		TransactionID: status.TransactionID,
		State:         entity.RemoteCommitCommitted,
		Receipts:      append([]entity.RemoteCommitReceipt(nil), status.Receipts...),
	})
	return nil
}

func (m *remoteEntityManager) ReadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (snapshot entity.RemoteSnapshotEnvelope, found bool, err error) {
	started := time.Now()
	defer func() {
		result := "hit"
		if err != nil {
			result = "error"
		} else if !found {
			result = "miss"
		}
		labels := metrics.Labels{"result": result, "consistency": remoteConsistencyLabel(consistency)}
		metrics.IncCounter("remote_entity.remote.read_total", labels, 1)
		metrics.ObserveDuration("remote_entity.remote.read_latency", labels, time.Since(started))
	}()
	if m == nil || m.remote == nil || m.remote.cache == nil || !key.Valid() {
		return entity.RemoteSnapshotEnvelope{}, false, entity.ErrRemoteRejected
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Reads automatically renew soft-state interest for this exact scope/policy.
	// A lost renewal only causes a later authoritative refill, never stale apply.
	_ = m.RenewRemoteSnapshotInterest(ctx, key)
	if consistency == entity.RemoteReadLinearizable {
		return m.remote.cache.LoadAuthoritative(ctx, key, consistency, minVersion)
	}
	snapshot, found, err = m.remote.cache.Get(ctx, key, consistency, minVersion)
	if err == nil && found {
		return snapshot, true, nil
	}
	if consistency == entity.RemoteReadCached && !errors.Is(err, entity.ErrRemoteSnapshotStale) {
		return snapshot, found, err
	}
	if consistency == entity.RemoteReadMonotonic && minVersion > 0 {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Millisecond)
		waitErr := m.remote.cache.WaitForVersion(waitCtx, key, minVersion)
		cancel()
		if waitErr == nil {
			return m.remote.cache.Get(ctx, key, consistency, minVersion)
		}
	}
	return m.remote.cache.LoadAuthoritative(ctx, key, consistency, minVersion)
}

func remoteConsistencyLabel(consistency entity.RemoteReadConsistency) string {
	switch consistency {
	case entity.RemoteReadCached:
		return "cached"
	case entity.RemoteReadLinearizable:
		return "linearizable"
	default:
		return "monotonic"
	}
}

var _ entity.RemoteSnapshotInterestManager = (*remoteEntityManager)(nil)

type remoteInterestPublisher interface {
	PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error
}

func (m *remoteEntityManager) RenewRemoteSnapshotInterest(ctx context.Context, key entity.RemoteSnapshotKey) error {
	if m == nil || m.remote == nil || !key.Valid() {
		return entity.ErrRemoteRejected
	}
	ttl := m.cfg.SnapshotInterestTTL
	now := time.Now().UnixNano()
	interest := entity.RemoteSnapshotInterest{ConsumerSID: m.localSid, Key: key, ExpiresAt: now + ttl.Nanoseconds()}
	stripe := &m.remote.localInterestLocks[uint64(key.EntityID)%uint64(len(m.remote.localInterestLocks))]
	stripe.Lock()
	defer stripe.Unlock()
	m.remote.localInterestMu.Lock()
	current, loaded := m.remote.localInterests[key]
	if loaded && current-now > (ttl/2).Nanoseconds() {
		m.remote.localInterestMu.Unlock()
		return nil
	}
	if !loaded && m.remote.localInterestCapacity > 0 && len(m.remote.localInterests) >= m.remote.localInterestCapacity {
		m.pruneLocalInterestsLocked(now)
		if len(m.remote.localInterests) >= m.remote.localInterestCapacity {
			m.remote.localInterestMu.Unlock()
			return entity.ErrRemoteOverloaded
		}
	}
	m.remote.localInterests[key] = interest.ExpiresAt
	m.remote.localInterestMu.Unlock()
	if err := m.remote.interests.renew(interest); err != nil {
		m.rollbackLocalInterest(key, interest.ExpiresAt)
		return err
	}
	if publisher, ok := m.syncer.(remoteInterestPublisher); ok {
		if err := publisher.PublishRemoteInterest(ctx, interest, false); err != nil {
			m.rollbackLocalInterest(key, interest.ExpiresAt)
			return err
		}
	}
	if m.remote.localInterestOps.Add(1)&1023 == 0 {
		m.remote.localInterestMu.Lock()
		m.pruneLocalInterestsLocked(now)
		m.remote.localInterestMu.Unlock()
	}
	return nil
}

func (m *remoteEntityManager) ReleaseRemoteSnapshotInterest(ctx context.Context, key entity.RemoteSnapshotKey) error {
	if m == nil || m.remote == nil || !key.Valid() {
		return entity.ErrRemoteRejected
	}
	interest := entity.RemoteSnapshotInterest{ConsumerSID: m.localSid, Key: key, ExpiresAt: time.Now().UnixNano()}
	stripe := &m.remote.localInterestLocks[uint64(key.EntityID)%uint64(len(m.remote.localInterestLocks))]
	stripe.Lock()
	defer stripe.Unlock()
	m.remote.localInterestMu.Lock()
	delete(m.remote.localInterests, key)
	m.remote.localInterestMu.Unlock()
	m.remote.interests.release(key, m.localSid)
	if publisher, ok := m.syncer.(remoteInterestPublisher); ok {
		return publisher.PublishRemoteInterest(ctx, interest, true)
	}
	return nil
}

func (m *remoteEntityManager) afterRemoteCommit(ctx context.Context, commit entity.RemoteCommit, receipt entity.RemoteCommitReceipt) error {
	if receipt.TransactionID != commit.TransactionID || receipt.EntityID != commit.EntityID || receipt.StateVersion != commit.NextVersion || receipt.MarkerEpoch != commit.MarkerEpoch || receipt.LockFence != commit.LockFence || receipt.RouteEpoch != commit.RouteEpoch {
		return fmt.Errorf("remote_entity: invalid commit receipt for %d", commit.EntityID)
	}
	if wrapper, ok := m.get(commit.EntityID); ok {
		if concrete := wrapper; concrete != nil {
			live := concrete.attachedEntity()
			if live != nil {
				if remote, ok := live.(entity.IThreadSafeRemoteEntity); ok {
					if err := remote.SetRemoteVersionVector(entity.RemoteVersionVector{StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, LockFence: commit.LockFence, RouteEpoch: commit.RouteEpoch}); err != nil {
						return err
					}
					if remote.RemoteOwnershipState() == entity.RemoteOwnershipQuarantined {
						if err := remote.TransitionRemoteOwnership(entity.RemoteOwnershipRecovering); err != nil {
							return err
						}
						target := entity.RemoteOwnershipLocalOwned
						if concrete.isMarked() {
							target = entity.RemoteOwnershipShared
						}
						if err := remote.TransitionRemoteOwnership(target); err != nil {
							return err
						}
					}
				} else {
					live.SetEntityVersion(int64(commit.NextVersion))
				}
				if participant, ok := live.(entity.IRemoteCommitParticipant); ok {
					if err := participant.AcknowledgeRemoteCommit(commit.Clone()); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, record := range commit.Snapshots {
		envelope := entity.RemoteSnapshotEnvelope{
			Key: record.Key, BaseVersion: record.BaseVersion, StateVersion: record.StateVersion,
			MarkerEpoch: record.MarkerEpoch, RouteEpoch: record.RouteEpoch,
			Schema: record.Schema, Codec: record.Codec, Checksum: record.Checksum, Full: record.Full,
			PublishedAt: time.Now().UnixNano(), Payload: entity.CopyFrozenRemoteSnapshotPayload(record.Data),
		}
		if err := m.remote.cache.Publish(ctx, envelope); err != nil {
			return err
		}
		if publisher, ok := m.snapshotPublisher(); ok {
			if err := publisher.PublishRemoteSnapshot(ctx, record.Clone()); err != nil {
				return err
			}
		}
	}
	for _, key := range commit.Invalidations {
		if err := m.remote.cache.Delete(ctx, key); err != nil {
			return err
		}
		if publisher, ok := m.snapshotPublisher(); ok {
			if err := publisher.DeleteRemoteSnapshot(ctx, key, commit.NextVersion); err != nil {
				return err
			}
		}
	}
	m.notifyRemoteVersion(commit.EntityID, commit.NextVersion)
	return nil
}

func (m *remoteEntityManager) snapshotPublisher() (entity.IRemoteSnapshotPublisher, bool) {
	if publisher, ok := m.syncer.(entity.IRemoteSnapshotPublisher); ok {
		return publisher, true
	}
	return nil, false
}

func (m *remoteEntityManager) trackRemoteTransaction(id entity.RemoteTransactionID) error {
	if m == nil || m.remote == nil || id.IsZero() {
		return entity.ErrRemoteRejected
	}
	m.remote.txMu.Lock()
	defer m.remote.txMu.Unlock()
	if m.remote.txs[id] == nil {
		if m.remote.txCapacity > 0 && len(m.remote.txs) >= m.remote.txCapacity {
			m.pruneRemoteTransactionsLocked(time.Now().UnixNano())
			if len(m.remote.txs) >= m.remote.txCapacity {
				m.evictOldestClosedRemoteTransactionLocked()
			}
			if len(m.remote.txs) >= m.remote.txCapacity {
				return entity.ErrRemoteOverloaded
			}
		}
		m.remote.txs[id] = &remoteTransactionTracker{done: make(chan struct{}), status: entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitAdmitted}}
	}
	return nil
}

func (m *remoteEntityManager) evictOldestClosedRemoteTransactionLocked() {
	var oldestID entity.RemoteTransactionID
	oldestAt := int64(^uint64(0) >> 1)
	found := false
	for id, tracker := range m.remote.txs {
		if tracker.closed && tracker.closedAt > 0 && tracker.closedAt < oldestAt {
			oldestID, oldestAt, found = id, tracker.closedAt, true
		}
	}
	if found {
		delete(m.remote.txs, oldestID)
	}
}

func (m *remoteEntityManager) pruneRemoteTransactionsLocked(now int64) {
	if m.remote.txTTL <= 0 {
		return
	}
	cutoff := now - m.remote.txTTL.Nanoseconds()
	for id, tracker := range m.remote.txs {
		if tracker.closed && tracker.closedAt > 0 && tracker.closedAt <= cutoff {
			delete(m.remote.txs, id)
		}
	}
}

func (m *remoteEntityManager) completeRemoteTransaction(id entity.RemoteTransactionID, status entity.RemoteCommitStatus) {
	if m == nil || m.remote == nil || id.IsZero() {
		return
	}
	m.remote.txMu.Lock()
	tracker := m.remote.txs[id]
	if tracker == nil {
		if m.remote.txCapacity > 0 && len(m.remote.txs) >= m.remote.txCapacity {
			m.pruneRemoteTransactionsLocked(time.Now().UnixNano())
			if len(m.remote.txs) >= m.remote.txCapacity {
				m.evictOldestClosedRemoteTransactionLocked()
			}
			if len(m.remote.txs) >= m.remote.txCapacity {
				m.remote.txMu.Unlock()
				metrics.IncCounter("remote_entity_transaction_tracker_drop_total", nil, 1)
				return
			}
		}
		tracker = &remoteTransactionTracker{done: make(chan struct{})}
		m.remote.txs[id] = tracker
	}
	tracker.status = status.Clone()
	terminal := status.State == entity.RemoteCommitCommitted || status.State == entity.RemoteCommitRejected || status.State == entity.RemoteCommitIndeterminate
	if terminal && !tracker.closed {
		tracker.closed = true
		tracker.closedAt = time.Now().UnixNano()
		close(tracker.done)
	}
	m.remote.txMu.Unlock()
}

func (m *remoteEntityManager) rollbackLocalInterest(key entity.RemoteSnapshotKey, expiresAt int64) {
	if m == nil || m.remote == nil {
		return
	}
	m.remote.localInterestMu.Lock()
	if m.remote.localInterests[key] == expiresAt {
		delete(m.remote.localInterests, key)
		m.remote.interests.release(key, m.localSid)
	}
	m.remote.localInterestMu.Unlock()
}

func (m *remoteEntityManager) pruneLocalInterestsLocked(now int64) {
	for key, expiresAt := range m.remote.localInterests {
		if expiresAt <= now {
			delete(m.remote.localInterests, key)
			m.remote.interests.release(key, m.localSid)
		}
	}
}

func (m *remoteEntityManager) waitRemoteTransaction(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.trackRemoteTransaction(id); err != nil {
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown, Cause: err.Error()}, err
	}
	m.remote.txMu.Lock()
	tracker := m.remote.txs[id]
	done := tracker.done
	status := tracker.status.Clone()
	closed := tracker.closed
	m.remote.txMu.Unlock()
	if closed {
		return status, remoteStatusError(status)
	}
	select {
	case <-done:
		m.remote.txMu.Lock()
		status = m.remote.txs[id].status.Clone()
		m.remote.txMu.Unlock()
		return status, remoteStatusError(status)
	case <-ctx.Done():
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown, Cause: ctx.Err().Error()}, errors.Join(entity.ErrRemoteCommitTimeout, ctx.Err())
	}
}

func remoteStatusError(status entity.RemoteCommitStatus) error {
	switch status.State {
	case entity.RemoteCommitPublished, entity.RemoteCommitCommitted:
		return nil
	case entity.RemoteCommitRejected:
		return fmt.Errorf("%w: %s", entity.ErrRemoteRejected, status.Cause)
	case entity.RemoteCommitIndeterminate:
		return fmt.Errorf("%w: %s", entity.ErrRemotePersistenceIndeterminate, status.Cause)
	default:
		return entity.ErrRemoteCommitNotFinalized
	}
}

func (m *remoteEntityManager) RemoteCommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	if m == nil || m.remote == nil || id.IsZero() {
		return entity.RemoteCommitStatus{}, entity.ErrRemoteRejected
	}
	m.remote.txMu.Lock()
	tracker := m.remote.txs[id]
	if tracker != nil {
		status := tracker.status.Clone()
		if status.State != entity.RemoteCommitIndeterminate {
			m.remote.txMu.Unlock()
			return status, nil
		}
	}
	m.remote.txMu.Unlock()
	if m.backend != nil {
		status, err := m.backend.CommitStatus(ctx, id)
		if err == nil && (status.State == entity.RemoteCommitCommitted || status.State == entity.RemoteCommitRejected) {
			m.completeRemoteTransaction(id, status)
		}
		return status.Clone(), err
	}
	return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, nil
}

func (m *remoteEntityManager) FlushRemoteTransaction(ctx context.Context, id entity.RemoteTransactionID) error {
	_, err := m.waitRemoteTransaction(ctx, id)
	return err
}

func (m *remoteEntityManager) FlushRemoteEntity(ctx context.Context, id int64, minVersion uint64) error {
	if minVersion == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.remote.versionMu.Lock()
	if m.remote.versions[id] >= minVersion {
		m.remote.versionMu.Unlock()
		return nil
	}
	waiter := remoteVersionWaiter{version: minVersion, done: make(chan struct{})}
	if m.remote.maxWaiters > 0 && m.remote.waiterCount >= m.remote.maxWaiters {
		m.remote.versionMu.Unlock()
		return entity.ErrRemoteOverloaded
	}
	m.remote.waiters[id] = append(m.remote.waiters[id], waiter)
	m.remote.waiterCount++
	m.remote.versionMu.Unlock()
	select {
	case <-waiter.done:
		return nil
	case <-ctx.Done():
		m.removeRemoteVersionWaiter(id, waiter.done)
		return ctx.Err()
	}
}

func (m *remoteEntityManager) FlushRemoteAll(ctx context.Context) error {
	m.remote.txMu.Lock()
	ids := make([]entity.RemoteTransactionID, 0, len(m.remote.txs))
	for id, tracker := range m.remote.txs {
		if !tracker.closed {
			ids = append(ids, id)
		}
	}
	m.remote.txMu.Unlock()
	for _, id := range ids {
		if err := m.FlushRemoteTransaction(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (m *remoteEntityManager) notifyRemoteVersion(id int64, version uint64) {
	m.remote.versionMu.Lock()
	if _, exists := m.remote.versions[id]; !exists {
		m.remote.versionOrder = append(m.remote.versionOrder, id)
	}
	if version > m.remote.versions[id] {
		m.remote.versions[id] = version
	}
	waiters := m.remote.waiters[id]
	remaining := waiters[:0]
	for _, waiter := range waiters {
		if version >= waiter.version {
			close(waiter.done)
			m.remote.waiterCount--
		} else {
			remaining = append(remaining, waiter)
		}
	}
	if len(remaining) == 0 {
		delete(m.remote.waiters, id)
	} else {
		m.remote.waiters[id] = remaining
	}
	for m.remote.maxVersions > 0 && len(m.remote.versions) > m.remote.maxVersions && len(m.remote.versionOrder) > 0 {
		oldest := m.remote.versionOrder[0]
		m.remote.versionOrder = m.remote.versionOrder[1:]
		if len(m.remote.waiters[oldest]) == 0 {
			delete(m.remote.versions, oldest)
		} else {
			m.remote.versionOrder = append(m.remote.versionOrder, oldest)
			break
		}
	}
	m.remote.versionMu.Unlock()
}

func (m *remoteEntityManager) removeRemoteVersionWaiter(id int64, done chan struct{}) {
	m.remote.versionMu.Lock()
	waiters := m.remote.waiters[id]
	for i := range waiters {
		if waiters[i].done == done {
			waiters = append(waiters[:i], waiters[i+1:]...)
			m.remote.waiterCount--
			break
		}
	}
	if len(waiters) == 0 {
		delete(m.remote.waiters, id)
	} else {
		m.remote.waiters[id] = waiters
	}
	m.remote.versionMu.Unlock()
}

func (m *remoteEntityManager) quarantineEntries(entries []*remoteWriteEntry, cause error) error {
	var joined error
	for _, entry := range entries {
		if entry == nil || entry.entity == nil {
			continue
		}
		if remote, ok := entry.entity.(entity.IThreadSafeRemoteEntity); ok {
			transitions := []entity.RemoteOwnershipState{entity.RemoteOwnershipFenced, entity.RemoteOwnershipRecovering, entity.RemoteOwnershipQuarantined}
			switch remote.RemoteOwnershipState() {
			case entity.RemoteOwnershipUnknown:
				transitions = transitions[1:]
			case entity.RemoteOwnershipFenced:
				transitions = transitions[1:]
			case entity.RemoteOwnershipRecovering:
				transitions = transitions[2:]
			case entity.RemoteOwnershipQuarantined:
				continue
			}
			for _, target := range transitions {
				if err := remote.TransitionRemoteOwnership(target); err != nil {
					joined = errors.Join(joined, fmt.Errorf("remote_entity: quarantine entity %d at %s: %w", entry.entity.ID(), target, err))
					break
				}
			}
		}
	}
	if joined != nil {
		return errors.Join(cause, joined)
	}
	return nil
}

func (m *remoteEntityManager) rollbackRemoteEntries(entries []*remoteWriteEntry) {
	for _, entry := range entries {
		if entry == nil || entry.entity == nil || !entry.finalized || entry.entity.GetMutex() == nil {
			continue
		}
		entry.entity.GetMutex().Lock()
		if participant, ok := entry.entity.(entity.IRemoteCommitParticipant); ok {
			participant.RollbackRemoteCommit(entry.commit.Clone())
		}
		entry.entity.GetMutex().Unlock()
	}
}

var _ entity.RemoteWriteBatchManager = (*remoteEntityManager)(nil)
var _ entity.RemoteCommitApplier = (*remoteEntityManager)(nil)
var _ entity.RemoteSnapshotReader = (*remoteEntityManager)(nil)

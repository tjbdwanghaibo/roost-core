package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

type remoteTestLoader struct {
	*mockLoader
	mu       sync.Mutex
	commits  map[entity.RemoteTransactionID][]entity.RemoteCommitReceipt
	versions map[int64]uint64
}

type flakySnapshotSyncer struct {
	attempts atomic.Int32
}

type appliedOutboxLoader struct {
	*remoteTestLoader
	mu     sync.Mutex
	status entity.RemoteCommitStatus
	marked bool
}

type transactionLocalRemoteEntity struct {
	*testRemoteEntity
}

func (*transactionLocalRemoteEntity) HasRemoteCommitLocked(entity.RemoteTransactionOutcome) bool {
	return true
}

func (l *appliedOutboxLoader) CommitStatus(context.Context, entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status.Clone(), nil
}

func (l *appliedOutboxLoader) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.marked {
		return nil, nil
	}
	return []entity.RemoteCommitStatus{l.status.Clone()}, nil
}

func (l *appliedOutboxLoader) MarkRemoteCommitPublished(_ context.Context, id entity.RemoteTransactionID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id != l.status.TransactionID {
		return entity.ErrRemoteRejected
	}
	l.marked = true
	l.status.State = entity.RemoteCommitCommitted
	return nil
}

func (s *flakySnapshotSyncer) PublishRemoteSnapshot(context.Context, entity.RemoteSnapshotRecord) error {
	if s.attempts.Add(1) == 1 {
		return errors.New("injected snapshot publish failure")
	}
	return nil
}

func (*flakySnapshotSyncer) DeleteRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, uint64) error {
	return nil
}

func (*flakySnapshotSyncer) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	return nil
}

func newRemoteTestLoader() *remoteTestLoader {
	return &remoteTestLoader{mockLoader: newMockLoader(), commits: make(map[entity.RemoteTransactionID][]entity.RemoteCommitReceipt), versions: make(map[int64]uint64)}
}

func (e *testRemoteEntity) BuildRemoteCommitLocked(lease entity.RemoteWriteLease, outcome entity.RemoteTransactionOutcome) (entity.RemoteCommit, error) {
	if e.buildErr != nil {
		return entity.RemoteCommit{}, e.buildErr
	}
	if outcome.DeleteIntents != nil && outcome.DeleteIntents.RemoteDeleteRequested(e.GUId()) {
		return entity.RemoteCommit{
			Delete: true, Schema: 1, Codec: 1,
			Deletes:       []entity.RemoteDataDelete{{Collection: "remote", ID: e.GUId()}},
			Invalidations: []entity.RemoteSnapshotKey{{EntityID: e.GUId(), Kind: e.GetEntityKind(), Scope: 1}},
		}, nil
	}
	e.dirty.dirty = false
	return entity.RemoteCommit{
		Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: "remote", ID: e.GUId(), Version: lease.BaseVersion + 1, Mask: 1, Data: []byte("state")}},
		Snapshots: []entity.RemoteSnapshotRecord{{
			Key:    entity.RemoteSnapshotKey{EntityID: lease.EntityID, Kind: e.GetEntityKind(), Scope: 1},
			Schema: 1, Codec: 1, Full: true, Data: []byte("snapshot"),
		}},
	}, nil
}

type batchDeleteIntent int64

func (intent batchDeleteIntent) RemoteDeleteRequested(id int64) bool { return int64(intent) == id }

func TestRemoteWriteBatchUsesExplicitDeleteIntentBeforeEntityRemoval(t *testing.T) {
	const kind entity.EntityKind = 119
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	live := newTestRemoteEntity(1399, 1, kind)
	loader.add(live)

	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	outcome := entity.NewRemoteTransactionOutcome(remoteTestTxID(19), "delete", "", true, 2)
	outcome.DeleteIntents = batchDeleteIntent(live.GUId())
	if err := batch.FinalizeLocked(outcome); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	if len(commits) != 1 || !commits[0].Delete || len(commits[0].Deletes) != 1 || live.IsRemoved() {
		t.Fatalf("commits=%+v removed=%t", commits, live.IsRemoved())
	}
	if err := batch.Abort(context.Background(), errors.New("test complete")); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (l *remoteTestLoader) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	receipts := make([]entity.RemoteCommitReceipt, 0, len(commits))
	for _, commit := range commits {
		receipt, err := l.CommitRemote(ctx, commit)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func (e *testRemoteEntity) AcknowledgeRemoteCommit(_ entity.RemoteCommit) error {
	e.dirty.dirty = false
	return nil
}

func (e *testRemoteEntity) RollbackRemoteCommit(_ entity.RemoteCommit) {
	e.dirty.dirty = true
}

func (l *remoteTestLoader) CommitRemote(_ context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing := l.commits[commit.TransactionID]; len(existing) > 0 {
		return existing[0], nil
	}
	if l.versions[commit.EntityID] != commit.BaseVersion {
		return entity.RemoteCommitReceipt{}, entity.ErrRemoteVersionConflict
	}
	l.versions[commit.EntityID] = commit.NextVersion
	receipt := entity.RemoteCommitReceipt{
		TransactionID: commit.TransactionID, EntityID: commit.EntityID,
		StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch,
		LockFence: commit.LockFence, RouteEpoch: commit.RouteEpoch, CommittedAt: time.Now().UnixNano(),
	}
	l.commits[commit.TransactionID] = []entity.RemoteCommitReceipt{receipt}
	return receipt, nil
}

func (l *remoteTestLoader) CommitStatus(_ context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	receipts := append([]entity.RemoteCommitReceipt(nil), l.commits[id]...)
	state := entity.RemoteCommitUnknown
	if len(receipts) > 0 {
		state = entity.RemoteCommitCommitted
	}
	return entity.RemoteCommitStatus{TransactionID: id, State: state, Receipts: receipts}, nil
}

func (l *remoteTestLoader) LoadRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	return entity.RemoteSnapshotEnvelope{}, false, nil
}

func (l *remoteTestLoader) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	return nil, nil
}

func (l *remoteTestLoader) MarkRemoteCommitPublished(context.Context, entity.RemoteTransactionID) error {
	return nil
}

func remoteTestTxID(value byte) entity.RemoteTransactionID {
	var id entity.RemoteTransactionID
	id[15] = value
	return id
}

func TestRemoteWriteBatchUsesTransactionLocalChangeParticipantWithoutDirtyState(t *testing.T) {
	const kind entity.EntityKind = 120
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	live := &transactionLocalRemoteEntity{testRemoteEntity: newTestRemoteEntity(1400, 1, kind)}
	loader.add(live)

	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close(context.Background()) })
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(10), "test", "request", true, 0)); err != nil {
		t.Fatal(err)
	}
	if commits := batch.Commits(); len(commits) != 1 {
		t.Fatalf("commits=%d, want transaction-local commit despite clean sync tracker", len(commits))
	}
}

func TestRemoteWriteBatchMemoryCommitPublishesImmutableSnapshot(t *testing.T) {
	const kind entity.EntityKind = 121
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := DefaultConfig()
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())

	live := newTestRemoteEntity(1401, 1, kind)
	live.SetEntityVersion(0)
	live.dirty.dirty = true
	loader.add(live)

	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	outcome := entity.NewRemoteTransactionOutcome(remoteTestTxID(1), "test", "request", true, 0)
	if err := batch.FinalizeLocked(outcome); err != nil {
		t.Fatal(err)
	}
	receipts, err := batch.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].StateVersion != 1 || live.EntityVersion() != 1 || live.dirty.dirty {
		t.Fatalf("receipts=%+v version=%d dirty=%v", receipts, live.EntityVersion(), live.dirty.dirty)
	}
	snapshot, ok, err := mgr.ReadRemoteSnapshot(context.Background(), entity.RemoteSnapshotKey{EntityID: live.GUId(), Kind: live.GetEntityKind(), Scope: 1}, entity.RemoteReadMonotonic, 1)
	if err != nil || !ok || string(snapshot.Payload.BytesCopy()) != "snapshot" {
		t.Fatalf("snapshot=%+v ok=%v err=%v", snapshot, ok, err)
	}
}

func TestRemoteWriteBatchAsyncRetainsGateUntilWALApply(t *testing.T) {
	const kind entity.EntityKind = 122
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := DefaultConfig()
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())

	live := newTestRemoteEntity(1402, 1, kind)
	live.dirty.dirty = true
	loader.add(live)

	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	txID := remoteTestTxID(2)
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(txID, "test", "request", true, 1)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	if _, err := batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	prepared := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
		prepared <- err
	}()
	if err := <-prepared; err == nil {
		t.Fatal("second write acquired gate before async apply")
	}
	if _, err := mgr.ApplyRemoteCommits(context.Background(), txID, commits); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.FlushRemoteTransaction(ctx, txID); err != nil {
		t.Fatal(err)
	}
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		next, probeErr := mgr.PrepareRemoteWriteBatch(probeCtx, []int64{live.GUId()})
		probeCancel()
		if probeErr == nil {
			_ = next.Abort(context.Background(), errors.New("probe"))
			_ = next.Close(context.Background())
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("async gate was not released")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestTransferRemoteOwnershipFencesPreviousOwner(t *testing.T) {
	const kind entity.EntityKind = 123
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	markers := newMockMarkerStore()
	mgr.SetOwnershipStore(markers)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	live := newTestRemoteEntity(1403, 1, kind)
	if err := live.SetRemoteVersionVector(entity.RemoteVersionVector{MarkerEpoch: 2, RouteEpoch: 3}); err != nil {
		t.Fatal(err)
	}
	loader.add(live)
	markers.leases[live.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 2, RouteEpoch: 3}

	next, err := mgr.TransferRemoteOwnership(context.Background(), live.GUId(), 2000)
	if err != nil {
		t.Fatal(err)
	}
	if next.OwnerSid != 2000 || next.MarkerEpoch != 3 || next.RouteEpoch != 4 {
		t.Fatalf("next=%+v", next)
	}
	if live.RemoteOwnershipState() != entity.RemoteOwnershipFenced || live.RemoteVersionVector().RouteEpoch != 4 {
		t.Fatalf("state=%s vector=%+v", live.RemoteOwnershipState(), live.RemoteVersionVector())
	}
	if _, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()}); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("previous owner admitted write: %v", err)
	}
}

func TestRemoteWriteBatchRollsBackEarlierFrozenEntityOnBuildFailure(t *testing.T) {
	const kind entity.EntityKind = 124
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	first := newTestRemoteEntity(1404, 1, kind)
	second := newTestRemoteEntity(1405, 1, kind)
	first.dirty.dirty = true
	second.dirty.dirty = true
	second.buildErr = errors.New("freeze failed")
	loader.add(first, second)
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{second.GUId(), first.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(3), "test", "request", true, 0)); err == nil {
		t.Fatal("expected freeze failure")
	}
	if !first.dirty.dirty {
		t.Fatal("first entity dirty state was not restored")
	}
	_ = batch.Abort(context.Background(), errors.New("expected"))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerRoutedWriteUsesMarkerHotCache(t *testing.T) {
	const kind entity.EntityKind = 126
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := DefaultConfig()
	cfg.MarkerCacheTTL = time.Minute
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	markers := newMockMarkerStore()
	mgr.SetOwnershipStore(markers)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	live := newTestRemoteEntity(1406, 1, kind)
	loader.add(live)

	for i := 0; i < 2; i++ {
		batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
		if err != nil {
			t.Fatal(err)
		}
		_ = batch.Abort(context.Background(), errors.New("probe"))
		if err := batch.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	markers.mu.Lock()
	gets := markers.gets
	markers.mu.Unlock()
	if gets != 1 {
		t.Fatalf("owner hot path performed %d marker reads, want one initial fill", gets)
	}
}

func TestStrictCommitTimeoutRetainsGateUntilOutcomeIsKnown(t *testing.T) {
	const kind entity.EntityKind = 130
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	live := newTestRemoteEntity(1407, 1, kind)
	live.dirty.dirty = true
	loader.add(live)
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	txID := remoteTestTxID(4)
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(txID, "test", "request", true, 2)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, err = batch.Commit(waitCtx)
	waitCancel()
	if !errors.Is(err, entity.ErrRemoteCommitTimeout) {
		t.Fatalf("strict timeout error=%v", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, probeErr := mgr.PrepareRemoteWriteBatch(probeCtx, []int64{live.GUId()})
	probeCancel()
	if probeErr == nil {
		t.Fatal("strict timeout released write gate before status was known")
	}
	if _, err := mgr.ApplyRemoteCommits(context.Background(), txID, commits); err != nil {
		t.Fatal(err)
	}
	flushCtx, flushCancel := context.WithTimeout(context.Background(), time.Second)
	defer flushCancel()
	if err := mgr.FlushRemoteTransaction(flushCtx, txID); err != nil {
		t.Fatal(err)
	}
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		next, nextErr := mgr.PrepareRemoteWriteBatch(probeCtx, []int64{live.GUId()})
		probeCancel()
		if nextErr == nil {
			_ = next.Abort(context.Background(), errors.New("probe"))
			_ = next.Close(context.Background())
			break
		}
		select {
		case <-flushCtx.Done():
			t.Fatalf("recovered strict transaction did not release gate: %v", nextErr)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestMemoryCommitPublishFailureQueriesStatusAndReconciles(t *testing.T) {
	const kind entity.EntityKind = 131
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = time.Millisecond
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	syncer := &flakySnapshotSyncer{}
	mgr.setSyncer(syncer)
	live := newTestRemoteEntity(1408, 1, kind)
	live.dirty.dirty = true
	loader.add(live)

	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	txID := remoteTestTxID(5)
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(txID, "test", "request", true, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit error = %v, want indeterminate", err)
	}
	if live.dirty.dirty {
		t.Fatal("committed dirty generation was incorrectly rolled back")
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		next, nextErr := mgr.PrepareRemoteWriteBatch(probeCtx, []int64{live.GUId()})
		cancel()
		if nextErr == nil {
			_ = next.Abort(context.Background(), errors.New("probe"))
			_ = next.Close(context.Background())
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconciliation did not restore the write path: %v", nextErr)
		}
		time.Sleep(time.Millisecond)
	}
	if got := syncer.attempts.Load(); got < 2 {
		t.Fatalf("snapshot publish attempts = %d, want retry", got)
	}
	status, err := mgr.RemoteCommitStatus(context.Background(), txID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != entity.RemoteCommitCommitted {
		t.Fatalf("transaction state = %v, want committed", status.State)
	}
}

func TestRemoteTrackingAndFlushWaitersAreBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 1
	cfg.TransactionTrackTTL = time.Hour
	cfg.SnapshotMaxWaiters = 1
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.stopRemoteFinalizer(ctx)
	})
	first := remoteTestTxID(21)
	second := remoteTestTxID(22)
	if err := mgr.trackRemoteTransaction(first); err != nil {
		t.Fatal(err)
	}
	if err := mgr.trackRemoteTransaction(second); !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("transaction capacity error = %v", err)
	}
	mgr.completeRemoteTransaction(first, entity.RemoteCommitStatus{TransactionID: first, State: entity.RemoteCommitCommitted})
	if err := mgr.trackRemoteTransaction(second); err != nil {
		t.Fatalf("completed transaction cache did not yield capacity: %v", err)
	}

	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() { waitDone <- mgr.FlushRemoteEntity(waitCtx, 999, 2) }()
	deadline := time.Now().Add(time.Second)
	for {
		mgr.remote.versionMu.Lock()
		count := mgr.remote.waiterCount
		mgr.remote.versionMu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first version waiter was not registered")
		}
		time.Sleep(time.Millisecond)
	}
	if err := mgr.FlushRemoteEntity(context.Background(), 1000, 2); !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("version waiter capacity error = %v", err)
	}
	cancelWait()
	if err := <-waitDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter error = %v", err)
	}
}

func TestRemoteFinalizerStopCancelsLongRetryImmediately(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = time.Hour
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	mgr.SetBackend(newRemoteTestLoader())
	mgr.startRemoteFinalizer()
	if !mgr.reserveRemoteFinalizeSlot() {
		t.Fatal("reserve finalizer slot")
	}
	txID := remoteTestTxID(99)
	if err := mgr.trackRemoteTransaction(txID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.deferRemoteClose(deferredRemoteClose{txID: txID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(mgr.remote.finalizeQueue) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := mgr.stopRemoteFinalizer(ctx); err != nil {
		t.Fatalf("stop should cancel retry immediately: %v", err)
	}
	if got := len(mgr.remote.finalizeSlots); got != 0 {
		t.Fatalf("reserved slots after stop=%d", got)
	}
}

func TestRemoteFinalizerReplaysAppliedOutboxWithoutRestart(t *testing.T) {
	const kind entity.EntityKind = 129
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id := testRemoteFullIDWithKind(1501, 1, kind)
	txID := remoteTestTxID(100)
	data := []byte("snapshot")
	commit := entity.RemoteCommit{
		TransactionID: txID, EntityID: id, Kind: kind,
		BaseVersion: 0, NextVersion: 1, MarkerEpoch: 1, RouteEpoch: 1,
		Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: "remote", ID: id, Version: 1, Mask: 1, Data: []byte("state")}},
		Snapshots: []entity.RemoteSnapshotRecord{{
			Key:         entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1},
			BaseVersion: 0, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1,
			Schema: 1, Codec: 1, Full: true, Data: data, Checksum: entity.RemoteSnapshotChecksum(data),
		}},
	}
	receipt := entity.RemoteCommitReceipt{TransactionID: txID, EntityID: id, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1}
	loader := &appliedOutboxLoader{
		remoteTestLoader: newRemoteTestLoader(),
		status:           entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitApplied, Receipts: []entity.RemoteCommitReceipt{receipt}, Commits: []entity.RemoteCommit{commit}},
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = time.Millisecond
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	syncer := &flakySnapshotSyncer{}
	mgr.setSyncer(syncer)
	mgr.startRemoteFinalizer()
	if !mgr.reserveRemoteFinalizeSlot() {
		t.Fatal("reserve finalizer slot")
	}
	mgr.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitIndeterminate})
	if err := mgr.deferRemoteClose(deferredRemoteClose{txID: txID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		loader.mu.Lock()
		marked := loader.marked
		loader.mu.Unlock()
		if marked && len(mgr.remote.finalizeSlots) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	loader.mu.Lock()
	marked := loader.marked
	loader.mu.Unlock()
	if !marked || syncer.attempts.Load() < 2 || len(mgr.remote.finalizeSlots) != 0 {
		t.Fatalf("outbox replay incomplete: marked=%v attempts=%d slots=%d", marked, syncer.attempts.Load(), len(mgr.remote.finalizeSlots))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.stopRemoteFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
}

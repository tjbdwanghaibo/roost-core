package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func prepareReplayBatch(t *testing.T, backend entity.IRemoteEntityBackend) (*Manager, *testRemoteEntity, entity.RemoteWriteBatch) {
	t.Helper()
	const kind entity.EntityKind = 119
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	loader := newRemoteTestLoader()
	if backend == nil {
		backend = loader
	}
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(newMockMarkerStore())
	live := newTestRemoteEntity(1488, 1, kind)
	live.dirty.dirty = true
	switch b := backend.(type) {
	case *remoteTestLoader:
		b.add(live)
	case *lostCommitReplyBackend:
		b.add(live)
	}
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.dirty = true
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(88), "replay", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	return mgr, live, batch
}
func TestCommittedReplayPreservesNewerLiveFenceAndDirtyState(t *testing.T) {
	mgr, live, batch := prepareReplayBatch(t, nil)
	commits := batch.Commits()
	if _, err := batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	newer := live.RemoteVersionVector()
	newer.LockFence++
	newer.StateVersion++
	if err := live.SetRemoteVersionVector(newer); err != nil {
		t.Fatal(err)
	}
	live.dirty.dirty = true
	if _, err := mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits); err != nil {
		t.Fatal(err)
	}
	if live.RemoteVersionVector() != newer || !live.dirty.dirty {
		t.Fatal("old receipt rewound/acknowledged newer local work")
	}
	stale := newer
	stale.LockFence--
	if err := live.SetRemoteVersionVector(stale); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("stale writer accepted: %v", err)
	}
}

type lostCommitReplyBackend struct{ *remoteTestLoader }

func (b *lostCommitReplyBackend) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if _, err := b.remoteTestLoader.CommitRemoteBatch(ctx, commits); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}
func TestMemoryLostCommitReplyKeepsFinalizerOwnership(t *testing.T) {
	backend := &lostCommitReplyBackend{newRemoteTestLoader()}
	mgr, live, batch := prepareReplayBatch(t, backend)
	_, err := batch.Commit(context.Background())
	if !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unknown commit=%v", err)
	}
	if live.dirty.dirty {
		t.Fatal("unknown result rolled back committed mutation")
	}
	if err = batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = mgr.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mgr.remote.writeSlots) != 0 {
		t.Fatal("finalizer leaked write slot")
	}
}

func TestMongoRejectedTransactionIsDurableAndDigestChecked(t *testing.T) {
	mgr, _, batch := prepareReplayBatch(t, nil)
	_ = mgr
	defer batch.Close(context.Background())
	defer batch.Abort(context.Background(), errors.New("test cleanup"))
	commits := batch.Commits()
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	for range 2 {
		if err := store.RejectRemoteCommitsInTransaction(context.Background(), commits, "lease expired"); err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.CommitStatus(context.Background(), commits[0].TransactionID)
	if err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, err = store.CommitRemoteBatch(context.Background(), commits); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("rejected transaction committed: %v", err)
	}
	altered := commits[0].Clone()
	altered.Mutations[0].Data = []byte("changed")
	if err = store.RejectRemoteCommitsInTransaction(context.Background(), []entity.RemoteCommit{altered}, "lease expired"); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("digest collision=%v", err)
	}
}
func TestBackendForwardsConcurrentCommitCapability(t *testing.T) {
	for _, tc := range []struct {
		storage StorageBackend
		want    bool
	}{{plainStorageBackend{}, false}, {NewMongoCommitter(newRemoteMongoFake(), "control", 1, 0), true}} {
		backend, err := NewBackend(nopLoader{}, tc.storage)
		if err != nil {
			t.Fatal(err)
		}
		if got := backend.SupportsConcurrentRemoteCommits(); got != tc.want {
			t.Fatalf("capability=%v want=%v", got, tc.want)
		}
	}
}

func (b *lostCommitReplyBackend) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if _, err := b.remoteTestLoader.CommitRemote(ctx, commit); err != nil {
		return entity.RemoteCommitReceipt{}, err
	}
	return entity.RemoteCommitReceipt{}, context.DeadlineExceeded
}
func TestDurableLeaseRejectionReleasesDeferredWriteGate(t *testing.T) {
	mgr, live, batch := prepareReplayBatch(t, nil)
	concrete := batch.(*remoteWriteBatch)
	concrete.outcome.Durability = 1
	if _, err := batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.RejectRemoteTransaction(concrete.outcome.TransactionID, "lease expired")
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	next, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	_ = next.Abort(ctx, errors.New("test cleanup"))
	_ = next.Close(ctx)
	if err := mgr.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mgr.remote.writeSlots) != 0 || mgr.Stats().ActiveTransactions != 0 {
		t.Fatal("rejection leaked transaction/gate capacity")
	}
}
func TestAdmittedTransactionStatusRefreshesDurableRejection(t *testing.T) {
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	id := remoteTestTxID(77)
	backend := &appliedOutboxLoader{remoteTestLoader: newRemoteTestLoader(), status: entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitRejected, Cause: "lease expired"}}
	mgr.SetBackend(backend)
	if err := mgr.trackRemoteTransaction(id); err != nil {
		t.Fatal(err)
	}
	status, err := mgr.RemoteCommitStatus(context.Background(), id)
	if err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if mgr.Stats().ActiveTransactions != 0 {
		t.Fatal("durable rejection left tracker admitted")
	}
}

type flakyMarkBackend struct {
	*remoteTestLoader
	fail atomic.Bool
}

func (b *flakyMarkBackend) MarkRemoteCommitPublished(context.Context, entity.RemoteTransactionID) error {
	if b.fail.Load() {
		return errors.New("injected publish mark failure")
	}
	return nil
}

// RR-20260926-11 复核残留：已 Committed 的事务重放发布失败，不能把 tracker 改回
// Indeterminate，后到的等待方 / FlushRemoteAll 仍应得到已提交结论。
func TestCommittedTrackerSurvivesFailedReplayPublication(t *testing.T) {
	const kind entity.EntityKind = 119
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	backend := &flakyMarkBackend{remoteTestLoader: newRemoteTestLoader()}
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(newMockMarkerStore())
	live := newTestRemoteEntity(1499, 1, kind)
	backend.add(live)
	batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.dirty = true
	tx := remoteTestTxID(77)
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "replay", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	if _, err = batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := mgr.waitRemoteTransaction(context.Background(), tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("before replay status=%+v err=%v", status, err)
	}

	backend.fail.Store(true)
	if _, err = mgr.ApplyRemoteCommits(context.Background(), tx, commits); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("replay publication failure must stay retryable for the caller: %v", err)
	}
	status, err := mgr.waitRemoteTransaction(context.Background(), tx)
	if err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("committed tracker overwritten by failed replay: state=%v err=%v", status.State, err)
	}
	if err = mgr.FlushRemoteAll(context.Background()); err != nil {
		t.Fatalf("FlushRemoteAll after failed replay: %v", err)
	}
	if status, err = mgr.RemoteCommitStatus(context.Background(), tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("status after failed replay=%+v err=%v", status, err)
	}

	// 非终态仍可被持久结论升级：Indeterminate 之后的成功发布写回 Committed。
	backend.fail.Store(false)
	other := remoteTestTxID(78)
	mgr.completeRemoteTransaction(other, entity.RemoteCommitStatus{TransactionID: other, State: entity.RemoteCommitIndeterminate, Cause: "probe"})
	mgr.completeRemoteTransaction(other, entity.RemoteCommitStatus{TransactionID: other, State: entity.RemoteCommitCommitted})
	if status, err = mgr.waitRemoteTransaction(context.Background(), other); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("indeterminate tracker not upgraded: state=%v err=%v", status.State, err)
	}
}

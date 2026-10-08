package remoteentity

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// B14：原两个发布者在解冻中途相遇的路径已收敛。唯一 outbox 发布者停在
// Recovering 时，投影器重放只报告持久结论，不抢着解冻或重新发布；放行后资源完整收尾。
func TestOutboxMidThawDoesNotAdmitASecondPublisher(t *testing.T) {
	f, live := newMidThawFixture(t, 1894)
	tx := remoteTestTxID(194)
	commits := f.asyncWrite(t, tx)
	f.projectorReportsUnknown(t, tx, commits)
	failuresBefore := remoteApplyErrors()
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	live.holdLocalOwned.Store(true)
	release := sync.OnceFunc(func() { close(live.localOwnedResume) })
	t.Cleanup(release)
	f.answerPoll(t, true)
	awaitSignal(t, deadline(t), live.localOwnedReached, "the outbox's thaw to reach Recovering")
	if got := live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipRecovering {
		t.Fatalf("state=%v", got)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	if got := f.syncer.published.Load(); got != 0 {
		t.Fatalf("published during paused thaw: %d", got)
	}
	release()
	f.finishPublication(t)
	if got := remoteApplyErrors(); got != failuresBefore {
		t.Fatalf("new apply errors: %d -> %d", failuresBefore, got)
	}
	want := entity.RemoteVersionVector{StateVersion: commits[0].NextVersion, MarkerEpoch: commits[0].MarkerEpoch, LockFence: commits[0].LockFence, RouteEpoch: commits[0].RouteEpoch}
	if got := live.RemoteVersionVector(); got != want {
		t.Fatalf("version=%+v want=%+v", got, want)
	}
	if status, err := f.mgr.RemoteCommitStatus(context.Background(), tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	select {
	case step := <-f.steps:
		t.Fatalf("continued after release: %s", step)
	default:
	}
}

type midThawRemoteEntity struct {
	*pausingRemoteEntity
	holdLocalOwned    atomic.Bool
	localOwnedReached chan struct{}
	localOwnedResume  chan struct{}
}

func (e *midThawRemoteEntity) TransitionRemoteOwnership(to entity.RemoteOwnershipState) error {
	if to == entity.RemoteOwnershipLocalOwned && e.holdLocalOwned.CompareAndSwap(true, false) {
		close(e.localOwnedReached)
		<-e.localOwnedResume
	}
	return e.pausingRemoteEntity.TransitionRemoteOwnership(to)
}

// newMidThawFixture 与 newProjectionOwnershipFixture 相同，只是 loader 里放的是 midThawRemoteEntity。
func newMidThawFixture(t *testing.T, id int64) (projectionOwnershipFixture, *midThawRemoteEntity) {
	t.Helper()
	const kind entity.EntityKind = 124
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	storage := &steppedStatusStorage{MongoCommitter: store, polled: make(chan struct{}), answer: make(chan bool)}
	loader := newRemoteTestLoader()
	live := &midThawRemoteEntity{
		pausingRemoteEntity: &pausingRemoteEntity{testRemoteEntity: newTestRemoteEntity(id, 1, kind), reached: make(chan struct{}), resume: make(chan struct{})},
		localOwnedReached:   make(chan struct{}), localOwnedResume: make(chan struct{}),
	}
	loader.add(live)
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	storage.finalizerCtx = mgr.remote.finalizeCtx
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(store)
	syncer := &countingSnapshotSyncer{}
	mgr.SetSyncer(syncer)
	steps := make(chan string, 64)
	mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	return projectionOwnershipFixture{mgr: mgr, store: store, storage: storage, syncer: syncer, live: live.pausingRemoteEntity, steps: steps}, live
}

// remoteApplyErrors 是 publisher 入口（投影器与 outbox 重放共用的 ApplyRemoteCommits）的失败累计。
func remoteApplyErrors() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "remote_entity.remote.apply_total" && metric.Labels["result"] == "error" {
			return metric.Value
		}
	}
	return 0
}

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

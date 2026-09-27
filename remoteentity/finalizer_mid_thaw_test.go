package remoteentity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// OPEN-ITEMS B14：RR-20260926-38 的“两个发布者同时处于解冻中途”窗口。
//
// 投影器报告未知后 finalizer 已把实体隔离（Quarantined）并在重试。投影器的重试发布停在 ack 的状态读取处（读到 Quarantined）；
// finalizer 回源读到 Applied、自己发布，它的 ack 已做完 Quarantined→Recovering、停在 Recovering→LocalOwned 之前；
// 此时放行投影器，它拿着过期的 Quarantined 继续解冻。
//
// bf-38 的“未验证项”预计后者会报错、靠下一轮收敛。实测不报错：同状态转换合法（ValidRemoteOwnershipTransition 的 from==to），
// 投影器的 Recovering→Recovering、Recovering→LocalOwned 都成功，finalizer 恢复后 LocalOwned→LocalOwned 同样成功。
// 断言两边都成功且只收尾一次：投影器发布返回 nil、publisher 失败计数不再增加（只有前置注入的那 1 次）、
// finalizer 走到 released、实体 LocalOwned、版本向量等于本提交、tracker Committed、写额度归零。
func TestPublishersMeetingMidThawBothConverge(t *testing.T) {
	f, live := newMidThawFixture(t, 1894)
	tx := remoteTestTxID(194)
	commits := f.asyncWrite(t, tx)
	f.projectorReportsUnknown(t, tx, commits)
	failuresBefore := remoteApplyErrors()

	// 投影器的下一次尝试：Mongo 事务已提交（Applied），发布停在 ack 的状态读取处，读到的是 Quarantined。
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	f.live.armed.Store(true)
	projectorErr := make(chan error, 1)
	go func() {
		_, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits)
		projectorErr <- err
	}()
	awaitSignal(t, deadline(t), f.live.reached, "the projector's acknowledge to read Quarantined")

	// finalizer 回源读到 Applied 并发布；它的解冻停在 Recovering→LocalOwned 之前。
	live.holdLocalOwned.Store(true)
	f.answerPoll(t, true)
	awaitSignal(t, deadline(t), live.localOwnedReached, "the finalizer's thaw to reach Recovering")
	if got := live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipRecovering {
		t.Fatalf("premise: entity state=%v while the finalizer is mid-thaw, want recovering", got)
	}

	// 投影器拿着过期的 Quarantined 继续解冻。
	close(f.live.resume)
	select {
	case err := <-projectorErr:
		if err != nil {
			t.Fatalf("projector publication while the finalizer was mid-thaw: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("projector publication did not return while the finalizer was mid-thaw")
	}
	close(live.localOwnedResume)
	f.awaitStep(t, "released")

	if got := remoteApplyErrors() - failuresBefore; got != 0 {
		t.Fatalf("publisher failures during the mid-thaw window=%d, want 0", got)
	}
	if got := live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("entity state=%v, want local_owned", got)
	}
	want := entity.RemoteVersionVector{StateVersion: commits[0].NextVersion, MarkerEpoch: commits[0].MarkerEpoch, LockFence: commits[0].LockFence, RouteEpoch: commits[0].RouteEpoch}
	if got := live.RemoteVersionVector(); got != want {
		t.Fatalf("version vector=%+v, want %+v", got, want)
	}
	if status, err := f.mgr.RemoteCommitStatus(context.Background(), tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("status=%v err=%v, want committed", status.State, err)
	}
	if slots := len(f.mgr.remote.writeSlots); slots != 0 {
		t.Fatalf("write slots=%d", slots)
	}
	select {
	case step := <-f.steps:
		t.Fatalf("finalizer continued after released: %s", step)
	default:
	}
}

// midThawRemoteEntity 在 pausingRemoteEntity 之上再加一个停点：holdLocalOwned 之后第一次转换到 LocalOwned 前停住。
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

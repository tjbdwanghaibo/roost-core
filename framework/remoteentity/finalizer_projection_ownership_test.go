package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-38：Durability 1/2 的 Remote 事务由 WAL 投影器负责完成（Mongo 原子提交 + 发布 + ack）。
// finalizer 只在投影器报告未知（tracker Indeterminate）或超过较长期限后回源；两个发布者碰到同一
// 事务时，acknowledgeRemoteCommit 必须幂等（版本向量相等且已 LocalOwned/Shared 视为成功）。
// REPRO-2026-09-26-04 §6 三个探针改写；所有交错都由通道控制，不依赖 sleep。

// pausingRemoteEntity 在 armed 之后的第一次 RemoteOwnershipState 读取处停住，
// 让两个发布者按测试指定的顺序交错。
type pausingRemoteEntity struct {
	*testRemoteEntity
	armed   atomic.Bool
	reached chan struct{}
	resume  chan struct{}
}

func (e *pausingRemoteEntity) RemoteOwnershipState() entity.RemoteOwnershipState {
	state := e.testRemoteEntity.RemoteOwnershipState()
	if e.armed.CompareAndSwap(true, false) {
		close(e.reached)
		<-e.resume
	}
	return state
}

// steppedStatusStorage 让测试逐次应答 finalizer 的回源读取：每次 CommitStatus 先在 polled 上报到，
// 再等 answer（true 读 Mongo 真实状态，false 回答 Unknown）。failNextCommit 让投影器的下一次
// 发布调用在后端提交处返回瞬时错误（事务状态 Indeterminate，投影器“报告未知”）。
type steppedStatusStorage struct {
	*MongoCommitter
	finalizerCtx   context.Context
	statusCalls    atomic.Int32
	polled         chan struct{}
	answer         chan bool
	failNextCommit atomic.Bool
}

func (s *steppedStatusStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	// 这里只控制 finalizer 回源的交错；提交路径现在也会读取持久状态，它不属于此屏障。
	if ctx != s.finalizerCtx {
		return s.MongoCommitter.CommitStatus(ctx, id)
	}
	s.statusCalls.Add(1)
	select {
	case s.polled <- struct{}{}:
	case <-ctx.Done():
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, ctx.Err()
	}
	var open bool
	select {
	case open = <-s.answer:
	case <-ctx.Done():
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, ctx.Err()
	}
	if !open {
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, nil
	}
	return s.MongoCommitter.CommitStatus(ctx, id)
}

func (s *steppedStatusStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if s.failNextCommit.CompareAndSwap(true, false) {
		return entity.RemoteCommitReceipt{}, errors.New("injected transient publication failure")
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

type countingSnapshotSyncer struct{ published atomic.Int32 }

func (s *countingSnapshotSyncer) PublishRemoteSnapshot(context.Context, entity.RemoteSnapshotRecord) error {
	s.published.Add(1)
	return nil
}
func (*countingSnapshotSyncer) DeleteRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, uint64) error {
	return nil
}
func (*countingSnapshotSyncer) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	return nil
}

type projectionOwnershipFixture struct {
	mgr     *Manager
	store   *MongoCommitter
	storage *steppedStatusStorage
	syncer  *countingSnapshotSyncer
	live    *pausingRemoteEntity
	steps   chan string
}

func newProjectionOwnershipFixture(t *testing.T, id int64) projectionOwnershipFixture {
	t.Helper()
	const kind entity.EntityKind = 124
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	storage := &steppedStatusStorage{MongoCommitter: store, polled: make(chan struct{}), answer: make(chan bool)}
	loader := newRemoteTestLoader()
	live := &pausingRemoteEntity{testRemoteEntity: newTestRemoteEntity(id, 1, kind), reached: make(chan struct{}), resume: make(chan struct{})}
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
		// 让可能停在回源读取上的 finalizer 退出：StopFinalizer 取消 finalizeCtx。
		_ = mgr.StopFinalizer(ctx)
	})
	return projectionOwnershipFixture{mgr: mgr, store: store, storage: storage, syncer: syncer, live: live, steps: steps}
}

// asyncWrite：Durability 1 写入，Commit 返回推测回执，Close 把 gate/写额度交给 finalizer。
func (f projectionOwnershipFixture) asyncWrite(t *testing.T, tx entity.RemoteTransactionID) []entity.RemoteCommit {
	t.Helper()
	batch, err := f.mgr.PrepareRemoteWriteBatch(context.Background(), []int64{f.live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	f.live.dirty.set(true)
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "async", "", true, 1)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	if _, err = batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return commits
}

func (f projectionOwnershipFixture) nextStep(t *testing.T, what string) string {
	t.Helper()
	select {
	case step := <-f.steps:
		return step
	case <-f.storage.polled:
		t.Fatalf("%s: finalizer read the durable status from the backend (reads=%d) while the WAL projector still owned the outcome",
			what, f.storage.statusCalls.Load())
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: finalizer made no progress", what)
	}
	return ""
}

func (f projectionOwnershipFixture) answerPoll(t *testing.T, open bool) {
	t.Helper()
	select {
	case <-f.storage.polled:
	case <-time.After(3 * time.Second):
		t.Fatal("finalizer did not read the durable status")
	}
	f.storage.answer <- open
}

func (f projectionOwnershipFixture) awaitStep(t *testing.T, want string) {
	t.Helper()
	select {
	case step := <-f.steps:
		if step != want {
			t.Fatalf("finalizer step=%s, want %s (owner=%v slots=%d)", step, want, f.live.testRemoteEntity.RemoteOwnershipState(), len(f.mgr.remote.writeSlots))
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("finalizer did not reach step %s", want)
	}
}

// 投影器正常完成：finalizer 不回源、不隔离实体、不重复发布快照；投影器发布后立即收尾释放。
func TestAsyncFinalizerLeavesProjectionToProjector(t *testing.T) {
	f := newProjectionOwnershipFixture(t, 1891)
	tx := remoteTestTxID(191)
	commits := f.asyncWrite(t, tx)
	first := f.nextStep(t, "projection pending")
	if first != "await_projection" {
		t.Fatalf("first finalizer step=%s, want await_projection", first)
	}
	if got := f.live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("entity state while projection pending=%v, want local_owned (not quarantined by the finalizer)", got)
	}
	// WAL 投影器：同一 Mongo 事务提交后发布。
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatalf("projector publication: %v", err)
	}
	if step := f.nextStep(t, "after projector publication"); step != "released" {
		t.Fatalf("finalizer step after publication=%s, want released", step)
	}
	if reads := f.storage.statusCalls.Load(); reads != 0 {
		t.Fatalf("finalizer backend status reads=%d, want 0", reads)
	}
	if published := f.syncer.published.Load(); published != 1 {
		t.Fatalf("snapshot publications=%d, want 1 (projector only)", published)
	}
	if got := f.live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipLocalOwned {
		t.Fatalf("entity state after completion=%v, want local_owned", got)
	}
	if slots := len(f.mgr.remote.writeSlots); slots != 0 {
		t.Fatalf("write slots=%d after completion", slots)
	}
	if status, err := f.mgr.RemoteCommitStatus(context.Background(), tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("status=%v err=%v, want committed", status.State, err)
	}
}

// 原双发布者交错已经取消。保留未知结果后的恢复压力：唯一发布者暂停时，
// finalizer 只能等待；幂等重放和显式恢复不能绕过它再发布。
func TestOutboxOwnsPublicationAfterIndeterminateProjection(t *testing.T) {
	f := newProjectionOwnershipFixture(t, 1892)
	tx := remoteTestTxID(192)
	commits := f.asyncWrite(t, tx)
	f.projectorReportsUnknown(t, tx, commits)
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	f.live.armed.Store(true)
	release := sync.OnceFunc(func() { close(f.live.resume) })
	t.Cleanup(release)
	f.answerPoll(t, true)
	select {
	case <-f.live.reached:
	case <-time.After(3 * time.Second):
		t.Fatal("outbox never entered acknowledgement")
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	if got := f.syncer.published.Load(); got != 0 {
		t.Fatalf("another caller published while outbox was paused: %d", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.mgr.RecoverOutbox(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent recovery bypassed publisher: %v", err)
	}
	release()
	f.finishPublication(t)
	if got := f.syncer.published.Load(); got != 1 {
		t.Fatalf("publications=%d want 1", got)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.RecoverOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.syncer.published.Load(); got != 1 {
		t.Fatalf("committed replay republished: %d", got)
	}
}

// 补发失败期间 finalizer 可以报告 retry；恢复后仍只释放一次，不能把 retry 次数当失败。
func (f projectionOwnershipFixture) finishPublication(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-f.storage.polled:
			f.storage.answer <- true
		case step := <-f.steps:
			if step == "released" {
				if len(f.mgr.remote.writeSlots) != 0 || f.live.testRemoteEntity.RemoteOwnershipState() != entity.RemoteOwnershipLocalOwned {
					t.Fatal("publication did not release and restore the local entity")
				}
				return
			}
			if step != "retry" && step != "await_projection" {
				t.Fatalf("unexpected finalizer step %s", step)
			}
		case <-timer.C:
			t.Fatal("publication did not converge")
		}
	}
}

// projectorReportsUnknown 让投影器的第一次发布以瞬时错误结束（tracker Indeterminate），
// finalizer 被唤醒后回源读到 Unknown，隔离实体并进入重试。
func (f projectionOwnershipFixture) projectorReportsUnknown(t *testing.T, tx entity.RemoteTransactionID, commits []entity.RemoteCommit) {
	t.Helper()
	select {
	case step := <-f.steps:
		if step != "await_projection" {
			t.Fatalf("first finalizer step=%s, want await_projection", step)
		}
	case <-f.storage.polled:
		// 修复前：finalizer 立即回源。按 Unknown 应答，让旧实现走到同样的“隔离 + 重试”状态。
		f.storage.answer <- false
		f.awaitStep(t, "retry")
	case <-time.After(3 * time.Second):
		t.Fatal("finalizer made no progress")
	}
	f.storage.failNextCommit.Store(true)
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("injected projector failure err=%v, want indeterminate", err)
	}
	f.answerPoll(t, false)
	f.awaitStep(t, "retry")
	if got := f.live.testRemoteEntity.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
		t.Fatalf("entity state after unknown=%v, want quarantined", got)
	}
}

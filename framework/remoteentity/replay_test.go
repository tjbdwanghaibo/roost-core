// 已提交事务的重放：保留更新的 fence 与脏状态、重放发布失败不改结论、并行窗口后缀重放、同 fence 版本不回退、删除确认。

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
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
	live.dirty.set(true)
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
	live.dirty.set(true)
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
	live.dirty.set(true)
	if _, err := mgr.ApplyRemoteCommits(context.Background(), commits[0].TransactionID, commits); err != nil {
		t.Fatal(err)
	}
	if live.RemoteVersionVector() != newer || !live.dirty.Dirty() {
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
	if live.dirty.Dirty() {
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

func (b *flakyMarkBackend) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	if b.fail.Load() {
		return errors.New("injected publish mark failure")
	}
	return b.remoteTestLoader.MarkRemoteCommitPublished(ctx, id)
}

// 已 Committed 的事务重放发布失败，不能把 tracker 改回 Indeterminate，后到的等待方 / FlushRemoteAll 仍得到已提交结论。
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
	live.dirty.set(true)
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
	if _, err = mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatalf("committed replay must not attempt publication again: %v", err)
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

var errInjectedPublishMark = errors.New("injected publish mark failure")

// scriptedPublicationStorage 按调用序号控制目标事务的发布路径，让竞态顺序确定：
//   - 投影器的回执读回（CommitRemote）第 1、2 次失败（投影器首次发布、finalizer 之后的第一次重放）；
//   - 投影器第 2 次发布等到下一写者拿到新 fence 后才继续（然后失败）；
//   - 投影器第 3 次发布在测试检查 tracker 之后才继续，成功并 ack。
//
// “已 Applied、只是发布失败”由投影器记为已投影、交给补发循环，不再重放；要覆盖的“投影失败 → 重放旧回执”由回执读回失败触发
// （结果未知，投影器照旧重试）。
//
// 除此之外全部走真实 MongoCommitter；非目标事务不受影响。
type scriptedPublicationStorage struct {
	*MongoCommitter
	target entity.RemoteTransactionID

	mu              sync.Mutex
	publishes       int
	marks           int
	firstMarkFailed chan struct{}
	allowRetry      chan struct{}
	thirdReached    chan struct{}
	allowThird      chan struct{}
}

func (s *scriptedPublicationStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if commit.TransactionID == s.target {
		s.mu.Lock()
		s.publishes++
		n := s.publishes
		s.mu.Unlock()
		var gate chan struct{}
		switch n {
		case 1:
			close(s.firstMarkFailed)
			return entity.RemoteCommitReceipt{}, errInjectedPublishMark
		case 2:
			gate = s.allowRetry
		case 3:
			close(s.thirdReached)
			gate = s.allowThird
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return entity.RemoteCommitReceipt{}, ctx.Err()
			}
		}
		if n == 2 {
			return entity.RemoteCommitReceipt{}, errInjectedPublishMark
		}
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

func (s *scriptedPublicationStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	if id == s.target {
		s.mu.Lock()
		s.marks++
		s.mu.Unlock()
	}
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

func remoteCommitRecord(durability coredata.Durability, commits []entity.RemoteCommit) coredata.CommitRecord {
	record := coredata.CommitRecord{ID: coredata.TransactionID(commits[0].TransactionID), Handler: "remote-e2e", Durability: durability}
	for i := range commits {
		commit := commits[i].Clone()
		record.Mutations = append(record.Mutations, coredata.Mutation{
			Key:  coredata.DocumentKey{Resource: "remote_entity", ID: commit.EntityID},
			Kind: coredata.MutationPut, ExpectedVersion: commit.BaseVersion, NextVersion: commit.NextVersion,
			Schema: commit.Schema, Codec: "remote", Remote: &commit,
		})
	}
	return record
}

func awaitSignal(t *testing.T, ctx context.Context, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", what)
	}
}

// 端到端：真实 nestwal WAL + engine.Projector/MongoStore + mongotest，Remote 走正式 Backend + MongoCommitter 权威。已提交事务
// 首次发布失败 → finalizer 抢先发布并释放 gate → 同实体下一写者拿到更大的 fence → 投影器重放旧回执（再失败一次）后 ack；
// 新写者随后正常提交，没有无限重试。
// 历史：RR-20260926-11。
func TestCommittedReplayAfterFinalizerAndNewerFenceAcksWAL(t *testing.T) {
	const kind entity.EntityKind = 122
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongo := newRemoteMongoFake()
	committer := NewMongoCommitter(mongo, "control", 1000, 0)
	tx1, tx2 := remoteTestTxID(111), remoteTestTxID(112)
	storage := &scriptedPublicationStorage{
		MongoCommitter: committer, target: tx1,
		firstMarkFailed: make(chan struct{}), allowRetry: make(chan struct{}),
		thirdReached: make(chan struct{}), allowThird: make(chan struct{}),
	}
	loader := newRemoteTestLoader()
	live := newTestRemoteEntity(1811, 1, kind)
	loader.add(live)
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(committer)
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(stop)
	})

	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(committer, mgr); err != nil {
		t.Fatal(err)
	}
	walOptions := nestwal.DefaultOptions(t.TempDir())
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 20 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 失败路径上放开所有闸门，避免投影器 goroutine 卡在注入点。
		for _, gate := range []chan struct{}{storage.allowRetry, storage.allowThird} {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
		closing, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = projector.Close(closing)
	})

	// tx1：async 持久级别，WAL 准入后 Commit 返回推测回执，Close 转交 finalizer。
	batch1, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.set(true)
	if err = batch1.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx1, "remote-e2e", "", true, uint8(nest.DurabilityAsync.Record()))); err != nil {
		t.Fatal(err)
	}
	commits1 := batch1.Commits()
	if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync.Record(), commits1)); err != nil {
		t.Fatal(err)
	}
	if _, err = batch1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(coredata.TransactionID(tx1))
	awaitSignal(t, ctx, storage.firstMarkFailed, "the projector's first publication failure")
	if err = batch1.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// finalizer 回源得到 Applied，自行发布并释放 gate；下一写者拿到更大的 fence。
	batch2, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatalf("next writer not admitted after finalizer: %v", err)
	}
	if got := live.RemoteVersionVector(); got.StateVersion != 1 || got.LockFence <= commits1[0].LockFence {
		t.Fatalf("next writer vector=%+v, want version 1 with fence > %d", got, commits1[0].LockFence)
	}
	newerFence := live.RemoteVersionVector().LockFence

	// 投影器重放旧回执：不能被新 fence 拒绝；这次发布标记再失败一次。
	close(storage.allowRetry)
	awaitSignal(t, ctx, storage.thirdReached, "the projector's second replay")
	if err = mgr.FlushRemoteTransaction(ctx, tx1); err != nil {
		t.Fatalf("committed tx1 reported as unresolved after a failed replay: %v", err)
	}
	if status, err := mgr.RemoteCommitStatus(ctx, tx1); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("tx1 status=%+v err=%v", status, err)
	}
	if got := live.RemoteVersionVector().LockFence; got != newerFence {
		t.Fatalf("replay rewound the newer fence: %d -> %d", newerFence, got)
	}
	live.dirty.set(true)
	close(storage.allowThird)

	// 新写者正常提交：它的 WAL 记录排在 tx1 之后，tx1 ack 后才会投影。
	if err = batch2.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx2, "remote-e2e", "", true, uint8(nest.DurabilityAsync.Record()))); err != nil {
		t.Fatal(err)
	}
	commits2 := batch2.Commits()
	if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync.Record(), commits2)); err != nil {
		t.Fatal(err)
	}
	if _, err = batch2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(coredata.TransactionID(tx2))
	if err = projector.Flush(ctx); err != nil {
		t.Fatalf("projector flush: %v", err)
	}
	// 投影后再转交 finalizer：让 tx2 按“投影器先确认、finalizer 再收尾”的固定先后走完。
	// 早先这样排是因为测试实体的 dirty 标记不是并发安全的；RR-20260926-63 已给 testDirty 加锁
	// （manager_test.go，按 IRemoteCommitParticipant 契约并发安全），这里的先后不再是为了规避数据竞争（OPEN-ITEMS A12）。
	if err = batch2.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = mgr.FlushRemoteTransaction(ctx, tx2); err != nil {
		t.Fatalf("tx2: %v", err)
	}
	if got := live.RemoteVersionVector(); got.StateVersion != 2 || got.LockFence != newerFence {
		t.Fatalf("vector after tx2=%+v, want version 2 fence %d", got, newerFence)
	}
	// 第三个写者能进入，说明 tx2 的 finalizer 也已收尾。
	batch3, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatalf("gate still held after tx2: %v", err)
	}
	_ = batch3.Abort(ctx, errors.New("test cleanup"))
	_ = batch3.Close(ctx)

	stats := projector.Stats()
	if stats.WALUnacked != 0 || stats.FatalProjectionConflicts != 0 || stats.ProjectionFailures != 2 {
		t.Fatalf("projector stats=%+v, want acked WAL and exactly the two injected failures", stats)
	}
	authority, err := committer.readAuthority(ctx, live.GUId())
	if err != nil || authority.Version != 2 {
		t.Fatalf("authority=%+v err=%v, want version 2", authority, err)
	}
	for _, tx := range []entity.RemoteTransactionID{tx1, tx2} {
		if status, err := committer.CommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitCommitted {
			t.Fatalf("durable %s status=%+v err=%v", tx, status, err)
		}
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err = mgr.StopFinalizer(stop); err != nil {
		t.Fatal(err)
	}
	if len(mgr.remote.writeSlots) != 0 {
		t.Fatalf("write slots leaked: %d", len(mgr.remote.writeSlots))
	}
}

// 并行投影窗口的成功后缀重放：真实 nestwal WAL + engine.Projector（RemoteProjectionWorkers=8）+ engine.MongoStore + mongotest，
// Remote 走正式 Backend + MongoCommitter 权威。同一窗口里两笔独立实体的 Remote 事务：前缀 tx1（实体 X）发布一次瞬时失败，
// 后缀 tx2（实体 Y）已成功持久并发布；窗口只确认连续成功前缀（0 条），后缀在重试时按持久身份重放，重放之前 Y 已被下一写者
// 拿到更大的 fence。验证：重试后 WAL 全部 ack、无 fatal、失败计数恰为 1；后缀重放不被新 fence 拒绝、不回退 Y 的 fence 与版本，
// tx2 的 tracker 保持 Committed；Projected 计入重放（3 = tx1 一次 + tx2 两次），最终以权威版本、持久回执与下一写者的正常提交
// 共同确认。
func TestParallelWindowReplaysSucceededSuffixUnderNewerFence(t *testing.T) {
	const kind entity.EntityKind = 122
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongo := newRemoteMongoFake()
	committer := NewMongoCommitter(mongo, "control", 1000, 0)
	tx1, tx2, tx3 := remoteTestTxID(131), remoteTestTxID(132), remoteTestTxID(133)
	storage := &suffixReplayStorage{
		MongoCommitter: committer, prefix: tx1, suffix: tx2,
		suffixPublished: make(chan struct{}), failPrefix: make(chan struct{}),
	}
	loader := newRemoteTestLoader()
	x, y := newTestRemoteEntity(1911, 1, kind), newTestRemoteEntity(1912, 1, kind)
	loader.add(x)
	loader.add(y)
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(committer)
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(stop)
	})

	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(committer, mgr); err != nil {
		t.Fatal(err)
	}
	if !store.SupportsRemoteParallelProjection() {
		t.Fatal("premise: formal MongoStore + Backend must enable parallel Remote projection")
	}
	walOptions := nestwal.DefaultOptions(t.TempDir())
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 20 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
		RemoteProjectionWorkers: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		storage.release()
		closing, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = projector.Close(closing)
	})

	// 两笔事务都在实体锁内准入（held），先放 tx2 再放 tx1，投影器第一次读到时两条同在一个窗口。
	prepare := func(tx entity.RemoteTransactionID, live *testRemoteEntity) (entity.RemoteWriteBatch, []entity.RemoteCommit) {
		t.Helper()
		batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
		if err != nil {
			t.Fatal(err)
		}
		live.dirty.set(true)
		if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "remote-e2e", "", true, uint8(nest.DurabilityAsync.Record()))); err != nil {
			t.Fatal(err)
		}
		commits := batch.Commits()
		if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync.Record(), commits)); err != nil {
			t.Fatal(err)
		}
		if _, err = batch.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return batch, commits
	}
	batch1, _ := prepare(tx1, x)
	batch2, commits2 := prepare(tx2, y)
	// OPEN-ITEMS B44：钉住“两笔进入同一窗口”。一次回放在开头就定下本轮读到的 WAL 末尾；后台回放若在
	// tx2 追加之前开始、扫到 tx1 时 tx1 恰好已释放，就会单独投影 tx1——串行投影里前缀停在注入点等后缀，
	// 后缀永远不开始，用例 10s 超时（负载下约 3/2000）。这里在两笔都已追加、都还 held 时同步跑一轮
	// 回放：回放之间由 replayGate 串行，它返回即说明更早开始的回放都已结束（且都在 tx1 前停下），
	// 此后的每一轮都读得到 tx2；tx1 在 WAL 里靠前且最后释放，第一轮能投影的回放必然同时带上两笔。
	if processed, _ := projector.ReplayPass(ctx); processed != 0 {
		t.Fatalf("premise: both transactions are held, but a replay pass projected %d records", processed)
	}
	projector.TransactionReleased(coredata.TransactionID(tx2))
	projector.TransactionReleased(coredata.TransactionID(tx1))

	// 后缀 tx2 成功发布；前缀 tx1 的发布停在注入点，窗口尚未返回。
	awaitSignal(t, ctx, storage.suffixPublished, "the suffix publication")
	if err = batch2.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = mgr.FlushRemoteTransaction(ctx, tx2); err != nil {
		t.Fatalf("suffix tx2 before replay: %v", err)
	}
	// Y 的下一写者拿到更大的 fence，之后投影器才会重放 tx2。
	batch3, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{y.GUId()})
	if err != nil {
		t.Fatalf("next writer on the suffix entity not admitted: %v", err)
	}
	newer := y.RemoteVersionVector()
	if newer.StateVersion != 1 || newer.LockFence <= commits2[0].LockFence {
		t.Fatalf("next writer vector=%+v, want version 1 with fence > %d", newer, commits2[0].LockFence)
	}
	storage.allowPrefixFailure()

	// 重试窗口：tx1 成功，tx2 按持久身份重放。
	if err = projector.Flush(ctx); err != nil {
		t.Fatalf("projector flush after the failed window: %v", err)
	}
	if got := storage.suffixMarkCount(); got != 1 {
		t.Fatalf("suffix publication count=%d, want exactly one despite WAL replay", got)
	}
	storage.mu.Lock()
	suffixCalls := storage.suffixCommits
	storage.mu.Unlock()
	if suffixCalls < 2 {
		t.Fatalf("suffix commit calls=%d, WAL replay did not occur", suffixCalls)
	}
	if got := y.RemoteVersionVector(); got != newer {
		t.Fatalf("suffix replay rewound the live vector: %+v -> %+v", newer, got)
	}
	if status, err := mgr.RemoteCommitStatus(ctx, tx2); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("suffix tx2 tracker after replay: status=%+v err=%v", status, err)
	}
	stats := projector.Stats()
	if stats.WALUnacked != 0 || stats.FatalProjectionConflicts != 0 || stats.ProjectionFailures != 1 || stats.Projected != 3 {
		t.Fatalf("projector stats=%+v, want acked WAL, one window failure and Projected=3 (suffix replayed)", stats)
	}

	if err = batch1.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = mgr.FlushRemoteTransaction(ctx, tx1); err != nil {
		t.Fatalf("prefix tx1: %v", err)
	}

	// 下一写者在新 fence 下正常提交并投影。
	y.dirty.set(true)
	if err = batch3.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx3, "remote-e2e", "", true, uint8(nest.DurabilityAsync.Record()))); err != nil {
		t.Fatal(err)
	}
	if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync.Record(), batch3.Commits())); err != nil {
		t.Fatal(err)
	}
	if _, err = batch3.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(coredata.TransactionID(tx3))
	if err = projector.Flush(ctx); err != nil {
		t.Fatalf("projector flush for the next writer: %v", err)
	}
	if err = batch3.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = mgr.FlushRemoteTransaction(ctx, tx3); err != nil {
		t.Fatalf("next writer tx3: %v", err)
	}
	if got := y.RemoteVersionVector(); got.StateVersion != 2 || got.LockFence != newer.LockFence {
		t.Fatalf("suffix entity after tx3=%+v, want version 2 fence %d", got, newer.LockFence)
	}
	if got := projector.Stats(); got.WALUnacked != 0 || got.FatalProjectionConflicts != 0 || got.ProjectionFailures != 1 {
		t.Fatalf("projector stats after tx3=%+v", got)
	}
	for id, want := range map[int64]int64{x.GUId(): 1, y.GUId(): 2} {
		authority, err := committer.readAuthority(ctx, id)
		if err != nil || authority.Version != want {
			t.Fatalf("authority %d=%+v err=%v, want version %d", id, authority, err, want)
		}
	}
	for _, tx := range []entity.RemoteTransactionID{tx1, tx2, tx3} {
		if status, err := committer.CommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitCommitted {
			t.Fatalf("durable %s status=%+v err=%v", tx, status, err)
		}
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err = mgr.StopFinalizer(stop); err != nil {
		t.Fatal(err)
	}
	if len(mgr.remote.writeSlots) != 0 {
		t.Fatalf("write slots leaked: %d", len(mgr.remote.writeSlots))
	}
}

// suffixReplayStorage 控制并行窗口内两笔事务的发布顺序，其余全部走真实 MongoCommitter：
//   - 后缀事务第一次发布成功后关闭 suffixPublished；
//   - 前缀事务第一次发布标记先等后缀发布成功、再等测试放行，然后返回瞬时失败，
//     保证窗口里“前缀失败、后缀成功”，且后缀重放发生在下一写者拿到新 fence 之后。
type suffixReplayStorage struct {
	*MongoCommitter
	prefix, suffix entity.RemoteTransactionID

	mu              sync.Mutex
	prefixMarks     int
	suffixMarks     int
	suffixPublished chan struct{}
	failPrefix      chan struct{}
	publishedOnce   sync.Once
	failOnce        sync.Once
	suffixCommits   int
}

// CommitRemote 让前缀的第一次回执读回停在注入点、随后失败（结果未知，投影器重试整个窗口）。“已 Applied、只是发布失败”由
// 投影器记为已投影并交给补发循环，不会触发窗口重试，所以注入点在回执读回。
func (s *suffixReplayStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if commit.TransactionID == s.suffix {
		s.mu.Lock()
		s.suffixCommits++
		s.mu.Unlock()
	}
	if commit.TransactionID == s.prefix {
		s.mu.Lock()
		s.prefixMarks++
		first := s.prefixMarks == 1
		s.mu.Unlock()
		if first {
			for _, gate := range []chan struct{}{s.suffixPublished, s.failPrefix} {
				select {
				case <-gate:
				case <-ctx.Done():
					return entity.RemoteCommitReceipt{}, ctx.Err()
				}
			}
			return entity.RemoteCommitReceipt{}, errInjectedPublishMark
		}
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

func (s *suffixReplayStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	switch id {
	case s.suffix:
		err := s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
		s.mu.Lock()
		s.suffixMarks++
		first := s.suffixMarks == 1
		s.mu.Unlock()
		if first && err == nil {
			s.publishedOnce.Do(func() { close(s.suffixPublished) })
		}
		return err
	}
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

func (s *suffixReplayStorage) suffixMarkCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suffixMarks
}

// release 在失败路径上放开所有闸门，避免投影器 goroutine 卡在注入点。
func (s *suffixReplayStorage) release() {
	s.publishedOnce.Do(func() { close(s.suffixPublished) })
	s.allowPrefixFailure()
}

func (s *suffixReplayStorage) allowPrefixFailure() {
	s.failOnce.Do(func() { close(s.failPrefix) })
}

// 同一 fence 下 StateVersion 不能回退：非 authority 装配（ownership store 不实现 WriteAuthority）里每一笔写都沿用实体当前的
// LockFence。已提交事务的迟到重放与同实体下一笔提交交错时，SetRemoteVersionVector 拒绝同 fence 下更小的 StateVersion，
// acknowledgeRemoteCommit 再次核对后把迟到重放判为过期（成功返回、不改向量）。
// 历史：RR-20260927-15。
func TestSameFenceReplayCannotRewindStateVersion(t *testing.T) {
	const kind entity.EntityKind = 119
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	loader := newRemoteTestLoader()
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	live := &vectorPausingEntity{testRemoteEntity: newTestRemoteEntity(1486, 1, kind), reached: make(chan struct{}), resume: make(chan struct{})}
	loader.add(live)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	write := func(tx entity.RemoteTransactionID) []entity.RemoteCommit {
		t.Helper()
		batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
		if err != nil {
			t.Fatalf("prepare %s: %v", tx, err)
		}
		live.dirty.set(true)
		if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "same-fence", "", true, 0)); err != nil {
			t.Fatal(err)
		}
		commits := batch.Commits()
		if _, err = batch.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", tx, err)
		}
		if err = batch.Close(ctx); err != nil {
			t.Fatal(err)
		}
		return commits
	}
	tx1, tx2, tx3 := remoteTestTxID(0xF1), remoteTestTxID(0xF2), remoteTestTxID(0xF3)
	commits1 := write(tx1)
	if got := live.RemoteVersionVector(); got.StateVersion != 1 || got.LockFence != commits1[0].LockFence {
		t.Fatalf("premise: vector after tx1=%+v", got)
	}

	// tx1 的迟到重放通过过期判断（版本恰等于 NextVersion），停在写向量之前。
	live.armed.Store(true)
	replay := make(chan error, 1)
	go func() {
		_, err := mgr.ApplyRemoteCommits(context.Background(), tx1, commits1)
		replay <- err
	}()
	awaitSignal(t, ctx, live.reached, "the replay to reach SetRemoteVersionVector")

	// 同实体下一笔在同一 fence 下提交，版本推进到 2。
	commits2 := write(tx2)
	if commits2[0].LockFence != commits1[0].LockFence {
		t.Fatalf("premise: non-authority assembly must reuse the fence (tx1 %d, tx2 %d)", commits1[0].LockFence, commits2[0].LockFence)
	}
	advanced := live.RemoteVersionVector()
	if advanced.StateVersion != 2 {
		t.Fatalf("premise: vector after tx2=%+v, want version 2", advanced)
	}

	close(live.resume)
	if err := <-replay; err != nil {
		t.Fatalf("late replay of committed tx1: %v", err)
	}
	if got := live.RemoteVersionVector(); got != advanced {
		t.Fatalf("late replay of tx1 rewound the version vector under the same fence: %+v -> %+v", advanced, got)
	}
	// 下一写者按未回退的版本准备并提交。
	commits3 := write(tx3)
	if commits3[0].BaseVersion != 2 || live.RemoteVersionVector().StateVersion != 3 {
		t.Fatalf("next writer base=%d vector=%+v, want base 2 and version 3", commits3[0].BaseVersion, live.RemoteVersionVector())
	}
}

// vectorPausingEntity 在 armed 之后第一次 SetRemoteVersionVector 处停住（写入之前），让重放与下一写者按测试顺序交错。
type vectorPausingEntity struct {
	*testRemoteEntity
	armed   atomic.Bool
	reached chan struct{}
	resume  chan struct{}
}

func (e *vectorPausingEntity) SetRemoteVersionVector(version entity.RemoteVersionVector) error {
	if e.armed.CompareAndSwap(true, false) {
		close(e.reached)
		<-e.resume
	}
	return e.testRemoteEntity.SetRemoteVersionVector(version)
}

// Remote 实体的删除提交到权威后确认照常成功：删除随事务走 deferEntityDelete，准入之后实例就被清空（ID 归零），投影器稍后确认
// 时不能把被清空的实例当成这个实体去确认（生成代码按身份核对会失败），否则发布（L2 墓碑 + 推送删除）被跳过，只读方读回删除前的快照。
// generatedAckEntity 用生成代码同一条确认规则（codegen/internal/entity/gen.go 的 AcknowledgeRemoteCommit 模板）。
// 历史：RR-20261006-01。
type generatedAckEntity struct{ *testRemoteEntity }

func (e generatedAckEntity) AcknowledgeRemoteCommit(commit entity.RemoteCommit) error {
	if commit.EntityID != e.GUId() {
		return fmt.Errorf("Guild: remote acknowledgement identity mismatch")
	}
	return e.testRemoteEntity.AcknowledgeRemoteCommit(commit)
}

type recordedSnapshotDelete struct {
	key     entity.RemoteSnapshotKey
	version uint64
}

type recordingSnapshotPublisher struct {
	mu      sync.Mutex
	deletes []recordedSnapshotDelete
}

func (*recordingSnapshotPublisher) PublishRemoteSnapshot(context.Context, entity.RemoteSnapshotRecord) error {
	return nil
}

func (p *recordingSnapshotPublisher) DeleteRemoteSnapshot(_ context.Context, key entity.RemoteSnapshotKey, version uint64) error {
	p.mu.Lock()
	p.deletes = append(p.deletes, recordedSnapshotDelete{key: key, version: version})
	p.mu.Unlock()
	return nil
}

func (*recordingSnapshotPublisher) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	return nil
}

// 删除提交确认时实例已被清空：提交照常成功、发布删除（缓存墓碑 + 推送），不把被清空的实例当成这个实体去确认。
func TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared(t *testing.T) {
	// kind 241：本包测试已用 12、91～96、119～135、143、144、196～198、213、217、236、237、243～245、249～254，不能撞号。
	const kind entity.EntityKind = 241
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx := context.Background()
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	publisher := &recordingSnapshotPublisher{}
	mgr.snapshots.transport = publisher
	live := generatedAckEntity{newTestRemoteEntity(4101, 1, kind)}
	loader.add(live)
	id := live.GUId()
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}

	batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	outcome := entity.NewRemoteTransactionOutcome(remoteTestTxID(41), "delete", "", true, 0)
	outcome.DeleteIntents = batchDeleteIntent(id)
	if err := batch.FinalizeLocked(outcome); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	if len(commits) != 1 || !commits[0].Delete {
		t.Fatalf("commits=%+v, want one delete commit", commits)
	}
	// 事务的删除已经把实例从内存删掉：实体管理器对它执行的就是 ClearBase（ID 归零）。
	live.ClearBase()
	if live.GUId() == id {
		t.Fatal("ClearBase did not reset the instance; the test no longer models the destroyed entity")
	}
	receipts, err := batch.Commit(ctx)
	if err != nil {
		t.Fatalf("the delete reached the authority but its acknowledgement failed: %v", err)
	}
	if len(receipts) != 1 || receipts[0].EntityID != id {
		t.Fatalf("receipts=%+v", receipts)
	}
	publisher.mu.Lock()
	deletes := append([]recordedSnapshotDelete(nil), publisher.deletes...)
	publisher.mu.Unlock()
	if len(deletes) != 1 || deletes[0].key != key || deletes[0].version != commits[0].NextVersion {
		t.Fatalf("published deletes=%+v, want the tombstone of %v at version %d", deletes, key, commits[0].NextVersion)
	}
	if got, found, err := mgr.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || found {
		t.Fatalf("after the delete the cache reads found=%v version=%d err=%v, want the tombstone", found, got.StateVersion, err)
	}
	if err := batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

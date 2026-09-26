package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

var errInjectedPublishMark = errors.New("injected publish mark failure")

// scriptedPublicationStorage 按调用序号控制目标事务的发布路径，让竞态顺序确定：
//   - MarkRemoteCommitPublished 第 1、3 次失败（投影器首次发布、finalizer 之后的第一次重放）；
//   - 投影器第 2 次发布（CommitRemote）等到下一写者拿到新 fence 后才继续；
//   - 投影器第 3 次发布在测试检查 tracker 之后才继续。
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
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

func (s *scriptedPublicationStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	if id == s.target {
		s.mu.Lock()
		s.marks++
		n := s.marks
		s.mu.Unlock()
		switch n {
		case 1:
			close(s.firstMarkFailed)
			return errInjectedPublishMark
		case 3:
			return errInjectedPublishMark
		}
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

// RR-20260926-11 端到端：真实 nestwal WAL + engine.Projector/MongoStore + mongotest 伪 Mongo，
// Remote 走正式 Backend + MongoCommitter 权威。已提交事务首次发布失败 → finalizer 抢先
// 发布并释放 gate → 同实体下一写者拿到更大的 fence → 投影器重放旧回执（再失败一次）
// 后 ack；新写者随后正常提交，没有无限重试。
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
	walOptions.WriterVersion = nestwal.WriterVersionV2
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
	live.dirty.dirty = true
	if err = batch1.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx1, "remote-e2e", "", true, uint8(nest.DurabilityAsync))); err != nil {
		t.Fatal(err)
	}
	commits1 := batch1.Commits()
	if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync, commits1)); err != nil {
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
	live.dirty.dirty = true
	close(storage.allowThird)

	// 新写者正常提交：它的 WAL 记录排在 tx1 之后，tx1 ack 后才会投影。
	if err = batch2.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx2, "remote-e2e", "", true, uint8(nest.DurabilityAsync))); err != nil {
		t.Fatal(err)
	}
	commits2 := batch2.Commits()
	if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync, commits2)); err != nil {
		t.Fatal(err)
	}
	if _, err = batch2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(coredata.TransactionID(tx2))
	if err = projector.Flush(ctx); err != nil {
		t.Fatalf("projector flush: %v", err)
	}
	// 投影后再转交 finalizer：测试实体的 dirty 标记不是并发安全的，避免与投影器的确认并发
	// （生成实体的 AcknowledgeRemoteCommit 走原子 AdvanceVersion，不受此限）。
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

package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// neverReachedStorage 模拟事务根本没有到达 Mongo 的瞬时失败：发出前 ctx 已过期、
// 连接断开或 StartSession 失败。其余能力（授权、状态、拒绝）走真实 MongoCommitter。
type neverReachedStorage struct{ *MongoCommitter }

func (neverReachedStorage) CommitRemoteBatch(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, context.DeadlineExceeded
}

func (neverReachedStorage) CommitRemote(context.Context, entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	return entity.RemoteCommitReceipt{}, context.DeadlineExceeded
}

// lateCommitStorage 模拟迟到提交：客户端拿到超时，服务端随后才提交；finalizer
// 第一次回源仍读不到事务，准备写拒绝时事务已经落库。
type lateCommitStorage struct {
	*MongoCommitter
	hideStatus atomic.Bool
	rejects    atomic.Int32
}

func (s *lateCommitStorage) RejectUnresolvedRemoteCommits(ctx context.Context, commits []entity.RemoteCommit, cause string) (entity.RemoteCommitStatus, error) {
	s.rejects.Add(1)
	return s.MongoCommitter.RejectUnresolvedRemoteCommits(ctx, commits, cause)
}

func (s *lateCommitStorage) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if _, err := s.MongoCommitter.CommitRemoteBatch(ctx, commits); err != nil {
		return nil, err
	}
	return nil, context.DeadlineExceeded
}

func (s *lateCommitStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if _, err := s.CommitRemoteBatch(ctx, []entity.RemoteCommit{commit}); err != nil {
		return entity.RemoteCommitReceipt{}, err
	}
	return entity.RemoteCommitReceipt{}, entity.ErrRemotePersistenceIndeterminate
}

func (s *lateCommitStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	if s.hideStatus.CompareAndSwap(true, false) {
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, nil
	}
	return s.MongoCommitter.CommitStatus(ctx, id)
}

type memoryUnresolvedFixture struct {
	mgr   *Manager
	store *MongoCommitter
	live  *testRemoteEntity
}

func newMemoryUnresolvedFixture(t *testing.T, id int64, wrap func(*MongoCommitter) StorageBackend) memoryUnresolvedFixture {
	t.Helper()
	const kind entity.EntityKind = 119
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	loader := newRemoteTestLoader()
	live := newTestRemoteEntity(id, 1, kind)
	loader.add(live)
	backend, err := NewBackend(loader, wrap(store))
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	return memoryUnresolvedFixture{mgr: mgr, store: store, live: live}
}

// commitMemoryWrite 执行一笔 Durability 0 写，返回 Commit 的错误和提交内容。
func (f memoryUnresolvedFixture) commitMemoryWrite(t *testing.T, tx entity.RemoteTransactionID) ([]entity.RemoteCommit, error) {
	t.Helper()
	batch, err := f.mgr.PrepareRemoteWriteBatch(context.Background(), []int64{f.live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	f.live.dirty.dirty = true
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "memory", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	_, commitErr := batch.Commit(context.Background())
	if err = batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return commits, commitErr
}

// RR-20260926-28：Durability 0 的事务从未到达 Mongo，finalizer 必须以同一事务 _id
// 写入持久拒绝挡住迟到提交，然后回滚、保持隔离（直到重新加载）并释放 gate/锁/写额度。
func TestMemoryNeverCommittedTransientErrorGetsDurableRejection(t *testing.T) {
	f := newMemoryUnresolvedFixture(t, 1777, func(store *MongoCommitter) StorageBackend { return neverReachedStorage{store} })
	tx := remoteTestTxID(91)
	commits, err := f.commitMemoryWrite(t, tx)
	if !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("commit err=%v, want indeterminate transient error", err)
	}

	// 下一写者在 gate 上排队；finalizer 拿到持久结论前不能得到结果。拿到后 gate 释放，
	// 但持有被拒绝内存修改的实例保持隔离，下一写者立即得到 ErrRemoteFenced 而不是超时。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()})
	if err == nil {
		_ = next.Abort(ctx, errors.New("test cleanup"))
		_ = next.Close(ctx)
		t.Fatal("instance holding a rejected mutation was admitted without reload")
	}
	if !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("same entity stays blocked after never-committed memory write: %v (owner=%v slots=%d)",
			err, f.live.RemoteOwnershipState(), len(f.mgr.remote.writeSlots))
	}
	if got := f.live.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
		t.Fatalf("rejected entity state=%v, want quarantined until reload", got)
	}

	durable, err := f.store.CommitStatus(ctx, tx)
	if err != nil || durable.State != entity.RemoteCommitRejected {
		t.Fatalf("durable status=%+v err=%v, want Rejected record", durable, err)
	}
	if _, err = f.store.CommitRemoteBatch(ctx, commits); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("late commit after durable rejection err=%v, want ErrRemoteRejected", err)
	}
	if !f.live.dirty.dirty {
		t.Fatal("rejected memory write was not rolled back")
	}
	if got := f.live.RemoteVersionVector().StateVersion; got != 0 {
		t.Fatalf("rejected write advanced local version to %d", got)
	}
	if status, err := f.mgr.RemoteCommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("tracker status=%+v err=%v", status, err)
	}
	if err = f.mgr.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.mgr.remote.writeSlots) != 0 {
		t.Fatalf("write slots leaked: %d", len(f.mgr.remote.writeSlots))
	}
}

// 拒绝写入撞上已经落库的迟到提交时，必须按读回的真实状态确认，不能回滚已提交数据。
func TestMemoryLateCommitBeatsFinalizerRejection(t *testing.T) {
	storage := &lateCommitStorage{}
	f := newMemoryUnresolvedFixture(t, 1778, func(store *MongoCommitter) StorageBackend {
		storage.MongoCommitter = store
		return storage
	})
	tx := remoteTestTxID(92)
	storage.hideStatus.Store(true)
	if _, err := f.commitMemoryWrite(t, tx); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()})
	if err != nil {
		t.Fatalf("finalizer did not settle late commit: %v", err)
	}
	_ = next.Abort(ctx, errors.New("test cleanup"))
	_ = next.Close(ctx)

	durable, err := f.store.CommitStatus(ctx, tx)
	if err != nil || durable.State != entity.RemoteCommitCommitted {
		t.Fatalf("durable status=%+v err=%v, want committed (published)", durable, err)
	}
	if f.live.dirty.dirty {
		t.Fatal("committed late write was rolled back")
	}
	if got := f.live.RemoteVersionVector().StateVersion; got != 1 {
		t.Fatalf("local version=%d, want acknowledged 1", got)
	}
	if status, err := f.mgr.RemoteCommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("tracker status=%+v err=%v", status, err)
	}
	if got := storage.rejects.Load(); got != 1 {
		t.Fatalf("finalizer rejection attempts=%d, want exactly one (then settle by the read-back state)", got)
	}
}

// walBackedStorage 记录 finalizer 的回源和拒绝调用，用于证明有 WAL 的持久级别不走拒绝。
type walBackedStorage struct {
	*MongoCommitter
	statusCalls chan struct{}
	rejects     atomic.Int32
}

func (s *walBackedStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	select {
	case s.statusCalls <- struct{}{}:
	default:
	}
	return s.MongoCommitter.CommitStatus(ctx, id)
}

func (s *walBackedStorage) RejectUnresolvedRemoteCommits(ctx context.Context, commits []entity.RemoteCommit, cause string) (entity.RemoteCommitStatus, error) {
	s.rejects.Add(1)
	return s.MongoCommitter.RejectUnresolvedRemoteCommits(ctx, commits, cause)
}

// Durability 1/2 的 Unknown 由 WAL 投影收敛（Applied 或投影器的持久拒绝），
// finalizer 不能抢先写拒绝，否则会挡住 WAL 里已经 durable 的事务。
func TestWALDurabilityUnknownIsNotRejectedByFinalizer(t *testing.T) {
	storage := &walBackedStorage{statusCalls: make(chan struct{}, 1)}
	f := newMemoryUnresolvedFixture(t, 1779, func(store *MongoCommitter) StorageBackend {
		storage.MongoCommitter = store
		return storage
	})
	tx := remoteTestTxID(93)
	batch, err := f.mgr.PrepareRemoteWriteBatch(context.Background(), []int64{f.live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	f.live.dirty.dirty = true
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "async", "", true, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err = batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for range 3 { // 至少两轮 finalizer 回源都看到 Unknown
		select {
		case <-storage.statusCalls:
		case <-ctx.Done():
			t.Fatal("finalizer did not poll the durable status")
		}
	}
	if got := storage.rejects.Load(); got != 0 {
		t.Fatalf("finalizer wrote %d rejection(s) for a WAL-backed transaction", got)
	}
	if status, err := f.store.CommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitUnknown {
		t.Fatalf("durable status=%+v err=%v, want no record", status, err)
	}
	// 投影器的持久拒绝仍是它的收尾路径：gate 释放，实体保持隔离。
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	if _, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()}); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("after projector rejection err=%v, want gate released and ErrRemoteFenced", err)
	}
}

func TestMongoRejectUnresolvedDefersToExistingTransaction(t *testing.T) {
	f := newMemoryUnresolvedFixture(t, 1780, func(store *MongoCommitter) StorageBackend { return store })
	ctx := context.Background()
	batch, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	f.live.dirty.dirty = true
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(94), "memory", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	_ = batch.Abort(ctx, errors.New("only the commit content is needed"))
	_ = batch.Close(ctx)

	// 从未到达：第一次写入拒绝，重复调用读回同一条拒绝，不改写它。
	unresolved := commits[0].Clone()
	unresolved.TransactionID = remoteTestTxID(95)
	for range 2 {
		status, err := f.store.RejectUnresolvedRemoteCommits(ctx, []entity.RemoteCommit{unresolved}, "unresolved")
		if err != nil || status.State != entity.RemoteCommitRejected {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	}
	// 提交先落库：撞键后返回真实的 Applied 与回执，记录保持不变。
	receipts, err := f.store.CommitRemoteBatch(ctx, commits)
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.store.RejectUnresolvedRemoteCommits(ctx, commits, "unresolved")
	if err != nil || status.State != entity.RemoteCommitApplied || len(status.Receipts) != 1 || status.Receipts[0].StateVersion != receipts[0].StateVersion {
		t.Fatalf("status=%+v err=%v, want the committed transaction", status, err)
	}
	if durable, err := f.store.CommitStatus(ctx, commits[0].TransactionID); err != nil || durable.State != entity.RemoteCommitApplied {
		t.Fatalf("rejection overwrote committed transaction: %+v err=%v", durable, err)
	}
	altered := commits[0].Clone()
	altered.Mutations[0].Data = []byte("changed")
	if _, err = f.store.RejectUnresolvedRemoteCommits(ctx, []entity.RemoteCommit{altered}, "unresolved"); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("digest collision err=%v", err)
	}
}

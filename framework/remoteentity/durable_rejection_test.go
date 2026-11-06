// 结果未知的持久收敛：Durability 0 由 finalizer 写持久拒绝，WAL 级别交给投影（不产生拒绝），迟到的提交按读回的真实状态确认。

package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
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
	store := newBoundMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
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
	f.live.dirty.set(true)
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

// Durability 0 的事务从未到达 Mongo：finalizer 以同一事务 _id 写入持久拒绝挡住迟到提交，然后回滚、保持隔离（直到重新加载）
// 并释放 gate / 锁 / 写额度。
// 历史：RR-20260926-28。
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
	if !f.live.dirty.Dirty() {
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
	if len(f.mgr.remote.writeSlots) != 0 || f.mgr.Stats().ActiveTransactions != 0 {
		t.Fatalf("rejection leaked capacity: write slots=%d active transactions=%d", len(f.mgr.remote.writeSlots), f.mgr.Stats().ActiveTransactions)
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
	if f.live.dirty.Dirty() {
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

// Durability 1/2/3 的 Unknown 由 WAL 投影收敛（Applied；写权威冲突是投影器 fatal，不产生拒绝），
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
	// RR-20260926-38 起，投影期间 finalizer 不回源；投影器报告结果未知（发布失败把 tracker 置为
	// Indeterminate，与 ApplyRemoteCommits 的瞬时失败相同）后才回源。
	f.mgr.completeRemoteTransaction(tx, entity.RemoteCommitStatus{TransactionID: tx, State: entity.RemoteCommitIndeterminate, Cause: "projector publication failed"})
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
	// WAL 级只会收敛到已提交：投影器把记录写进权威并发布后，finalizer 拿到 Committed、确认并释放 gate，下一写者进入。
	if _, err := f.store.CommitRemoteBatch(ctx, commits); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.ApplyRemoteCommits(ctx, tx, commits); err != nil {
		t.Fatal(err)
	}
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()})
	if err != nil {
		t.Fatalf("after the projector committed the WAL record, next writer err=%v, want admitted", err)
	}
	_ = next.Abort(ctx, errors.New("test cleanup"))
	_ = next.Close(ctx)
	if got := storage.rejects.Load(); got != 0 {
		t.Fatalf("finalizer wrote %d rejection(s) for a WAL-backed transaction", got)
	}
	if status, err := f.mgr.RemoteCommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("tracker status=%+v err=%v, want Committed", status, err)
	}
}

func TestMongoRejectUnresolvedDefersToExistingTransaction(t *testing.T) {
	f := newMemoryUnresolvedFixture(t, 1780, func(store *MongoCommitter) StorageBackend { return store })
	ctx := context.Background()
	batch, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	f.live.dirty.set(true)
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
	// 拒绝记录是持久结论：同一事务的迟到提交读到它只返回 ErrRemoteRejected，内容不同的同 _id 拒绝按 digest 冲突拒绝。
	if _, err = f.store.CommitRemoteBatch(ctx, []entity.RemoteCommit{unresolved}); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("late commit of a rejected transaction err=%v, want ErrRemoteRejected", err)
	}
	reused := unresolved.Clone()
	reused.Mutations[0].Data = []byte("changed")
	if _, err = f.store.RejectUnresolvedRemoteCommits(ctx, []entity.RemoteCommit{reused}, "unresolved"); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("rejection with a different digest on a rejected _id err=%v, want ErrRemoteRejected", err)
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

// 守卫：WAL 级（Durability 1/2/3）正式链路不产生 Remote Rejected。投影时权威已被接管（所有权代际前进，CAS 不命中），
// 真实 nestwal WAL + engine.Projector + engine.MongoStore + 正式 MongoCommitter 上：投影器以 ErrProjectionConflict fatal
// （进程级 fail-stop），不写 Remote 事务记录；tracker 不变成 Rejected；strict / pipelined 的 Commit 只得到结果未知；
// finalizer 投影期限到后回源读到 Unknown，只隔离重试，不为 WAL 事务写持久拒绝。
// 保证范围：正式 MongoStore / MongoCommitter 投影链路上的写权威冲突；自定义投影存储不在此列。
// 历史：维护者 2026-10-14 精简 WAL 级 Rejected 处理（删除 RejectRemoteTransaction / RejectRemoteCommitsInTransaction 注入点）。
func TestWALRemoteAuthorityConflictNeverProducesRejected(t *testing.T) {
	for i, policy := range []nest.DurabilityPolicy{nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		t.Run(policy.String(), func(t *testing.T) {
			storage := &walBackedStorage{statusCalls: make(chan struct{}, 1)}
			f := newMemoryUnresolvedFixture(t, 1790+int64(i), func(store *MongoCommitter) StorageBackend {
				storage.MongoCommitter = store
				return storage
			})
			f.mgr.cfg.FinalizeProjectionTimeout = 50 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			store, err := engine.NewMongoStore(f.store.mongo, engine.MongoStoreConfig{Databases: f.store.databases.Load(), DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if err = store.SetRemoteProjection(f.store, f.mgr); err != nil {
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
				_ = wal.Close(context.Background())
				t.Fatal(err)
			}
			t.Cleanup(func() {
				closing, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = projector.Close(closing)
			})

			tx := remoteTestTxID(0xE0 + byte(i))
			durability := policy.Record()
			batch, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()})
			if err != nil {
				t.Fatal(err)
			}
			f.live.dirty.set(true)
			if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "wal-conflict", "", true, uint8(durability))); err != nil {
				t.Fatal(err)
			}
			commits := batch.Commits()
			// 单实例前提被打破、所有权已在别处前进：所有权代际推进，本批次携带的旧代际在投影时 CAS 不命中。
			takeover := int64(commits[0].MarkerEpoch + 1)
			if _, err = f.store.mongo.(*mongotest.Client).Collection("control", remoteMetaCollection).UpdateOne(ctx, bson.M{"_id": f.live.GUId()}, bson.M{"$set": bson.M{"_owner_epoch": takeover}}); err != nil {
				t.Fatal(err)
			}
			if err = projector.Commit(ctx, remoteCommitRecord(durability, commits)); err != nil {
				t.Fatal(err)
			}
			projector.TransactionReleased(coredata.TransactionID(tx))
			if err = projector.Flush(ctx); !errors.Is(err, engine.ErrProjectionConflict) {
				t.Fatalf("projector flush = %v, want the fatal ErrProjectionConflict", err)
			}
			if stats := projector.Stats(); stats.FatalProjectionConflicts != 1 || stats.Projected != 0 {
				t.Fatalf("projector stats=%+v, want one fatal conflict and nothing projected", stats)
			}

			commitCtx, commitCancel := context.WithTimeout(ctx, 200*time.Millisecond)
			_, commitErr := batch.Commit(commitCtx)
			commitCancel()
			switch policy {
			case nest.DurabilityAsync:
				if commitErr != nil {
					t.Fatalf("async commit err=%v, want the speculative success", commitErr)
				}
			default:
				if !errors.Is(commitErr, entity.ErrRemotePersistenceIndeterminate) {
					t.Fatalf("%s commit err=%v, want an unknown outcome (ErrRemotePersistenceIndeterminate)", policy, commitErr)
				}
			}
			if err = batch.Close(ctx); err != nil {
				t.Fatal(err)
			}
			for range 3 { // 投影期限过后 finalizer 至少两轮回源都读到 Unknown
				select {
				case <-storage.statusCalls:
				case <-ctx.Done():
					t.Fatal("finalizer did not poll the durable status after the projection deadline")
				}
			}
			if state := f.mgr.localRemoteCommitState(tx); state == entity.RemoteCommitRejected {
				t.Fatalf("tracker state=%v: the WAL chain produced a Remote Rejected", state)
			}
			if got := storage.rejects.Load(); got != 0 {
				t.Fatalf("finalizer wrote %d rejection(s) for a WAL-backed transaction", got)
			}
			if status, err := f.store.CommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitUnknown {
				t.Fatalf("durable status=%+v err=%v, want no Remote transaction record", status, err)
			}
			if authority, err := f.store.readAuthority(ctx, f.live.GUId()); err != nil || authority.Version != 0 {
				t.Fatalf("authority=%+v err=%v, want the conflicting write not applied (version 0)", authority, err)
			}
			if got := f.live.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
				t.Fatalf("entity state=%v, want quarantined while the outcome stays unknown", got)
			}
		})
	}
}

// 新建实体与冷加载补空的 DAO 以整文档登记，掩码是 dataengine.AllFields（最高位为 1）。BSON 只有有符号 64 位整数，
// Remote 事务记录照原样存 uint64 掩码会在投影时 “overflows int64”、记录已持久而投影永远失败（R15 之后每个新建 Remote
// 实体的第一笔写都这样）。记录按位存掩码，读回与写入相同。
func TestMongoCommitterStoresEveryMutationMaskBitForBit(t *testing.T) {
	f := newMemoryUnresolvedFixture(t, 1790, func(store *MongoCommitter) StorageBackend { return store })
	ctx := context.Background()
	batch, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	f.live.dirty.set(true)
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(96), "memory", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	_ = batch.Abort(ctx, errors.New("only the commit content is needed"))
	_ = batch.Close(ctx)
	if len(commits) != 1 || len(commits[0].Mutations) == 0 {
		t.Fatalf("commits=%+v, want one commit with a data mutation", commits)
	}
	masks := []uint64{coredata.AllFields, 1 << 63, 0b101}
	for i := range commits[0].Mutations {
		commits[0].Mutations[i].Mask = masks[i%len(masks)]
	}
	if _, err := f.store.CommitRemoteBatch(ctx, commits); err != nil {
		t.Fatalf("commit with a whole-document mask: %v", err)
	}
	status, err := f.store.CommitStatus(ctx, commits[0].TransactionID)
	if err != nil || len(status.Commits) != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	for i, mutation := range status.Commits[0].Mutations {
		if want := commits[0].Mutations[i].Mask; mutation.Mask != want {
			t.Fatalf("mutation %d mask read back %#x, want %#x", i, mutation.Mask, want)
		}
	}
}

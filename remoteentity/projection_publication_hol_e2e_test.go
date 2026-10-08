package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20261006-69（F05-2）：Remote 事务的发布失败不能让 owner 的 WAL 投影队头阻塞。
//
// 修前 ProjectFenced 在 Mongo 事务里把 Remote 提交写成 Applied 之后调 ApplyRemoteCommits 发布；发布失败
// （同步总线不可用、标记已发布失败）时它返回 ErrRemotePersistenceIndeterminate，投影器把这当普通错误退避重试
// 同一条记录，其后所有 WAL 记录——包括普通 DAO——都排队等总线恢复，strict 写在等待期间超时成结果未知。
// 此时 Mongo 已是 Applied，发布与投影本可解耦。承诺：发布失败不挡投影（这条记为已投影，后面的记录照常投影），
// 发布由 owner 自己的补发循环（与启动时的 RecoverOutbox 同一路径）在恢复后补上——即使这个事务没有写者在等
// （重放），也不需要等到下次重启。
type failingPublicationStorage struct {
	*MongoCommitter
	target entity.RemoteTransactionID
	fail   atomic.Bool
	marks  atomic.Int32
}

var errInjectedPublication = errors.New("injected publication failure")

func (s *failingPublicationStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	if id == s.target {
		s.marks.Add(1)
		if s.fail.Load() {
			return errInjectedPublication
		}
	}
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

func TestPublicationFailureDoesNotBlockLaterProjection(t *testing.T) {
	const kind entity.EntityKind = 123
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mongo := newRemoteMongoFake()
	committer := NewMongoCommitter(mongo, "control", 1000, 0)
	tx1 := remoteTestTxID(168)
	storage := &failingPublicationStorage{MongoCommitter: committer, target: tx1}
	storage.fail.Store(true)
	loader := newRemoteTestLoader()
	live := newTestRemoteEntity(6811, 1, kind)
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
		CloseWAL: true, IdlePoll: 10 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = projector.Close(closing)
	})

	// tx1：Remote 记录进 WAL；写者不 Close（模拟重放：没有 finalizer 项在等它）。
	batch1, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.set(true)
	if err = batch1.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx1, "remote-hol", "", true, uint8(nest.DurabilityAsync.Record()))); err != nil {
		t.Fatal(err)
	}
	if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync.Record(), batch1.Commits())); err != nil {
		t.Fatal(err)
	}
	if _, err = batch1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(coredata.TransactionID(tx1))

	// 普通 DAO 记录排在 tx1 之后。
	var plainID coredata.TransactionID
	plainID[0], plainID[15] = 0x68, 2
	payload, _ := bson.Marshal(bson.M{"_id": int64(6812), "value": int32(1)})
	plain := coredata.CommitRecord{ID: plainID, Durability: nest.DurabilityStrict.Record(), Mutations: []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: "game", Resource: "rr68_plain", ID: 6812},
		Kind: coredata.MutationPut, NextVersion: 1, Data: payload,
	}}}
	if err = projector.Commit(ctx, plain); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(plainID)

	deadline := time.Now().Add(2 * time.Second)
	for projector.Stats().WALUnacked != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stats := projector.Stats(); stats.WALUnacked != 0 {
		t.Fatalf("a failing Remote publication blocked the projection of later records: stats=%+v publish attempts=%d", stats, storage.marks.Load())
	}
	if status, err := committer.CommitStatus(ctx, tx1); err != nil || status.State != entity.RemoteCommitApplied {
		t.Fatalf("tx1 durable status=%+v err=%v, want Applied (persisted, publication pending)", status, err)
	}

	// 总线恢复：补发循环发布 tx1，持久状态变为 Committed——没有写者 Close，也没有重启。
	storage.fail.Store(false)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, err := committer.CommitStatus(ctx, tx1); err == nil && status.State == entity.RemoteCommitCommitted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status, err := committer.CommitStatus(ctx, tx1); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("tx1 was never published after the bus recovered: status=%+v err=%v", status, err)
	}
	if stats := projector.Stats(); stats.FatalProjectionConflicts != 0 {
		t.Fatalf("projector stats=%+v", stats)
	}
	if err = batch1.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = mgr.FlushRemoteTransaction(ctx, tx1); err != nil {
		t.Fatalf("tx1 after recovery: %v", err)
	}
}

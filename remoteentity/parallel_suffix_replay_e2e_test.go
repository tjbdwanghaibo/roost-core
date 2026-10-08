package remoteentity

import (
	"context"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// OPEN-ITEMS B08：D2 并行窗口的“成功后缀重放”端到端（RR-20260926-11 残留，RR-21 之后正式装配默认并行）。
//
// 真实 nestwal WAL（V2）+ engine.Projector（RemoteProjectionWorkers=8）+ engine.MongoStore + mongotest 伪 Mongo，
// Remote 走正式 Backend + MongoCommitter 权威。同一并行窗口里两笔独立实体的 Remote 事务：
// 前缀 tx1（实体 X）发布一次瞬时失败，后缀 tx2（实体 Y）已成功持久并发布；窗口只确认连续成功前缀（0 条），
// 后缀在重试时按持久身份重放。重放之前 Y 已被下一写者拿到更大的 fence。
//
// 验证：重试后 WAL 全部 ack、无 fatal、失败计数恰为 1；后缀重放不被新 fence 拒绝、不回退 Y 的 fence 与版本，
// tx2 的 tracker 保持 Committed；Projected 计入重放（3 = tx1 一次 + tx2 两次），所以不能用 committed == projected 判一致；
// 最终以权威版本、持久回执与下一写者的正常提交共同确认。
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

// CommitRemote 让前缀的第一次回执读回停在注入点、随后失败（结果未知，投影器重试整个窗口）。RR-20261006-69 更正：
// 之前注入的是前缀的 MarkRemoteCommitPublished 失败；那是“已 Applied、只是发布失败”，现在投影器记为已投影、交给补发
// 循环，不再触发窗口重试。
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
